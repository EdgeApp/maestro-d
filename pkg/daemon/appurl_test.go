package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildServers stands in for Mason and its Space: Mason checks the bearer
// key and redirects to a signed Space URL, which serves the build with its
// ETag and records the headers each request carried.
type buildServers struct {
	mason, space *httptest.Server
	mu           sync.Mutex
	spaceHeaders []http.Header
	etag         string
	honor304     bool
	body         string
	disposition  string
}

func newBuildServers(t *testing.T) *buildServers {
	b := &buildServers{etag: `"v1"`, body: "PK\x03\x04 the build"}
	b.space = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.spaceHeaders = append(b.spaceHeaders, r.Header.Clone())
		etag, honor, body, disp := b.etag, b.honor304, b.body, b.disposition
		b.mu.Unlock()
		if r.URL.Query().Get("sig") != "s3cret" {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		w.Header().Set("ETag", etag)
		if honor && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if disp != "" {
			w.Header().Set("Content-Disposition", disp)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(b.space.Close)
	b.mason = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mason-key" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, b.space.URL+"/builds/app-release.apk?sig=s3cret", http.StatusFound)
	}))
	t.Cleanup(b.mason.Close)
	return b
}

func (b *buildServers) lastSpaceHeader() http.Header {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spaceHeaders[len(b.spaceHeaders)-1]
}

func TestApps_InstallFromURL(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	var calls appCalls
	appDeps(env, &calls)
	b := newBuildServers(t)
	ctx := context.Background()
	req := InstallRequest{URL: b.mason.URL + "/api/v2/builds/7/download", AppID: "app.teamstation.develop",
		Headers: map[string]string{"Authorization": "Bearer mason-key", "X-Mason-Extra": "x"}}

	// A remote caller may name a URL: the daemon downloads through the
	// redirect, without the caller's headers past it, and installs.
	res, err := env.remote.InstallApp(ctx, "emulator-5554", req)
	if err != nil || res.Cached || res.ETag != `"v1"` || res.AppID != "app.teamstation.develop" {
		t.Fatalf("install: %+v %v", res, err)
	}
	if len(calls.installs) != 1 || calls.installs[0] != "android:app-release.apk" || calls.files[0] != b.body {
		t.Fatalf("installer calls: %+v", calls)
	}
	if h := b.lastSpaceHeader(); h.Get("Authorization") != "" || h.Get("X-Mason-Extra") != "" {
		t.Fatalf("the Space got the caller's headers: %v", h)
	}
	if entries, _ := filepath.Glob(filepath.Join(env.s.cfg.ReportsDir, ".install-*")); len(entries) != 0 {
		t.Fatalf("leftover: %v", entries)
	}
	info, err := os.Stat(filepath.Join(Home(), installsFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("install record: %v %v", info, err)
	}

	// The same build again is cached: nothing installed. The Space was
	// asked with If-None-Match.
	res, err = env.remote.InstallApp(ctx, "emulator-5554", req)
	if err != nil || !res.Cached || res.ETag != `"v1"` || len(calls.installs) != 1 {
		t.Fatalf("second install: %+v %v %+v", res, err, calls)
	}
	if h := b.lastSpaceHeader(); h.Get("If-None-Match") != `"v1"` {
		t.Fatalf("If-None-Match: %v", h)
	}
	// A 304 is cached too.
	b.mu.Lock()
	b.honor304 = true
	b.mu.Unlock()
	if res, err = env.remote.InstallApp(ctx, "emulator-5554", req); err != nil || !res.Cached || len(calls.installs) != 1 {
		t.Fatalf("304: %+v %v", res, err)
	}
	// reinstall downloads and installs whatever the cache says.
	reinstall := req
	reinstall.Reinstall = true
	if res, err = env.remote.InstallApp(ctx, "emulator-5554", reinstall); err != nil || res.Cached || len(calls.installs) != 2 {
		t.Fatalf("reinstall: %+v %v", res, err)
	}
	// Another device, or a new build, installs.
	if res, err = env.remote.InstallApp(ctx, "SIM-1", req); err != nil || res.Cached || len(calls.installs) != 3 {
		t.Fatalf("another device: %+v %v", res, err)
	}
	b.mu.Lock()
	b.etag, b.disposition = `"v2"`, `attachment; filename="TeamStation.ipa"`
	b.mu.Unlock()
	if res, err = env.remote.InstallApp(ctx, "emulator-5554", req); err != nil || res.Cached || res.ETag != `"v2"` ||
		calls.installs[len(calls.installs)-1] != "ios:TeamStation.ipa" {
		t.Fatalf("new build: %+v %v %+v", res, err, calls.installs)
	}
	// A build without an ETag is installed every time, and forgets the
	// earlier one.
	b.mu.Lock()
	b.etag, b.honor304 = "", false
	b.mu.Unlock()
	if res, err = env.remote.InstallApp(ctx, "emulator-5554", req); err != nil || res.Cached || res.ETag != "" {
		t.Fatalf("no etag: %+v %v", res, err)
	}
	if got := env.s.installedETag("emulator-5554", req.AppID); got != "" {
		t.Fatalf("kept %q", got)
	}
}

func TestApps_InstallFromURLErrors(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	var calls appCalls
	appDeps(env, &calls)
	b := newBuildServers(t)
	ctx := context.Background()
	good := b.mason.URL + "/download?token=hidden-query"
	for name, c := range map[string]struct {
		req   InstallRequest
		code  Code
		quiet string
	}{
		"both":     {InstallRequest{App: "x.apk", URL: good, AppID: "a"}, CodeUsage, ""},
		"no appId": {InstallRequest{URL: good}, CodeUsage, ""},
		"not http": {InstallRequest{URL: "ftp://host/x.apk", AppID: "a"}, CodeUsage, ""},
		"bad url":  {InstallRequest{URL: "http://[::1", AppID: "a"}, CodeUsage, ""},
		"bad key": {InstallRequest{URL: good, AppID: "a", Headers: map[string]string{"Authorization": "Bearer wrong"}},
			CodeUsage, "hidden-query"},
		"neither": {InstallRequest{}, CodeUsage, ""},
	} {
		_, err := env.remote.InstallApp(ctx, "emulator-5554", c.req)
		if codeOf(t, err) != c.code {
			t.Errorf("%s: %v", name, err)
		}
		if c.quiet != "" && strings.Contains(err.Error(), c.quiet) {
			t.Errorf("%s: the error quotes the query: %v", name, err)
		}
	}
	// A server that is gone is a DEVICE_ERROR that does not quote the query.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	_, err := env.remote.InstallApp(ctx, "emulator-5554", InstallRequest{URL: gone.URL + "/x.apk?sig=s3cret", AppID: "a"})
	if codeOf(t, err) != CodeDeviceError || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("gone: %v", err)
	}
	// A download cut short is a DEVICE_ERROR, and leaves no file.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("PK"))
	}))
	defer short.Close()
	if _, err := env.remote.InstallApp(ctx, "emulator-5554", InstallRequest{URL: short.URL + "/x.apk", AppID: "a"}); codeOf(t, err) != CodeDeviceError {
		t.Fatalf("short: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(env.s.cfg.ReportsDir, ".install-*")); len(entries) != 0 {
		t.Fatalf("leftover: %v", entries)
	}
	// An installer failure is DEVICE_ERROR, and nothing is cached.
	if _, err := env.remote.InstallApp(ctx, "bad-1", InstallRequest{URL: b.mason.URL + "/d", AppID: "a",
		Headers: map[string]string{"Authorization": "Bearer mason-key"}}); codeOf(t, err) != CodeDeviceError {
		t.Fatalf("installer failure: %v", err)
	}
	if got := env.s.installedETag("bad-1", "a"); got != "" {
		t.Fatalf("a failed install was recorded: %q", got)
	}
	if len(calls.installs) != 0 {
		t.Fatalf("installs: %v", calls.installs)
	}
}

// A download's file name gives the installer its extension.
func TestAppFileName(t *testing.T) {
	mk := func(disp, path string) *http.Response {
		r := &http.Response{Header: http.Header{}}
		if disp != "" {
			r.Header.Set("Content-Disposition", disp)
		}
		if path != "" {
			r.Request = httptest.NewRequest(http.MethodGet, path, nil)
		}
		return r
	}
	for _, c := range []struct {
		resp     *http.Response
		platform string
		want     string
	}{
		{mk(`attachment; filename="../x/My App.ipa"`, "/d"), "android", "My App.ipa"},
		{mk(`attachment; filename="notes.txt"`, "/b/app.apk"), "ios", "app.apk"},
		{mk("", "/download"), "ios", "app.ipa"},
		{mk("", "/download"), "tvos", "app.ipa"},
		{mk("", ""), "", "app.apk"},
	} {
		if got := appFileName(c.resp, c.platform); got != c.want {
			t.Errorf("%+v: %s, want %s", c, got, c.want)
		}
	}
	if got := redactedURL("https://u:p@host/a/b?sig=x#f"); got != "https://host/a/b" {
		t.Errorf("redacted: %s", got)
	}
	if got := redactedURL("http://[::1"); got != "(unparseable url)" {
		t.Errorf("redacted: %s", got)
	}
}

// A record that cannot be read is empty, and one that cannot be written is
// logged, not fatal.
func TestInstallRecord_Broken(t *testing.T) {
	env := newCallerEnv(t, Config{})
	if err := os.WriteFile(filepath.Join(Home(), installsFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := env.s.installedETag("d", "a"); got != "" {
		t.Fatalf("got %q", got)
	}
	env.s.noteInstalled("d", "a", `"e"`)
	if got := env.s.installedETag("d", "a"); got != `"e"` {
		t.Fatalf("after a write: %q", got)
	}
	// The record's directory gone: the write is logged.
	t.Setenv(EnvHome, filepath.Join(t.TempDir(), "missing"))
	env.s.noteInstalled("d", "a", `"e"`)
}
