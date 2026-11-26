package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestE2E_InstallApp installs a build from a URL through the binary: the
// headers come from a file, never the command line; the same ETag again
// is cached.
func TestE2E_InstallApp(t *testing.T) {
	e := newE2E(t)
	var mu sync.Mutex
	var sawKey []bool
	seen := func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return append([]bool(nil), sawKey...)
	}
	builds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawKey = append(sawKey, r.Header.Get("Authorization") == "Bearer build-key")
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer build-key" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("ETag", `"b1"`)
		_, _ = w.Write([]byte("PK\x03\x04 build"))
	}))
	defer builds.Close()
	headers := filepath.Join(e.cwd, "headers.json")
	if err := os.WriteFile(headers, []byte(`{"Authorization": "Bearer build-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"installApp", "--url", builds.URL + "/app.apk", "--headers-file", headers, "--app-id", "com.example", "--device", "mock-1", "--platform", "mock"}
	r := e.ok(args...)
	if !strings.Contains(r.stdout, "Installed com.example on mock-1") {
		t.Fatalf("install: %s%s", r.stdout, r.stderr)
	}
	r = e.ok(append(args, "--json")...)
	if !strings.Contains(r.stdout, `"cached": true`) || !strings.Contains(r.stdout, `"etag": "\"b1\""`) {
		t.Fatalf("again: %s%s", r.stdout, r.stderr)
	}
	r = e.ok(args...)
	if !strings.Contains(r.stdout, "com.example already installed on mock-1 (cached)") {
		t.Fatalf("cached: %s%s", r.stdout, r.stderr)
	}
	if got := seen(); len(got) != 3 || !got[0] {
		t.Fatalf("requests: %v", got)
	}

	// Without the file the server refuses: USAGE, and the key is never printed.
	r = e.fails(2, "installApp", "--url", builds.URL+"/app.apk", "--app-id", "com.example", "--device", "mock-1", "--platform", "mock")
	if strings.Contains(r.stdout+r.stderr, "build-key") {
		t.Fatalf("key printed: %s%s", r.stdout, r.stderr)
	}
	// A headers file others can read, or that is not a JSON object, is refused.
	if err := os.Chmod(headers, 0o644); err != nil {
		t.Fatal(err)
	}
	r = e.fails(2, args...)
	if !strings.Contains(r.stderr, "readable by others") {
		t.Fatalf("open file: %s", r.stderr)
	}
	bad := filepath.Join(e.cwd, "bad.json")
	if err := os.WriteFile(bad, []byte(`["Bearer build-key"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	r = e.fails(2, "installApp", "--url", builds.URL+"/app.apk", "--headers-file", bad, "--app-id", "com.example", "--device", "mock-1")
	if !strings.Contains(r.stderr, "is not a JSON object of strings") || strings.Contains(r.stderr, "build-key") {
		t.Fatalf("bad file: %s", r.stderr)
	}
	e.fails(2, "installApp", "--url", builds.URL+"/app.apk", "--device", "mock-1")
	e.fails(2, "installApp", "--headers-file", filepath.Join(e.cwd, "missing.json"), "--url", "x", "--app-id", "a", "--device", "mock-1")
}
