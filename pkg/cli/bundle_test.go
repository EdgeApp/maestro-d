package cli

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// tree writes files under dir and returns dir.
func tree(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for rel, data := range files {
		write(t, filepath.Join(dir, filepath.FromSlash(rel)), []byte(data))
	}
	return dir
}

func keys(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func wantUsage(t *testing.T, err error, contains ...string) {
	t.Helper()
	de, ok := err.(*daemon.Error)
	if !ok || de.Code != daemon.CodeUsage {
		t.Fatalf("expected USAGE, got %v", err)
	}
	for _, c := range contains {
		if !strings.Contains(de.Message, c) {
			t.Fatalf("%q lacks %q", de.Message, c)
		}
	}
}

func TestPackFlow_FollowsEveryReference(t *testing.T) {
	dir := tree(t, t.TempDir(), map[string]string{
		"flows/main.yaml": "appId: x\nenv:\n  SUB: sub\nonFlowStart:\n  - runScript: scripts/setup.js\nonFlowComplete:\n  - runFlow: teardown.yaml\n---\n" +
			"- runFlow: ${SUB}/a.yaml\n" +
			"- runFlow:\n    when:\n      true: ${false}\n    file: sub/a.yaml\n    else: sub/else.yaml\n" +
			"- retry:\n    file: ../shared/retry.yaml\n" +
			"- repeat:\n    times: 2\n    commands:\n      - addMedia: [\"${PHOTO}\"]\n" +
			"- runScript: \"output.inline = 'x.js'\"\n" +
			"- runScript:\n    file: scripts/file.js\n" +
			"- assertScreenshot: refs/login\n" +
			"- assertScreenshot: refs/missing\n" +
			"- runWebViewScript: scripts/web.js\n" +
			"- runBrowserScript: scripts/browser.js\n" +
			"- uploadFile:\n    path: data/a.txt\n    paths: [data/b.txt]\n" +
			"- loadAuthState: data/auth.json\n" +
			"- defineVariables:\n    env:\n      OUT: shots\n" +
			"- takeScreenshot: ${OUT}/done\n" +
			"- saveAuthState: data/saved.json\n" +
			"- runFlow:\n    commands:\n      - runScript: scripts/nested.js\n",
		"flows/config.yaml":          "flows: ['*']\n",
		"flows/scripts/setup.js":     "output.s = 1",
		"flows/teardown.yaml":        "- tapOn: X\n",
		"flows/sub/a.yaml":           "- runScript: ../scripts/file.js\n- runFlow: a.yaml\n",
		"flows/sub/else.yaml":        "- tapOn: Else\n",
		"shared/retry.yaml":          "- tapOn: R\n",
		"flows/photo.png":            "png",
		"flows/scripts/file.js":      "output.f = 1",
		"flows/refs/login.png":       "png",
		"flows/scripts/web.js":       "1",
		"flows/scripts/browser.js":   "2",
		"flows/data/a.txt":           "a",
		"flows/data/b.txt":           "a",
		"flows/data/auth.json":       "{}",
		"flows/scripts/nested.js":    "3",
		"flows/not-referenced.yaml":  "- tapOn: no\n",
		"flows/data/dynamic/one.csv": "1",
		"flows/data/dynamic/two.csv": "2",
	})
	b, err := packFlow([]string{filepath.Join(dir, "flows", "main.yaml")}, packFlowOpts{
		Env: map[string]string{"PHOTO": "photo.png"}, Include: []string{"data/dynamic/*.csv"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "flows/config.yaml,flows/data/a.txt,flows/data/auth.json,flows/data/b.txt,flows/data/dynamic/one.csv,flows/data/dynamic/two.csv," +
		"flows/main.yaml,flows/photo.png,flows/refs/login.png,flows/scripts/browser.js,flows/scripts/file.js,flows/scripts/nested.js," +
		"flows/scripts/setup.js,flows/scripts/web.js,flows/sub/a.yaml,flows/sub/else.yaml,flows/teardown.yaml,shared/retry.yaml"
	if got := keys(b.Manifest.Files); got != want {
		t.Fatalf("files:\n%s\nwant\n%s", got, want)
	}
	if b.Manifest.Root != "flows/main.yaml" || b.Dir != dir || b.Roots[filepath.Join(dir, "flows", "main.yaml")] != "flows/main.yaml" {
		t.Fatalf("root %q dir %q roots %v", b.Manifest.Root, b.Dir, b.Roots)
	}
	// Shared files are deduplicated: data/a.txt and data/b.txt hold the
	// same bytes, so one unit uploads both.
	distinct := map[string]bool{}
	for _, h := range b.Manifest.Files {
		distinct[h] = true
	}
	if b.Manifest.Files["flows/data/a.txt"] != b.Manifest.Files["flows/data/b.txt"] || len(b.Units) != len(distinct) || len(distinct) == len(b.Manifest.Files) {
		t.Fatalf("dedupe: %d units for %d files", len(b.Units), len(b.Manifest.Files))
	}
	if b.ManifestUnit.SHA256 != b.SHA256 || b.SHA256 != b.Manifest.Hash() {
		t.Fatal("manifest unit")
	}
	data, _ := os.ReadFile(b.ManifestUnit.File)
	if !strings.Contains(string(data), `"root":"flows/main.yaml"`) {
		t.Fatalf("manifest blob: %s", data)
	}
	if b.Ref(filepath.Join(dir, "flows", "main.yaml")) != "bundle:sha256:"+b.SHA256+"/flows/main.yaml" {
		t.Fatal("Ref")
	}
	// The same tree packs to the same manifest.
	again, err := packFlow([]string{filepath.Join(dir, "flows", "main.yaml")}, packFlowOpts{
		Env: map[string]string{"PHOTO": "photo.png"}, Include: []string{"data/dynamic/*.csv"},
	})
	if err != nil || again.SHA256 != b.SHA256 {
		t.Fatal("not deterministic")
	}
}

func TestPackFlow_Errors(t *testing.T) {
	dir := tree(t, t.TempDir(), map[string]string{
		"missing-var.yaml":  "- runFlow: ${MISSING}.yaml\n",
		"missing-file.yaml": "- addMedia: [nope.png]\n",
		"missing-flow.yaml": "- runFlow: nope.yaml\n",
		"abs-input.yaml":    "- runScript: /etc/x.js\n",
		"abs-output.yaml":   "- takeScreenshot: /tmp/x\n",
		"up-output.yaml":    "- takeScreenshot: ../x\n",
		"abs-shot.yaml":     "- assertScreenshot: /nonexistent/ref.png\n",
		"var-shot.yaml":     "- assertScreenshot: ${NOPE}\n",
		"bad.yaml":          "- tapOn: [\n",
		"dir-input.yaml":    "- addMedia: [adir]\n",
		"baseline.yaml":     "- assertScreenshot: new-baseline\n- takeScreenshot: ${UNSET}/x\n",
		"adir/x":            "x",
	})
	p := func(n string) string { return filepath.Join(dir, n) }
	_, err := packFlow([]string{p("missing-var.yaml")}, packFlowOpts{})
	wantUsage(t, err, `step "runFlow: ${MISSING}.yaml"`, "file", "-e NAME=VALUE")
	_, err = packFlow([]string{p("missing-file.yaml")}, packFlowOpts{})
	wantUsage(t, err, "nope.png: no such file", "files")
	_, err = packFlow([]string{p("missing-flow.yaml")}, packFlowOpts{})
	wantUsage(t, err, "no such flow file")
	_, err = packFlow([]string{p("abs-input.yaml")}, packFlowOpts{})
	wantUsage(t, err, "is absolute")
	_, err = packFlow([]string{p("abs-output.yaml")}, packFlowOpts{})
	wantUsage(t, err, "output paths must be relative", "takeScreenshot")
	_, err = packFlow([]string{p("up-output.yaml")}, packFlowOpts{})
	wantUsage(t, err, "may not contain ..")
	_, err = packFlow([]string{p("abs-shot.yaml")}, packFlowOpts{})
	wantUsage(t, err, "output paths must be relative")
	_, err = packFlow([]string{p("var-shot.yaml")}, packFlowOpts{})
	wantUsage(t, err, "cannot be resolved")
	_, err = packFlow([]string{p("bad.yaml")}, packFlowOpts{})
	wantUsage(t, err, "pack --flow")
	_, err = packFlow([]string{p("nope.yaml")}, packFlowOpts{})
	wantUsage(t, err, "pack --flow")
	_, err = packFlow([]string{p("dir-input.yaml")}, packFlowOpts{})
	wantUsage(t, err, "no such file")
	_, err = packFlow(nil, packFlowOpts{})
	wantUsage(t, err, "no flow file")
	_, err = packFlow([]string{p("baseline.yaml")}, packFlowOpts{Include: []string{"*.nomatch"}})
	wantUsage(t, err, "no file matches")
	_, err = packFlow([]string{p("baseline.yaml")}, packFlowOpts{Include: []string{"[bad"}})
	wantUsage(t, err, "--include")
	// A missing assertScreenshot reference is a baseline, not an input:
	// the pack does not stop for it, and leaves it unpacked.
	b, err := packFlow([]string{p("baseline.yaml")}, packFlowOpts{})
	if err != nil || keys(b.Manifest.Files) != "baseline.yaml" {
		t.Fatalf("baseline: %v %v", err, b)
	}
}

func TestPackFlow_Base(t *testing.T) {
	cwd := tree(t, t.TempDir(), map[string]string{
		"photo.png":     "png",
		"sub/x.yaml":    "- takeScreenshot: x\n",
		"scripts/x.js":  "1",
		"inline.yaml":   "a real file the temp root does not replace",
		"../up/u.yaml":  "- tapOn: U\n",
		"config.yaml":   "flows: ['*']\n",
		"inline-2.yaml": "",
	})
	tmp := tree(t, t.TempDir(), map[string]string{
		"inline.yaml": `[{"addMedia": ["photo.png"]}, {"runFlow": "sub/x.yaml"}, {"runScript": "scripts/x.js"}, {"takeScreenshot": "shot"}]`,
	})
	b, err := packFlow([]string{filepath.Join(tmp, "inline.yaml")}, packFlowOpts{Base: cwd})
	if err != nil {
		t.Fatal(err)
	}
	// The root sits at the manifest root, beside cwd's files, with the
	// temporary file's bytes.
	if b.Manifest.Root != "inline.yaml" || b.Dir != cwd || keys(b.Manifest.Files) != "config.yaml,inline.yaml,photo.png,scripts/x.js,sub/x.yaml" {
		t.Fatalf("base: %q %q %s", b.Manifest.Root, b.Dir, keys(b.Manifest.Files))
	}
	u, _ := packPath(filepath.Join(tmp, "inline.yaml"))
	if b.Manifest.Files["inline.yaml"] != u.SHA256 {
		t.Fatal("the root's bytes are the temporary file's")
	}
	// A reference above the base widens the common directory.
	tree(t, tmp, map[string]string{"up.yaml": "- runFlow: ../up/u.yaml\n"})
	b, err = packFlow([]string{filepath.Join(tmp, "up.yaml")}, packFlowOpts{Base: cwd})
	if err != nil || b.Dir != filepath.Dir(cwd) || b.Manifest.Root != filepath.Base(cwd)+"/up.yaml" {
		t.Fatalf("widened: %v %+v", err, b)
	}
}

func TestPackFlow_MultipleRoots(t *testing.T) {
	dir := tree(t, t.TempDir(), map[string]string{
		"a/main.yaml": "- runScript: x.js\n", "a/x.js": "1", "b/else.yaml": "- tapOn: E\n",
	})
	b, err := packFlow([]string{filepath.Join(dir, "a", "main.yaml"), filepath.Join(dir, "b", "else.yaml")}, packFlowOpts{})
	if err != nil || b.Manifest.Root != "a/main.yaml" || b.Roots[filepath.Join(dir, "b", "else.yaml")] != "b/else.yaml" || b.Dir != dir {
		t.Fatalf("%v %+v", err, b)
	}
}

func TestHelpers_ExpandAndCommonDir(t *testing.T) {
	if v, ok := expand("${A}/${ B }/x", map[string]string{"A": "a", "B": "b"}); !ok || v != "a/b/x" {
		t.Fatal(v, ok)
	}
	if _, ok := expand("${A}/${C}", map[string]string{"A": "a"}); ok {
		t.Fatal("unresolved")
	}
	if got := commonDir(map[string]string{"/a/b/c.yaml": "", "/a/d/e.yaml": ""}); got != "/a" {
		t.Fatal(got)
	}
	if got := commonDir(map[string]string{"/x.yaml": "", "/a/e.yaml": ""}); got != "/" {
		t.Fatal(got)
	}
	if mergeEnv(map[string]string{"A": "1"}, nil)["A"] != "1" || mergeEnv(map[string]string{"A": "1"}, map[string]string{"A": "2"})["A"] != "2" {
		t.Fatal("mergeEnv")
	}
}

// One-shots with flow-file fields send bundle refs whose closure includes
// the subflow's own references, and the subflow's outputs come back
// relative to the bundle's common directory.
func TestHubFiles_FlowFieldsSendBundles(t *testing.T) {
	e := newHubEnv(t)
	ctx := context.Background()
	if _, err := e.c.Attach(ctx, "mock-1", daemon.AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	tree(t, e.cwd, map[string]string{
		"sub/open.yaml":    "- runScript: scripts/x.js\n- assertTrue: ${output.x}\n- takeScreenshot: x\n",
		"sub/scripts/x.js": "output.x = true",
		"other/else.yaml":  "- takeScreenshot: ${NAME}\n",
		"retry.yaml":       "- tapOn: R\n",
	})
	run := func(name string, value any) *daemon.StepResult {
		t.Helper()
		h := e.files(ctx, false)
		v, err := h.rewrite(daemon.CommandSpecs[name], value)
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.c.Command(ctx, "mock-1", name, v, true)
		if err != nil {
			t.Fatalf("%s %v: %v", name, v, err)
		}
		if err := h.fetchOutputs(res.Outputs); err != nil {
			t.Fatal(err)
		}
		return res
	}
	// Bare: a bundle: ref, and the subflow's screenshot is fetched to
	// sub/x.png beside it.
	run("runFlow", "sub/open.yaml")
	if _, err := os.Stat(filepath.Join(e.cwd, "sub", "x.png")); err != nil {
		t.Fatal("subflow output not fetched beside the subflow")
	}
	// Keyed, with else as a file: one bundle holds both.
	run("runFlow", map[string]any{"file": "sub/open.yaml", "when": map[string]any{"true": "${false}"}, "else": "other/else.yaml", "env": map[string]any{"NAME": "e"}})
	if _, err := os.Stat(filepath.Join(e.cwd, "other", "e.png")); err != nil {
		t.Fatal("else output not fetched")
	}
	// retry.file.
	h := e.files(ctx, true)
	v, err := h.rewrite(daemon.CommandSpecs["retry"], map[string]any{"file": "retry.yaml"})
	if err != nil || !strings.HasPrefix(v.(map[string]any)["file"].(string), "bundle:sha256:") || !strings.HasSuffix(v.(map[string]any)["file"].(string), "/retry.yaml") {
		t.Fatalf("retry: %v %v", v, err)
	}
	// Inline else steps and an existing ref are left alone; a missing
	// flow file stops the one-shot.
	v, err = h.rewrite(daemon.CommandSpecs["runFlow"], map[string]any{"else": []any{map[string]any{"tapOn": "X"}}})
	if err != nil || v.(map[string]any)["else"] == nil {
		t.Fatal(v, err)
	}
	if v, _ := h.rewrite(daemon.CommandSpecs["runFlow"], "bundle:sha256:x/y"); v != "bundle:sha256:x/y" {
		t.Fatal(v)
	}
	_, err = h.rewrite(daemon.CommandSpecs["runFlow"], "missing.yaml")
	wantUsage(t, err)
	// An output the bundle names that would leave its directory is skipped.
	h.bundleDir = e.cwd
	if err := h.fetchOutputs([]string{"outputs/../escape.png", "not-outputs/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.cwd), "escape.png")); err == nil {
		t.Fatal("escaped")
	}
}
