package daemon

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fmp4"
)

// U18: the viewer page is the one route a TCP listener serves without its
// token, and only at exactly GET /v1/viewer.
func TestViewer_ServedWithoutTokenPathExactly(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var n int32
	s := New(Config{Name: "viewer", Token: "secret", ReportsDir: t.TempDir()}, testDeps(&n, 0))
	hs := httptest.NewServer(s.TCPHandler())
	defer hs.Close()

	do := func(method, path, token string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, hs.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := do(http.MethodGet, "/v1/viewer", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("viewer: %d %v", resp.StatusCode, resp.Header)
	}
	for _, want := range []string{"<video", "location.hash", "MediaSource.isTypeSupported", "X-Maestro-Codecs", "X-Maestro-Lease",
		"seekLive", "Unmute", "playing", "error: ", "isPCM(codecs)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// It carries no device data and no token.
	for _, no := range []string{"mock-1", "secret"} {
		if strings.Contains(body, no) {
			t.Errorf("the page contains %q", no)
		}
	}
	// Every other path, and another method, still needs the token.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/viewer/x"}, {http.MethodGet, "/v1/viewerx"}, {http.MethodGet, "/v1/viewer/"},
		{http.MethodPost, "/v1/viewer"}, {http.MethodGet, "/v1/status"}, {http.MethodGet, "/v1/devices/mock-1/stream"},
	} {
		if resp, _ := do(c.method, c.path, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d", c.method, c.path, resp.StatusCode)
		}
	}
	// With the token, a near miss is no route.
	if resp, _ := do(http.MethodGet, "/v1/viewerx", "secret"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("/v1/viewerx with the token: %d", resp.StatusCode)
	}
	if resp, _ := do(http.MethodGet, "/v1/viewer", "secret"); resp.StatusCode != 200 {
		t.Errorf("viewer with the token: %d", resp.StatusCode)
	}
}

// U18 (retimer): the viewer's retimer gives every sample its real
// duration, the gap to its track's next sample, so Media Source
// Extensions never see a sample start more than twice the previous one's
// duration after it (a discontinuity, after which Chrome drops every frame
// until the next keyframe: the Pixel viewer froze for seconds). It runs
// the page's own makeRetimer under node on a muxed stream with irregular
// frame gaps, a still screen and an audio gap, fed in odd-sized chunks.
func TestViewer_RetimerGivesRealDurations(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	page := string(viewerHTML)
	begin, end := strings.Index(page, "// retimer:begin"), strings.Index(page, "// retimer:end")
	if begin < 0 || end < begin {
		t.Fatal("the page has no retimer:begin/end block")
	}

	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		t.Fatal(err)
	}
	m, err := fmp4.NewMuxer(fmp4.Video{Width: format.Width, Height: format.Height, SPS: format.SPS, PPS: format.PPS},
		&fmp4.Audio{Codec: fmp4.CodecAAC, SampleRate: 48000, Channels: 2, ASC: fake.ASC()}, 15)
	if err != nil {
		t.Fatal(err)
	}
	in := append([]byte{}, m.Init()...)
	// Video gaps in ms: steady, jittered (33 then 100 trips the rule), a
	// 9.2 s still screen, then steady again. Audio: AAC frames with one
	// 60 ms gap.
	var videoAt []time.Duration
	at := 7 * time.Second
	for i, gap := range []int{0, 66, 67, 33, 100, 66, 9200, 100, 66, 66, 33, 100, 66} {
		at += time.Duration(gap) * time.Millisecond
		if i < len(frames) {
			videoAt = append(videoAt, at)
		}
	}
	aac := fake.AACFrames()
	audioAt := 7 * time.Second
	vi := 0
	for k := 0; vi < len(videoAt); k++ {
		for vi < len(videoAt) && videoAt[vi] <= audioAt {
			f, err := m.Video(videoAt[vi], frames[vi].Key, frames[vi].Data)
			if err != nil {
				t.Fatal(err)
			}
			in = append(in, f.Bytes...)
			vi++
		}
		f, err := m.Audio(audioAt, aac[k%len(aac)])
		if err != nil {
			t.Fatal(err)
		}
		in = append(in, f.Bytes...)
		audioAt += 1024 * time.Second / 48000
		if k == 40 {
			audioAt += 60 * time.Millisecond
		}
	}
	// The input must trip the rule, or the check proves nothing.
	if msecontinuous(t, in) {
		t.Fatal("the input stream never trips the discontinuity rule")
	}

	dir := t.TempDir()
	stream := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(stream, in, 0o644); err != nil {
		t.Fatal(err)
	}
	script := page[begin:end] + `
const data = require('fs').readFileSync(process.argv[2])
const out = []
const r = makeRetimer((b) => out.push(Buffer.from(b)))
for (let o = 0, i = 0; o < data.length; i++) {
  const n = [7, 1000, 13, 333, 8, 4096][i % 6]
  r.push(new Uint8Array(data.subarray(o, o + n)))
  o += n
}
r.flush()
process.stdout.write(Buffer.concat(out))
`
	js := filepath.Join(dir, "retime.js")
	if err := os.WriteFile(js, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(node, js, stream)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	if len(out) != len(in) || !bytes.Equal(out[:len(m.Init())], m.Init()) {
		t.Fatalf("retimed stream is %d bytes (in %d), or its init segment changed", len(out), len(in))
	}
	if !msecontinuous(t, out) {
		t.Error("the retimed stream still trips the discontinuity rule")
	}
	// Every sample but each track's last lasts exactly until the next.
	for track, ss := range samples(t, out) {
		for i := 0; i+1 < len(ss); i++ {
			if ss[i].dur != ss[i+1].time-ss[i].time {
				t.Errorf("track %d sample %d: duration %d, gap %d", track, i, ss[i].dur, ss[i+1].time-ss[i].time)
			}
		}
	}
}

type fragSample struct{ time, dur uint64 }

// samples lists each track's samples in order, from a stream of the
// muxer's one-sample fragments.
func samples(t *testing.T, b []byte) map[uint32][]fragSample {
	t.Helper()
	out := map[uint32][]fragSample{}
	for off := 0; off+8 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[off:]))
		if n < 8 || off+n > len(b) {
			t.Fatalf("bad box at %d", off)
		}
		if string(b[off+4:off+8]) == "moof" {
			// moof: mfhd 16; traf: tfhd 16 (track id at +12), tfdt v1 20
			// (time at +12), trun (duration at +20).
			track := binary.BigEndian.Uint32(b[off+44:])
			tm := binary.BigEndian.Uint64(b[off+60:])
			dur := binary.BigEndian.Uint32(b[off+68+20:])
			out[track] = append(out[track], fragSample{tm, uint64(dur)})
		}
		off += n
	}
	return out
}

// msecontinuous reports whether no sample starts more than twice the
// previous sample's duration after it (MSE's coded frame processing,
// step 6).
func msecontinuous(t *testing.T, b []byte) bool {
	ok := true
	for _, ss := range samples(t, b) {
		for i := 1; i < len(ss); i++ {
			if ss[i].time-ss[i-1].time > 2*ss[i-1].dur {
				ok = false
			}
		}
	}
	return ok
}

// U18 (live edge): the viewer's liveStep seeks only to leave a hole or a
// lag over JUMP_S; a shorter lag over CATCH_UP_S plays at the catch-up
// rate until it is under CAUGHT_UP_S. Runs the page's own liveStep under
// node.
func TestViewer_LiveStep(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	page := string(viewerHTML)
	begin, end := strings.Index(page, "// live:begin"), strings.Index(page, "// live:end")
	if begin < 0 || end < begin {
		t.Fatal("the page has no live:begin/end block")
	}
	script := page[begin:end] + `
const cases = [
  [10, 20, 9, 1],      // before the newest range: to its start
  [0, 20, 16.5, 1],    // 3.5 s behind: seek near the edge
  [0, 20, 18.7, 1],    // 1.3 s behind: catch up faster
  [0, 20, 19.2, 1.5],  // 0.8 s behind while catching up: keep it
  [0, 20, 19.2, 1],    // 0.8 s behind at 1x: keep it
  [0, 20, 19.5, 1.5],  // 0.5 s behind: back to 1x
  [15, 20, 14.9, 1.5], // a hole while catching up: seek, 1x
]
process.stdout.write(JSON.stringify(cases.map((c) => liveStep(...c))))
`
	js := filepath.Join(t.TempDir(), "live.js")
	if err := os.WriteFile(js, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, js).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	want := `[{"seek":10,"rate":1},{"seek":19.4,"rate":1},{"rate":1.5},{"rate":1.5},{"rate":1},{"rate":1},{"seek":15,"rate":1}]`
	if string(out) != want {
		t.Errorf("liveStep = %s\nwant      %s", out, want)
	}
}

// The viewer asks again without audio for LPCM in either form: ipcm
// (integer) and fpcm (floating point), the two an iPhone sends. Runs the
// page's own isPCM under node.
func TestViewer_PCMAudioIsVideoOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	page := string(viewerHTML)
	begin, end := strings.Index(page, "// pcm:begin"), strings.Index(page, "// pcm:end")
	if begin < 0 || end < begin {
		t.Fatal("the page has no pcm:begin/end block")
	}
	script := page[begin:end] + `
const cases = ['avc3.640033,ipcm', 'avc3.640033,fpcm', 'avc3.640033, fpcm', 'avc3.640028,mp4a.40.2', 'avc3.640028', '']
process.stdout.write(JSON.stringify(cases.map(isPCM)))
`
	js := filepath.Join(t.TempDir(), "pcm.js")
	if err := os.WriteFile(js, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, js).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if want := `[true,true,true,false,false,false]`; string(out) != want {
		t.Errorf("isPCM = %s, want %s", out, want)
	}
}

// Review 2.harness-review.3: play cancels the response it read whenever
// it returns, and stops with an error when playback fails, so a reconnect
// never leaves the old request open: the stream ends, an append the
// buffer refuses, a decode error, a MediaSource that ended (with or
// without an event). Runs the page's own play (with its retimer and
// liveStep) under node against a stub video element, MediaSource and a
// fetch body that the request's AbortController ends, as a browser's
// does.
func TestViewer_PlayCancelsItsResponse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	page := string(viewerHTML)
	var script strings.Builder
	for _, block := range []string{"live", "retimer", "play"} {
		begin, end := strings.Index(page, "// "+block+":begin"), strings.Index(page, "// "+block+":end")
		if begin < 0 || end < begin {
			t.Fatalf("the page has no %s:begin/end block", block)
		}
		script.WriteString(page[begin:end])
	}
	script.WriteString(playHarness)
	js := filepath.Join(t.TempDir(), "play.js")
	if err := os.WriteFile(js, []byte(script.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, js).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	want := `[{"name":"ends","rejected":null,"aborted":true,"appended":8},` +
		`{"name":"append-error","rejected":"bad data","aborted":true,"appended":8},` +
		`{"name":"decode-error","rejected":"playback failed: decode failed","aborted":true,"appended":8},` +
		`{"name":"source-ended","rejected":"playback failed: the media source is ended","aborted":true,"appended":8},` +
		`{"name":"closed-silently","rejected":"playback failed: the media source is closed","aborted":true,"appended":8}]`
	if string(out) != want {
		t.Errorf("play:\n got %s\nwant %s", out, want)
	}
}

// playHarness stubs the page's globals for play (a video element,
// MediaSource and its SourceBuffer, a fetch body) and runs its scenarios,
// printing what each ended with.
const playHarness = `
// Stubs for the page's globals: a video element, MediaSource and its
// SourceBuffer, and a fetch body that the request's AbortController ends
// as a browser's fetch does.
let state = {}
const unmute = { hidden: true }
const video = Object.assign(new EventTarget(), { error: null, muted: true, currentTime: 0, playbackRate: 1, seeking: false, src: '', play: async () => {} })
let appendError = null
let lastMS = null
class SourceBuffer extends EventTarget {
  constructor() {
    super()
    this.updating = false
    this.buffered = { length: 0 }
    this.appended = 0
  }
  appendBuffer(data) {
    if (appendError) throw appendError
    this.appended += data.length
    this.updating = true
    setTimeout(() => {
      this.updating = false
      this.dispatchEvent(new Event('updateend'))
    }, 0)
  }
  remove() {}
}
class MediaSource extends EventTarget {
  constructor() {
    super()
    this.readyState = 'closed'
    this.sb = null
    lastMS = this
  }
  addSourceBuffer() {
    this.sb = new SourceBuffer()
    return this.sb
  }
}
URL.createObjectURL = (ms) => {
  setTimeout(() => {
    ms.readyState = 'open'
    ms.dispatchEvent(new Event('sourceopen'))
  }, 0)
  return 'blob:stream'
}
// stream is a response whose body yields chunks until the request is
// aborted (it then errors, as fetch's body does) or end() is called.
function stream(chunks) {
  const ctl = new AbortController()
  let controller
  const body = new ReadableStream({
    start(c) {
      controller = c
      for (const b of chunks) c.enqueue(b)
      ctl.signal.addEventListener('abort', () => {
        try {
          c.error(new DOMException('The operation was aborted.', 'AbortError'))
        } catch {}
      })
    },
  }, { highWaterMark: 0 })
  return { s: { res: { body }, ctl, mime: 'video/mp4; codecs="avc3.640028"', fps: '15', audio: false }, end: () => controller.close(), push: (b) => controller.enqueue(b) }
}
const tick = (ms = 20) => new Promise((r) => setTimeout(r, ms))
const ftyp = () => new Uint8Array([0, 0, 0, 8, 0x66, 0x74, 0x79, 0x70])
async function scenario(name, act) {
  video.error = null
  appendError = null
  const st = stream([ftyp()])
  const p = play(st.s).then(() => null, (err) => String((err && err.message) || err))
  await tick()
  await act(st)
  const rejected = await Promise.race([p, tick(2000).then(() => 'still playing')])
  return { name, rejected, aborted: st.s.ctl.signal.aborted, appended: lastMS.sb ? lastMS.sb.appended : 0 }
}
;(async () => {
  const out = []
  out.push(await scenario('ends', (st) => st.end()))
  out.push(await scenario('append-error', async (st) => {
    appendError = new Error('bad data')
    st.push(ftyp())
  }))
  out.push(await scenario('decode-error', () => {
    video.error = { code: 3, message: 'decode failed' }
    lastMS.readyState = 'ended'
    video.dispatchEvent(new Event('error'))
  }))
  out.push(await scenario('source-ended', () => {
    lastMS.readyState = 'ended'
    lastMS.dispatchEvent(new Event('sourceended'))
  }))
  out.push(await scenario('closed-silently', () => {
    lastMS.readyState = 'closed'
  }))
  process.stdout.write(JSON.stringify(out))
})()
`
