package daemon

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/executor"
)

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestBlobStore_PutVerifiesAndResumes(t *testing.T) {
	b := NewBlobStore(t.TempDir())
	data := []byte("hello, blob store")
	h := sha(data)

	// A body whose hash is not the name is refused and leaves nothing.
	if _, e := b.Put(h, "", strings.NewReader("other bytes")); e == nil || !strings.Contains(e.Message, "mismatch") {
		t.Fatalf("mismatch: %v", e)
	}
	if _, ok := b.Has(h); ok || b.Offset(h) != 0 {
		t.Fatal("a mismatched body must not become visible")
	}

	// A first chunk leaves a partial upload at its offset…
	res, e := b.Put(h, "bytes 0-4/17", bytes.NewReader(data[:5]))
	if e != nil || res.Complete || res.Offset != 5 || b.Offset(h) != 5 {
		t.Fatalf("chunk 1: %+v %v", res, e)
	}
	// …a resume past it is refused, naming the offset…
	if _, e := b.Put(h, "bytes 9-16/17", bytes.NewReader(data[9:])); e == nil || e.Details["offset"] != int64(5) {
		t.Fatalf("gap: %v", e)
	}
	// …and the rest from Upload-Offset completes and verifies.
	res, e = b.Put(h, "bytes 5-16/17", bytes.NewReader(data[5:]))
	if e != nil || !res.Complete || !res.Created {
		t.Fatalf("chunk 2: %+v %v", res, e)
	}
	if size, ok := b.Has(h); !ok || size != int64(len(data)) {
		t.Fatal("blob not stored")
	}
	// A PUT of a stored blob is answered at once.
	res, e = b.Put(h, "", strings.NewReader("ignored"))
	if e != nil || !res.Complete || res.Created {
		t.Fatalf("repeat: %+v %v", res, e)
	}
	if _, e := b.Put(sha([]byte("x")), "bytes nonsense", strings.NewReader("x")); e == nil || e.Code != CodeUsage {
		t.Fatalf("bad Content-Range: %v", e)
	}
	// More bytes than the range's total are refused.
	other := []byte("abc")
	if _, e := b.Put(sha(other), "bytes 0-2/2", bytes.NewReader(other)); e == nil {
		t.Fatal("expected an error for a body longer than its total")
	}
}

func TestBlobStore_ConcurrentPutsOfOneHash(t *testing.T) {
	b := NewBlobStore(t.TempDir())
	data := bytes.Repeat([]byte("0123456789"), 10000)
	h := sha(data)
	var wg sync.WaitGroup
	errs := make(chan *Error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := b.Put(h, "", bytes.NewReader(data)); e != nil {
				errs <- e
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent put: %v", e)
	}
	got, err := os.ReadFile(b.path(h))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("stored blob differs")
	}
}

func TestBlobStore_Expire(t *testing.T) {
	b := NewBlobStore(t.TempDir())
	old, fresh := []byte("old"), []byte("fresh")
	for _, d := range [][]byte{old, fresh} {
		if _, e := b.Put(sha(d), "", bytes.NewReader(d)); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := b.Put(sha([]byte("partial!")), "bytes 0-1/8", strings.NewReader("pa")); e != nil {
		t.Fatal(e)
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(b.path(sha(old)), past, past)
	_ = os.Chtimes(b.partial(sha([]byte("partial!"))), past, past)
	if n := b.Expire(BlobExpiry); n != 2 {
		t.Fatalf("expired %d, want 2", n)
	}
	if _, ok := b.Has(sha(old)); ok {
		t.Fatal("old blob kept")
	}
	if _, ok := b.Has(sha(fresh)); !ok {
		t.Fatal("fresh blob removed")
	}
	// Using a blob refreshes it.
	_ = os.Chtimes(b.path(sha(fresh)), past, past)
	b.Has(sha(fresh))
	if n := b.Expire(BlobExpiry); n != 0 {
		t.Fatal("a blob just used was expired")
	}
	if NewBlobStore(filepath.Join(t.TempDir(), "none")).Expire(BlobExpiry) != 0 {
		t.Fatal("empty store")
	}
}

func zipBytes(t *testing.T, entries map[string]string, extra func(*zip.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, body)
	}
	if extra != nil {
		extra(zw)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// orderedZip builds a zip whose entries keep their order: "f:name=body"
// is a file, "l:name=target" a symlink.
func orderedZip(t *testing.T, entries ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		kind, rest, _ := strings.Cut(e, ":")
		name, body, _ := strings.Cut(rest, "=")
		hdr := &zip.FileHeader{Name: name}
		if kind == "l" {
			hdr.SetMode(os.ModeSymlink | 0o777)
		} else {
			hdr.SetMode(0o644)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBlobStore_Materialize(t *testing.T) {
	b := NewBlobStore(t.TempDir())
	put := func(d []byte) string {
		if _, e := b.Put(sha(d), "", bytes.NewReader(d)); e != nil {
			t.Fatal(e)
		}
		return sha(d)
	}
	png := []byte("\x89PNG not really")
	h := put(png)
	dir := t.TempDir()

	// The copy keeps its basename and extension.
	p, err := b.Materialize("blob:sha256:"+h+"/photo.png", dir)
	if err != nil || filepath.Base(p) != "photo.png" || !strings.HasPrefix(p, dir) {
		t.Fatalf("materialize: %q %v", p, err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, png) {
		t.Fatal("copy differs")
	}
	if p2, err := b.Materialize("blob:sha256:"+h+"/photo.png", dir); err != nil || p2 != p {
		t.Fatal("a second materialize reuses the copy")
	}

	// A packed directory is unpacked; an .apk (a zip itself) is not.
	app := zipBytes(t, map[string]string{"Info.plist": "plist", "Frameworks/x.dylib": "bin"}, func(zw *zip.Writer) {
		hdr := &zip.FileHeader{Name: "link"}
		hdr.SetMode(os.ModeSymlink | 0o777)
		w, _ := zw.CreateHeader(hdr)
		_, _ = io.WriteString(w, "Info.plist")
	})
	ha := put(app)
	p, err = b.Materialize("blob:sha256:"+ha+"/My.app", dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(p, "Frameworks", "x.dylib")); err != nil || fi.IsDir() {
		t.Fatalf("unpacked tree: %v", err)
	}
	if l, err := os.Readlink(filepath.Join(p, "link")); err != nil || l != "Info.plist" {
		t.Fatalf("symlink: %q %v", l, err)
	}
	p, err = b.Materialize("blob:sha256:"+ha+"/My.apk", dir)
	if fi, _ := os.Stat(p); err != nil || fi.IsDir() {
		t.Fatalf("an .apk stays a file: %v", err)
	}

	// A zip entry or symlink that leaves the directory is refused.
	for name, z := range map[string][]byte{
		"slip": zipBytes(t, map[string]string{"../evil": "x"}, nil),
		"abslink": zipBytes(t, nil, func(zw *zip.Writer) {
			hdr := &zip.FileHeader{Name: "l"}
			hdr.SetMode(os.ModeSymlink | 0o777)
			w, _ := zw.CreateHeader(hdr)
			_, _ = io.WriteString(w, "/etc/hosts")
		}),
		"uplink": zipBytes(t, nil, func(zw *zip.Writer) {
			hdr := &zip.FileHeader{Name: "l"}
			hdr.SetMode(os.ModeSymlink | 0o777)
			w, _ := zw.CreateHeader(hdr)
			_, _ = io.WriteString(w, "../../x")
		}),
	} {
		hz := put(z)
		if _, err := b.Materialize("blob:sha256:"+hz+"/Bad.app", dir); err == nil {
			t.Fatalf("%s: expected a refusal", name)
		}
	}

	// A chain of links the zip itself makes cannot lead an entry out: each
	// check resolves the links already written. The chain's hops are each
	// lexically inside, so only a resolved check refuses them.
	for name, z := range map[string][]byte{
		"chain":   orderedZip(t, "l:a/l=..", "l:a/l/m=..", "f:a/l/m/x/evil=pwned"),
		"relink":  orderedZip(t, "l:a/l=..", "l:a/q=z/..", "l:a/z=l", "f:ok=1"),
		"through": orderedZip(t, "l:a/l=..", "l:b=a/l/a/l/a/l/.."),
		"dotdot":  orderedZip(t, "f:a/..=x"),
	} {
		hz := put(z)
		if _, err := b.Materialize("blob:sha256:"+hz+"/Chain.app", dir); err == nil {
			t.Fatalf("%s: expected a refusal", name)
		}
		if _, err := os.Lstat(filepath.Join(dir, hz, "x", "evil")); err == nil {
			t.Fatalf("%s: wrote outside the directory", name)
		}
		if _, err := os.Lstat(filepath.Join(dir, hz, "Chain.app")); err == nil {
			t.Fatalf("%s: a refused tree is removed", name)
		}
	}
	// Links that stay inside, through other links, are kept.
	hz := put(orderedZip(t, "f:Versions/A/Bin=b", "l:Versions/Current=A", "l:Bin=Versions/Current/Bin", "l:Versions/A/up=../Current/Bin"))
	p, err = b.Materialize("blob:sha256:"+hz+"/Fw.app", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(p, "Versions", "A", "up")); err != nil || string(got) != "b" {
		t.Fatalf("framework links: %q %v", got, err)
	}

	// Bad refs are refused before any file is opened; a missing blob says so.
	for _, ref := range []string{
		"blob:sha256:../../../.ssh/id_ed25519/x",
		"blob:sha256:" + strings.ToUpper(h) + "/x.png",
		"blob:sha256:" + h,
		"blob:sha256:" + h + "/a/b",
		"blob:sha256:" + h + "/..",
		"blob:md5:" + h + "/x",
	} {
		if _, err := b.Materialize(ref, dir); err == nil {
			t.Fatalf("%s: expected a refusal", ref)
		}
	}
	if _, err := b.Materialize("blob:sha256:"+sha([]byte("never"))+"/x.png", dir); err == nil || !strings.Contains(err.Error(), "not been uploaded") {
		t.Fatalf("missing: %v", err)
	}
	if _, err := (sessionBlobs{store: b, dir: dir}).Resolve("bundle:sha256:" + h + "/main.yaml"); err == nil {
		t.Fatal("bundle refs arrive with flow bundles")
	}
}

func TestBlobRoutes(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	data := bytes.Repeat([]byte("blob"), 1000)
	h := sha(data)
	file := writeTemp(t, "data.bin", data)

	// A hash that is not 64 lowercase hex is FORBIDDEN on both routes.
	for _, bad := range []string{"..%2F..%2F..%2F.ssh%2Fid_ed25519", strings.ToUpper(h), "abc"} {
		for _, method := range []string{http.MethodHead, http.MethodPut} {
			req, _ := http.NewRequest(method, env.remote.BaseURL()+"/v1/devices/mock-1/blobs/"+bad, strings.NewReader("x"))
			req.Header.Set("Authorization", "Bearer t")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get(HeaderErrorCode) != string(CodeForbidden) {
				t.Fatalf("%s %s: %d %q", method, bad, resp.StatusCode, resp.Header.Get(HeaderErrorCode))
			}
		}
	}

	have, off, err := env.remote.HasBlob(ctx, "mock-1", h)
	if err != nil || have || off != 0 {
		t.Fatalf("HEAD before: %v %v %v", have, off, err)
	}
	// A partial upload is reported and resumed from its offset.
	if _, e := env.s.blobs.Put(h, "bytes 0-999/4000", bytes.NewReader(data[:1000])); e != nil {
		t.Fatal(e)
	}
	if _, off, _ := env.remote.HasBlob(ctx, "mock-1", h); off != 1000 {
		t.Fatalf("Upload-Offset %d", off)
	}
	uploaded, err := env.remote.UploadBlob(ctx, "mock-1", h, file, int64(len(data)))
	if err != nil || !uploaded {
		t.Fatalf("upload: %v %v", uploaded, err)
	}
	if have, size, _ := env.remote.HasBlob(ctx, "mock-1", h); !have || size != int64(len(data)) {
		t.Fatal("HEAD after")
	}
	// The second time nothing is sent.
	if uploaded, err := env.remote.UploadBlob(ctx, "mock-1", h, file, int64(len(data))); err != nil || uploaded {
		t.Fatalf("dedupe: %v %v", uploaded, err)
	}
	// A wrong hash fails USAGE and is not retried.
	other := writeTemp(t, "other.bin", []byte("other"))
	if _, err := env.remote.UploadBlob(ctx, "mock-1", sha([]byte("x")), other, 5); codeOf(t, err) != CodeUsage {
		t.Fatalf("mismatch: %v", err)
	}
	// An empty file uploads with no range.
	empty := writeTemp(t, "empty", nil)
	if _, err := env.remote.UploadBlob(ctx, "mock-1", sha(nil), empty, 0); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if _, err := env.remote.UploadBlob(ctx, "mock-1", h, filepath.Join(t.TempDir(), "gone"), 1); err != nil {
		// stored already: no read needed
		t.Fatalf("stored blob needs no file: %v", err)
	}
	if _, err := env.remote.UploadBlob(ctx, "mock-1", sha([]byte("gone")), filepath.Join(t.TempDir(), "gone"), 4); codeOf(t, err) != CodeUsage {
		t.Fatalf("missing local file: %v", err)
	}
}

// TestClient_UploadResumesAfterADrop cuts the first PUT's connection part
// way through; the client asks for the offset and sends only the rest.
func TestClient_UploadResumesAfterADrop(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var n int32
	s := New(Config{Name: "drop", ReportsDir: t.TempDir()}, testDeps(&n, 0))
	data := bytes.Repeat([]byte("resumable "), 5000)
	h := sha(data)
	var mu sync.Mutex
	var puts []string
	inner := s.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			mu.Lock()
			puts = append(puts, r.Header.Get("Content-Range"))
			first := len(puts) == 1
			mu.Unlock()
			if first {
				// Store a prefix, then drop the connection.
				_, _ = s.blobs.Put(h, r.Header.Get("Content-Range"), io.LimitReader(r.Body, 12345))
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				_ = conn.(*net.TCPConn).SetLinger(0)
				conn.Close()
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	defer hs.Close()
	c := NewHTTPClient(hs.URL, "", nil)
	c.sleep = noSleep
	var logged []string
	RetryLog = func(format string, args ...any) { logged = append(logged, format) }
	defer func() { RetryLog = nil }()
	file := writeTemp(t, "big.bin", data)
	if _, err := c.UploadBlob(context.Background(), "mock-1", h, file, int64(len(data))); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(puts) != 2 || puts[1] != "bytes 12345-49999/50000" || len(logged) == 0 {
		t.Fatalf("puts %v, retries logged %d", puts, len(logged))
	}
	if _, ok := s.blobs.Has(h); !ok {
		t.Fatal("not stored")
	}
}

// TestClient_UploadResumesAfterPeerUnavailable puts a hub in front of the
// daemon whose link to the device's hub drops mid-upload: the first PUT
// stores a prefix and gets 503 PEER_UNAVAILABLE, the next ask (HEAD) gets
// a body-less 503, and the next PUT a gateway's bare 504. The client asks
// again after each and finishes from the daemon's offset.
func TestClient_UploadResumesAfterPeerUnavailable(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var n int32
	s := New(Config{Name: "peer", ReportsDir: t.TempDir()}, testDeps(&n, 0))
	data := bytes.Repeat([]byte("over the mesh "), 4000)
	h := sha(data)
	var mu sync.Mutex
	var calls []string
	inner := s.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.Header.Get("Content-Range"))
		i := len(calls)
		mu.Unlock()
		switch i {
		case 2: // first PUT: the link drops after a prefix
			_, _ = s.blobs.Put(h, r.Header.Get("Content-Range"), io.LimitReader(r.Body, 10000))
			writeJSON(w, http.StatusServiceUnavailable, FailureEnvelope(Errorf(CodePeerUnavailable, "hub hub-b: link closed")))
			return
		case 3: // the ask while the link is still down
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case 5: // a gateway in between, mid-PUT
			_, _ = s.blobs.Put(h, r.Header.Get("Content-Range"), io.LimitReader(r.Body, 20000))
			http.Error(w, "upstream timed out", http.StatusGatewayTimeout)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer hs.Close()
	c := NewHTTPClient(hs.URL, "", nil)
	c.sleep = noSleep
	var logged int
	RetryLog = func(string, ...any) { logged++ }
	defer func() { RetryLog = nil }()
	file := writeTemp(t, "big.apk", data)
	uploaded, err := c.UploadBlob(context.Background(), "mock-1", h, file, int64(len(data)))
	if err != nil || !uploaded {
		t.Fatalf("upload: %v (uploaded %v), calls %v", err, uploaded, calls)
	}
	want := []string{"HEAD ", "PUT bytes 0-55999/56000", "HEAD ", "HEAD ", "PUT bytes 10000-55999/56000", "HEAD ", "PUT bytes 30000-55999/56000"}
	if strings.Join(calls, "|") != strings.Join(want, "|") || logged < 3 {
		t.Fatalf("calls %v, retries logged %d", calls, logged)
	}
	if _, ok := s.blobs.Has(h); !ok {
		t.Fatal("not stored")
	}
}

// TestClient_UploadFinalErrorsAreNotRetried: the daemon's own DEVICE_ERROR
// (a 502 with an envelope) ends the upload at once.
func TestClient_UploadFinalErrorsAreNotRetried(t *testing.T) {
	data := []byte("final")
	h := sha(data)
	var puts int
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		puts++
		writeJSON(w, http.StatusBadGateway, FailureEnvelope(Errorf(CodeDeviceError, "disk full")))
	}))
	defer hs.Close()
	c := NewHTTPClient(hs.URL, "", nil)
	c.sleep = noSleep
	_, err := c.UploadBlob(context.Background(), "mock-1", h, writeTemp(t, "f.bin", data), int64(len(data)))
	if codeOf(t, err) != CodeDeviceError || puts != 1 {
		t.Fatalf("err %v after %d PUTs", err, puts)
	}
}

// TestClient_HasBlobReadsTheErrorCodeHeader: a HEAD's refusal has no body,
// so HasBlob takes its code from X-Maestro-Error-Code. A 404 without one is
// a blob the daemon lacks; a 404 naming a code is that error, and a 503
// tells PEER_UNAVAILABLE from DAEMON_UNAVAILABLE.
func TestClient_HasBlobReadsTheErrorCodeHeader(t *testing.T) {
	h := sha([]byte("x"))
	for _, tc := range []struct {
		status int
		header string
		want   Code
	}{
		{http.StatusNotFound, "", ""},
		{http.StatusNotFound, string(CodeDeviceNotFound), CodeDeviceNotFound},
		{http.StatusServiceUnavailable, string(CodePeerUnavailable), CodePeerUnavailable},
		{http.StatusServiceUnavailable, "", CodeDaemonUnavailable},
		{http.StatusConflict, string(CodeDeviceInUse), CodeDeviceInUse},
		{http.StatusConflict, "NOT_A_CODE", CodeBusy},
	} {
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.header != "" {
				w.Header().Set(HeaderErrorCode, tc.header)
			}
			w.WriteHeader(tc.status)
		}))
		c := NewHTTPClient(hs.URL, "", nil)
		have, _, err := c.HasBlob(context.Background(), "mock-1", h)
		hs.Close()
		if tc.want == "" {
			if err != nil || have {
				t.Errorf("%d %q: %v %v", tc.status, tc.header, have, err)
			}
			continue
		}
		if codeOf(t, err) != tc.want {
			t.Errorf("%d %q: %v, want %s", tc.status, tc.header, err, tc.want)
		}
	}
}

func TestParseBlobRefAndHash(t *testing.T) {
	h := sha([]byte("x"))
	if hx, base, err := executor.ParseBlobRef("blob:sha256:" + h + "/photo.png"); err != nil || hx != h || base != "photo.png" {
		t.Fatalf("%q %q %v", hx, base, err)
	}
	if !executor.ValidBlobHash(h) || executor.ValidBlobHash("../../../.ssh/id_ed25519") {
		t.Fatal("hash check")
	}
}
