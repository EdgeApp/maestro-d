package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	imgcolor "image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// storeBlobs is a BlobResolver over in-memory blobs: Resolve copies one to
// <dir>/<hex>/<basename>, as the daemon's store does.
type storeBlobs struct {
	dir   string
	blobs map[string][]byte
}

func (f *storeBlobs) Dir() string { return f.dir }

func (f *storeBlobs) Resolve(ref string) (string, error) {
	h, base, err := ParseBlobRef(ref)
	if err != nil {
		return "", err
	}
	data, ok := f.blobs[h]
	if !ok {
		return "", errors.New("not uploaded")
	}
	p := filepath.Join(f.dir, h, base)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	return p, os.WriteFile(p, data, 0o644)
}

func (f *storeBlobs) add(data []byte, base string) string {
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	f.blobs[h] = data
	return "blob:sha256:" + h + "/" + base
}

func newStoreBlobs(t *testing.T) *storeBlobs {
	return &storeBlobs{dir: filepath.Join(t.TempDir(), "blobs"), blobs: map[string][]byte{}}
}

func mockPNG(t *testing.T) []byte {
	t.Helper()
	data, err := mock.New(mock.Config{}).Screenshot()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func redPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, imgcolor.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// remoteBlobCtx is a remote call's policy with a blob store and an outputs
// directory under the session's report dir.
func remoteBlobCtx(b *storeBlobs, reportDir string) context.Context {
	return remoteCtx(CallerPolicy{Remote: true, Blobs: b, OutputsDir: filepath.Join(reportDir, "outputs")})
}

func TestPolicy_RemoteAssertScreenshot(t *testing.T) {
	s, dir := newTestSession(t, SessionConfig{FlowDir: t.TempDir()}, mock.Config{})
	b := newStoreBlobs(t)
	ctx := remoteBlobCtx(b, dir)

	// A packed reference is compared (and validateArtifactPath accepts it).
	ref := b.add(mockPNG(t), "ref.png")
	out := s.Execute(ctx, &flow.AssertScreenshotStep{Path: ref, ThresholdPercentage: 95})
	if !out.Success || len(out.Outputs) != 0 {
		t.Fatalf("packed reference: %+v %v", out.Error, out.Outputs)
	}
	// Extensionless on the owner too: the ref names login.png.
	login := b.add(mockPNG(t), "login.png")
	if out := s.Execute(ctx, &flow.AssertScreenshotStep{Path: login, ThresholdPercentage: 95}); !out.Success {
		t.Fatalf("login.png: %+v", out.Error)
	}

	// A missing relative reference becomes a baseline under outputs/.
	out = s.Execute(ctx, &flow.AssertScreenshotStep{Path: "1a2b3c4d/login", ThresholdPercentage: 95})
	if !out.Success || len(out.Outputs) != 1 || out.Outputs[0] != "outputs/1a2b3c4d/login.png" {
		t.Fatalf("baseline: %+v %v", out.Error, out.Outputs)
	}
	if _, err := os.Stat(filepath.Join(dir, "outputs", "1a2b3c4d", "login.png")); err != nil {
		t.Fatal(err)
	}
	// The second time the baseline exists under outputs/ and is compared.
	if out := s.Execute(ctx, &flow.AssertScreenshotStep{Path: "1a2b3c4d/login", ThresholdPercentage: 95}); !out.Success || len(out.Outputs) != 0 {
		t.Fatalf("compare against baseline: %+v %v", out.Error, out.Outputs)
	}

	// A mismatch writes outputs/<hash8>/ref_diff.png, lists it, and the
	// message names that output.
	red := b.add(redPNG(t), "ref.png")
	out = s.Execute(ctx, &flow.AssertScreenshotStep{Path: red, ThresholdPercentage: 95})
	h, _, _ := ParseBlobRef(red)
	want := "outputs/" + h[:8] + "/ref_diff.png"
	if out.Success || len(out.Outputs) != 1 || out.Outputs[0] != want || !strings.Contains(out.Message, want) {
		t.Fatalf("mismatch: %q %v (want %s)", out.Message, out.Outputs, want)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(want))); err != nil {
		t.Fatal(err)
	}

	// An absolute host reference stays FORBIDDEN, and a bad ref too.
	host := filepath.Join(t.TempDir(), "host.png")
	_ = os.WriteFile(host, mockPNG(t), 0o644)
	wantForbidden(t, s.Execute(ctx, &flow.AssertScreenshotStep{Path: host}), "path")
	wantForbidden(t, s.Execute(ctx, &flow.AssertScreenshotStep{Path: "blob:sha256:../../x/ref.png"}), "path")
	wantForbidden(t, s.Execute(ctx, &flow.AssertScreenshotStep{Path: "../x"}), "path")
}

func TestPolicy_LocalAssertScreenshotUnchanged(t *testing.T) {
	flowDir := t.TempDir()
	s, _ := newTestSession(t, SessionConfig{FlowDir: flowDir}, mock.Config{})
	local := WithCallerPolicy(context.Background(), &CallerPolicy{Blobs: newStoreBlobs(t)})
	// A unix-socket call writes the baseline beside the flow, as today…
	if out := s.Execute(local, &flow.AssertScreenshotStep{Path: "base", ThresholdPercentage: 95}); !out.Success || out.Outputs != nil {
		t.Fatalf("baseline: %+v", out.Error)
	}
	if _, err := os.Stat(filepath.Join(flowDir, "base.png")); err != nil {
		t.Fatal("baseline not beside the flow")
	}
	// …a mismatch writes ref_diff.png beside ref.png…
	_ = os.WriteFile(filepath.Join(flowDir, "ref.png"), redPNG(t), 0o644)
	if out := s.Execute(local, &flow.AssertScreenshotStep{Path: "ref.png", ThresholdPercentage: 95}); out.Success {
		t.Fatal("expected a mismatch")
	}
	if _, err := os.Stat(filepath.Join(flowDir, "ref_diff.png")); err != nil {
		t.Fatal("diff not beside the reference")
	}
	// …and a ".." escape is still refused with no policy at all.
	out := s.Execute(context.Background(), &flow.AssertScreenshotStep{Path: "../../../../../../../tmp/escape", ThresholdPercentage: 95})
	if out.Success || !strings.Contains(out.Message, "escapes the workspace") {
		t.Fatalf("traversal: %v", out.Message)
	}
}

func TestPolicy_ClientOutputs(t *testing.T) {
	s, dir := newTestSession(t, SessionConfig{FlowDir: t.TempDir()}, mock.Config{})
	b := newStoreBlobs(t)
	ctx := WithCallerPolicy(context.Background(), &CallerPolicy{ClientOutputs: true, Blobs: b, OutputsDir: filepath.Join(dir, "outputs")})
	out := s.Execute(ctx, &flow.TakeScreenshotStep{Path: "abcd1234/name"})
	if !out.Success || len(out.Outputs) != 1 || out.Outputs[0] != "outputs/abcd1234/name.png" {
		t.Fatalf("client output: %+v %v", out.Error, out.Outputs)
	}
	// A baseline and a diff for a local client-named call land in outputs/.
	if out := s.Execute(ctx, &flow.AssertScreenshotStep{Path: "abcd1234/base", ThresholdPercentage: 95}); len(out.Outputs) != 1 {
		t.Fatalf("client baseline: %v", out.Outputs)
	}
	red := b.add(redPNG(t), "r.png")
	if out := s.Execute(ctx, &flow.AssertScreenshotStep{Path: red, ThresholdPercentage: 95}); len(out.Outputs) != 1 || !strings.HasSuffix(out.Outputs[0], "/r_diff.png") {
		t.Fatalf("client diff: %v", out.Outputs)
	}
	// An absolute local path still works, and ".." is left to upstream.
	abs := filepath.Join(t.TempDir(), "abs.png")
	if out := s.Execute(ctx, &flow.TakeScreenshotStep{Path: abs}); !out.Success || out.Outputs != nil {
		t.Fatalf("absolute: %+v %v", out.Error, out.Outputs)
	}
	p := &CallerPolicy{ClientOutputs: true, OutputsDir: filepath.Join(dir, "outputs")}
	if got := p.clientOutput("../up", ""); got != "../up" {
		t.Fatal(got)
	}
	if got := p.clientOutput(filepath.Join(dir, "outputs", "x.png"), ""); len(p.outputs) != 1 || got == "" {
		t.Fatal("an outputs path is recorded")
	}
}

func TestPolicy_BlobRefsAndBareValues(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{FlowDir: t.TempDir()}, mock.Config{})
	b := newStoreBlobs(t)
	ctx := remoteBlobCtx(b, s.OutputDir())
	script := b.add([]byte("output.fromBlob = 'yes'"), "x.js")

	// runScript: {file: x.js} as a blob ref is mapped and runs.
	if out := s.Execute(ctx, &flow.RunScriptStep{File: script}); !out.Success {
		t.Fatalf("runScript file blob: %+v", out.Error)
	}
	if v, _ := s.Eval("output.fromBlob"); v != "yes" {
		t.Fatalf("script did not run: %v", v)
	}
	// The bare form (script) too.
	if out := s.Execute(ctx, mustStep(t, `{"runScript": "`+b.add([]byte("output.bare = 1"), "y.js")+`"}`)); !out.Success {
		t.Fatalf("bare runScript blob: %+v", out.Error)
	}
	// Inline JavaScript is left as it is, remote or not.
	if out := s.Execute(ctx, mustStep(t, `{"runScript": "output.ok = true"}`)); !out.Success {
		t.Fatalf("inline: %+v", out.Error)
	}
	// runScript.file without .js is not a path.
	step := &flow.RunScriptStep{File: "output.f = 2"}
	s.fr.script.policy = (&CallerPolicy{Remote: true, Blobs: b}).beginCall(s.OutputDir(), func() {})
	s.fr.script.policyStep(step)
	if step.File != "output.f = 2" || s.fr.script.policy.Denial() != nil {
		t.Fatalf("non-.js file: %q %v", step.File, s.fr.script.policy.Denial())
	}

	// addMedia with a bare scalar and a sequence of blob refs.
	a, c := b.add([]byte("A"), "a.png"), b.add([]byte("B"), "b.png")
	for _, st := range []flow.Step{&flow.AddMediaStep{Files: []string{a}}, &flow.AddMediaStep{Files: []string{a, c}}} {
		s.fr.script.ExpandStep(st)
		for _, f := range st.(*flow.AddMediaStep).Files {
			if !strings.HasPrefix(f, b.dir) || filepath.Ext(f) != ".png" {
				t.Fatalf("addMedia file %q", f)
			}
		}
	}
	s.fr.script.policy = nil

	// ResolvePath keeps the basename and extension; a plain path is
	// unchanged.
	s.fr.script.policy = (&CallerPolicy{Blobs: b}).beginCall(s.OutputDir(), func() {})
	got := s.fr.script.ResolvePath(a)
	if filepath.Base(got) != "a.png" || !strings.HasPrefix(got, b.dir) {
		t.Fatalf("ResolvePath(blob) = %q", got)
	}
	if got := s.fr.script.ResolvePath("plain.png"); got != filepath.Join(s.fr.script.FlowDir(), "plain.png") {
		t.Fatalf("ResolvePath(plain) = %q", got)
	}
	s.fr.script.policy = nil

	// A ref the daemon cannot resolve is denied; so is any ref without a store.
	wantForbidden(t, s.Execute(ctx, &flow.AddMediaStep{Files: []string{"blob:sha256:" + strings.Repeat("0", 64) + "/x.png"}}), "files")
	wantForbidden(t, s.Execute(remoteCtx(CallerPolicy{Remote: true}), &flow.AddMediaStep{Files: []string{a}}), "files")
}

func TestPolicy_OutputFields(t *testing.T) {
	s, dir := newTestSession(t, SessionConfig{}, mock.Config{})
	ctx := remoteBlobCtx(newStoreBlobs(t), dir)
	// An extensionless takeScreenshot path gives …/name.png.
	out := s.Execute(ctx, &flow.TakeScreenshotStep{Path: "0011aabb/name"})
	if !out.Success || len(out.Outputs) != 1 || out.Outputs[0] != "outputs/0011aabb/name.png" {
		t.Fatalf("takeScreenshot: %+v %v", out.Error, out.Outputs)
	}
	// saveAuthState is rewritten under outputs/ too; startRecording's path
	// (a device path on Android) passes through.
	p := (&CallerPolicy{Remote: true, OutputsDir: filepath.Join(dir, "outputs")}).beginCall(dir, func() {})
	s.fr.script.policy = p
	save := &flow.SaveAuthStateStep{Path: "x/auth.json"}
	rec := &flow.StartRecordingStep{Path: "/sdcard/r.mp4"}
	s.fr.script.policyStep(save)
	s.fr.script.policyStep(rec)
	if save.Path != filepath.Join(dir, "outputs", "x", "auth.json") || rec.Path != "/sdcard/r.mp4" || p.Denial() != nil {
		t.Fatalf("save %q rec %q %v", save.Path, rec.Path, p.Denial())
	}
	// Idempotent: a path already under outputs/ passes again.
	s.fr.script.policyStep(save)
	if save.Path != filepath.Join(dir, "outputs", "x", "auth.json") {
		t.Fatal(save.Path)
	}
	// An output that is a ref is refused.
	bad := &flow.SaveAuthStateStep{Path: "blob:sha256:x/y"}
	s.fr.script.policyStep(bad)
	if p.Denial() == nil {
		t.Fatal("a ref output must be denied")
	}
	s.fr.script.policy = nil
	// takeOutputs ignores missing and stale files.
	q := &CallerPolicy{outputs: []outputRec{{path: filepath.Join(dir, "missing")}}}
	if got := q.takeOutputs(dir); got != nil {
		t.Fatalf("missing: %v", got)
	}
	var nilPolicy *CallerPolicy
	if nilPolicy.takeOutputs(dir) != nil {
		t.Fatal("nil policy")
	}
}

func TestPolicy_DiffPathAndAllows(t *testing.T) {
	se := NewScriptEngine()
	defer se.Close()
	if got := se.DiffPath("/a/ref.png"); got != core.DiffScreenshotPath("/a/ref.png") {
		t.Fatalf("no policy: %q", got)
	}
	if se.PolicyAllows("/a/ref.png") {
		t.Fatal("no policy allows nothing")
	}
	out := filepath.Join(t.TempDir(), "outputs")
	bundle := t.TempDir()
	se.policy = (&CallerPolicy{Remote: true, OutputsDir: out, BundleDir: bundle}).beginCall(t.TempDir(), func() {})
	se.policy.callerNames = map[string]string{"/blobs/h/login.png": "12345678/login.png", "/blobs/h/noext": "12345678/noext"}
	for in, want := range map[string]string{
		"/blobs/h/login.png":                     filepath.Join(out, "12345678", "login_diff.png"),
		"/blobs/h/noext.png":                     filepath.Join(out, "12345678", "noext_diff.png"),
		filepath.Join(out, "aa", "b.png"):        filepath.Join(out, "aa", "b_diff.png"),
		filepath.Join(bundle, "sub", "shot.png"): filepath.Join(out, "sub", "shot_diff.png"),
		"/elsewhere/odd.png":                     filepath.Join(out, "odd_diff.png"),
	} {
		if got := se.DiffPath(in); got != want {
			t.Errorf("DiffPath(%s) = %s, want %s", in, got, want)
		}
	}
	if !se.PolicyAllows(filepath.Join(out, "x.png")) || se.PolicyAllows("/etc/hosts") {
		t.Fatal("PolicyAllows")
	}
}

func TestParseBlobRef(t *testing.T) {
	h := strings.Repeat("ab", 32)
	for _, bad := range []string{
		"blob:sha256:../../../.ssh/id_ed25519",
		"blob:sha256:" + strings.ToUpper(h) + "/x",
		"blob:sha256:" + h,
		"blob:sha256:" + h + "/",
		"blob:sha256:" + h + "/.",
		"blob:sha256:" + h + "/..",
		"blob:sha256:" + h + "/a/b",
		"blob:sha256:" + h + "/a\\b",
		"bundle:sha256:" + h + "/x",
	} {
		if _, _, err := ParseBlobRef(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	if hx, base, err := ParseBlobRef("blob:sha256:" + h + "/photo.png"); err != nil || hx != h || base != "photo.png" {
		t.Fatal(hx, base, err)
	}
	if withPNG("a") != "a.png" || withPNG("a.jpg") != "a.jpg" {
		t.Fatal("withPNG")
	}
}
