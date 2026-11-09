package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	imgcolor "image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// hubEnv is a daemon (with the CLI's real wiring and the mock driver)
// behind a TCP handler, and a client that targets it.
type hubEnv struct {
	c   *daemon.Client
	cwd string
}

func newHubEnv(t *testing.T) *hubEnv {
	t.Helper()
	t.Setenv(daemon.EnvHome, t.TempDir())
	s := daemon.New(daemon.Config{Name: "files", ReportsDir: filepath.Join(t.TempDir(), "reports")}, daemonDeps())
	hs := httptest.NewServer(s.TCPHandler())
	t.Cleanup(hs.Close)
	// Detach every session before TempDir cleanup removes the reports dir
	// (cleanups run last-registered first), so no report writer is still
	// writing into it.
	t.Cleanup(func() {
		s.Shutdown("test done")
		select {
		case <-s.Done():
		case <-time.After(10 * time.Second):
			t.Error("daemon did not shut down")
		}
	})
	c := daemon.NewHTTPClient(hs.URL, "", nil)
	c.ClientOutputs = true
	return &hubEnv{c: c, cwd: t.TempDir()}
}

// files is newHubFiles working in the env's directory.
func (e *hubEnv) files(ctx context.Context, quiet bool) *hubFiles {
	h := newHubFiles(ctx, e.c, "mock-1", nil, quiet)
	h.cwd = e.cwd
	return h
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPack_FileAndDeterministicDirectory(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "photo.png")
	write(t, f, []byte("png bytes"))
	u, err := packPath(f)
	if err != nil || u.Name != "photo.png" || u.File != f || u.Dir || u.Size != 9 || !strings.HasPrefix(u.Ref, "blob:sha256:"+u.SHA256+"/") {
		t.Fatalf("file unit: %+v %v", u, err)
	}

	app := filepath.Join(dir, "My.app")
	write(t, filepath.Join(app, "Info.plist"), []byte("plist"))
	write(t, filepath.Join(app, "Frameworks", "x.dylib"), []byte("lib"))
	_ = os.Chmod(filepath.Join(app, "Frameworks", "x.dylib"), 0o755)
	_ = os.Symlink("Info.plist", filepath.Join(app, "link"))
	a, err := packPath(app)
	if err != nil || !a.Dir || a.Name != "My.app" || filepath.Ext(a.File) != ".zip" {
		t.Fatalf("dir unit: %+v %v", a, err)
	}
	// Touching every file changes no hash: timestamps are fixed.
	later := time.Now().Add(time.Hour)
	_ = filepath.Walk(app, func(p string, _ os.FileInfo, _ error) error { _ = os.Chtimes(p, later, later); return nil })
	b, err := packPath(app)
	if err != nil || b.SHA256 != a.SHA256 {
		t.Fatalf("not deterministic: %s vs %s (%v)", a.SHA256, b.SHA256, err)
	}
	zr, err := zip.OpenReader(a.File)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, zf := range zr.File {
		names = append(names, zf.Name)
	}
	zr.Close()
	if strings.Join(names, ",") != "Frameworks/,Frameworks/x.dylib,Info.plist,link" {
		t.Fatalf("entries: %v", names)
	}
	if _, err := packPath(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file")
	}
}

func TestHubFiles_RewriteEveryPathField(t *testing.T) {
	env := newHubEnv(t)
	ctx := context.Background()
	write(t, filepath.Join(env.cwd, "x.js"), []byte("output.a = 1"))
	write(t, filepath.Join(env.cwd, "photo.png"), []byte("A"))
	write(t, filepath.Join(env.cwd, "b.png"), []byte("B"))
	write(t, filepath.Join(env.cwd, "auth.json"), []byte("{}"))

	isRef := func(v any) bool { s, _ := v.(string); return strings.HasPrefix(s, "blob:sha256:") }
	for name, spec := range daemon.CommandSpecs {
		for _, f := range spec.Fields {
			if !f.Path {
				continue
			}
			file := "auth.json"
			if f.PathSuffix != "" {
				file = "x.js"
			}
			h := env.files(ctx, true)
			var value any = file
			if f.Type == "[]string" {
				value = []any{file}
			}
			out, err := h.rewrite(spec, map[string]any{f.Key: value, "optional": true})
			if err != nil {
				t.Fatalf("%s.%s: %v", name, f.Key, err)
			}
			got := out.(map[string]any)[f.Key]
			if l, ok := got.([]any); ok {
				got = l[0]
			}
			if !isRef(got) || out.(map[string]any)["optional"] != true {
				t.Errorf("%s.%s = %v, want a blob ref", name, f.Key, got)
			}
		}
	}

	h := env.files(ctx, true)
	spec := func(n string) daemon.CommandSpec { return daemon.CommandSpecs[n] }
	// Bare values fill BareField: addMedia photo.png, addMedia [a, b],
	// runScript x.js.
	if v, _ := h.rewrite(spec("addMedia"), "photo.png"); !isRef(v) {
		t.Fatalf("bare addMedia: %v", v)
	}
	v, _ := h.rewrite(spec("addMedia"), []any{"photo.png", "b.png", 3})
	if l := v.([]any); !isRef(l[0]) || !isRef(l[1]) || l[2] != 3 {
		t.Fatalf("addMedia list: %v", v)
	}
	if v, _ := h.rewrite(spec("runScript"), "x.js"); !isRef(v) {
		t.Fatalf("bare runScript: %v", v)
	}
	// Inline JavaScript, a non-.js file value and a ref stay as they are.
	for _, in := range []any{"output.ok = true", map[string]any{"file": "output.f = 2"}, map[string]any{"script": "blob:sha256:x/y.js"}} {
		if v, _ := h.rewrite(spec("runScript"), in); !equalValue(v, in) {
			t.Fatalf("%v rewritten to %v", in, v)
		}
	}
	// A command with no file fields, a nil value and a typed scalar pass.
	if v, _ := h.rewrite(spec("tapOn"), "Login"); v != "Login" {
		t.Fatal(v)
	}
	if v, _ := h.rewrite(spec("addMedia"), nil); v != nil {
		t.Fatal(v)
	}
	if v, _ := h.rewrite(spec("takeScreenshot"), true); v != true {
		t.Fatal(v)
	}
	// A missing input file fails USAGE.
	if _, err := h.rewrite(spec("addMedia"), "nope.png"); err == nil {
		t.Fatal("missing file")
	}
}

func equalValue(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		if len(am) != len(bm) {
			return false
		}
		for k := range am {
			if am[k] != bm[k] {
				return false
			}
		}
		return true
	}
	return a == b
}

func TestHubFiles_OutputsAndScreenshotReferences(t *testing.T) {
	env := newHubEnv(t)
	ctx := context.Background()
	if _, err := env.c.Attach(ctx, "mock-1", daemon.AttachConfig{Platform: "mock", Driver: "mock"}); err != nil {
		t.Fatal(err)
	}
	run := func(name string, value any) (*hubFiles, *daemon.StepResult, error) {
		h := env.files(ctx, false)
		v, err := h.rewrite(daemon.CommandSpecs[name], value)
		if err != nil {
			t.Fatal(err)
		}
		res, err := env.c.Command(ctx, "mock-1", name, v, true)
		if res != nil {
			if ferr := h.fetchOutputs(res.Outputs); ferr != nil {
				t.Fatal(ferr)
			}
		}
		return h, res, err
	}

	// An extensionless takeScreenshot path: the client sends
	// <hash8>/<basename>, and writes name.png back, relative or absolute.
	h, res, err := run("takeScreenshot", "shots/name")
	if err != nil || len(res.Outputs) != 1 {
		t.Fatalf("takeScreenshot: %v %+v", err, res)
	}
	sent := ""
	for k := range h.fetch {
		sent = k
	}
	if !strings.HasPrefix(sent, "outputs/"+hash8(filepath.Join(env.cwd, "shots", "name"))+"/name") {
		t.Fatalf("client name %q", sent)
	}
	if _, err := os.Stat(filepath.Join(env.cwd, "shots", "name.png")); err != nil {
		t.Fatal("not fetched to name.png")
	}
	abs := filepath.Join(t.TempDir(), "abs", "shot.png")
	if _, _, err := run("takeScreenshot", map[string]any{"path": abs}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatal("absolute path not fetched")
	}

	// A missing assertScreenshot reference is a baseline fetched back to
	// the caller's path (extensionless: login.png).
	if _, _, err := run("assertScreenshot", "login"); err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(env.cwd, "login.png")
	data, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal("baseline not fetched")
	}
	// Now login.png exists: `assertScreenshot login` packs it and compares.
	h, res, err = run("assertScreenshot", "login")
	if err != nil || !res.OK || len(res.Outputs) != 0 {
		t.Fatalf("compare: %v %+v", err, res)
	}
	// A mismatch's diff comes back as login_diff.png beside the reference.
	write(t, ref, redImage(t))
	_, res, err = run("assertScreenshot", map[string]any{"path": "login", "thresholdPercentage": 95})
	if err == nil || len(res.Outputs) != 1 {
		t.Fatalf("mismatch: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(env.cwd, "login_diff.png")); err != nil {
		t.Fatal("diff not fetched beside the reference")
	}
	if !bytes.Equal(data, data) || h == nil {
		t.Fatal("unreachable")
	}
	// An unlisted output is not fetched.
	if err := env.files(ctx, true).fetchOutputs([]string{"outputs/x/y.png"}); err != nil {
		t.Fatal(err)
	}
	// A fetch that fails reports it.
	h = env.files(ctx, true)
	h.fetch["outputs/none.png"] = filepath.Join(env.cwd, "none.png")
	if err := h.fetchOutputs([]string{"outputs/none.png"}); err == nil {
		t.Fatal("expected a fetch error")
	}
	if withPNG("a") != "a.png" || core.DiffScreenshotPath("a.png") != "a_diff.png" {
		t.Fatal("helpers")
	}
}

func redImage(t *testing.T) []byte {
	t.Helper()
	// A 1x1 red PNG: the mock's 1x1 capture is transparent.
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, imgcolor.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUploadAttachFiles(t *testing.T) {
	env := newHubEnv(t)
	ctx := context.Background()
	write(t, filepath.Join(env.cwd, "app.apk"), []byte("PK\x03\x04"))
	cfg := daemon.AttachConfig{AppFile: filepath.Join(env.cwd, "app.apk"), Extra: map[string]any{"headed": true}}
	o := &daemonOpts{Target: "h", JSON: true}
	if err := uploadAttachFiles(ctx, env.c, o, "mock-1", &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.AppFile, "blob:sha256:") || !strings.HasSuffix(cfg.AppFile, "/app.apk") ||
		cfg.Extra["headed"] != true {
		t.Fatalf("cfg: %+v", cfg)
	}
	// Untargeted: nothing changes.
	local := daemon.AttachConfig{AppFile: "app.apk"}
	if err := uploadAttachFiles(ctx, env.c, &daemonOpts{}, "mock-1", &local); err != nil || local.AppFile != "app.apk" {
		t.Fatal(local.AppFile, err)
	}
	bad := daemon.AttachConfig{AppFile: "gone.apk"}
	if err := uploadAttachFiles(ctx, env.c, o, "mock-1", &bad); err == nil {
		t.Fatal("missing file")
	}
}

func TestAppIDOf(t *testing.T) {
	dir := t.TempDir()
	plistXML := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.example.app</string></dict></plist>`
	app := filepath.Join(dir, "My.app")
	write(t, filepath.Join(app, "Info.plist"), []byte(plistXML))
	if id, err := appIDOf(app); err != nil || id != "com.example.app" {
		t.Fatalf(".app: %q %v", id, err)
	}
	if id, err := appIDOf(app + "/"); err != nil || id != "com.example.app" {
		t.Fatalf(".app/: %q %v", id, err)
	}
	ipa := filepath.Join(dir, "My.ipa")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"Payload/My.app/Info.plist": plistXML, "Payload/My.app/Other/Info.plist": "x"} {
		w, _ := zw.Create(name)
		_, _ = io.WriteString(w, body)
	}
	_ = zw.Close()
	write(t, ipa, buf.Bytes())
	if id, err := appIDOf(ipa); err != nil || id != "com.example.app" {
		t.Fatalf(".ipa: %q %v", id, err)
	}
	// Failures: no Info.plist, no bundle id, not a zip, an unknown type.
	write(t, filepath.Join(dir, "Empty.app", "x"), nil)
	write(t, filepath.Join(dir, "NoID.app", "Info.plist"), []byte(`<plist version="1.0"><dict></dict></plist>`))
	write(t, filepath.Join(dir, "bad.ipa"), []byte("nope"))
	write(t, filepath.Join(dir, "Bad.app", "Info.plist"), []byte("\x00\x01 not a plist"))
	var empty bytes.Buffer
	zw = zip.NewWriter(&empty)
	_ = zw.Close()
	write(t, filepath.Join(dir, "none.ipa"), empty.Bytes())
	for _, p := range []string{"Empty.app", "NoID.app", "bad.ipa", "none.ipa", "Bad.app", "x.zip"} {
		if _, err := appIDOf(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s: expected an error", p)
		}
	}

	// .apk: aapt2 from the newest build-tools; without one, no id.
	sdk := t.TempDir()
	t.Setenv("ANDROID_HOME", sdk)
	t.Setenv("ANDROID_SDK_ROOT", "")
	apk := filepath.Join(dir, "s.apk")
	write(t, apk, []byte("PK"))
	if _, err := appIDOf(apk); err == nil || !strings.Contains(err.Error(), "aapt2") {
		t.Fatalf("no aapt2: %v", err)
	}
	for _, v := range []string{"9.0.0", "35.0.1", "34.0.0"} {
		write(t, filepath.Join(sdk, "build-tools", v, "aapt2"), []byte("#!/bin/sh\n"))
	}
	var ran string
	appExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		ran = name + " " + strings.Join(args, " ")
		if strings.Contains(apk, "fail") {
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "printf", "io.appium.settings\n")
	}
	t.Cleanup(func() { appExecCommand = exec.CommandContext })
	if id, err := appIDOf(apk); err != nil || id != "io.appium.settings" {
		t.Fatalf(".apk: %q %v", id, err)
	}
	if !strings.Contains(ran, filepath.Join("35.0.1", "aapt2")+" dump packagename "+apk) {
		t.Fatalf("ran %q", ran)
	}
	appExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}
	if _, err := appIDOf(apk); err == nil {
		t.Fatal("aapt2 failure")
	}
	appExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}
	if _, err := appIDOf(apk); err == nil {
		t.Fatal("empty aapt2 output")
	}
	if !versionLess("34.0.0", "35.0.1") || versionLess("35.0.1", "9.0.0") || !versionLess("1.0", "1.0.1") {
		t.Fatal("versionLess")
	}
}

func TestDaemonInstallAndUninstallApp(t *testing.T) {
	var calls []string
	prevIOS, prevSim, prevAI, prevAU := iosInstall, isIOSSim, androidInstall, androidUninstall
	t.Cleanup(func() { iosInstall, isIOSSim, androidInstall, androidUninstall = prevIOS, prevSim, prevAI, prevAU })
	iosInstall = func(udid, path string, sim bool) error {
		calls = append(calls, "ios-install "+udid+" "+path+" "+map[bool]string{true: "sim", false: "device"}[sim])
		return nil
	}
	isIOSSim = func(udid string) bool { return strings.HasPrefix(udid, "SIM") }
	androidInstall = func(serial, path string) error { calls = append(calls, "adb-install "+serial+" "+path); return nil }
	androidUninstall = func(serial, pkg string) error { calls = append(calls, "adb-uninstall "+serial+" "+pkg); return nil }
	appExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if strings.Contains(strings.Join(args, " "), "fail.app") {
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "true")
	}
	t.Cleanup(func() { appExecCommand = exec.CommandContext })
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(daemonInstallApp(ctx, "emulator-5554", "android", "/x/a.apk"))
	must(daemonInstallApp(ctx, "SIM-1", "ios", "/x/A.app"))
	must(daemonInstallApp(ctx, "00008110-000A", "ios", "/x/A.ipa"))
	must(daemonInstallApp(ctx, "mock-1", "mock", "/x/a.apk"))
	must(daemonUninstallApp(ctx, "emulator-5554", "android", "io.appium.settings"))
	must(daemonUninstallApp(ctx, "SIM-1", "ios", "com.example"))
	must(daemonUninstallApp(ctx, "00008110-000A", "ios", "com.example"))
	must(daemonUninstallApp(ctx, "mock-1", "mock", "com.example"))
	if err := daemonUninstallApp(ctx, "SIM-1", "ios", "fail.app"); err == nil {
		t.Fatal("uninstall failure")
	}
	if err := daemonInstallApp(ctx, "x", "tvos", "/a"); err == nil {
		t.Fatal("unknown platform")
	}
	if err := daemonUninstallApp(ctx, "x", "tvos", "a"); err == nil {
		t.Fatal("unknown platform")
	}
	want := []string{
		"adb-install emulator-5554 /x/a.apk",
		"ios-install SIM-1 /x/A.app sim",
		"ios-install 00008110-000A /x/A.ipa device",
		"adb-uninstall emulator-5554 io.appium.settings",
		"xcrun simctl uninstall SIM-1 com.example",
		"xcrun devicectl device uninstall app --device 00008110-000A com.example",
	}
	if strings.Join(calls[:6], "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
}

// The blob routes answer the CLI's upload for any device id, attached or
// not, and only the daemon's own store is touched.
func TestUploadPath_Messages(t *testing.T) {
	env := newHubEnv(t)
	write(t, filepath.Join(env.cwd, "a.bin"), []byte("abc"))
	a := filepath.Join(env.cwd, "a.bin")
	if _, err := uploadPath(context.Background(), env.c, "any", a, false); err != nil {
		t.Fatal(err)
	}
	if _, err := uploadPath(context.Background(), env.c, "any", a, false); err != nil {
		t.Fatal(err)
	}
	if _, err := uploadPath(context.Background(), env.c, "any", "gone.bin", true); err == nil {
		t.Fatal("missing")
	}
	bad := daemon.NewHTTPClient("http://127.0.0.1:1", "", nil)
	if _, err := uploadPath(context.Background(), bad, "any", a, true); err == nil {
		t.Fatal("unreachable")
	}
	_ = http.StatusOK
}
