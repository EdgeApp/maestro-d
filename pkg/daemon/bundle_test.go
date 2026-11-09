package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/executor"
)

// bundleOf stores files in the env's blob store and returns their manifest.
func bundleOf(t *testing.T, env *callerEnv, root string, files map[string]string) *BundleManifest {
	t.Helper()
	m := &BundleManifest{Root: root, Files: map[string]string{}}
	for rel, data := range files {
		h := sha([]byte(data))
		if _, e := env.s.blobs.Put(h, "", strings.NewReader(data)); e != nil {
			t.Fatal(e)
		}
		m.Files[rel] = h
	}
	return m
}

// uploadManifest stores m as a blob and returns its bundle ref for root.
func uploadManifest(t *testing.T, env *callerEnv, m *BundleManifest, root string) string {
	t.Helper()
	data, _ := json.Marshal(m)
	h := sha(data)
	if h != m.Hash() {
		t.Fatal("Hash is not the sha256 of the JSON encoding")
	}
	if _, e := env.s.blobs.Put(h, "", bytes.NewReader(data)); e != nil {
		t.Fatal(e)
	}
	return "bundle:sha256:" + h + "/" + root
}

func attachRemote(t *testing.T, env *callerEnv) string {
	t.Helper()
	info, err := env.remote.Attach(context.Background(), "mock-1", AttachConfig{Platform: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	return info.ReportDir
}

func TestBundle_FlowRunsInsideTheBundle(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	reportDir := attachRemote(t, env)
	m := bundleOf(t, env, "main.yaml", map[string]string{
		"main.yaml": "appId: x\nonFlowStart:\n  - runScript: scripts/setup.js\nonFlowComplete:\n  - runFlow: teardown.yaml\n---\n" +
			"- runFlow: sub/open.yaml\n- runScript: scripts/compute.js\n- assertTrue: ${output.ok && output.setup}\n- addMedia: [photo.png]\n- takeScreenshot: done\n",
		"sub/open.yaml":      "- tapOn: General\n- takeScreenshot: x\n",
		"scripts/compute.js": "output.ok = true",
		"scripts/setup.js":   "output.setup = true",
		"teardown.yaml":      "- runScript: scripts/compute.js\n- takeScreenshot: td\n",
		"photo.png":          "\x89PNG photo",
	})
	res, err := env.remote.Flow(ctx, "mock-1", FlowRequest{Bundle: m}, true)
	if err != nil {
		t.Fatalf("bundled flow: %v %+v", err, res)
	}
	sort.Strings(res.Outputs)
	// onFlowComplete's runFlow resolved inside the bundle: its screenshot
	// is listed (failures there are ignored, so this is the proof).
	if strings.Join(res.Outputs, ",") != "outputs/done.png,outputs/sub/x.png,outputs/td.png" {
		t.Fatalf("outputs: %v", res.Outputs)
	}
	dir := filepath.Join(reportDir, "bundles", m.Hash())
	if _, err := os.Stat(filepath.Join(dir, "sub", "open.yaml")); err != nil {
		t.Fatalf("bundle not materialized: %v", err)
	}
	// The same bundle again reuses the directory; the two subflow outputs
	// do not collide.
	if _, err := env.remote.Flow(ctx, "mock-1", FlowRequest{Bundle: m, Env: map[string]string{"A": "b"}}, true); err != nil {
		t.Fatal(err)
	}
	// An inline yaml flow still works locally, and the session's flowDir
	// was restored: a local relative runFlow does not resolve in the bundle.
	if _, err := env.local.Flow(ctx, "mock-1", FlowRequest{YAML: "appId: x\n---\n- tapOn: X\n"}, true); err != nil {
		t.Fatal(err)
	}
	_, err = env.local.Command(ctx, "mock-1", "runFlow", "sub/open.yaml", true)
	if err == nil {
		t.Fatal("flowDir still points into the bundle")
	}
	// bundle with file or yaml is USAGE.
	_, err = env.remote.Flow(ctx, "mock-1", FlowRequest{Bundle: m, YAML: "- tapOn: X\n"}, true)
	wantErrCode(t, err, CodeUsage)
}

func TestBundle_StepListRootBelowCommonDir(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	attachRemote(t, env)
	m := bundleOf(t, env, "flows/steps.yaml", map[string]string{
		"flows/steps.yaml":      "- runFlow: sub/x.yaml\n- runScript: scripts/x.js\n- assertTrue: ${output.x}\n- runFlow: ../shared/login.yaml\n",
		"flows/sub/x.yaml":      "- tapOn: X\n",
		"flows/scripts/x.js":    "output.x = true",
		"shared/login.yaml":     "- takeScreenshot: x\n",
		"shared/unused-but.txt": "beside",
	})
	res, err := env.remote.Steps(ctx, "mock-1", StepsRequest{Bundle: m}, true)
	if err != nil || res.Passed != 4 {
		t.Fatalf("bundled steps: %v %+v", err, res)
	}
	if strings.Join(res.Outputs, ",") != "outputs/shared/x.png" {
		t.Fatalf("outputs: %v", res.Outputs)
	}
	// continueOnError keeps its meaning.
	bad := bundleOf(t, env, "s.yaml", map[string]string{"s.yaml": "- assertTrue: ${false}\n- tapOn: X\n"})
	res, err = env.remote.Steps(ctx, "mock-1", StepsRequest{Bundle: bad, ContinueOnError: true}, true)
	if err == nil || res == nil || res.Failed != 1 || res.Passed != 1 {
		t.Fatalf("continueOnError: %v %+v", err, res)
	}
	res, _ = env.remote.Steps(ctx, "mock-1", StepsRequest{Bundle: bad}, true)
	if res == nil || len(res.Results) != 1 {
		t.Fatalf("stop at the first failure: %+v", res)
	}
	// A remote cwd is refused even with a bundle; bundle with steps is USAGE.
	_, err = env.remote.Steps(ctx, "mock-1", StepsRequest{Bundle: m, Cwd: "/tmp"}, true)
	wantErrCode(t, err, CodeForbidden)
	_, err = env.remote.Steps(ctx, "mock-1", StepsRequest{Bundle: m, YAML: "- tapOn: X\n"}, true)
	wantErrCode(t, err, CodeUsage)
	// A root that does not parse is USAGE.
	broken := bundleOf(t, env, "b.yaml", map[string]string{"b.yaml": "- tapOn: [\n"})
	_, err = env.remote.Steps(ctx, "mock-1", StepsRequest{Bundle: broken}, true)
	wantErrCode(t, err, CodeUsage)
}

func TestBundle_PolicyStillApplies(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	attachRemote(t, env)
	shell := bundleOf(t, env, "runShell.yaml", map[string]string{"runShell.yaml": "- runShell: echo x\n"})
	_, err := env.remote.Flow(ctx, "mock-1", FlowRequest{Bundle: shell}, true)
	wantErrCode(t, err, CodeForbidden)
	escape := bundleOf(t, env, "escape.yaml", map[string]string{"escape.yaml": "- runFlow: ../../../../../../etc/hosts\n"})
	_, err = env.remote.Flow(ctx, "mock-1", FlowRequest{Bundle: escape}, true)
	wantErrCode(t, err, CodeForbidden)
	// Nested in a subflow too.
	nested := bundleOf(t, env, "a.yaml", map[string]string{"a.yaml": "- runFlow: b.yaml\n", "b.yaml": "- runShell: echo x\n"})
	_, err = env.remote.Steps(ctx, "mock-1", StepsRequest{Bundle: nested}, true)
	wantErrCode(t, err, CodeForbidden)
	// A unix-socket caller's bundled runShell runs.
	if _, err := env.local.Flow(ctx, "mock-1", FlowRequest{Bundle: shell}, true); err != nil {
		t.Fatalf("local bundled runShell: %v", err)
	}
}

func TestBundle_ManifestValidation(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	reportDir := attachRemote(t, env)
	good := strings.Repeat("a", 64)
	for name, m := range map[string]*BundleManifest{
		"absolute key":    {Root: "a.yaml", Files: map[string]string{"a.yaml": good, "/etc/hosts": good}},
		"dotdot key":      {Root: "a.yaml", Files: map[string]string{"a.yaml": good, "../x": good}},
		"inner dotdot":    {Root: "a.yaml", Files: map[string]string{"a.yaml": good, "x/../../y": good}},
		"empty key":       {Root: "a.yaml", Files: map[string]string{"a.yaml": good, "": good}},
		"dot element":     {Root: "a.yaml", Files: map[string]string{"a.yaml": good, "./x": good}},
		"backslash":       {Root: "a.yaml", Files: map[string]string{"a.yaml": good, `..\x`: good}},
		"absolute root":   {Root: "/a.yaml", Files: map[string]string{"a.yaml": good}},
		"dotdot root":     {Root: "../a.yaml", Files: map[string]string{"a.yaml": good}},
		"empty root":      {Root: "", Files: map[string]string{"a.yaml": good}},
		"root not a key":  {Root: "b.yaml", Files: map[string]string{"a.yaml": good}},
		"bad hash":        {Root: "a.yaml", Files: map[string]string{"a.yaml": "../../../.ssh/id_ed25519"}},
		"upper-case hash": {Root: "a.yaml", Files: map[string]string{"a.yaml": strings.Repeat("A", 64)}},
		"short hash":      {Root: "a.yaml", Files: map[string]string{"a.yaml": "abc"}},
		"no files at all": {Root: "a.yaml"},
	} {
		_, err := env.remote.Flow(ctx, "mock-1", FlowRequest{Bundle: m}, true)
		if codeOf(t, err) != CodeForbidden {
			t.Errorf("%s (flows): %v", name, err)
		}
		_, err = env.remote.Steps(ctx, "mock-1", StepsRequest{Bundle: m}, true)
		if codeOf(t, err) != CodeForbidden {
			t.Errorf("%s (steps): %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(reportDir, "bundles")); err == nil {
		t.Fatal("a bundles/ directory was created for an invalid manifest")
	}
	var nilManifest *BundleManifest
	if e := nilManifest.Validate(); e == nil || e.Code != CodeForbidden {
		t.Fatal("nil manifest")
	}
	big := &BundleManifest{Root: "a", Files: map[string]string{}}
	for i := 0; i <= maxBundleFiles; i++ {
		big.Files[fmt.Sprintf("f%d", i)] = good
	}
	big.Files["a"] = good
	if e := big.Validate(); e == nil || e.Code != CodeUsage {
		t.Fatalf("too many files: %v", e)
	}
	// A valid manifest whose blobs were never uploaded is USAGE, naming
	// the missing file.
	missing := &BundleManifest{Root: "a.yaml", Files: map[string]string{"a.yaml": good}}
	_, err := env.remote.Flow(ctx, "mock-1", FlowRequest{Bundle: missing}, true)
	if de := wantErrCode(t, err, CodeUsage); !strings.Contains(de.Message, "a.yaml") {
		t.Fatalf("missing: %v", de)
	}
	// Two keys where one is the other's directory cannot be built.
	clash := bundleOf(t, env, "a", map[string]string{"a": "- tapOn: X\n"})
	clash.Files["a/b"] = clash.Files["a"]
	_, err = env.remote.Flow(ctx, "mock-1", FlowRequest{Bundle: clash}, true)
	wantErrCode(t, err, CodeUsage)
}

func TestBundle_RefsInOneShots(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	reportDir := attachRemote(t, env)
	m := bundleOf(t, env, "sub/open.yaml", map[string]string{
		"sub/open.yaml":        "- runScript: scripts/x.js\n- assertTrue: ${output.x}\n- takeScreenshot: x\n",
		"sub/scripts/x.js":     "output.x = true",
		"sub/else.yaml":        "- takeScreenshot: e\n",
		"retry/r.yaml":         "- tapOn: X\n",
		"sub/escape-attempt.y": "- tapOn: X\n",
	})
	ref := uploadManifest(t, env, m, "sub/open.yaml")
	// A runFlow one-shot's subflow runs its own runScript: the
	// materialized bundle is one of the call's roots. Its output is named
	// relative to the bundle root.
	res, err := env.remote.Command(ctx, "mock-1", "runFlow", ref, true)
	if err != nil || strings.Join(res.Outputs, ",") != "outputs/sub/x.png" {
		t.Fatalf("runFlow bundle ref: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(reportDir, "bundles", m.Hash(), "sub", "scripts", "x.js")); err != nil {
		t.Fatal("bundle ref not materialized")
	}
	// runFlow.else as a file, and retry.file.
	elseRef := strings.TrimSuffix(ref, "sub/open.yaml") + "sub/else.yaml"
	res, err = env.remote.Command(ctx, "mock-1", "runFlow", map[string]any{"file": ref, "when": map[string]any{"true": "${false}"}, "else": elseRef}, true)
	if err != nil || strings.Join(res.Outputs, ",") != "outputs/sub/e.png" {
		t.Fatalf("runFlow else: %v %+v", err, res)
	}
	if _, err := env.remote.Command(ctx, "mock-1", "retry", map[string]any{"file": strings.TrimSuffix(ref, "sub/open.yaml") + "retry/r.yaml"}, true); err != nil {
		t.Fatalf("retry bundle ref: %v", err)
	}
	// Bad refs are FORBIDDEN before any file is opened.
	for _, bad := range []string{
		"bundle:sha256:../../../.ssh/id_ed25519/x.yaml",
		"bundle:sha256:" + strings.Repeat("A", 64) + "/x.yaml",
		strings.TrimSuffix(ref, "sub/open.yaml") + "../etc/hosts",
		strings.TrimSuffix(ref, "sub/open.yaml") + "not/in/bundle.yaml",
		"bundle:sha256:" + strings.Repeat("b", 64) + "/never-uploaded.yaml",
		"bundle:sha256:" + strings.Repeat("c", 64),
		"bundle:md5:x/y",
	} {
		_, err := env.remote.Command(ctx, "mock-1", "runFlow", bad, true)
		if codeOf(t, err) != CodeForbidden {
			t.Errorf("%s: %v", bad, err)
		}
	}
	// A manifest blob that is not a valid manifest is refused too.
	h := sha([]byte("not json"))
	_, _ = env.s.blobs.Put(h, "", strings.NewReader("not json"))
	_, err = env.remote.Command(ctx, "mock-1", "runFlow", "bundle:sha256:"+h+"/x.yaml", true)
	wantErrCode(t, err, CodeForbidden)
	invalid := &BundleManifest{Root: "../x", Files: map[string]string{"../x": strings.Repeat("a", 64)}}
	data, _ := json.Marshal(invalid)
	_, _ = env.s.blobs.Put(sha(data), "", bytes.NewReader(data))
	_, err = env.remote.Command(ctx, "mock-1", "runFlow", "bundle:sha256:"+sha(data)+"/x", true)
	wantErrCode(t, err, CodeForbidden)
}

func TestBundle_ReadManifest(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	b := NewBlobStore(t.TempDir())
	if _, err := b.ReadManifest("nope"); err == nil {
		t.Fatal("bad hash")
	}
	huge := bytes.Repeat([]byte(" "), maxManifestBytes+1)
	h := sha(huge)
	if _, e := b.Put(h, "", bytes.NewReader(huge)); e != nil {
		t.Fatal(e)
	}
	if _, err := b.ReadManifest(h); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("huge: %v", err)
	}
}

// Every field the generated registry marks Path, Flow or Output is
// exactly the set the caller policy walks.
func TestPolicyFields_EqualCommandSpecMarks(t *testing.T) {
	marked := map[string][]string{}
	for name, spec := range CommandSpecs {
		for _, f := range spec.Fields {
			if f.Path || f.Flow || f.Output {
				marked[name] = append(marked[name], f.Key)
			}
		}
		sort.Strings(marked[name])
		if len(marked[name]) == 0 {
			delete(marked, name)
		}
	}
	policy := executor.PolicyFields()
	a, _ := json.Marshal(marked)
	b, _ := json.Marshal(policy)
	if string(a) != string(b) {
		t.Fatalf("CommandSpecs marks %s\ncaller policy walks %s", a, b)
	}
	for _, k := range []string{"file", "else"} {
		found := false
		for _, f := range CommandSpecs["runFlow"].Fields {
			found = found || (f.Key == k && f.Flow && !f.Path)
		}
		if !found {
			t.Fatalf("runFlow.%s is not marked Flow", k)
		}
	}
	for _, f := range CommandSpecs["uploadFile"].Fields {
		if (f.Key == "path" || f.Key == "paths") && !f.Path {
			t.Fatalf("uploadFile.%s is not marked Path", f.Key)
		}
	}
}
