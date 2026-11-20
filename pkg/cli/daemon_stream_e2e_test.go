package cli

// End-to-end tests of screen streams on mock devices (I1–I6): the stream
// command, its refusals, concurrent viewers, steps beside a stream, the
// REST route and viewer page on a TCP daemon, and recordings.

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// probeStream is one stream of a file as ffprobe reports it.
type probeStream struct {
	CodecName string `json:"codec_name"`
	CodecTag  string `json:"codec_tag_string"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Frames    string `json:"nb_read_frames"`
}

// ffprobe returns a file's streams, or skips the test without ffprobe
// (failing instead when MAESTRO_D_REQUIRE_FFPROBE=1).
func ffprobe(t *testing.T, file string, extra ...string) []probeStream {
	t.Helper()
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		if os.Getenv("MAESTRO_D_REQUIRE_FFPROBE") == "1" {
			t.Fatal("ffprobe is required (MAESTRO_D_REQUIRE_FFPROBE=1)")
		}
		t.Skip("ffprobe not on PATH")
	}
	args := append([]string{"-v", "error", "-count_frames", "-show_entries",
		"stream=codec_name,codec_tag_string,width,height,nb_read_frames", "-of", "json"}, extra...)
	out, err := exec.Command(bin, append(args, file)...).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", file, err)
	}
	var res struct {
		Streams []probeStream `json:"streams"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	return res.Streams
}

// videoOf checks a file holds 640x360 H.264 (the mock's clip) with at
// least min frames, and returns its streams.
func videoOf(t *testing.T, file string, min int) []probeStream {
	t.Helper()
	streams := ffprobe(t, file)
	if len(streams) == 0 || streams[0].CodecName != "h264" || streams[0].CodecTag != "avc3" ||
		streams[0].Width != 640 || streams[0].Height != 360 {
		t.Fatalf("%s: %+v", file, streams)
	}
	if n := atoi(streams[0].Frames); n < min {
		t.Fatalf("%s: %d frames, want at least %d", file, n, min)
	}
	return streams
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// serveTCP starts `serve` with a TCP listener and a token, and returns its
// address.
func (e *e2e) serveTCP(name, token string, extraEnv ...string) string {
	e.t.Helper()
	serve := exec.Command(e.bin, "serve", "--daemon", name, "--http", "127.0.0.1:0", "--token", token, "--idle-timeout", "0")
	serve.Dir = e.cwd
	serve.Env = e.env(extraEnv...)
	stdout, w, err := os.Pipe()
	if err != nil {
		e.t.Fatal(err)
	}
	serve.Stdout = w
	if err := serve.Start(); err != nil {
		e.t.Fatal(err)
	}
	_ = w.Close()
	exited := make(chan struct{})
	go func() { _ = serve.Wait(); close(exited) }()
	e.t.Cleanup(func() {
		_ = serve.Process.Kill()
		<-exited
	})
	addr := ""
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "READY") {
			for _, f := range strings.Fields(line) {
				if strings.HasPrefix(f, "http=") {
					addr = strings.TrimPrefix(f, "http=")
				}
			}
			break
		}
	}
	if addr == "" {
		e.t.Fatal("serve printed no READY line with an http address")
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return addr
}

// httpGet sends a GET with an optional bearer token and returns the
// answer and its body.
func httpGet(t *testing.T, url, token string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Maestro-Api-Version", daemon.APIVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// waitStarted waits until a stream's -o file has its first bytes (the
// command makes the file with them).
func waitStarted(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream to %s never started", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestE2E_Stream(t *testing.T) {
	e := newE2E(t)
	addr := e.serveTCP("ss-e2e", "e2e-token", "MAESTRO_D_MOCK_DEVICES=mock-1,mock-2")
	d := []string{"--daemon", "ss-e2e"}
	run := func(args ...string) result { e.t.Helper(); return e.run(append(args, d...)...) }
	ok := func(args ...string) result { e.t.Helper(); return e.ok(append(args, d...)...) }
	file := func(name string) string { return filepath.Join(e.cwd, name) }
	ok("attach", "--device", "mock-1", "--platform", "mock")

	t.Run("I2 audio beside steps", func(t *testing.T) {
		var wg sync.WaitGroup
		codes := make(chan int, 5)
		var r result
		wg.Add(1)
		go func() {
			defer wg.Done()
			r = run("stream", "--device", "mock-1", "--audio", "--duration", "4", "-o", file("audio.mp4"))
		}()
		waitStarted(t, file("audio.mp4"))
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes <- run("tapOn", "x", "--device", "mock-1").code
			}()
		}
		wg.Wait()
		close(codes)
		for c := range codes {
			if c != 0 {
				t.Fatalf("a step beside the stream: exit %d", c)
			}
		}
		if r.code != 0 || !strings.Contains(r.stderr, "audio=true") {
			t.Fatalf("stream: exit %d\n%s", r.code, r.stderr)
		}
		streams := videoOf(t, file("audio.mp4"), 30)
		if len(streams) != 2 || streams[1].CodecName != "aac" {
			t.Fatalf("no aac: %+v", streams)
		}
	})

	t.Run("I1 file and stdout", func(t *testing.T) {
		r := ok("stream", "--device", "mock-1", "--fps", "15", "--duration", "3", "-o", file("a.mp4"))
		if !strings.Contains(r.stderr, "stream: avc3.") || !strings.Contains(r.stderr, "fps=15 audio=false") {
			t.Fatalf("stderr %q", r.stderr)
		}
		videoOf(t, file("a.mp4"), 30)
		r = ok("stream", "--device", "mock-1", "--duration", "2")
		if err := os.WriteFile(file("b.mp4"), []byte(r.stdout), 0o644); err != nil {
			t.Fatal(err)
		}
		videoOf(t, file("b.mp4"), 15)
	})

	t.Run("I3 concurrent streams", func(t *testing.T) {
		var wg sync.WaitGroup
		var first result
		wg.Add(1)
		go func() {
			defer wg.Done()
			first = run("stream", "--device", "mock-1", "--fps", "15", "--duration", "4", "-o", file("c1.mp4"))
		}()
		waitStarted(t, file("c1.mp4"))
		second := run("stream", "--device", "mock-1", "--fps", "60", "--duration", "2", "-o", file("c2.mp4"))
		wg.Wait()
		if first.code != 0 || second.code != 0 || !strings.Contains(second.stderr, "fps=15") {
			t.Fatalf("first exit %d, second exit %d %q", first.code, second.code, second.stderr)
		}
		videoOf(t, file("c1.mp4"), 30)
		videoOf(t, file("c2.mp4"), 15)
		// The first field is key_frame; a frame with side data (the clip's
		// first keyframe carries an x264 SEI) prints an empty field after it.
		out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v", "-read_intervals", "%+#1",
			"-show_entries", "frame=key_frame", "-of", "csv=p=0", file("c2.mp4")).Output()
		if err != nil || strings.Split(strings.TrimSpace(string(out)), ",")[0] != "1" {
			t.Fatalf("the second stream's first frame is not a keyframe: %q %v", out, err)
		}
	})

	t.Run("I4 refusals and detach", func(t *testing.T) {
		r := run("stream", "--device", "mock-1", "--fps", "2", "-o", file("x.mp4"))
		if r.code != 2 || envelopeCode(t, r.stderr) != daemon.CodeUsage {
			t.Fatalf("--fps 2: exit %d\n%s", r.code, r.stderr)
		}
		resp, body := httpGet(t, "http://"+addr+"/v1/devices/mock-2/stream", "e2e-token")
		var env daemon.Envelope
		if resp.StatusCode != 404 || json.Unmarshal(body, &env) != nil || env.Error == nil || env.Error.Code != daemon.CodeDeviceNotAttached {
			t.Fatalf("REST stream of an unattached device: %d %s", resp.StatusCode, body)
		}
		done := make(chan result, 1)
		go func() { done <- run("stream", "--device", "mock-1", "-o", file("detached.mp4")) }()
		// Detach once the stream runs: its file appears with the first
		// bytes. (A detach before the stream's own implicit attach would
		// only be undone by it.)
		waitStarted(t, file("detached.mp4"))
		time.Sleep(time.Second)
		ok("detach", "--device", "mock-1")
		select {
		case r = <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("detach did not end the stream")
		}
		if r.code != 0 {
			t.Fatalf("stream after detach: exit %d\n%s", r.code, r.stderr)
		}
		videoOf(t, file("detached.mp4"), 10)
	})

	t.Run("I5 viewer and token", func(t *testing.T) {
		resp, body := httpGet(t, "http://"+addr+"/v1/viewer", "")
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "<video") {
			t.Fatalf("viewer: %d %v", resp.StatusCode, resp.Header)
		}
		if resp, _ = httpGet(t, "http://"+addr+"/v1/devices/mock-1/stream", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("stream without a token: %d", resp.StatusCode)
		}
	})

	t.Run("I6 recordings", func(t *testing.T) {
		ok("startRecording", "clip", "--device", "mock-1", "--platform", "mock")
		time.Sleep(2 * time.Second)
		r := ok("stopRecording", "--device", "mock-1", "--json")
		var res daemon.StepResult
		if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || res.Data != "recordings/clip.mp4" {
			t.Fatalf("stopRecording: %s", r.stdout)
		}
		videoOf(t, filepath.Join(res.ReportDir, "recordings", "clip.mp4"), 15)

		flowFile := file("five.yaml")
		_ = os.WriteFile(flowFile, []byte("appId: com.example\nstepDelay: 1000\n---\n- tapOn: a\n- tapOn: b\n- tapOn: c\n- tapOn: d\n- tapOn: e\n- tapOn: f\n"), 0o644)
		out := file("report")
		r = e.run("--platform", "mock", "test", "--record", "--output", out, "--flatten", flowFile)
		if r.code != 0 {
			t.Fatalf("test --record: exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		recs, _ := filepath.Glob(filepath.Join(out, "assets", "*", "recording.mp4"))
		if len(recs) != 1 {
			t.Fatalf("recordings: %v", recs)
		}
		videoOf(t, recs[0], 60)
	})
}
