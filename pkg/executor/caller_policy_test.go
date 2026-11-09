package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func remoteCtx(p CallerPolicy) context.Context {
	return WithCallerPolicy(context.Background(), &p)
}

func wantForbidden(t *testing.T, out *StepOutcome, field string) {
	t.Helper()
	if out.Success || out.Forbidden == nil {
		t.Fatalf("expected a FORBIDDEN outcome, got success=%v forbidden=%v err=%v", out.Success, out.Forbidden, out.Error)
	}
	if field != "" && out.Forbidden.Field != field {
		t.Fatalf("denied field = %q, want %q (%+v)", out.Forbidden.Field, field, out.Forbidden)
	}
	if out.Cancelled {
		t.Fatal("a denial is reported as FORBIDDEN, not as a cancellation")
	}
}

// secretScript writes a script on the "host" that would set output.leaked
// if anything ran it.
func secretScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret.js")
	if err := os.WriteFile(p, []byte("output.leaked = 'yes'"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPolicyFields_CoversEveryPathField(t *testing.T) {
	got := PolicyFields()
	want := map[string][]string{
		"addMedia":         {"files"},
		"assertScreenshot": {"path"},
		"loadAuthState":    {"path"},
		"retry":            {"file"},
		"runBrowserScript": {"file"},
		"runFlow":          {"else", "file"},
		"runScript":        {"file", "script"},
		"runWebViewScript": {"file"},
		"saveAuthState":    {"path"},
		"takeScreenshot":   {"path"},
		"uploadFile":       {"path", "paths"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PolicyFields() = %v\nwant %v", got, want)
	}
}

func TestPolicy_RemoteRunShellForbiddenEverywhere(t *testing.T) {
	cases := map[string]string{
		"top level":         `{"runShell": "echo hi"}`,
		"runFlow.commands":  `{"runFlow": {"commands": [{"tapOn": "A"}, {"runShell": "echo hi"}]}}`,
		"retry.commands":    `{"retry": {"maxRetries": 1, "commands": [{"runShell": "echo hi"}]}}`,
		"repeat":            `{"repeat": {"times": 2, "commands": [{"runShell": "echo hi"}]}}`,
		"runFlow when else": `{"runFlow": {"when": {"true": "${false}"}, "commands": [{"tapOn": "A"}], "else": [{"runShell": "echo hi"}]}}`,
	}
	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
			out := s.Execute(remoteCtx(CallerPolicy{Remote: true}), mustStep(t, step))
			wantForbidden(t, out, "")
			if out.Forbidden.Command != "runShell" {
				t.Fatalf("denial = %+v", out.Forbidden)
			}
		})
	}
}

func TestPolicy_RemoteRunShellInStepsAndInlineFlow(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
	steps, err := flow.ParseSteps([]byte("- tapOn: A\n- runShell: echo hi\n- tapOn: B\n"), "request")
	if err != nil {
		t.Fatal(err)
	}
	outs := s.ExecuteAll(remoteCtx(CallerPolicy{Remote: true}), steps, true)
	if len(outs) != 2 || !outs[0].Success {
		t.Fatalf("the run must stop at the denied step, even with continueOnError: %d outcomes", len(outs))
	}
	wantForbidden(t, outs[1], "")

	f, err := flow.Parse([]byte("appId: x\n---\n- tapOn: A\n- runShell: echo hi\n"), "inline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wantForbidden(t, s.RunFlow(remoteCtx(CallerPolicy{Remote: true}), *f), "")
}

func TestPolicy_AllowShellRunsIt(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
	out := s.Execute(remoteCtx(CallerPolicy{Remote: true, AllowShell: true}), mustStep(t, `{"runShell": {"command": "echo hello", "output": "said"}}`))
	if !out.Success {
		t.Fatalf("runShell with AllowShell: %+v", out.Error)
	}
	if s.Vars()["said"] != "hello" {
		t.Fatalf("runShell did not run: %v", s.Vars())
	}
}

func TestPolicy_LocalCallerUnchanged(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
	local := WithCallerPolicy(context.Background(), &CallerPolicy{})
	out := s.Execute(local, mustStep(t, `{"runShell": {"command": "echo local", "output": "said"}}`))
	if !out.Success || s.Vars()["said"] != "local" {
		t.Fatalf("a local caller's runShell must run: %+v %v", out.Error, s.Vars())
	}
	script := secretScript(t)
	out = s.Execute(local, mustStep(t, `{"runScript": "`+script+`"}`))
	if !out.Success || s.Vars()["leaked"] != "yes" {
		t.Fatalf("a local caller's host path must work: %+v", out.Error)
	}
	// No policy at all (upstream's maestro-runner test) behaves the same.
	out = s.Execute(context.Background(), mustStep(t, `{"addMedia": ["/no/such/photo.png"]}`))
	if out.Forbidden != nil {
		t.Fatal("no policy, no denial")
	}
}

func TestPolicy_HostPathsForbidden(t *testing.T) {
	script := secretScript(t)
	cases := []struct {
		name, step, field string
	}{
		{"addMedia", `{"addMedia": ["/etc/hosts"]}`, "files"},
		{"addMedia relative", `{"addMedia": ["photo.png"]}`, "files"},
		{"runScript", `{"runScript": "` + script + `"}`, ""},
		{"runScript file", `{"runScript": {"file": "` + script + `"}}`, ""},
		{"uploadFile", `{"uploadFile": {"id": "f", "path": "/etc/hosts"}}`, "path"},
		{"uploadFile paths", `{"uploadFile": {"id": "f", "paths": ["/etc/hosts"]}}`, "paths"},
		{"runWebViewScript", `{"runWebViewScript": {"file": "/etc/x.js"}}`, "file"},
		{"runBrowserScript", `{"runBrowserScript": {"file": "/etc/x.js"}}`, "file"},
		{"loadAuthState", `{"loadAuthState": "/etc/state.json"}`, "path"},
		{"assertScreenshot", `{"assertScreenshot": "/etc/ref.png"}`, ""},
		{"runFlow file", `{"runFlow": "/etc/hosts"}`, ""},
		{"retry file", `{"retry": {"maxRetries": 1, "file": "/etc/hosts"}}`, ""},
		{"nested runFlow", `{"runFlow": {"commands": [{"addMedia": ["/etc/hosts"]}]}}`, "files"},
		{"nested repeat runScript", `{"repeat": {"times": 1, "commands": [{"runScript": "` + script + `"}]}}`, ""},
		{"from a variable", `{"addMedia": ["${HOSTFILE}"]}`, "files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
			s.SetVar("HOSTFILE", "/etc/hosts")
			out := s.Execute(remoteCtx(CallerPolicy{Remote: true}), mustStep(t, tc.step))
			wantForbidden(t, out, tc.field)
			if s.Vars()["leaked"] != "" {
				t.Fatal("the host script was read and run")
			}
		})
	}
}

func TestPolicy_DenialStopsTheRunAndReadsNothing(t *testing.T) {
	script := secretScript(t)
	s, dir := newTestSession(t, SessionConfig{}, mock.Config{})
	steps, err := flow.ParseSteps([]byte("- runScript: "+script+"\n- evalScript: ${output.after = 'ran'}\n"), "request")
	if err != nil {
		t.Fatal(err)
	}
	outs := s.ExecuteAll(remoteCtx(CallerPolicy{Remote: true}), steps, false)
	if len(outs) != 1 {
		t.Fatalf("ran %d steps, want 1", len(outs))
	}
	wantForbidden(t, outs[0], "")
	vars := s.Vars()
	if vars["leaked"] != "" || vars["after"] != "" {
		t.Fatalf("nothing may run after a denial: %v", vars)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "denied"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("denied/ must exist and stay empty: %v %v", entries, err)
	}
	// The next call starts with a clean policy.
	out := s.Execute(remoteCtx(CallerPolicy{Remote: true}), mustStep(t, `{"tapOn": "OK"}`))
	if !out.Success {
		t.Fatalf("a later allowed call: %+v", out.Error)
	}
}

func TestPolicy_InlineJavaScriptPasses(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
	out := s.Execute(remoteCtx(CallerPolicy{Remote: true}), mustStep(t, `{"runScript": "output.ok = true"}`))
	if !out.Success || s.Vars()["ok"] != "true" {
		t.Fatalf("inline runScript from a remote caller: %+v", out.Error)
	}
}

func TestPolicy_OutputsRewrittenUnderOutputsDir(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
	outputs := filepath.Join(t.TempDir(), "outputs")
	ctx := remoteCtx(CallerPolicy{Remote: true, OutputsDir: outputs})
	out := s.Execute(ctx, mustStep(t, `{"takeScreenshot": "shots/home"}`))
	if !out.Success {
		t.Fatalf("relative takeScreenshot: %+v", out.Error)
	}
	if _, err := os.Stat(filepath.Join(outputs, "shots", "home.png")); err != nil {
		t.Fatalf("screenshot not under outputs/: %v", err)
	}
	// Repeated (a repeat loop re-expands the same step): idempotent.
	out = s.Execute(ctx, mustStep(t, `{"repeat": {"times": 2, "commands": [{"takeScreenshot": "loop"}]}}`))
	if !out.Success {
		t.Fatalf("repeat takeScreenshot: %+v", out.Error)
	}
	for _, bad := range []string{`{"takeScreenshot": "/tmp/x"}`, `{"takeScreenshot": "../x"}`, `{"saveAuthState": "a/../../x.json"}`, `{"takeScreenshot": "blob:sha256:00/x.png"}`} {
		wantForbidden(t, s.Execute(ctx, mustStep(t, bad)), "path")
	}
	// No outputs directory: refused rather than written to the host.
	wantForbidden(t, s.Execute(remoteCtx(CallerPolicy{Remote: true}), mustStep(t, `{"takeScreenshot": "x"}`)), "path")
	// A local caller writes where it asked.
	local := filepath.Join(t.TempDir(), "local")
	out = s.Execute(WithCallerPolicy(context.Background(), &CallerPolicy{}), mustStep(t, `{"takeScreenshot": "`+local+`"}`))
	if _, err := os.Stat(local + ".png"); !out.Success || err != nil {
		t.Fatalf("local takeScreenshot: %v %v", out.Error, err)
	}
}

func TestPolicy_PathsUnderARootPass(t *testing.T) {
	bundle := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bundle, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "scripts", "x.js"), []byte("output.fromBundle = 'yes'"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "sub.yaml"), []byte("appId: x\n---\n- runScript: scripts/x.js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s, err := NewSession(context.Background(), mock.New(mock.Config{}), RunnerConfig{OutputDir: dir}, SessionConfig{FlowDir: bundle})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := remoteCtx(CallerPolicy{Remote: true, BundleDir: bundle})
	out := s.Execute(ctx, mustStep(t, `{"runFlow": "sub.yaml"}`))
	if !out.Success || s.Vars()["fromBundle"] != "yes" {
		t.Fatalf("a subflow inside the bundle and its relative script: %+v %v", out.Error, s.Vars())
	}
	// A .. that leaves the bundle is denied.
	wantForbidden(t, s.Execute(ctx, mustStep(t, `{"runScript": "../outside.js"}`)), "")
}

// A root's own symlinks cannot lead a path out of it: within checks the
// path as the file system resolves it, not just as written.
func TestPolicy_SymlinkOutOfARootDenied(t *testing.T) {
	outside := secretScript(t)
	bundle := t.TempDir()
	for link, target := range map[string]string{
		"a/l":    "..",                  // the bundle itself, lexically inside
		"direct": filepath.Dir(outside), // straight out
		"inside": "a",                   // stays in
	} {
		full := filepath.Join(bundle, filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bundle, "a", "x.js"), []byte("output.fromBundle = 'yes'"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(bundle, outside)
	if err != nil {
		t.Fatal(err)
	}
	fromParent, err := filepath.Rel(filepath.Dir(bundle), outside)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(context.Background(), mock.New(mock.Config{}), RunnerConfig{OutputDir: t.TempDir()}, SessionConfig{FlowDir: bundle})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := remoteCtx(CallerPolicy{Remote: true, BundleDir: bundle})
	for _, path := range []string{
		"direct/" + filepath.Base(outside),
		"a/l/a/l/" + filepath.ToSlash(rel),       // "a/l/.." would be the bundle's parent
		"a/l/../" + filepath.ToSlash(fromParent), // lexically a/…, really <bundle>/../…
	} {
		wantForbidden(t, s.Execute(ctx, mustStep(t, `{"runScript": "`+path+`"}`)), "")
		if s.Vars()["leaked"] != "" {
			t.Fatalf("%s: the host script ran", path)
		}
	}
	if out := s.Execute(ctx, mustStep(t, `{"runScript": "inside/x.js"}`)); !out.Success || s.Vars()["fromBundle"] != "yes" {
		t.Fatalf("a link that stays inside: %+v", out.Error)
	}
	if _, err := RealPath(filepath.Join(bundle, "loop")); err != nil {
		t.Fatalf("a missing tail is taken as written: %v", err)
	}
	if err := os.Symlink("loop", filepath.Join(bundle, "loop")); err != nil {
		t.Fatal(err)
	}
	if _, err := RealPath(filepath.Join(bundle, "loop")); err == nil {
		t.Fatal("a symlink loop is an error")
	}
	if got, err := RealPath("."); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("relative: %q %v", got, err)
	}
}

type fakeBlobs struct {
	dir string
	err error
}

func (f fakeBlobs) Dir() string { return f.dir }
func (f fakeBlobs) Resolve(ref string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return filepath.Join(f.dir, filepath.Base(ref)), nil
}

func TestPolicy_BlobRefs(t *testing.T) {
	blobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(blobDir, "x.js"), []byte("output.blob = 'ran'"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
	ref := `blob:sha256:` + strings.Repeat("a", 64) + `/x.js`
	out := s.Execute(remoteCtx(CallerPolicy{Remote: true, Blobs: fakeBlobs{dir: blobDir}}), mustStep(t, `{"runScript": "`+ref+`"}`))
	if !out.Success || s.Vars()["blob"] != "ran" {
		t.Fatalf("a mapped blob ref: %+v", out.Error)
	}
	// No blob store, or an unknown blob: denied.
	wantForbidden(t, s.Execute(remoteCtx(CallerPolicy{Remote: true}), mustStep(t, `{"addMedia": ["`+ref+`"]}`)), "files")
	wantForbidden(t, s.Execute(remoteCtx(CallerPolicy{Remote: true, Blobs: fakeBlobs{dir: blobDir, err: errors.New("not uploaded")}}),
		mustStep(t, `{"addMedia": ["`+ref+`"]}`)), "files")
	// A local caller without a blob store keeps the value as it is.
	p := (&CallerPolicy{}).beginCall(t.TempDir(), nil)
	if got := p.checkInput(PolicyDenial{Path: ref}, ""); got != ref {
		t.Fatalf("local ref = %q", got)
	}
	// A local caller with a store maps it.
	p = (&CallerPolicy{Blobs: fakeBlobs{dir: blobDir}}).beginCall(t.TempDir(), nil)
	if got := p.checkInput(PolicyDenial{Path: ref}, ""); got != filepath.Join(blobDir, "x.js") {
		t.Fatalf("local mapped ref = %q", got)
	}
}

func TestPolicy_Helpers(t *testing.T) {
	if CallerPolicyFrom(nil) != nil || CallerPolicyFrom(context.Background()) != nil {
		t.Fatal("no policy in a bare context")
	}
	var nilPolicy *CallerPolicy
	if nilPolicy.beginCall("x", nil) != nil || nilPolicy.Denial() != nil {
		t.Fatal("nil policy")
	}
	p := (&CallerPolicy{Remote: true}).beginCall(t.TempDir(), nil)
	p.AddRoot("")
	if got := p.deny(PolicyDenial{Path: "/", Reason: "r"}); filepath.Base(got) != "denied" {
		t.Fatalf("deny of / = %q", got)
	}
	if p.Denial() == nil || p.Denial().Reason != "r" {
		t.Fatal("first denial kept")
	}
	p.deny(PolicyDenial{Path: "/x", Reason: "second"})
	if p.Denial().Reason != "r" {
		t.Fatal("the first denial wins")
	}
	if underAbsDir("/a/b", "/a/../c") || !underAbsDir("/a/b", "/a/c/..") {
		t.Fatal("underAbsDir")
	}
	for _, c := range []struct {
		path, dir string
		want      bool
	}{
		{"/a", "/a", true}, {"/a/b", "/a", true}, {"/a/./b/", "/a/", true}, {"/a/..b", "/a", true},
		{"/a/../b", "/a", false}, {"/ab", "/a", false}, {"/", "/a", false}, {"a/b", "a", true}, {"..", ".", false},
	} {
		if got := PathUnder(c.path, c.dir); got != c.want {
			t.Errorf("PathUnder(%q, %q) = %v", c.path, c.dir, got)
		}
	}
	var se ScriptEngine
	se.policyStep(nil)
	if _, ok := se.policyResolve("x"); ok {
		t.Fatal("no policy, no decision")
	}
}

func TestSession_EnvImport(t *testing.T) {
	t.Setenv("MAESTRO_D_POLICY_SECRET", "s3cret")
	t.Setenv("MAESTRO_D_POLICY_PASSED", "passed")
	s, _ := newTestSession(t, SessionConfig{ImportEnv: true}, mock.Config{})
	if v, err := s.Eval("MAESTRO_D_POLICY_SECRET"); err != nil || v != "s3cret" {
		t.Fatalf("ImportEnv: %v %v", v, err)
	}
	s, _ = newTestSession(t, SessionConfig{PassEnv: []string{"MAESTRO_D_POLICY_PASSED", "MAESTRO_D_POLICY_UNSET"}}, mock.Config{})
	if _, err := s.Eval("MAESTRO_D_POLICY_SECRET"); err == nil {
		t.Fatal("without ImportEnv the daemon's environment must not reach the session")
	}
	if v, err := s.Eval("MAESTRO_D_POLICY_PASSED"); err != nil || v != "passed" {
		t.Fatalf("PassEnv: %v %v", v, err)
	}
	if _, ok := s.Vars()["MAESTRO_D_POLICY_SECRET"]; ok {
		t.Fatal("vars")
	}
}
