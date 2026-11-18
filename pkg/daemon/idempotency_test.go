package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestIdemCache_WaitReplayExpireBound(t *testing.T) {
	c := newIdemCache()
	now := time.Now()
	c.now = func() time.Time { return now }

	e, run := c.begin("d", "a")
	if !run {
		t.Fatal("first sight runs")
	}
	again, run := c.begin("d", "a")
	if run || again != e {
		t.Fatal("a repeat while running waits on the same entry")
	}
	go c.finish(e, 201, http.Header{"X": {"y"}}, []byte("body"))
	<-again.done
	rec := httptest.NewRecorder()
	again.replay(rec)
	if rec.Code != 201 || rec.Body.String() != "body" || rec.Header().Get(HeaderReplayed) != "true" || rec.Header().Get("X") != "y" {
		t.Fatalf("replay: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	// Other devices are separate.
	if _, run := c.begin("other", "a"); !run {
		t.Fatal("ids are per device")
	}
	// At the TTL the entry is gone.
	now = now.Add(idemTTL + time.Second)
	if _, run := c.begin("d", "a"); !run {
		t.Fatal("expired entries run again")
	}
	// Bounded at 256: the oldest is evicted.
	for i := 0; i < idemPerDevice+10; i++ {
		e, _ := c.begin("bound", fmt.Sprint(i))
		c.finish(e, 200, nil, nil)
	}
	if n := c.len("bound"); n != idemPerDevice {
		t.Fatalf("held %d, want %d", n, idemPerDevice)
	}
	if _, run := c.begin("bound", "0"); !run {
		t.Fatal("the oldest id was evicted")
	}
	if c.len("none") != 0 {
		t.Fatal("len of an unknown device")
	}
}

// slowCommandEnv is a server whose mock steps take delay, reached over a
// TCP handler as a LAN client.
func slowCommandEnv(t *testing.T, delay, grace time.Duration) (*Server, string) {
	t.Helper()
	t.Setenv(EnvHome, t.TempDir())
	var n int32
	s := New(Config{Name: "idem", ReportsDir: t.TempDir(), HTTPAddr: "0.0.0.0:7433", Token: "t"}, testDeps(&n, delay))
	if grace > 0 {
		s.idemGrace = grace
	}
	s.info = Info{Name: "idem", APIVersion: APIVersion}
	t.Cleanup(s.detachAll)
	hs := httptest.NewServer(fromAddr("192.0.2.20:1", s.TCPHandler()))
	t.Cleanup(hs.Close)
	if _, err := NewHTTPClient(hs.URL, "t", nil).Attach(context.Background(), "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	return s, hs.URL
}

// slowCount is a runFlow value: a slow driver step, then output.<v>++.
func slowCount(v string) string {
	return `{"commands": [{"tapOn": "X"}, {"evalScript": "${output.` + v + ` = (output.` + v + ` || 0) + 1}"}]}`
}

func postWithID(t *testing.T, ctx context.Context, url, id, body string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/devices/mock-1/commands/runFlow", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set(HeaderRequestID, id)
	return http.DefaultClient.Do(req)
}

func TestIdempotent_RepeatIsAnsweredOnce(t *testing.T) {
	_, url := slowCommandEnv(t, 300*time.Millisecond, 0)
	ctx := context.Background()
	body := slowCount("n")
	var wg sync.WaitGroup
	var statuses [2]int
	var replayed [2]string
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i == 1 {
				time.Sleep(50 * time.Millisecond) // a repeat while the first runs
			}
			resp, err := postWithID(t, ctx, url, "same-id", body)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			statuses[i], replayed[i] = resp.StatusCode, resp.Header.Get(HeaderReplayed)
		}(i)
	}
	wg.Wait()
	if statuses != [2]int{200, 200} || replayed[0] != "" || replayed[1] != "true" {
		t.Fatalf("statuses %v replayed %v", statuses, replayed)
	}
	// After completion, the cache answers again.
	resp, err := postWithID(t, ctx, url, "same-id", body)
	if err != nil || resp.Header.Get(HeaderReplayed) != "true" {
		t.Fatalf("after completion: %v", err)
	}
	resp.Body.Close()
	c := NewHTTPClient(url, "t", nil)
	v, err := c.Eval(ctx, "mock-1", "output.n")
	if err != nil || fmt.Sprint(v) != "1" {
		t.Fatalf("executed %v times (%v), want once", v, err)
	}
	// A retry whose own client goes away while it waits just leaves.
	first := make(chan struct{})
	go func() {
		defer close(first)
		if resp, err := postWithID(t, context.Background(), url, "abandon", slowCount("m")); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := postWithID(t, short, url, "abandon", slowCount("m")); err == nil {
		t.Fatal("the abandoned retry should time out")
	}
	<-first
}

func TestIdempotent_DroppedClientKeepsRunningForARetry(t *testing.T) {
	_, url := slowCommandEnv(t, 400*time.Millisecond, 2*time.Second)

	// The first attempt's connection drops mid-step.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := postWithID(t, ctx, url, "dropped", slowCount("k")); err == nil {
		t.Fatal("expected the first attempt to be cut off")
	}
	// The retry joins it and gets its outcome.
	resp, err := postWithID(t, context.Background(), url, "dropped", slowCount("k"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get(HeaderReplayed) != "true" {
		t.Fatalf("retry: %d replayed=%q", resp.StatusCode, resp.Header.Get(HeaderReplayed))
	}
	v, _ := NewHTTPClient(url, "t", nil).Eval(context.Background(), "mock-1", "output.k")
	if fmt.Sprint(v) != "1" {
		t.Fatalf("ran %v times", v)
	}
}

func TestIdempotent_DroppedClientWithoutRetryIsCancelled(t *testing.T) {
	_, url := slowCommandEnv(t, 0, 100*time.Millisecond)
	loop := `{"while": {"visible": "Login"}, "commands": [{"tapOn": "Login"}]}`
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/devices/mock-1/commands/repeat", strings.NewReader(loop))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set(HeaderRequestID, "loop")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected a timeout")
	}
	// The endless loop is cancelled after the grace, freeing the device.
	done := make(chan error, 1)
	go func() {
		_, err := NewHTTPClient(url, "t", nil).Command(context.Background(), "mock-1", "tapOn", "X", true)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned request was never cancelled")
	}
}

func TestClient_RetriesWithTheSameID(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	var calls atomic.Int32
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get(HeaderRequestID))
		mu.Unlock()
		switch calls.Add(1) {
		case 1: // a dropped connection
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		case 2: // the owner's hub is unreachable
			writeError(w, Errorf(CodePeerUnavailable, "hub hub-a is unreachable").WithDetails("hub", "hub-a"))
		case 3: // a gateway's error page
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte("<html>gateway timeout</html>"))
		default:
			writeJSON(w, 200, StepResult{Envelope: Envelope{OK: true}, Step: "tapOn", Type: "tapOn"})
		}
	}))
	defer hs.Close()
	c := NewHTTPClient(hs.URL, "", nil)
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	var logged int
	RetryLog = func(string, ...any) { logged++ }
	defer func() { RetryLog = nil }()
	res, err := c.Command(context.Background(), "mock-1", "tapOn", "X", true)
	if err != nil || !res.OK {
		t.Fatalf("after retries: %v", err)
	}
	if len(ids) != 4 || ids[0] == "" || ids[0] != ids[1] || ids[1] != ids[2] || ids[2] != ids[3] {
		t.Fatalf("ids %v", ids)
	}
	if fmt.Sprint(waits) != "[500ms 1s 2s]" || logged != 3 {
		t.Fatalf("waits %v logged %d", waits, logged)
	}
	// Each call gets a new id.
	calls.Store(10)
	if _, err := c.Command(context.Background(), "mock-1", "tapOn", "X", true); err != nil {
		t.Fatal(err)
	}
	if ids[4] == ids[0] {
		t.Fatal("a new call needs a new id")
	}
}

func TestClient_NoRetryForFinalAnswers(t *testing.T) {
	var calls atomic.Int32
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/v1/devices/mock-1/detach" {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		writeError(w, Errorf(CodeDeviceError, "the driver died"))
	}))
	defer hs.Close()
	c := NewHTTPClient(hs.URL, "", nil)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := c.Command(context.Background(), "mock-1", "tapOn", "X", true)
	if codeOf(t, err) != CodeDeviceError || calls.Load() != 1 {
		t.Fatalf("a daemon's DEVICE_ERROR is final: %v after %d calls", err, calls.Load())
	}
	// A detach (not idempotent) is not retried.
	calls.Store(0)
	if err := c.Detach(context.Background(), "mock-1"); codeOf(t, err) != CodeDaemonUnavailable || calls.Load() != 1 {
		t.Fatalf("detach: %v after %d", err, calls.Load())
	}
	// The unix-socket client sends no ids and never retries.
	u := NewUnixClient("/nonexistent/sock")
	req, _ := u.newRequest(context.Background(), http.MethodPost, devPath("mock-1", "/commands/tapOn"), nil)
	if req.Header.Get(HeaderRequestID) != "" {
		t.Fatal("unix client sent a request id")
	}
	// Refused, retried three times, then DAEMON_UNAVAILABLE.
	r := NewHTTPClient("http://127.0.0.1:1", "", nil)
	r.sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := r.Status(context.Background()); codeOf(t, err) != CodeDaemonUnavailable {
		t.Fatalf("refused: %v", err)
	}
}

func TestClient_InterruptedBetweenRetries(t *testing.T) {
	c := NewHTTPClient("http://127.0.0.1:1", "", nil)
	c.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	_, err := c.Status(context.Background())
	if codeOf(t, err) != CodeInterrupted {
		t.Fatalf("%v", err)
	}
}

func TestRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{syscall.ECONNRESET, true},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{io.EOF, true},
		{errors.New("write: broken pipe"), true},
		{&net.DNSError{Err: "no such host", Name: "x"}, false},
		{errors.New("x509: certificate signed by unknown authority"), false},
	} {
		if got := retryableErr(tc.err); got != tc.want {
			t.Errorf("%v: %v", tc.err, got)
		}
	}
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{502, "<html/>", true},
		{504, "", true},
		{503, `{"ok":false,"exitCode":3,"error":{"code":"PEER_UNAVAILABLE","message":"x"}}`, true},
		{503, `{"ok":false,"exitCode":3,"error":{"code":"DAEMON_UNAVAILABLE","message":"x"}}`, false},
		{502, `{"ok":false,"exitCode":4,"error":{"code":"DEVICE_ERROR","message":"x"}}`, false},
		{503, "busy", false},
	} {
		if got := retryableResponse(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%d %s: %v", tc.status, tc.body, got)
		}
	}
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/devices/x/commands/tapOn?wait=false", true},
		{"POST", "/v1/devices/x/steps", true},
		{"POST", "/v1/devices/x/flows", true},
		{"POST", "/v1/devices/x/attach", false},
		{"GET", "/v1/devices/x/commands/tapOn", false},
		{"POST", "/v1/shutdown", false},
	} {
		if got := carriesRequestID(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s: %v", tc.method, tc.path, got)
		}
	}
	if NewHTTPClient("h:1", "", nil).BaseURL() != "http://h:1" {
		t.Fatal("BaseURL")
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Fatal("sleepCtx ignores cancellation")
	}
}
