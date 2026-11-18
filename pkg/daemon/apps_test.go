package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/executor"
)

type appCalls struct {
	installs   []string // platform:path
	uninstalls []string // platform:appId
	files      []string // the installed file's content
}

func appDeps(env *callerEnv, calls *appCalls) {
	env.s.deps.InstallApp = func(ctx context.Context, id, platform, path string) error {
		if id == "bad-1" {
			return errors.New("adb: device offline")
		}
		calls.installs = append(calls.installs, platform+":"+filepath.Base(path))
		data, _ := os.ReadFile(path)
		calls.files = append(calls.files, string(data))
		return nil
	}
	env.s.deps.UninstallApp = func(ctx context.Context, id, platform, appID string) error {
		if appID == "missing" {
			return errors.New("not installed")
		}
		calls.uninstalls = append(calls.uninstalls, platform+":"+appID)
		return nil
	}
	env.s.deps.AppID = func(path string) (string, error) {
		if strings.HasSuffix(path, ".apk") {
			return "io.appium.settings", nil
		}
		return "", errors.New("no aapt2")
	}
}

func TestApps_InstallAndUninstall(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	var calls appCalls
	appDeps(env, &calls)
	ctx := context.Background()
	apk := []byte("PK\x03\x04 an apk")
	h := sha(apk)
	if _, err := env.remote.UploadBlob(ctx, "emulator-5554", h, writeTemp(t, "settings.apk", apk), int64(len(apk))); err != nil {
		t.Fatal(err)
	}

	// A remote install of an uploaded blob: the installer gets a copy with
	// its basename, and the app id comes back.
	res, err := env.remote.InstallApp(ctx, "emulator-5554", InstallRequest{App: "blob:sha256:" + h + "/settings.apk", Reinstall: true})
	if err != nil || res.AppID != "io.appium.settings" {
		t.Fatalf("install: %+v %v", res, err)
	}
	if len(calls.installs) != 1 || calls.installs[0] != "android:settings.apk" || calls.files[0] != string(apk) {
		t.Fatalf("installer calls: %+v", calls)
	}
	// reinstall uninstalls first.
	if len(calls.uninstalls) != 1 || calls.uninstalls[0] != "android:io.appium.settings" {
		t.Fatalf("reinstall: %+v", calls.uninstalls)
	}
	// The temporary copy is gone.
	if entries, _ := filepath.Glob(filepath.Join(env.s.cfg.ReportsDir, ".install-*")); len(entries) != 0 {
		t.Fatalf("leftover: %v", entries)
	}

	// A remote host path, and a bad ref, are FORBIDDEN; a missing blob is USAGE.
	_, err = env.remote.InstallApp(ctx, "emulator-5554", InstallRequest{App: "/etc/hosts.apk"})
	wantErrCode(t, err, CodeForbidden)
	_, err = env.remote.InstallApp(ctx, "emulator-5554", InstallRequest{App: "blob:sha256:../../x/a.apk"})
	wantErrCode(t, err, CodeForbidden)
	_, err = env.remote.InstallApp(ctx, "emulator-5554", InstallRequest{App: "blob:sha256:" + sha([]byte("no")) + "/a.apk"})
	wantErrCode(t, err, CodeUsage)
	_, err = env.remote.InstallApp(ctx, "emulator-5554", InstallRequest{})
	wantErrCode(t, err, CodeUsage)

	// A local caller may name a host path; the platform comes from the
	// registry when the file does not tell, and an unreadable id is omitted.
	app := writeTemp(t, "thing.bin", []byte("x"))
	res, err = env.local.InstallApp(ctx, "mock-1", InstallRequest{App: app})
	if err != nil || res.AppID != "" || calls.installs[1] != "mock:thing.bin" {
		t.Fatalf("local install: %+v %v %v", res, err, calls.installs)
	}
	if _, err := env.local.InstallApp(ctx, "bad-1", InstallRequest{App: app, Platform: "ios"}); codeOf(t, err) != CodeDeviceError {
		t.Fatal("installer failure is DEVICE_ERROR")
	}

	if err := env.remote.UninstallApp(ctx, "emulator-5554", "io.appium.settings"); err != nil {
		t.Fatal(err)
	}
	if err := env.remote.UninstallApp(ctx, "emulator-5554", "missing"); codeOf(t, err) != CodeDeviceError {
		t.Fatal("uninstall failure is DEVICE_ERROR")
	}

	// Without hooks both routes fail INTERNAL.
	env.s.deps.InstallApp, env.s.deps.UninstallApp = nil, nil
	_, err = env.local.InstallApp(ctx, "mock-1", InstallRequest{App: app})
	wantErrCode(t, err, CodeInternal)
	wantErrCode(t, env.local.UninstallApp(ctx, "mock-1", "x"), CodeInternal)
}

func TestAppPlatform(t *testing.T) {
	for in, want := range map[string]string{"a.apk": "android", "b.APK": "android", "c.app/": "ios", "d.ipa": "ios", "e.zip": ""} {
		if got := appPlatform(in); got != want {
			t.Fatalf("%s: %q", in, got)
		}
	}
}

func TestAttach_BlobAppFileAndCaps(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	var got AttachConfig
	next := env.s.deps.NewDriver
	env.s.deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
		got = cfg
		return next(ctx, id, cfg)
	}
	apk, caps := []byte("PK\x03\x04apk"), []byte(`{"platformName":"Android"}`)
	for name, d := range map[string][]byte{"app.apk": apk, "caps.json": caps} {
		if _, err := env.remote.UploadBlob(ctx, "mock-1", sha(d), writeTemp(t, name, d), int64(len(d))); err != nil {
			t.Fatal(err)
		}
	}
	_, err := env.remote.Attach(ctx, "mock-1", AttachConfig{Platform: "mock",
		AppFile: "blob:sha256:" + sha(apk) + "/app.apk"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got.AppFile) != "app.apk" || !strings.Contains(got.AppFile, string(filepath.Separator)+"blobs"+string(filepath.Separator)) {
		t.Fatalf("appFile %q", got.AppFile)
	}
	// A remote caller gets no Appium capabilities, blob or not
	// (5.harness-review.1); a unix-socket caller's blob capsFile is mapped.
	_, err = env.remote.Attach(ctx, "mock-2", AttachConfig{Platform: "mock", Extra: map[string]any{"capsFile": "blob:sha256:" + sha(caps) + "/caps.json"}})
	wantErrCode(t, err, CodeForbidden)
	if _, err = env.local.Attach(ctx, "mock-2", AttachConfig{Platform: "mock",
		Extra: map[string]any{"capsFile": "blob:sha256:" + sha(caps) + "/caps.json"}}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(got.Extra["capsFile"].(string)); !bytes.Equal(data, caps) {
		t.Fatalf("capsFile %v", got.Extra["capsFile"])
	}
	_, err = env.remote.Attach(ctx, "mock-3", AttachConfig{Platform: "mock", AppFile: "blob:sha256:nothex/app.apk"})
	wantErrCode(t, err, CodeForbidden)
	_, err = env.local.Attach(ctx, "mock-3", AttachConfig{Platform: "mock", Extra: map[string]any{"capsFile": "blob:sha256:" + sha([]byte("none")) + "/c.json"}})
	wantErrCode(t, err, CodeUsage)
	_, err = env.local.Attach(ctx, "mock-3", AttachConfig{Platform: "mock", Extra: map[string]any{"capsFile": "blob:sha256:" + sha([]byte("none")) + "/../c.json"}})
	wantErrCode(t, err, CodeForbidden)
	_, err = env.remote.Attach(ctx, "mock-3", AttachConfig{Platform: "mock", AppFile: "blob:sha256:" + sha([]byte("none")) + "/a.apk"})
	wantErrCode(t, err, CodeUsage)
}

// An attach that fails after it materialized its blobs removes them: a
// packed .app is a full unzipped copy, and every retry would leave another.
func TestAttach_FailedAttachRemovesItsBlobs(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	apk := []byte("PK\x03\x04apk")
	if _, err := env.remote.UploadBlob(ctx, "mock-1", sha(apk), writeTemp(t, "app.apk", apk), int64(len(apk))); err != nil {
		t.Fatal(err)
	}
	blobDirs := func() []string {
		var out []string
		_ = filepath.WalkDir(env.s.cfg.ReportsDir, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && d.Name() == "blobs" {
				out = append(out, p)
			}
			return nil
		})
		return out
	}
	app := "blob:sha256:" + sha(apk) + "/app.apk"
	// The driver does not start.
	env.s.deps.NewDriver = func(context.Context, string, AttachConfig) (core.Driver, DriverInfo, func(), error) {
		return nil, DriverInfo{}, nil, io.ErrUnexpectedEOF
	}
	_, err := env.remote.Attach(ctx, "mock-1", AttachConfig{Platform: "mock", AppFile: app})
	wantErrCode(t, err, CodeDeviceError)
	if d := blobDirs(); len(d) != 0 {
		t.Fatalf("a failed driver start left its blobs: %v", d)
	}
	// The second blob is missing, after the first was materialized (a
	// capsFile blob comes only from a unix-socket caller).
	_, err = env.local.Attach(ctx, "mock-1", AttachConfig{Platform: "mock", AppFile: app,
		Extra: map[string]any{"capsFile": "blob:sha256:" + sha([]byte("none")) + "/c.json"}})
	wantErrCode(t, err, CodeUsage)
	if d := blobDirs(); len(d) != 0 {
		t.Fatalf("a failed blob mapping left its blobs: %v", d)
	}
	// A blobs directory that was there before the attach stays.
	out := t.TempDir()
	keep := filepath.Join(out, "blobs", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = env.local.Attach(ctx, "mock-1", AttachConfig{Platform: "mock", AppFile: app, OutputDir: out})
	wantErrCode(t, err, CodeDeviceError)
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("an earlier blobs directory was removed: %v", err)
	}
	// ...and the failed attach's own blobs in it are gone.
	if ents, _ := os.ReadDir(filepath.Dir(keep)); len(ents) != 1 || ents[0].Name() != "keep.txt" {
		names := []string{}
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("a failed attach in a reused outputDir left %v", names)
	}
	// A successful attach keeps its blobs, in a directory of its own.
	env.s.deps.NewDriver = testDeps(new(int32), 0).NewDriver
	var got AttachConfig
	next := env.s.deps.NewDriver
	env.s.deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
		got = cfg
		return next(ctx, id, cfg)
	}
	if _, err := env.local.Attach(ctx, "mock-1", AttachConfig{Platform: "mock", AppFile: app, OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(filepath.Dir(keep), got.AppFile); err != nil || len(strings.Split(rel, string(filepath.Separator))) != 3 {
		t.Fatalf("appFile %q is not in a directory of the attach's own under blobs/", got.AppFile)
	}
	if _, err := os.Stat(got.AppFile); err != nil {
		t.Fatalf("a successful attach lost its blob: %v", err)
	}
}

func TestArtifacts_ListAndFetch(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	info, err := env.remote.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := env.remote.Command(ctx, "mock-1", "takeScreenshot", map[string]any{"path": "ab12cd34/shot"}, true)
	if err != nil || len(res.Outputs) != 1 || res.Outputs[0] != "outputs/ab12cd34/shot.png" {
		t.Fatalf("takeScreenshot: %+v %v", res, err)
	}
	// Blob copies and the denied/ stand-in are inputs, not artifacts.
	_ = os.MkdirAll(filepath.Join(info.ReportDir, "blobs", "x"), 0o755)
	_ = os.WriteFile(filepath.Join(info.ReportDir, "blobs", "x", "in.png"), []byte("x"), 0o644)
	_ = os.Symlink("/etc/hosts", filepath.Join(info.ReportDir, "hosts-link"))
	list, err := env.remote.Artifacts(ctx, "mock-1")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, a := range list.Artifacts {
		paths = append(paths, a.Path)
	}
	sort.Strings(paths)
	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "outputs/ab12cd34/shot.png") || !strings.Contains(joined, "report.json") || strings.Contains(joined, "blobs/") || strings.Contains(joined, "hosts-link") {
		t.Fatalf("artifacts: %v", paths)
	}
	var buf bytes.Buffer
	if err := env.remote.Artifact(ctx, "mock-1", "outputs/ab12cd34/shot.png", &buf); err != nil || !bytes.HasPrefix(buf.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("fetch: %v", err)
	}
	for _, rel := range []string{"report.json", "junit-report.xml"} {
		buf.Reset()
		if err := env.remote.Artifact(ctx, "mock-1", rel, &buf); err != nil || buf.Len() == 0 {
			t.Fatalf("%s: %v", rel, err)
		}
	}
	// Traversal, an absolute path and a symlink out are FORBIDDEN; a
	// missing file and a directory are USAGE.
	for _, raw := range []string{"..%2F..%2Fetc%2Fhosts", "outputs%2F..%2F..%2Fx", "%2Fetc%2Fhosts", "hosts-link"} {
		req, _ := http.NewRequest(http.MethodGet, env.remote.BaseURL()+"/v1/devices/mock-1/artifacts/"+raw, nil)
		req.Header.Set("Authorization", "Bearer t")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: %d", raw, resp.StatusCode)
		}
	}
	wantErrCode(t, env.remote.Artifact(ctx, "mock-1", "nope.png", &buf), CodeUsage)
	wantErrCode(t, env.remote.Artifact(ctx, "mock-1", "outputs", &buf), CodeUsage)
	if _, e := artifactPath(info.ReportDir, ""); e == nil {
		t.Fatal("empty path")
	}
	// Not attached: DEVICE_NOT_ATTACHED.
	_, err = env.remote.Artifacts(ctx, "mock-9")
	wantErrCode(t, err, CodeDeviceNotAttached)
	wantErrCode(t, env.remote.Artifact(ctx, "mock-9", "x", &buf), CodeDeviceNotAttached)
}

func TestOutputs_RemoteAndLocal(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	info, err := env.local.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	// A local call writes where it does today and lists no outputs.
	local := filepath.Join(t.TempDir(), "local.png")
	res, err := env.local.Command(ctx, "mock-1", "takeScreenshot", map[string]any{"path": local}, true)
	if err != nil || res.Outputs != nil {
		t.Fatalf("local: %+v %v", res, err)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatal("local screenshot not written")
	}
	// A loopback TCP caller that names its own outputs gets them under
	// outputs/, and the message names the output, not the daemon's path.
	env.lo.ClientOutputs = true
	res, err = env.lo.Command(ctx, "mock-1", "takeScreenshot", "deadbeef/lo", true)
	if err != nil || len(res.Outputs) != 1 || res.Outputs[0] != "outputs/deadbeef/lo.png" || strings.Contains(res.Message, info.ReportDir) {
		t.Fatalf("client outputs: %+v %v", res, err)
	}
	// A batch collects every result's outputs.
	steps, err := env.remote.Steps(ctx, "mock-1", StepsRequest{YAML: "- takeScreenshot: a/one\n- tapOn: X\n- takeScreenshot: b/two\n"}, true)
	if err != nil || strings.Join(steps.Outputs, ",") != "outputs/a/one.png,outputs/b/two.png" {
		t.Fatalf("steps outputs: %+v %v", steps, err)
	}
	// A flow too.
	fr, err := env.remote.Flow(ctx, "mock-1", FlowRequest{YAML: "appId: x\n---\n- takeScreenshot: f/shot\n"}, true)
	if err != nil || len(fr.Outputs) != 1 || fr.Outputs[0] != "outputs/f/shot.png" {
		t.Fatalf("flow outputs: %+v %v", fr, err)
	}
	// Raw absolute and ".." output paths are FORBIDDEN.
	for _, p := range []string{"/etc/x.png", "../x.png"} {
		_, err := env.remote.Command(ctx, "mock-1", "takeScreenshot", map[string]any{"path": p}, true)
		wantErrCode(t, err, CodeForbidden)
	}
	if _, err := os.Stat(filepath.Join(info.ReportDir, "..", "x.png")); err == nil {
		t.Fatal("a .. output was written")
	}
}

func TestStepResult_ForRemote(t *testing.T) {
	r := StepResult{Message: "Screenshot saved: /r/d/outputs/x.png"}
	r.Error = &ErrorBody{Message: "Check the diff image at /r/d/outputs/a_diff.png"}
	r.ForRemote("/r/d/")
	if r.Message != "Screenshot saved: outputs/x.png" || r.Error.Message != "Check the diff image at outputs/a_diff.png" {
		t.Fatalf("%+v", r)
	}
	r.ForRemote("")
}

// Every field the generated registry marks Path or Output is one the caller
// policy walks (TestPolicyFields_EqualCommandSpecMarks checks the whole set),
// and the bare fields and suffixes are as the clients expect.
func TestPolicyFields_CoverPathAndOutputFields(t *testing.T) {
	policy := executor.PolicyFields()
	n := 0
	for name, spec := range CommandSpecs {
		for _, f := range spec.Fields {
			if !f.Path && !f.Output {
				continue
			}
			n++
			found := false
			for _, pf := range policy[name] {
				found = found || pf == f.Key
			}
			if !found {
				t.Errorf("%s.%s is marked Path/Output but the caller policy does not walk it", name, f.Key)
			}
		}
		if spec.BareField != "" && spec.BareField != spec.Scalar && name != "addMedia" {
			t.Errorf("%s: unexpected BareField %q", name, spec.BareField)
		}
	}
	if n < 11 {
		t.Fatalf("only %d marked fields", n)
	}
	if CommandSpecs["addMedia"].BareField != "files" || CommandSpecs["takeScreenshot"].BareField != "path" {
		t.Fatal("bare fields")
	}
	for _, f := range CommandSpecs["runScript"].Fields {
		if (f.Key == "script" || f.Key == "file") && f.PathSuffix != ".js" {
			t.Fatalf("runScript.%s suffix %q", f.Key, f.PathSuffix)
		}
	}
	for _, f := range CommandSpecs["startRecording"].Fields {
		if f.Path || f.Output {
			t.Fatal("startRecording.path is not a host file")
		}
	}
}
