package executor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// bundleBlobs resolves one bundle ref to a directory of files written
// under its dir, as the daemon materializes bundles/<sha>/.
type bundleBlobs struct {
	dir   string
	files map[string]string
}

func (f *bundleBlobs) Dir() string { return filepath.Join(f.dir, "blobs") }

func (f *bundleBlobs) Resolve(ref string) (string, error) {
	h, root, err := ParseBundleRef(ref)
	if err != nil {
		return "", err
	}
	if _, ok := f.files[root]; !ok {
		return "", errors.New("not in the bundle")
	}
	b := filepath.Join(f.dir, "bundles", h)
	for rel, data := range f.files {
		p := filepath.Join(b, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			return "", err
		}
	}
	return filepath.Join(b, filepath.FromSlash(root)), nil
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPolicy_BundleDirAndFlowDirForTheCall(t *testing.T) {
	sessionDir := t.TempDir()
	s, report := newTestSession(t, SessionConfig{FlowDir: sessionDir}, mock.Config{})
	bundle := filepath.Join(report, "bundles", "abc")
	writeFiles(t, bundle, map[string]string{
		"flows/steps.yaml":   "",
		"flows/sub/x.yaml":   "- takeScreenshot: x\n",
		"flows/scripts/x.js": "output.x = 'ran'",
		"shared/login.yaml":  "- takeScreenshot: y\n",
	})
	p := CallerPolicy{Remote: true, BundleDir: bundle, FlowDir: filepath.Join(bundle, "flows"), OutputsDir: filepath.Join(report, "outputs")}
	outs := s.ExecuteAll(remoteCtx(p), []flow.Step{
		&flow.RunFlowStep{File: "sub/x.yaml"},
		&flow.RunScriptStep{Script: "scripts/x.js"},
		&flow.RunFlowStep{File: "../shared/login.yaml"},
		&flow.TakeScreenshotStep{Path: "top"},
	}, false)
	var got []string
	for _, o := range outs {
		if !o.Success {
			t.Fatalf("%s: %+v", o.Description, o.Error)
		}
		got = append(got, o.Outputs...)
	}
	// Outputs are named relative to the bundle root, from each step's
	// flow directory.
	if strings.Join(got, ",") != "outputs/flows/sub/x.png,outputs/shared/y.png,outputs/flows/top.png" {
		t.Fatalf("outputs: %v", got)
	}
	if v, _ := s.Eval("output.x"); v != "ran" {
		t.Fatalf("scripts/x.js did not run from the bundle: %v", v)
	}
	if s.fr.script.FlowDir() != sessionDir {
		t.Fatalf("flowDir not restored: %s", s.fr.script.FlowDir())
	}
	// Escaping the bundle is still FORBIDDEN, and flowDir is restored after
	// a denial too.
	wantForbidden(t, s.Execute(remoteCtx(p), &flow.RunFlowStep{File: "../../../../../../etc/hosts"}), "file")
	if s.fr.script.FlowDir() != sessionDir {
		t.Fatal("flowDir not restored after a denial")
	}
}

func TestPolicy_BundleRefIsACallRoot(t *testing.T) {
	s, report := newTestSession(t, SessionConfig{FlowDir: t.TempDir()}, mock.Config{})
	h := strings.Repeat("ab", 32)
	b := &bundleBlobs{dir: report, files: map[string]string{
		"sub/open.yaml":    "- runScript: scripts/x.js\n- takeScreenshot: shot\n",
		"sub/scripts/x.js": "output.y = 1",
		"other/else.yaml":  "- takeScreenshot: e\n",
	}}
	p := CallerPolicy{Remote: true, Blobs: b, OutputsDir: filepath.Join(report, "outputs")}
	out := s.Execute(remoteCtx(p), &flow.RunFlowStep{File: "bundle:sha256:" + h + "/sub/open.yaml"})
	if !out.Success || strings.Join(out.Outputs, ",") != "outputs/sub/shot.png" {
		t.Fatalf("bundle ref: %+v %v", out.Error, out.Outputs)
	}
	// else as a bundle ref, in the same bundle; retry.file too.
	out = s.Execute(remoteCtx(p), &flow.RunFlowStep{File: "bundle:sha256:" + h + "/sub/open.yaml",
		When: &flow.Condition{Script: "${false}"}, ElseFile: "bundle:sha256:" + h + "/other/else.yaml"})
	if !out.Success || strings.Join(out.Outputs, ",") != "outputs/other/e.png" {
		t.Fatalf("else ref: %+v %v", out.Error, out.Outputs)
	}
	if out := s.Execute(remoteCtx(p), &flow.RetryStep{File: "bundle:sha256:" + h + "/sub/open.yaml"}); !out.Success {
		t.Fatalf("retry ref: %+v", out.Error)
	}
	// A ref whose root is not in the bundle, or is malformed, is FORBIDDEN.
	wantForbidden(t, s.Execute(remoteCtx(p), &flow.RunFlowStep{File: "bundle:sha256:" + h + "/nope.yaml"}), "file")
	wantForbidden(t, s.Execute(remoteCtx(p), &flow.RunFlowStep{File: "bundle:sha256:" + h + "/../x.yaml"}), "file")
}

func TestPolicy_LocalBundleOutputs(t *testing.T) {
	// A ClientOutputs (loopback) caller's bundled outputs are named
	// relative to the bundle root too; a plain local call is unchanged.
	s, report := newTestSession(t, SessionConfig{FlowDir: t.TempDir()}, mock.Config{})
	bundle := filepath.Join(report, "bundles", "abc")
	writeFiles(t, bundle, map[string]string{"sub/a.yaml": "- takeScreenshot: x\n"})
	p := CallerPolicy{ClientOutputs: true, BundleDir: bundle, FlowDir: bundle, OutputsDir: filepath.Join(report, "outputs")}
	out := s.Execute(remoteCtx(p), &flow.RunFlowStep{File: "sub/a.yaml"})
	if !out.Success || strings.Join(out.Outputs, ",") != "outputs/sub/x.png" {
		t.Fatalf("client outputs in a bundle: %+v %v", out.Error, out.Outputs)
	}
	plain := CallerPolicy{BundleDir: bundle, FlowDir: bundle}
	out = s.Execute(remoteCtx(plain), &flow.RunFlowStep{File: "sub/a.yaml"})
	if !out.Success || len(out.Outputs) != 0 {
		t.Fatalf("local: %+v %v", out.Error, out.Outputs)
	}
	if _, err := os.Stat(filepath.Join(bundle, "sub", "x.png")); err != nil {
		t.Fatal("a local bundled run writes beside the subflow")
	}
}

func TestParseBundleRefAndValidBundlePath(t *testing.T) {
	h := strings.Repeat("cd", 32)
	if hx, root, err := ParseBundleRef("bundle:sha256:" + h + "/flows/main.yaml"); err != nil || hx != h || root != "flows/main.yaml" {
		t.Fatal(hx, root, err)
	}
	for _, bad := range []string{
		"blob:sha256:" + h + "/x",
		"bundle:sha256:" + h,
		"bundle:sha256:../../x/y.yaml",
		"bundle:sha256:" + strings.ToUpper(h) + "/x",
		"bundle:sha256:" + h + "/",
		"bundle:sha256:" + h + "/../x",
		"bundle:sha256:" + h + "//x",
	} {
		if _, _, err := ParseBundleRef(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	for _, bad := range []string{"", "/a", "a/../b", "..", ".", "./a", "a//b", "a/", `a\b`, "a\x00b"} {
		if ValidBundlePath(bad) == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	for _, good := range []string{"a", "a/b.yaml", "flows/sub/x.y", "..a/b..", ".hidden"} {
		if err := ValidBundlePath(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	// outputName keeps a path outside any bundle as given.
	p := &CallerPolicy{}
	p.addBundle("")
	if p.outputName("x/y", "") != "x/y" || p.outputName("x", t.TempDir()) != "x" {
		t.Fatal("outputName outside a bundle")
	}
}
