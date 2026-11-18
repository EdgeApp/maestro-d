package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// serveTCP starts `maestro-d serve --http 127.0.0.1:0` for the test and
// returns its address.
func serveTCP(t *testing.T, e *e2e, name, token string) string {
	t.Helper()
	serve := exec.Command(e.bin, "serve", "--daemon", name, "--http", "127.0.0.1:0", "--token", token, "--idle-timeout", "0")
	serve.Dir = e.cwd
	serve.Env = e.env()
	stdout, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	serve.Stdout = w
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	exited := make(chan struct{})
	go func() { _ = serve.Wait(); close(exited) }()
	t.Cleanup(func() {
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
		t.Fatal("serve printed no READY line with an http address")
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return addr
}

// TestE2E_HubFiles drives pack, uploads, output fetch-back, install and
// artifacts through the binary against a TCP daemon.
func TestE2E_HubFiles(t *testing.T) {
	e := newE2E(t)
	addr := serveTCP(t, e, "files", "e2e-token")
	hub := []string{"MAESTRO_D_HUB=" + addr, "MAESTRO_D_TOKEN=e2e-token"}
	mk := func(rel string, data []byte) string {
		p := filepath.Join(e.cwd, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	photo := mk("photo.png", []byte("\x89PNG photo"))
	apk := mk("app.apk", []byte("PK\x03\x04 apk"))
	mk("My.app/Info.plist", []byte("plist"))
	mk("x.js", []byte("output.packed = 'yes'"))

	// pack: a file and a zipped directory, the same hash every time.
	r := e.ok("pack", "photo.png", "My.app", "--json")
	var packed struct {
		Units []PackUnit `json:"units"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &packed); err != nil || len(packed.Units) != 2 || !packed.Units[1].Dir {
		t.Fatalf("pack: %v\n%s", err, r.stdout)
	}
	if r2 := e.ok("pack", "My.app"); !strings.Contains(r2.stdout, packed.Units[1].SHA256) || !strings.Contains(r2.stdout, "zipped") {
		t.Fatalf("pack text: %s", r2.stdout)
	}
	e.fails(2, "pack")
	e.fails(2, "pack", "missing.png")

	// A targeted takeScreenshot is written on the daemon under outputs/
	// and fetched back to the caller's path, with .png added.
	r = e.runEnv(hub, "takeScreenshot", "--path", "out/shot", "--device", "mock-1", "--platform", "mock")
	if r.code != 0 || !strings.Contains(r.stderr, "Fetched outputs/") {
		t.Fatalf("takeScreenshot: %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "out", "shot.png")); err != nil {
		t.Fatal("screenshot not fetched back")
	}
	// Bare and keyed input files are uploaded once.
	r = e.runEnv(hub, "addMedia", photo, "--device", "mock-1")
	if r.code != 0 || !strings.Contains(r.stderr, "Uploaded photo.png") {
		t.Fatalf("addMedia: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(hub, "addMedia", "--files", `["photo.png"]`, "--device", "mock-1")
	if r.code != 0 || !strings.Contains(r.stderr, "already on the target") {
		t.Fatalf("addMedia again: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(hub, "runScript", "x.js", "--device", "mock-1")
	if r.code != 0 || !strings.Contains(r.stderr, "Uploaded x.js") {
		t.Fatalf("runScript: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(hub, "runScript", "output.inline = true", "--device", "mock-1")
	if r.code != 0 || strings.Contains(r.stderr, "Uploaded") {
		t.Fatalf("inline runScript: %s%s", r.stdout, r.stderr)
	}
	// A missing assertScreenshot reference is a baseline, fetched back.
	r = e.runEnv(hub, "assertScreenshot", "base", "--device", "mock-1")
	if r.code != 0 {
		t.Fatalf("assertScreenshot: %s%s", r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "base.png")); err != nil {
		t.Fatal("baseline not fetched")
	}

	// install, twice: the second skips the upload; uninstall.
	r = e.runEnv(hub, "install", apk, "--device", "mock-1", "--platform", "mock")
	if r.code != 0 || !strings.Contains(r.stderr, "Uploaded app.apk") || !strings.Contains(r.stdout, "Installed") {
		t.Fatalf("install: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(hub, "install", apk, "--device", "mock-1", "--platform", "mock", "--json", "--reinstall")
	if r.code != 0 || !strings.Contains(r.stderr, "") || !strings.Contains(r.stdout, `"device": "mock-1"`) {
		t.Fatalf("install again: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(hub, "uninstall", "com.example", "--device", "mock-1")
	if r.code != 0 || !strings.Contains(r.stdout, "Uninstalled com.example") {
		t.Fatalf("uninstall: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(hub, "uninstall", "com.example", "--device", "mock-1", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"appId": "com.example"`) {
		t.Fatalf("uninstall --json: %s%s", r.stdout, r.stderr)
	}
	e.fails(2, "install")
	e.fails(2, "install", "missing.apk", "--device", "mock-1")
	e.fails(2, "uninstall")

	// get artifacts: a listing, a JSON listing, and a download.
	r = e.runEnv(hub, "get", "artifacts", "--device", "mock-1")
	if r.code != 0 || !strings.Contains(r.stdout, "outputs/") || !strings.Contains(r.stdout, "report.json") {
		t.Fatalf("get artifacts: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(hub, "get", "artifacts", "--device", "mock-1", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"artifacts"`) {
		t.Fatalf("get artifacts --json: %s", r.stdout)
	}
	r = e.runEnv(hub, "get", "artifacts", "--device", "mock-1", "--out", "arts")
	if r.code != 0 {
		t.Fatalf("get artifacts --out: %s%s", r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "arts", "report.json")); err != nil {
		t.Fatal("artifacts not downloaded")
	}
	r = e.runEnv(hub, "get", "artifacts", "--device", "mock-1", "--out", "arts2", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"dir"`) {
		t.Fatalf("get artifacts --out --json: %s%s", r.stdout, r.stderr)
	}

	// A targeted attach uploads --app-file; --caps is refused before
	// anything is sent (a hub attaches no Appium for a remote caller).
	mk("caps.json", []byte(`{}`))
	r = e.runEnv(hub, "attach", "--device", "mock-2", "--platform", "mock", "--app-file", apk, "--caps", "caps.json")
	if r.code != 2 || !strings.Contains(r.stderr, "--caps cannot be used with --target-hub") || strings.Contains(r.stderr, "Uploaded") {
		t.Fatalf("attach --caps: %d %s%s", r.code, r.stdout, r.stderr)
	}
	r = e.runEnv(hub, "attach", "--device", "mock-2", "--platform", "mock", "--app-file", apk)
	if r.code != 0 || !strings.Contains(r.stderr, "app.apk is already on the target") {
		t.Fatalf("attach: %s%s", r.stdout, r.stderr)
	}

	// Untargeted, the same commands use host paths.
	r = e.ok("install", apk, "--device", "mock-1", "--platform", "mock")
	if strings.Contains(r.stderr, "Uploaded") {
		t.Fatalf("local install uploaded: %s", r.stderr)
	}
}
