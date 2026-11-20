package scrcpy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/internal/probetest"
	"github.com/devicelab-dev/maestro-runner/pkg/device"
)

// U35: the recorded Pixel 8 sockets, served by a fake adb forward, run
// through a Session and a Recorder into an MP4 that ffprobe decodes as
// H.264 at the recorded size, with AAC audio.
func TestRecordedFixture_EndToEnd(t *testing.T) {
	video, audio := readFixture(t, "video.bin"), readFixture(t, "audio.bin")
	var want collector
	_ = readVideo(bytes.NewReader(video), want.emit)
	_ = readAudio(bytes.NewReader(audio), want.emit)
	frames, aacFrames := want.count(capture.Video), want.count(capture.Audio)
	cfg := want.packets()[0].Video

	// The dumps are replayed in real time, each media packet when its PTS
	// comes due on a clock both sockets share, as the device sent them.
	start := time.Now()
	base := firstPTS(video)
	dev := &fakeDevice{serial: "FIXTURE", shellOut: ServerSHA256, serve: func(i int, c net.Conn) {
		defer c.Close()
		switch i {
		case 0:
			_, _ = c.Write([]byte{0})
			paced(c, video, start, base)
		case 1:
			paced(c, audio, start, base)
		}
		_, _ = io.Copy(io.Discard, c)
	}}
	useFakeServer(t, "wait")
	src, _ := newTestSource(t, dev, 36)
	sess := capture.NewSession(src)
	defer sess.Close()
	rec, err := capture.NewRecorder(sess, t.TempDir(), capture.Options{FPS: 15, Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := sess.Subscribe(capture.Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	var got collector
	timeout := time.After(10 * time.Second)
	for got.count(capture.Video) < frames || got.count(capture.Audio) < aacFrames {
		select {
		case p, ok := <-sub.Packets():
			if !ok {
				t.Fatalf("subscriber ended: %v", sub.Err())
			}
			got.emit(p)
		case <-timeout:
			t.Fatalf("got %d/%d frames, %d/%d audio frames", got.count(capture.Video), frames, got.count(capture.Audio), aacFrames)
		}
	}
	sub.Close()
	dest := filepath.Join(t.TempDir(), "pixel.mp4")
	if err := rec.Stop(dest); err != nil {
		t.Fatal(err)
	}
	res := probetest.Probe(t, dest)
	v, ok := res.Find("h264")
	if !ok || v.Width != cfg.Width || v.Height != cfg.Height || v.FrameCount() != frames {
		t.Errorf("video %+v, want h264 %dx%d with %d frames", v, cfg.Width, cfg.Height, frames)
	}
	a, ok := res.Find("aac")
	if !ok || a.SampleRate != "48000" || a.Channels != 2 {
		t.Errorf("audio %+v, want aac 48000 Hz stereo", a)
	}
}

// TestRecordFixture records fixtures/video.bin and fixtures/audio.bin from
// a real device: about 3 s of both sockets, from the codec id on, cut at
// the last whole packet. It runs only with MAESTRO_D_RECORD_SCRCPY_FIXTURE=1
// and ANDROID_SERIAL naming the device, and swipes the screen while it
// records so the video carries more than one frame.
func TestRecordFixture(t *testing.T) {
	if os.Getenv("MAESTRO_D_RECORD_SCRCPY_FIXTURE") != "1" {
		t.Skip("set MAESTRO_D_RECORD_SCRCPY_FIXTURE=1 and ANDROID_SERIAL to record the fixtures from a device")
	}
	serial := os.Getenv("ANDROID_SERIAL")
	if serial == "" {
		t.Fatal("ANDROID_SERIAL names the device to record")
	}
	dev, err := device.New(serial)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dev.Shell("getprop ro.build.version.sdk")
	if err != nil {
		t.Fatal(err)
	}
	sdk, _ := strconv.Atoi(strings.TrimSpace(out))
	src := New(dev, Config{SDK: sdk, Server: serverJar})
	src.logf = t.Logf
	var mu sync.Mutex
	dumps := map[string]*bytes.Buffer{"video": {}, "audio": {}}
	src.dump = func(stream string) io.Writer { return lockedWriter{&mu, dumps[stream]} }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		for _, dir := range []string{"1000 900 1000 300", "1000 300 1000 900", "1000 900 1000 300"} {
			time.Sleep(700 * time.Millisecond)
			_ = exec.Command(dev.ADBPath(), "-s", serial, "shell", "input swipe "+dir+" 300").Run()
		}
	}()
	var c collector
	if err := src.Run(ctx, capture.Options{FPS: 15, Audio: true}, c.emit); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for stream, b := range dumps {
		n := wholePackets(b.Bytes())
		name := filepath.Join("fixtures", stream+".bin")
		if err := os.WriteFile(name, b.Bytes()[:n], 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d bytes", name, n)
	}
	t.Logf("%d video configs, %d frames, %d audio configs, %d audio frames",
		c.count(capture.VideoConfig), c.count(capture.Video), c.count(capture.AudioConfig), c.count(capture.Audio))
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// firstPTS is the PTS of a socket dump's first media packet.
func firstPTS(b []byte) uint64 {
	for n := 4; n+12 <= len(b); {
		v := binary.BigEndian.Uint64(b[n : n+8])
		if v&flagSession != 0 {
			n += 12
			continue
		}
		if v&flagConfig == 0 {
			return v & ptsMask
		}
		n += 12 + int(binary.BigEndian.Uint32(b[n+8:n+12]))
	}
	return 0
}

// paced writes a socket dump to w: the codec id, session and config
// packets at once, each media packet once start plus its PTS minus base
// has passed.
func paced(w io.Writer, b []byte, start time.Time, base uint64) {
	_, _ = w.Write(b[:4])
	for n := 4; n+12 <= len(b); {
		v := binary.BigEndian.Uint64(b[n : n+8])
		size := 0
		if v&flagSession == 0 {
			size = int(binary.BigEndian.Uint32(b[n+8 : n+12]))
			if v&flagConfig == 0 && v&ptsMask > base {
				time.Sleep(time.Until(start.Add(time.Duration(v&ptsMask-base) * time.Microsecond)))
			}
		}
		if _, err := w.Write(b[n : n+12+size]); err != nil {
			return
		}
		n += 12 + size
	}
}

// wholePackets is the length of b's codec id and whole packets.
func wholePackets(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	n := 4
	for n+12 <= len(b) {
		size := 0
		if binary.BigEndian.Uint64(b[n:n+8])&flagSession == 0 {
			size = int(binary.BigEndian.Uint32(b[n+8 : n+12]))
		}
		if n+12+size > len(b) {
			break
		}
		n += 12 + size
	}
	return n
}

func TestWholePackets(t *testing.T) {
	v := syntheticVideo(t, 2)
	if wholePackets(v) != len(v) || wholePackets(v[:len(v)-1]) >= len(v) || wholePackets(v[:3]) != 0 {
		t.Error("wholePackets")
	}
}
