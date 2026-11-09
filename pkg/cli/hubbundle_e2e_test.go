package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2E_HubBundles drives pack --flow, a targeted run of a flow, of a
// step list and of stdin, and a targeted runFlow one-shot through the
// binary against a TCP daemon; then the same flow locally.
func TestE2E_HubBundles(t *testing.T) {
	e := newE2E(t)
	addr := serveTCP(t, e, "bundles", "e2e-token")
	hub := []string{"MAESTRO_D_HUB=" + addr, "MAESTRO_D_TOKEN=e2e-token"}
	mk := func(rel, data string) {
		p := filepath.Join(e.cwd, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("flow/main.yaml", "appId: x\n---\n- launchApp\n- runFlow: sub/open.yaml\n- runScript: scripts/compute.js\n- assertTrue: ${output.ok}\n- addMedia: [photo.png]\n- takeScreenshot: done\n")
	mk("flow/sub/open.yaml", "- tapOn: General\n- takeScreenshot: x\n")
	mk("flow/scripts/compute.js", "output.ok = true")
	mk("flow/photo.png", "\x89PNG photo")
	mk("flow/steps.yaml", "- assertTrue: ${false}\n- runFlow: sub/open.yaml\n- takeScreenshot: list\n- addMedia: [\"${PIC}\"]\n")
	mk("flow/data/one.csv", "1")
	mk("flow/missing.yaml", "- runFlow: ${MISSING}.yaml\n")

	// pack --flow, as text and as JSON; its errors.
	r := e.ok("pack", "--flow", "flow/main.yaml")
	if !strings.Contains(r.stdout, "root main.yaml") || !strings.Contains(r.stdout, "sub/open.yaml") {
		t.Fatalf("pack --flow: %s", r.stdout)
	}
	r = e.ok("pack", "--flow", "flow/steps.yaml", "-e", "PIC=photo.png", "--include", "data/*.csv", "--json")
	var packed struct {
		Bundle FlowBundle `json:"bundle"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &packed); err != nil || packed.Bundle.Manifest.Files["data/one.csv"] == "" {
		t.Fatalf("pack --flow --json: %v\n%s", err, r.stdout)
	}
	r = e.fails(2, "pack", "--flow", "flow/missing.yaml")
	if !strings.Contains(r.stderr+r.stdout, "runFlow: ${MISSING}.yaml") {
		t.Fatalf("missing: %s%s", r.stdout, r.stderr)
	}
	e.fails(2, "pack", "-e", "A=b", "flow/photo.png")

	// A targeted run packs the flow, uploads it, runs it on the daemon,
	// and fetches its outputs back beside the flow.
	r = e.runEnv(hub, "run", "flow/main.yaml", "--device", "mock-1", "--platform", "mock")
	if r.code != 0 || !strings.Contains(r.stderr, "Bundle ") || !strings.Contains(r.stderr, "uploaded 4 blobs") {
		t.Fatalf("run: %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, rel := range []string{"flow/done.png", "flow/sub/x.png"} {
		if _, err := os.Stat(filepath.Join(e.cwd, rel)); err != nil {
			t.Fatalf("%s not fetched back", rel)
		}
	}
	// Again: nothing to upload; --json keeps stderr quiet.
	r = e.runEnv(hub, "run", "flow/main.yaml", "--device", "mock-1")
	if r.code != 0 || !strings.Contains(r.stderr, "uploaded 0 blobs, 4 already") {
		t.Fatalf("run again: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(hub, "run", "flow/main.yaml", "--device", "mock-1", "--json")
	if r.code != 0 || strings.Contains(r.stderr, "Bundle ") || !strings.Contains(r.stdout, `"outputs/sub/x.png"`) {
		t.Fatalf("run --json: %s%s", r.stdout, r.stderr)
	}
	// A step list posts /steps {bundle, continueOnError}.
	r = e.runEnv(hub, "run", "flow/steps.yaml", "--device", "mock-3", "--platform", "mock", "-e", "PIC=photo.png", "--continue-on-error")
	if r.code == 0 || !strings.Contains(r.stdout, "1 failed") || !strings.Contains(r.stdout, "3 passed") {
		t.Fatalf("step list --continue-on-error: %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "flow", "list.png")); err != nil {
		t.Fatal("step list output not fetched")
	}
	r = e.runEnv(hub, "run", "flow/steps.yaml", "--device", "mock-3", "-e", "PIC=photo.png")
	if r.code == 0 || !strings.Contains(r.stdout, "0 passed") {
		t.Fatalf("step list stops: %s%s", r.stdout, r.stderr)
	}
	// An unresolvable path stops the run before any upload.
	r = e.runEnv(hub, "run", "flow/missing.yaml", "--device", "mock-1")
	if r.code != 2 || strings.Contains(r.stderr, "Bundle ") {
		t.Fatalf("missing: %s%s", r.stdout, r.stderr)
	}
	// stdin is a root that sits in the working directory.
	cmd := exec.Command(e.bin, "run", "-", "--device", "mock-1", "--include", "flow/data/*.csv")
	cmd.Dir = e.cwd
	cmd.Env = e.env(hub...)
	cmd.Stdin = strings.NewReader("- runFlow: flow/sub/open.yaml\n- takeScreenshot: stdin-shot\n")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("run -: %v\n%s%s", err, out.String(), errb.String())
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "stdin-shot.png")); err != nil {
		t.Fatalf("stdin output not fetched: %s", errb.String())
	}
	// A runFlow one-shot packs its closure and fetches the subflow's
	// output relative to that bundle's directory.
	_ = os.Remove(filepath.Join(e.cwd, "flow", "sub", "x.png"))
	r = e.runEnv(hub, "runFlow", "flow/sub/open.yaml", "--device", "mock-1")
	if r.code != 0 || !strings.Contains(r.stdout, "bundle:sha256:") {
		t.Fatalf("runFlow: %s%s", r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "flow", "sub", "x.png")); err != nil {
		t.Fatal("runFlow output not fetched beside the subflow")
	}

	// Untargeted, run passes the host path as before.
	r = e.ok("run", "flow/main.yaml", "--device", "mock-2", "--platform", "mock")
	if strings.Contains(r.stderr, "Bundle ") {
		t.Fatalf("a local run packed: %s", r.stderr)
	}
}
