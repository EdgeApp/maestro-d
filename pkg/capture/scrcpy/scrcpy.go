// Package scrcpy is the Android capture.Source: it pushes the pinned
// scrcpy-server (drivers/android/scrcpy-server-v5.0.1, Apache-2.0, see
// drivers/android/LICENSE-scrcpy) to the device, starts it over
// `adb shell` with video (H.264) and, where the device can, audio (AAC),
// connects to its sockets through an adb forward, and turns its stream
// into capture packets. Nothing is decoded or re-encoded on the host.
//
// The source runs on demand: a capture.Session starts it for its first
// subscriber, at that subscriber's frame rate (a ceiling the device's
// encoder honours), and stops it a while after the last one leaves.
package scrcpy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/config"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// The pinned server.
const (
	// ServerVersion is the scrcpy-server version; the server refuses a
	// client that names another.
	ServerVersion = "5.0.1"
	// ServerFile is the jar's file name in the drivers directory.
	ServerFile = "scrcpy-server-v" + ServerVersion
	// ServerSHA256 is the jar's sha256 (the v5.0.1 release asset).
	ServerSHA256 = "764eb6f79811d5211fe9df341120882ba9994c7a61b897d7bf3fb662e53bc536"
	// RemoteJar is where the jar is pushed on the device.
	RemoteJar = "/data/local/tmp/maestro-d-scrcpy-server.jar"
)

// Android SDK levels the source distinguishes.
const (
	minSDK            = 21 // Android 5.0: MediaCodec surface input
	minAudioSDK       = 30 // Android 11: audio capture
	playbackSourceSDK = 33 // Android 13: audio_source=playback with audio_dup
)

// Timings of the connect loop.
var (
	connectTimeout = 5 * time.Second
	connectRetry   = 100 * time.Millisecond
	// exitWait is how long a stopped server gets to print its last line.
	exitWait = time.Second
)

// commandContext starts the long-running `adb shell` that runs the
// server; tests replace it with a fake adb.
var commandContext = exec.CommandContext

// Device is the adb access the source needs. *device.AndroidDevice
// satisfies it.
type Device interface {
	Serial() string
	ADBPath() string
	Shell(cmd string) (string, error)
	Push(local, remote string) error
	ForwardTCPToAbstractSocket(localPort int, name string) error
	RemoveTCPForward(localPort int) error
}

// Config configures a Source.
type Config struct {
	// SDK is the device's ro.build.version.sdk. It decides audio (below):
	// 0 means unknown and is treated as a current Android.
	SDK int
	// Server is the host path of the server jar; empty means
	// <drivers>/android/scrcpy-server-v5.0.1.
	Server string
}

// Source captures an Android device through scrcpy-server.
type Source struct {
	dev    Device
	sdk    int
	server string
	// logf writes the server's output to the daemon log; tests replace it.
	logf func(format string, args ...any)
	// dump, when set (the fixture recorder), receives each socket's bytes
	// after the dummy byte: "video" and "audio".
	dump func(stream string) io.Writer
}

// New returns an on-demand source for dev.
func New(dev Device, cfg Config) *Source {
	server := cfg.Server
	if server == "" {
		server = filepath.Join(config.GetDriversDir("android"), ServerFile)
	}
	return &Source{dev: dev, sdk: cfg.SDK, server: server, logf: logger.Info}
}

// Mode is capture.OnDemand.
func (*Source) Mode() capture.Mode { return capture.OnDemand }

// FPSControl is true: max_fps is a ceiling the device's encoder honours.
func (*Source) FPSControl() bool { return true }

// PlansAudio reports whether a run started for first carries audio. On
// Android 13 and later it always does, through the playback source, which
// leaves the device's speaker playing. On Android 11 and 12 the only
// source mutes the speaker while it runs, so a run carries audio only when
// its first subscriber asked for it. Before Android 11 there is none.
func (s *Source) PlansAudio(first capture.Options) bool {
	switch {
	case s.sdk == 0 || s.sdk >= playbackSourceSDK:
		return true
	case s.sdk >= minAudioSDK:
		return first.Audio
	}
	return false
}

// repeatFrameAfter is the encoder's KEY_REPEAT_PREVIOUS_FRAME_AFTER, in µs,
// for a run at fps: scrcpy-server's own 100 ms, or the frame interval when
// that is longer. The server sets 100 ms and then applies
// video_codec_options over it, so a still screen's repeated frames never
// come faster than the requested rate (they came at 10 fps at fps 3).
func repeatFrameAfter(fps int) int {
	const scrcpyDefault = 100_000
	if fps <= 0 {
		return scrcpyDefault
	}
	return max(scrcpyDefault, 1_000_000/fps)
}

// serverArgs are the server's key=value options for a run.
func (s *Source) serverArgs(scid uint32, fps int, audio bool) string {
	args := []string{
		fmt.Sprintf("scid=%08x", scid),
		"log_level=info",
		"tunnel_forward=true",
		"control=false",
		"cleanup=false",
		"video_codec=h264",
		"max_fps=" + strconv.Itoa(fps),
		fmt.Sprintf("video_codec_options=i-frame-interval:int=1,repeat-previous-frame-after:long=%d", repeatFrameAfter(fps)),
		"audio=" + strconv.FormatBool(audio),
		"audio_codec=aac",
	}
	if audio && (s.sdk == 0 || s.sdk >= playbackSourceSDK) {
		args = append(args, "audio_source=playback", "audio_dup=true")
	}
	return strings.Join(append(args,
		"send_device_meta=false",
		"send_stream_meta=true",
		"send_frame_meta=true",
	), " ")
}

func failed(format string, args ...any) error {
	return &capture.Unavailable{Reason: capture.ReasonScrcpyFailed, Message: fmt.Sprintf(format, args...)}
}

// pushed holds, per device serial, whether the device holds the pinned
// jar. pushedMu guards the map only: each device's check and push hold its
// own lock, so a slow device (wireless adb, a first push) never holds up
// capture start on another.
var (
	pushedMu sync.Mutex
	pushed   = map[string]*deviceJar{}
)

// deviceJar is one device's jar state; mu is held across its check and
// push.
type deviceJar struct {
	mu sync.Mutex
	ok bool
}

// jarOf returns serial's jar state, made on first use.
func jarOf(serial string) *deviceJar {
	pushedMu.Lock()
	defer pushedMu.Unlock()
	j := pushed[serial]
	if j == nil {
		j = &deviceJar{}
		pushed[serial] = j
	}
	return j
}

// ensureServer checks the host jar against the pinned sha256 and pushes it
// to the device unless the device's copy already matches. A device found
// up to date is not asked again until a run on it fails (forgetServer).
func (s *Source) ensureServer() error {
	data, err := os.ReadFile(s.server)
	if err != nil {
		return failed("read the scrcpy-server jar: %v", err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != ServerSHA256 {
		return failed("%s has sha256 %s, want %s (scrcpy-server %s)", s.server, got, ServerSHA256, ServerVersion)
	}
	serial := s.dev.Serial()
	j := jarOf(serial)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.ok {
		return nil
	}
	out, err := s.dev.Shell("sha256sum " + RemoteJar + " 2>/dev/null")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(out), ServerSHA256) {
		if err := s.dev.Push(s.server, RemoteJar); err != nil {
			return failed("push scrcpy-server to %s: %v", serial, err)
		}
	}
	j.ok = true
	return nil
}

// forgetServer drops the device from pushed, so the next run checks the
// device's jar again and pushes it when it is gone (an emulator wiped, or
// /data/local/tmp cleared, since the last check).
func (s *Source) forgetServer() {
	j := jarOf(s.dev.Serial())
	j.mu.Lock()
	j.ok = false
	j.mu.Unlock()
}

// randomSCID is a random 31-bit session id; it names the device socket.
func randomSCID() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:]) & 0x7fffffff
}

// freePort returns a TCP port on 127.0.0.1 that was free a moment ago.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Run captures until ctx ends (it returns nil then) or the server fails
// (an *capture.Unavailable naming scrcpy-failed, with the server's last
// log line). Below SDK 21 it answers android-too-old at once. A failed run
// forgets that the device holds the jar, so the next run checks again.
func (s *Source) Run(ctx context.Context, opts capture.Options, emit func(capture.Packet)) error {
	err := s.run(ctx, opts, emit)
	if err != nil {
		s.forgetServer()
	}
	return err
}

func (s *Source) run(ctx context.Context, opts capture.Options, emit func(capture.Packet)) error {
	if s.sdk > 0 && s.sdk < minSDK {
		return &capture.Unavailable{Reason: capture.ReasonAndroidTooOld,
			Message: fmt.Sprintf("the device runs Android SDK %d; screen capture needs SDK %d (Android 5.0) or later", s.sdk, minSDK)}
	}
	opts, err := opts.Normalize()
	if err != nil {
		return err
	}
	if err := s.ensureServer(); err != nil {
		return err
	}
	serial := s.dev.Serial()
	scid := randomSCID()
	port, err := freePort()
	if err != nil {
		return failed("pick a local port: %v", err)
	}
	if err := s.dev.ForwardTCPToAbstractSocket(port, fmt.Sprintf("scrcpy_%08x", scid)); err != nil {
		return failed("adb forward: %v", err)
	}
	defer func() { _ = s.dev.RemoveTCPForward(port) }()

	audio := s.PlansAudio(opts)
	log := &serverLog{prefix: "[scrcpy " + serial + "] ", logf: s.logf}
	cmd := commandContext(ctx, s.dev.ADBPath(), "-s", serial, "shell",
		"CLASSPATH="+RemoteJar+" app_process / com.genymobile.scrcpy.Server "+ServerVersion+" "+s.serverArgs(scid, opts.FPS, audio))
	cmd.Stdout, cmd.Stderr = log, log
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = exitWait
	s.logf("[scrcpy %s] starting scrcpy-server %s (max_fps=%d audio=%t)", serial, ServerVersion, opts.FPS, audio)
	if err := cmd.Start(); err != nil {
		return failed("start scrcpy-server: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	defer func() {
		_ = killProcessGroup(cmd)
		<-exited
	}()
	serverEnded := func(what string, err error) error {
		select {
		case <-exited:
		case <-time.After(exitWait):
		}
		last := log.Last()
		if last == "" {
			last = "no output"
		}
		return failed("scrcpy-server on %s %s (%v); its last line: %s", serial, what, err, last)
	}

	video, err := connect(ctx, port, exited)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return serverEnded("did not connect", err)
	}
	conns := []net.Conn{video}
	var audioConn net.Conn
	if audio {
		// The server accepts the audio socket before it streams anything, so
		// a run without it would never send video: retry like the video
		// socket, then fail the run (the next subscriber starts it again).
		if audioConn, err = dialAudio(ctx, port, exited); err != nil {
			_ = video.Close()
			if ctx.Err() != nil {
				return nil
			}
			return serverEnded("did not connect the audio socket", err)
		}
		conns = append(conns, audioConn)
	}
	// The sockets close when ctx ends or Run returns; the audio reader is
	// waited for before Run returns.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer func() {
		close(stop)
		wg.Wait()
	}()
	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
		}
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return ctx.Err() != nil
		}
	}

	if audioConn != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := readAudio(s.tee("audio", audioConn), emit)
			if stopped() {
				return
			}
			var none errNoAudio
			if errors.As(err, &none) {
				s.logf("[scrcpy %s] no audio: %v", serial, err)
				emit(capture.Packet{Kind: capture.AudioConfig})
				return
			}
			s.logf("[scrcpy %s] audio ended: %v", serial, err)
		}()
	}
	err = readVideo(s.tee("video", video), emit)
	if ctx.Err() != nil {
		return nil
	}
	return serverEnded("stopped", err)
}

// tee copies a socket's bytes to the fixture recorder when one is set.
func (s *Source) tee(stream string, r io.Reader) io.Reader {
	if s.dump == nil {
		return r
	}
	return io.TeeReader(r, s.dump(stream))
}

func addr(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// connect dials the forward until the server's dummy byte arrives on the
// first socket. Until the server listens, the adb forward accepts and at
// once closes each connection; the loop retries every 100 ms for up to
// 5 s, and gives up early when ctx ends or the server exits.
func connect(ctx context.Context, port int, exited <-chan struct{}) (net.Conn, error) {
	deadline := time.Now().Add(connectTimeout)
	var last error
	for {
		conn, err := net.DialTimeout("tcp", addr(port), time.Until(deadline))
		if err == nil {
			if err = readDummy(ctx, conn, deadline, exited); err == nil {
				return conn, nil
			}
			_ = conn.Close()
		}
		last = err
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("no connection within %v: %w", connectTimeout, last)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-exited:
			return nil, fmt.Errorf("the server exited before it listened: %w", last)
		case <-time.After(connectRetry):
		}
	}
}

// dialAudio dials the forward for the audio socket, which carries no
// dummy byte, retrying every 100 ms for up to 5 s; it gives up early when
// ctx ends or the server exits.
func dialAudio(ctx context.Context, port int, exited <-chan struct{}) (net.Conn, error) {
	deadline := time.Now().Add(connectTimeout)
	for {
		conn, err := net.DialTimeout("tcp", addr(port), time.Until(deadline))
		if err == nil {
			return conn, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("no connection within %v: %w", connectTimeout, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-exited:
			return nil, fmt.Errorf("the server exited: %w", err)
		case <-time.After(connectRetry):
		}
	}
}

// readDummy reads the one byte the server writes on accepting the first
// socket. ctx ending or the server exiting closes the connection.
func readDummy(ctx context.Context, conn net.Conn, deadline time.Time, exited <-chan struct{}) error {
	_ = conn.SetReadDeadline(deadline)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-exited:
			_ = conn.Close()
		case <-done:
		}
	}()
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		return err
	}
	return conn.SetReadDeadline(time.Time{})
}

// serverLog writes the server's output to the daemon log a line at a
// time and remembers the last non-empty line.
type serverLog struct {
	prefix string
	logf   func(format string, args ...any)

	mu      sync.Mutex
	partial []byte
	last    string
}

func (l *serverLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	for {
		i := strings.IndexByte(string(l.partial), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := strings.TrimRight(string(l.partial[:i]), "\r")
		l.partial = l.partial[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		l.last = line
		l.logf("%s%s", l.prefix, line)
	}
}

// Last is the last non-empty line, or the unterminated tail.
func (l *serverLog) Last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if tail := strings.TrimSpace(string(l.partial)); tail != "" {
		return tail
	}
	return l.last
}
