package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// box is an MP4 box of type typ with n payload bytes.
func box(typ string, n int) []byte {
	b := make([]byte, 8+n)
	binary.BigEndian.PutUint32(b, uint32(8+n))
	copy(b[4:], typ)
	return b
}

// U22
func TestClient_Stream(t *testing.T) {
	var requests atomic.Int32
	var mode atomic.Value // "ok" | "partial" | "endless" | "drop" | "expired" | "slow"
	mode.Store("ok")
	init := append(box("ftyp", 16), box("moov", 40)...)
	frag := append(box("moof", 30), box("mdat", 100)...)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/devices/dev-1/stream" || r.Header.Get(HeaderLease) != "L1" {
			http.Error(w, "bad request "+r.URL.String(), 400)
			return
		}
		switch mode.Load() {
		case "drop":
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_ = c.Close()
			return
		case "expired":
			w.Header().Set(HeaderErrorCode, string(CodeLeaseExpired))
			WriteError(w, Errorf(CodeLeaseExpired, "lease L1 ended"))
			return
		case "slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		if q := r.URL.Query(); q.Get("fps") != "30" || q.Get("audio") != "true" {
			http.Error(w, "query "+r.URL.RawQuery, 400)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set(HeaderStreamFPS, "30")
		w.Header().Set(HeaderStreamAudio, "true")
		w.Header().Set(HeaderStreamCodecs, "avc3.640016,mp4a.40.2")
		f := w.(http.Flusher)
		_, _ = w.Write(init)
		f.Flush()
		switch mode.Load() {
		case "endless":
			for r.Context().Err() == nil {
				_, _ = w.Write(frag)
				f.Flush()
				time.Sleep(5 * time.Millisecond)
			}
		case "partial":
			_, _ = w.Write(frag)
			_, _ = w.Write(frag[:40]) // a moof and part of its mdat
			f.Flush()
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_ = c.Close()
		default:
			// Byte by byte: boxes arrive split across reads.
			for _, b := range append(append([]byte(nil), frag...), frag...) {
				_, _ = w.Write([]byte{b})
				f.Flush()
			}
		}
	}))
	defer hs.Close()
	c := NewHTTPClient(hs.URL, "tok", nil)
	c.SetLease("dev-1", "L1")
	ctx := context.Background()
	opts := StreamOptions{FPS: 30, Audio: true}

	var out bytes.Buffer
	info, err := c.Stream(ctx, "dev-1", opts, &out)
	if err != nil {
		t.Fatal(err)
	}
	if info != (StreamInfo{FPS: "30", Audio: true, Codecs: "avc3.640016,mp4a.40.2"}) {
		t.Fatalf("info %+v", info)
	}
	if want := append(append(append([]byte(nil), init...), frag...), frag...); !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("body: %d bytes, want %d", out.Len(), len(want))
	}

	// A stream cut mid-fragment keeps whole boxes only; the cut is an
	// error (the daemon did not end it), and it is not retried.
	mode.Store("partial")
	out.Reset()
	requests.Store(0)
	_, err = c.Stream(ctx, "dev-1", opts, &out)
	if codeOf(t, err) != CodeDaemonUnavailable || requests.Load() != 1 {
		t.Fatalf("partial: %v after %d requests", err, requests.Load())
	}
	if want := append(append([]byte(nil), init...), frag...); !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("partial body: %d bytes, want %d", out.Len(), len(want))
	}

	// A dropped connection is not retried.
	mode.Store("drop")
	requests.Store(0)
	if _, err = c.Stream(ctx, "dev-1", opts, io.Discard); codeOf(t, err) != CodeDaemonUnavailable || requests.Load() != 1 {
		t.Fatalf("drop: %v after %d requests", err, requests.Load())
	}

	// Duration ends a running stream cleanly; so does ctx.
	mode.Store("endless")
	out.Reset()
	start := time.Now()
	if _, err = c.Stream(ctx, "dev-1", StreamOptions{FPS: 30, Audio: true, Duration: 200 * time.Millisecond}, &out); err != nil {
		t.Fatalf("duration: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second || (out.Len()-len(init))%len(frag) != 0 {
		t.Fatalf("duration: %v, %d bytes", d, out.Len())
	}
	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = c.Stream(cctx, "dev-1", opts, io.Discard)
	cancel()
	if err != nil {
		t.Fatalf("ctx end: %v", err)
	}
	// A writer that fails ends the stream with USAGE.
	_, err = c.Stream(ctx, "dev-1", opts, writerFunc(func([]byte) (int, error) { return 0, errors.New("disk full") }))
	if codeOf(t, err) != CodeUsage {
		t.Fatalf("writer error: %v", err)
	}
	// Cancelled before the headers: INTERRUPTED.
	mode.Store("slow")
	cctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = c.Stream(cctx, "dev-1", opts, io.Discard)
	cancel()
	if codeOf(t, err) != CodeInterrupted {
		t.Fatalf("cancelled before headers: %v", err)
	}
	// LEASE_EXPIRED forgets the lease.
	mode.Store("expired")
	if _, err = c.Stream(ctx, "dev-1", opts, io.Discard); codeOf(t, err) != CodeLeaseExpired || c.LeaseOf("dev-1") != "" {
		t.Fatalf("expired: %v, lease %q", err, c.LeaseOf("dev-1"))
	}
	// An unreachable daemon.
	dead := NewHTTPClient("127.0.0.1:1", "", nil)
	if _, err = dead.Stream(ctx, "dev-1", StreamOptions{}, io.Discard); codeOf(t, err) != CodeDaemonUnavailable {
		t.Fatalf("unreachable: %v", err)
	}
}

func TestBoxSize(t *testing.T) {
	large := make([]byte, 16)
	binary.BigEndian.PutUint32(large, 1)
	copy(large[4:], "mdat")
	binary.BigEndian.PutUint64(large[8:], 40)
	cases := []struct {
		b    []byte
		n    int
		ok   bool
		name string
	}{
		{box("moof", 4)[:7], 0, false, "short header"},
		{box("moof", 4), 12, true, "plain"},
		{large, 40, true, "largesize"},
		{large[:12], 0, false, "short largesize"},
		{[]byte{0, 0, 0, 0, 'm', 'd', 'a', 't'}, 0, false, "to the end"},
		{[]byte{0, 0, 0, 4, 'm', 'd', 'a', 't'}, 0, false, "below its header"},
	}
	for _, c := range cases {
		if n, ok := boxSize(c.b); n != c.n || ok != c.ok {
			t.Errorf("%s: %d %t", c.name, n, ok)
		}
	}
	// A failed write is remembered.
	bw := &boxWriter{w: writerFunc(func([]byte) (int, error) { return 0, errors.New("x") })}
	if _, err := bw.Write(box("ftyp", 4)); err == nil {
		t.Fatal("no error")
	}
	if _, err := bw.Write(box("ftyp", 4)); err == nil {
		t.Fatal("no error the second time")
	}
}
