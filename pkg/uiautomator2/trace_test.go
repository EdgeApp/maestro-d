package uiautomator2

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientTracesOnlyWhenAskedAndRedacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value":null}`))
	}))
	defer srv.Close()

	c := &Client{http: srv.Client(), baseURL: srv.URL, logger: createLogger(), sessionID: "s"}
	if _, err := c.request("POST", "/session/s/element/e/value", map[string]any{"text": "1234"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/tmp/maestro-client.log"); err == nil {
		if data, _ := os.ReadFile("/tmp/maestro-client.log"); strings.Contains(string(data), "/session/s/element/e/value") {
			t.Error("the default client wrote /tmp/maestro-client.log")
		}
	}

	path := filepath.Join(t.TempDir(), "client.log")
	c.SetLogPath(path)
	_ = c.SendKeyActions("pin")
	_ = c.SetClipboard("sécret")
	if _, err := c.request("POST", "/session/s/element/e/value", map[string]any{"text": "1234"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(data)
	for _, leak := range []string{"1234", `"value":"p"`, `"value":"i"`, `"value":"n"`, "c8OpY3JldA=="} {
		if strings.Contains(trace, leak) {
			t.Errorf("trace leaks %q:\n%s", leak, trace)
		}
	}
	if !strings.Contains(trace, "<redacted 4 chars>") || !strings.Contains(trace, "/actions") {
		t.Errorf("trace lacks the redacted request lines:\n%s", trace)
	}
	c.SetLogPath(filepath.Join(path, "bad", "x.log")) // unopenable: keeps the old trace
	if c.logger == nil {
		t.Error("an unopenable path dropped the logger")
	}
}

func TestCloseEndsTheTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value":null}`))
	}))
	defer srv.Close()
	c := &Client{http: srv.Client(), baseURL: srv.URL, logger: createLogger(), sessionID: "s"}
	path := filepath.Join(t.TempDir(), "client.log")
	c.SetLogPath(path)
	c.SetLogPath(path) // a second trace closes the first file
	_ = c.Close()
	if c.traceFile != nil || c.traced {
		t.Error("Close left the trace open")
	}
	size := func() int64 { fi, _ := os.Stat(path); return fi.Size() }
	before := size()
	_, _ = c.request("GET", "/status", nil)
	if size() != before {
		t.Error("a request after Close still wrote to the trace")
	}
}
