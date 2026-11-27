package cli

// End-to-end test of the maestro-d CLI against the mock driver: builds
// the binary, lets the first one-shot spawn the daemon, and checks output
// and exit codes across the command surface.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

type e2e struct {
	t    *testing.T
	bin  string
	home string
	cwd  string
	// coverDir, from MAESTRO_D_E2E_COVERDIR, is where the -cover binary
	// and the daemons it spawns write their coverage (GOCOVERDIR).
	coverDir string
	// drop lists inherited variables (or prefixes ending in *) that every
	// command runs without.
	drop []string
}

// envE2ECoverDir makes newE2E build the binary with -cover; `make
// cover-changed` sets it so code only a subprocess runs is counted.
const envE2ECoverDir = "MAESTRO_D_E2E_COVERDIR"

func newE2E(t *testing.T) *e2e {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets only")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "maestro-d")
	args := []string{"build", "-o", dir + string(os.PathSeparator), "./cmd/maestro-d", "./cmd/maestro-d-hub"}
	coverDir := os.Getenv(envE2ECoverDir)
	if coverDir != "" {
		if err := os.MkdirAll(coverDir, 0o755); err != nil {
			t.Fatal(err)
		}
		args = []string{"build", "-cover", "-o", dir + string(os.PathSeparator), "./cmd/maestro-d", "./cmd/maestro-d-hub"}
	}
	build := exec.Command("go", args...)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	e := &e2e{t: t, bin: bin, home: filepath.Join(dir, "home"), cwd: dir, coverDir: coverDir}
	t.Cleanup(func() {
		e.run("stop", "--all")
	})
	return e
}

type result struct {
	stdout, stderr string
	code           int
}

// targetEnv are the variables that point a client at a TCP daemon or hub.
// The harness drops them, so a targeting shell never reaches the cases
// that test the local daemon; TestE2E_TargetHub sets them for itself.
var targetEnv = []string{"MAESTRO_D_HUB", "MAESTRO_D_TOKEN", "MAESTRO_D_CA"}

// env is the environment of every command the harness runs, plus extra.
func (e *e2e) env(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, t := range append(targetEnv, e.drop...) {
			if p, ok := strings.CutSuffix(t, "*"); ok {
				drop = drop || strings.HasPrefix(k, p)
			} else {
				drop = drop || k == t
			}
		}
		if !drop {
			env = append(env, kv)
		}
	}
	env = append(env,
		daemon.EnvHome+"="+e.home,
		daemon.EnvBin+"="+e.bin,
		daemon.EnvName+"=",
		daemon.EnvDevice+"=",
		"MAESTRO_D_HUB_WEB_PORT=0", // every fixture hub selects its own web port
		"NO_COLOR=1",
	)
	if e.coverDir != "" {
		env = append(env, "GOCOVERDIR="+e.coverDir)
	}
	return append(env, extra...)
}

func (e *e2e) run(args ...string) result {
	e.t.Helper()
	return e.runEnv(nil, args...)
}

// runEnv is run with extra environment variables.
func (e *e2e) runEnv(extra []string, args ...string) result {
	e.t.Helper()
	bin := e.bin
	if len(args) > 0 && args[0] == "maestro-d-hub" {
		bin = filepath.Join(filepath.Dir(e.bin), "maestro-d-hub")
		args = args[1:]
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = e.cwd
	cmd.Env = e.env(extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatalf("run %v: %v", args, err)
	}
	e.t.Logf("$ %s %s\n%s%s(exit %d)", filepath.Base(bin), strings.Join(args, " "), out.String(), errb.String(), code)
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

func (e *e2e) ok(args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 {
		e.t.Fatalf("%v: exit %d\n%s%s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

// okEnv is ok with extra environment variables.
func (e *e2e) okEnv(extra []string, args ...string) result {
	e.t.Helper()
	r := e.runEnv(extra, args...)
	if r.code != 0 {
		e.t.Fatalf("%v: exit %d\n%s%s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

func (e *e2e) fails(code int, args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	if r.code != code {
		e.t.Fatalf("%v: exit %d, want %d\n%s%s", args, r.code, code, r.stdout, r.stderr)
	}
	return r
}

func envelopeCode(t *testing.T, stderr string) daemon.Code {
	t.Helper()
	i := strings.Index(stderr, "{")
	if i < 0 {
		t.Fatalf("no JSON envelope on stderr:\n%s", stderr)
	}
	var env daemon.Envelope
	if err := json.Unmarshal([]byte(stderr[i:]), &env); err != nil {
		t.Fatalf("envelope: %v\n%s", err, stderr)
	}
	if env.Error == nil {
		t.Fatalf("envelope has no error:\n%s", stderr)
	}
	return env.Error.Code
}

func TestE2E_OneShotLifecycle(t *testing.T) {
	e := newE2E(t)

	// Nothing running yet.
	r := e.fails(3, "status")
	if envelopeCode(t, r.stderr) != daemon.CodeDaemonUnavailable {
		t.Fatal("status should report DAEMON_UNAVAILABLE")
	}
	// No device attached → usage.
	r = e.fails(2, "tapOn", "Login")
	if envelopeCode(t, r.stderr) != daemon.CodeUsage {
		t.Fatal("expected USAGE without a device")
	}
	if !strings.Contains(r.stderr, "Started daemon") {
		t.Fatalf("first call should spawn the daemon:\n%s", r.stderr)
	}

	// First device command attaches.
	r = e.ok("tapOn", "Login", "--device", "mock-1", "--platform", "mock")
	if !strings.Contains(r.stderr, "Attached mock-1") {
		t.Fatalf("expected spawn + attach on stderr:\n%s", r.stderr)
	}
	if !strings.Contains(r.stdout, "✓ tapOn") {
		t.Fatalf("stdout: %s", r.stdout)
	}
	// Second command reuses the only attached device.
	r = e.ok("tapOn", "--id", "btn", "--index", "2", "--optional")
	if strings.Contains(r.stderr, "Attach") {
		t.Fatalf("should not re-attach:\n%s", r.stderr)
	}
	if !strings.Contains(r.stdout, `id="btn"`) {
		t.Fatalf("--id should take the next token:\n%s", r.stdout)
	}
	// A bool field given bare does not swallow the positional after it.
	r = e.ok("launchApp", "--clearState", "com.example", "--json")
	if !strings.Contains(r.stdout, `"clearState": true`) && !strings.Contains(r.stdout, "(clearState)") {
		t.Fatalf("--clearState should be a bare bool:\n%s", r.stdout)
	}
	// Value-less.
	e.ok("back")
	// JSON output carries the full result.
	r = e.ok("assertVisible", "Login", "--json")
	var sr daemon.StepResult
	if err := json.Unmarshal([]byte(r.stdout), &sr); err != nil || !sr.OK || sr.Type != "assertVisible" {
		t.Fatalf("json result: %v %s", err, r.stdout)
	}
	// Failure → exit 1 + COMMAND_FAILED envelope.
	r = e.fails(1, "assertTrue", "false")
	if envelopeCode(t, r.stderr) != daemon.CodeCommandFailed {
		t.Fatal("expected COMMAND_FAILED")
	}
	if !strings.Contains(r.stdout, "✗ assertTrue") {
		t.Fatalf("stdout: %s", r.stdout)
	}
	// Optional failure → exit 0.
	e.ok("assertTrue", "false", "--optional")
	// Platform gate skips.
	r = e.ok("tapOn", "X", "--platform", "ios")
	if !strings.Contains(r.stdout, "skip") && !strings.Contains(r.stdout, "gate") {
		t.Fatalf("expected skip: %s", r.stdout)
	}
	// --daemon is honoured wherever it sits on the line, and --name is the
	// selector field, not the daemon.
	r = e.fails(3, "tapOn", "Login", "--daemon", "other", "--no-spawn")
	if envelopeCode(t, r.stderr) != daemon.CodeDaemonUnavailable {
		t.Fatalf("--daemon after the value should select daemon \"other\":\n%s", r.stderr)
	}
	r = e.ok("tapOn", "--name", "email")
	if !strings.Contains(r.stdout, `name="email"`) {
		t.Fatalf("--name should be the selector field:\n%s", r.stdout)
	}
	// A flag that is not a field of the command is a usage error.
	r = e.fails(2, "tapOn", "--nonsense", "x", "--json")
	if envelopeCode(t, r.stderr) != daemon.CodeUsage {
		t.Fatal("expected USAGE for unknown field")
	}
	// No value at all: fine where the scalar has a default (`- launchApp`
	// uses the attach app id), a usage error elsewhere.
	e.ok("launchApp")
	e.ok("takeScreenshot")
	r = e.fails(2, "tapOn", "--json")
	if envelopeCode(t, r.stderr) != daemon.CodeUsage || !strings.Contains(r.stderr, "needs a value") {
		t.Fatalf("expected USAGE for bare tapOn: %s", r.stderr)
	}
	// --json always leaves a document on stdout, even when no step ran.
	if envelopeCode(t, r.stdout) != daemon.CodeUsage {
		t.Fatalf("expected the USAGE envelope on stdout too: %q", r.stdout)
	}

	// Vars / eval.
	e.ok("set", "A=1", "B=two")
	r = e.ok("get", "vars")
	if !strings.Contains(r.stdout, "A=1") || !strings.Contains(r.stdout, "B=two") {
		t.Fatalf("vars: %s", r.stdout)
	}
	if strings.Contains(r.stdout, "PATH=") {
		t.Fatalf("process environment leaked into vars: %s", r.stdout)
	}
	r = e.ok("eval", "output.c = A + 'x'; output.c")
	if strings.TrimSpace(r.stdout) != "1x" {
		t.Fatalf("eval: %q", r.stdout)
	}
	// Variables set here are visible to ${} expressions in later steps.
	e.ok("assertTrue", "${B == 'two'}")
	e.fails(1, "assertTrue", "${B == 'three'}")

	// Inspection.
	shot := filepath.Join(e.cwd, "shot.png")
	e.ok("get", "screenshot", "-o", shot)
	if st, err := os.Stat(shot); err != nil || st.Size() == 0 {
		t.Fatalf("screenshot: %v", err)
	}
	r = e.ok("get", "info", "--json")
	if !strings.Contains(r.stdout, `"mock"`) {
		t.Fatalf("info: %s", r.stdout)
	}
	r = e.ok("get", "hierarchy")
	if !strings.Contains(r.stdout, `"text": "Mock Element"`) {
		t.Fatalf("hierarchy: %s", r.stdout)
	}
	r = e.ok("get", "hierarchy", "--find", "mock el")
	if !strings.Contains(r.stdout, "Mock Element") || strings.Contains(r.stdout, "{") {
		t.Fatalf("hierarchy --find: %s", r.stdout)
	}
	r = e.ok("get", "hierarchy", "--compact")
	if !strings.Contains(r.stdout, "Button") || strings.Contains(r.stdout, "{") {
		t.Fatalf("hierarchy --compact: %s", r.stdout)
	}
	r = e.ok("get", "hierarchy", "--raw")
	if !strings.Contains(r.stdout, `"type": "View"`) {
		t.Fatalf("hierarchy --raw: %s", r.stdout)
	}
	e.ok("get", "state")

	// Steps list via run - and a flow file.
	steps := filepath.Join(e.cwd, "steps.yaml")
	os.WriteFile(steps, []byte("- launchApp: com.example\n- tapOn: One\n- assertTrue: 'false'\n- tapOn: Two\n"), 0o644)
	r = e.fails(1, "run", steps)
	if strings.Contains(r.stdout, "Two") {
		t.Fatalf("should stop at first failure:\n%s", r.stdout)
	}
	r = e.fails(1, "run", steps, "--continue-on-error")
	if !strings.Contains(r.stdout, "Two") {
		t.Fatalf("should continue:\n%s", r.stdout)
	}
	flowFile := filepath.Join(e.cwd, "flow.yaml")
	os.WriteFile(flowFile, []byte("appId: com.example\nenv:\n  USER: bob\n---\n- launchApp\n- tapOn: ${USER}\n"), 0o644)
	r = e.ok("run", flowFile)
	if !strings.Contains(r.stdout, "runFlow") || !strings.Contains(r.stdout, "bob") {
		t.Fatalf("flow output:\n%s", r.stdout)
	}
	// runFlow one-shot with an env pair (upstream lets a flow's own env
	// header win, so use a flow without one).
	flow2 := filepath.Join(e.cwd, "flow2.yaml")
	os.WriteFile(flow2, []byte("appId: com.example\n---\n- tapOn: ${USER}\n"), 0o644)
	r = e.ok("runFlow", flow2, "--env", "USER=alice", "--json")
	if !strings.Contains(r.stdout, "alice") {
		t.Fatalf("runFlow env:\n%s", r.stdout)
	}
	// Compound step from --yaml.
	rep := filepath.Join(e.cwd, "repeat.yaml")
	os.WriteFile(rep, []byte("times: 2\ncommands:\n  - tapOn: R\n"), 0o644)
	r = e.ok("repeat", "--yaml", rep)
	if strings.Count(r.stdout, "R") < 2 {
		t.Fatalf("repeat substeps:\n%s", r.stdout)
	}

	// Two devices → --device required.
	e.ok("attach", "--device", "mock-2", "--platform", "mock")
	r = e.fails(2, "tapOn", "Login")
	if !strings.Contains(r.stderr, "mock-1") || !strings.Contains(r.stderr, "mock-2") {
		t.Fatalf("should list attached devices:\n%s", r.stderr)
	}
	e.ok("tapOn", "Login", "--device", "mock-2")
	r = e.ok("status", "--json")
	if strings.Count(r.stdout, `"attached": true`) != 2 {
		t.Fatalf("status: %s", r.stdout)
	}
	// A second daemon cannot attach a device this one holds.
	r = e.fails(5, "tapOn", "X", "--device", "mock-2", "--platform", "mock", "-n", "other")
	if envelopeCode(t, r.stderr) != daemon.CodeDeviceInUse {
		t.Fatal("expected DEVICE_IN_USE")
	}
	e.ok("detach", "--device", "mock-2")
	e.ok("tapOn", "Login") // back to a single attached device
	r = e.ok("ps")
	if !strings.Contains(r.stdout, "default") || !strings.Contains(r.stdout, "other") {
		t.Fatalf("ps: %s", r.stdout)
	}

	// Stop: daemon gone, status fails, run files cleaned.
	e.ok("stop")
	e.ok("stop", "-n", "other")
	e.fails(3, "status")
	e.fails(3, "tapOn", "X", "--no-spawn")
	if _, err := os.Stat(filepath.Join(e.home, "run", "default", "daemon.sock")); err == nil {
		t.Fatal("socket left behind")
	}
}

func TestE2E_AttachFailureAndInterrupt(t *testing.T) {
	e := newE2E(t)
	// The mock driver has no failure mode for attach, so exercise an
	// unsupported platform instead: CreateDriver rejects it → DEVICE_ERROR.
	r := e.fails(4, "tapOn", "X", "--device", "zzz", "--platform", "nope")
	if envelopeCode(t, r.stderr) != daemon.CodeDeviceError {
		t.Fatal("expected DEVICE_ERROR")
	}

	// Interrupt: a slow step gets cancelled → 130.
	e.ok("attach", "--device", "mock-1", "--platform", "mock")
	// The mock driver answers every step instantly; a while-loop repeat is
	// paced by the executor (300ms per iteration) and honours cancellation.
	loop := filepath.Join(e.cwd, "loop.yaml")
	os.WriteFile(loop, []byte("while:\n  visible: Login\ncommands:\n  - tapOn: Login\n"), 0o644)
	cmd := exec.Command(e.bin, "repeat", "--yaml", loop)
	cmd.Dir = e.cwd
	cmd.Env = e.env()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	_ = cmd.Process.Signal(os.Interrupt)
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 130 {
		t.Fatalf("expected exit 130, got %v\n%s", err, errb.String())
	}
	// The device is free again right away.
	e.ok("tapOn", "Login")
}

// TestE2E_DeviceManagementRefusals drives device catalog, create, start and
// delete through the binary for the refusals that are decided before any
// simulator or emulator tool runs, so it needs none installed.
func TestE2E_DeviceManagementRefusals(t *testing.T) {
	e := newE2E(t)
	cases := []struct {
		code int
		want daemon.Code
		args []string
	}{
		{2, daemon.CodeUsage, []string{"device", "catalog", "--platform", "windows"}},
		{2, daemon.CodeUsage, []string{"device", "catalog", "extra"}},
		{2, daemon.CodeUsage, []string{"device", "create", "--name", "x"}},
		{2, daemon.CodeUsage, []string{"device", "create", "--platform", "android", "--name", "e2e-x", "--data-size", "12G", "--no-boot"}},
		{2, daemon.CodeUsage, []string{"device", "create", "--platform", "ios", "--name", "e2e-x", "--image", "system-images;a;b;c"}},
		{2, daemon.CodeUsage, []string{"device", "start", "202611080630-no-such-device"}},
		{4, daemon.CodeDeviceNotFound, []string{"device", "delete", "202611080630-no-such-device"}},
		{2, daemon.CodeUsage, []string{"device", "delete"}},
	}
	for _, tc := range cases {
		r := e.fails(tc.code, tc.args...)
		if got := envelopeCode(t, r.stderr); got != tc.want {
			t.Errorf("%v: code %s, want %s", tc.args, got, tc.want)
		}
	}
	r := e.fails(2, "device", "create", "--platform", "android", "--name", "e2e-x", "--data-size", "12G", "--json")
	if !strings.Contains(r.stdout, "above the 10G limit") {
		t.Errorf("--json puts the envelope on stdout:\n%s", r.stdout)
	}
	e.ok("stop")
}

func TestE2E_HarnessDropsTargetEnv(t *testing.T) {
	t.Setenv("MAESTRO_D_HUB", "203.0.113.1:7433")
	t.Setenv("MAESTRO_D_TOKEN", "parent-token")
	t.Setenv("MAESTRO_D_CA", "/parent/ca.pem")
	e := &e2e{t: t, home: t.TempDir(), bin: "maestro-d"}
	for _, kv := range e.env() {
		for _, k := range targetEnv {
			if strings.HasPrefix(kv, k+"=") {
				t.Fatalf("%s reached the harness environment", kv)
			}
		}
	}
	if env := e.env("MAESTRO_D_HUB=x"); env[len(env)-1] != "MAESTRO_D_HUB=x" {
		t.Fatal("a case can still set it for itself")
	}
}

// TestE2E_TargetHub drives a TCP daemon (serve --http --token) with
// one-shots through MAESTRO_D_HUB and --target-hub, and checks that no
// local daemon is spawned.
func TestE2E_TargetHub(t *testing.T) {
	e := newE2E(t)
	serve := exec.Command(e.bin, "serve", "--daemon", "th", "--http", "127.0.0.1:0", "--token", "e2e-token", "--idle-timeout", "0")
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
	// Reap it as soon as it exits, so `stop` sees it gone.
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
	hub := []string{"MAESTRO_D_HUB=" + addr, "MAESTRO_D_TOKEN=e2e-token"}

	// MAESTRO_D_HUB and MAESTRO_D_TOKEN target the TCP daemon.
	r := e.runEnv(hub, "tapOn", "Login", "--device", "mock-1", "--platform", "mock")
	if r.code != 0 || !strings.Contains(r.stdout, "✓ tapOn") || strings.Contains(r.stderr, "Started daemon") {
		t.Fatalf("targeted tapOn: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	// --target-hub=ADDR and --token on the line do the same; tapOn's text
	// after a bare flag is still the text.
	r = e.ok("tapOn", "--target-hub="+addr, "--token", "e2e-token", "Login")
	if !strings.Contains(r.stdout, `text="Login"`) {
		t.Fatalf("stdout: %s", r.stdout)
	}
	r = e.ok("status", "--target-hub="+addr, "--token", "e2e-token", "--json")
	if !strings.Contains(r.stdout, `"name": "th"`) {
		t.Fatalf("status: %s", r.stdout)
	}
	e.ok("device", "list", "--target-hub="+addr, "--token", "e2e-token")
	// A step list runs from its YAML; the file stays on the client.
	steps := filepath.Join(e.cwd, "remote-steps.yaml")
	_ = os.WriteFile(steps, []byte("- tapOn: One\n- tapOn: Two\n"), 0o644)
	r = e.runEnv(hub, "run", steps)
	if r.code != 0 || !strings.Contains(r.stdout, "2 passed") {
		t.Fatalf("run: %s%s", r.stdout, r.stderr)
	}
	flowFile := filepath.Join(e.cwd, "remote-flow.yaml")
	_ = os.WriteFile(flowFile, []byte("appId: com.example\n---\n- tapOn: ${USER}\n"), 0o644)
	r = e.runEnv(hub, "run", flowFile, "-e", "USER=carol")
	if r.code != 0 || !strings.Contains(r.stdout, "carol") {
		t.Fatalf("run flow: %s%s", r.stdout, r.stderr)
	}

	// Without the token: 401 → USAGE, exit 2.
	r = e.fails(2, "tapOn", "Login", "--target-hub="+addr)
	if envelopeCode(t, r.stderr) != daemon.CodeUsage || !strings.Contains(r.stderr, "bearer token") {
		t.Fatalf("no token: %s", r.stderr)
	}
	// Unreachable: exit 3, and nothing is spawned.
	r = e.fails(3, "tapOn", "Login", "--target-hub=127.0.0.1:1", "--token", "x")
	if envelopeCode(t, r.stderr) != daemon.CodeDaemonUnavailable {
		t.Fatal("expected DAEMON_UNAVAILABLE")
	}
	// --output-dir names a directory on the daemon's host.
	e.fails(2, "tapOn", "Login", "--target-hub="+addr, "--token", "e2e-token", "--output-dir", "/tmp/x")

	r = e.ok("ps", "--json")
	if strings.Contains(r.stdout, `"name": "default"`) || !strings.Contains(r.stdout, `"name": "th"`) {
		t.Fatalf("a targeted command spawned a local daemon:\n%s", r.stdout)
	}
	e.ok("stop", "--daemon", "th")
}

// TestE2E_Leases drives a single `hub start` with listed mock devices
// (MAESTRO_D_MOCK_DEVICES): a targeted command takes its own lease, a
// second client home gets DEVICE_IN_USE, lease prints the lease on file,
// release frees the device, a selector lease expires and another client
// takes the device over, and leases.json is reused and pruned.
func TestE2E_Leases(t *testing.T) {
	e := newE2E(t)
	mocks := []string{"MAESTRO_D_MOCK_DEVICES=mock-1,mock-2"}
	tokenFile := filepath.Join(e.cwd, "hub-token")
	if err := os.WriteFile(tokenFile, []byte("lease-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No token, or a token file others can read: refused before starting.
	if r := e.runEnv(mocks, "maestro-d-hub", "start"); r.code != 2 || !strings.Contains(r.stderr, "needs --token") {
		t.Fatalf("no token: exit %d\n%s", r.code, r.stderr)
	}
	if r := e.runEnv(mocks, "maestro-d-hub", "start", "--token-file", tokenFile); r.code != 2 || !strings.Contains(r.stderr, "chmod 600") {
		t.Fatalf("readable token file: exit %d\n%s", r.code, r.stderr)
	}
	_ = os.Chmod(tokenFile, 0o600)
	if r := e.runEnv(mocks, "maestro-d-hub", "start", "--token-file", tokenFile); r.code != 2 || !strings.Contains(r.stderr, "needs --peer-token") {
		t.Fatalf("no peer token: exit %d\n%s", r.code, r.stderr)
	}
	writeSharedMocks(t, e.home, "mock-1", "mock-2")
	mesh := []string{"--name", "lh-hub", "--no-mdns", "--peer-token", "lease-peer", "--lease-max", "1h", "--lease-max", "mock-2=8s"}
	r := e.runEnv(mocks, append([]string{"maestro-d-hub", "start", "--listen", "127.0.0.1:0", "--token-file", tokenFile, "--json"}, mesh...)...)
	if r.code != 0 {
		t.Fatalf("hub start: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	var started struct {
		Daemon daemon.Info `json:"daemon"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &started); err != nil || started.Daemon.Hub == "" || started.Daemon.HTTPAddr == "" {
		t.Fatalf("hub start --json: %v\n%s", err, r.stdout)
	}
	addr := started.Daemon.HTTPAddr
	for _, a := range started.Daemon.StartArgs {
		if strings.Contains(a, "lease-token") {
			t.Fatalf("the token reached the hub's command line: %v", started.Daemon.StartArgs)
		}
	}
	if r := e.runEnv(mocks, append([]string{"maestro-d-hub", "start", "--token", "lease-token"}, mesh...)...); r.code != 2 || !strings.Contains(r.stderr, "HUB_RUNNING") {
		t.Fatalf("second hub start: exit %d\n%s", r.code, r.stdout)
	}
	hub := []string{"MAESTRO_D_HUB=" + addr, "MAESTRO_D_TOKEN=lease-token"}
	home2 := []string{"MAESTRO_D_HUB=" + addr, "MAESTRO_D_TOKEN=lease-token", daemon.EnvHome + "=" + filepath.Join(e.cwd, "home2")}
	leaseFile := func(home string) string {
		data, _ := os.ReadFile(filepath.Join(home, "leases.json"))
		return string(data)
	}
	leaseOf := func(stdout string) daemon.Lease {
		t.Helper()
		var res daemon.LeaseResult
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatalf("lease --json: %v\n%s", err, stdout)
		}
		return res.Lease
	}

	r = e.runEnv(hub, "status")
	if !strings.Contains(r.stdout, "hub:") {
		t.Fatalf("status names the hub: %s", r.stdout)
	}
	r = e.runEnv(hub, "device", "list")
	if !strings.Contains(r.stdout, "LEASE") || !strings.Contains(r.stdout, "available") {
		t.Fatalf("device list: %s", r.stdout)
	}
	// Listings carry each device's maximum window: --lease-max 1h for the
	// hub, mock-2=8s for one device.
	r = e.runEnv(hub, "device", "list", "--platform", "mock", "--json")
	var listed daemon.DevicesResult
	if err := json.Unmarshal([]byte(r.stdout), &listed); err != nil || len(listed.Devices) != 2 ||
		listed.Devices[0].MaxSec != 3600 || listed.Devices[1].MaxSec != 8 {
		t.Fatalf("maxSec in the listing: %v\n%s", err, r.stdout)
	}
	// No client leases on its own: without a lease the hub refuses the
	// command, naming `maestro-d lease`, and no lease is taken.
	r = e.runEnv(hub, "tapOn", "Login", "--device", "mock-1")
	if r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeLeaseRequired || !strings.Contains(r.stderr, "maestro-d lease --device mock-1") {
		t.Fatalf("no lease: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if f := leaseFile(e.home); strings.Contains(f, "mock-1") {
		t.Fatalf("a lease was taken: %s", f)
	}
	// A window over the maximum is refused, naming it; nothing is granted.
	r = e.runEnv(hub, "lease", "--device", "mock-1", "--ttl", "2h")
	if r.code != 2 || !strings.Contains(r.stderr, "1h0m0s") {
		t.Fatalf("over the maximum: exit %d\n%s", r.code, r.stderr)
	}
	// An explicit lease, by default the device's maximum window, recorded
	// with the qualified id the hub returns.
	onFile := leaseOf(e.runEnv(hub, "lease", "--device", "mock-1", "--json").stdout)
	if onFile.Device != "mock-1@lh-hub" || onFile.TTLSec != 3600 || onFile.MaxSec != 3600 {
		t.Fatalf("lease: %+v", onFile)
	}
	if f := leaseFile(e.home); !strings.Contains(f, `"device": "mock-1@lh-hub"`) || !strings.Contains(f, addr) {
		t.Fatalf("leases.json: %s", f)
	}
	// Commands use it, and never move its end.
	e.okEnv(hub, "tapOn", "Login", "--device", "mock-1")
	time.Sleep(1100 * time.Millisecond)
	e.okEnv(hub, "tapOn", "Again", "--device", "mock-1")
	// lease --device with a live lease on file prints that lease, as it is.
	if again := leaseOf(e.runEnv(hub, "lease", "--device", "mock-1", "--json").stdout); again.ID != onFile.ID || again.ExpiresAt != onFile.ExpiresAt {
		t.Fatalf("use moved the window, or lease printed a new one: %+v vs %+v", again, onFile)
	}
	r = e.runEnv(hub, "device", "list", "--platform", "mock")
	if !regexpMatch(`mock-1@lh-hub .* yes .*\(\d+m\d+s\)`, r.stdout) {
		t.Fatalf("device list shows the holder and the time left:\n%s", r.stdout)
	}
	// Only renew moves it; over the maximum it is refused.
	renewed := leaseOf(e.runEnv(hub, "renew", "--device", "mock-1", "--ttl", "30m", "--json").stdout)
	if renewed.ID != onFile.ID || renewed.TTLSec != 1800 {
		t.Fatalf("renew: %+v", renewed)
	}
	if r = e.runEnv(hub, "renew", "--device", "mock-1", "--ttl", "2h"); r.code != 2 || !strings.Contains(r.stderr, "maximum") {
		t.Fatalf("renew over the maximum: exit %d\n%s", r.code, r.stderr)
	}
	if r = e.runEnv(hub, "renew"); r.code != 0 || !strings.Contains(r.stdout, "mock-1@lh-hub renewed") {
		t.Fatalf("renew every lease: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	// Another client home: DEVICE_IN_USE for a lease, LEASE_REQUIRED for a
	// command (exit 5 both); renew with nothing on file is LEASE_REQUIRED.
	r = e.runEnv(home2, "lease", "--device", "mock-1")
	if r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeDeviceInUse {
		t.Fatalf("second client lease: exit %d\n%s", r.code, r.stderr)
	}
	r = e.runEnv(home2, "tapOn", "Login", "--device", "mock-1")
	if r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeLeaseRequired {
		t.Fatalf("second client command: exit %d\n%s", r.code, r.stderr)
	}
	if r = e.runEnv(home2, "renew", "--device", "mock-1"); r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeLeaseRequired {
		t.Fatalf("renew without a lease: exit %d\n%s", r.code, r.stderr)
	}
	e.ok("release", "--device", "mock-1", "--target-hub="+addr, "--token", "lease-token")
	if strings.Contains(leaseFile(e.home), "mock-1") {
		t.Fatalf("release left the entry: %s", leaseFile(e.home))
	}
	if r = e.runEnv(hub, "release", "--device", "mock-1"); r.code != 0 || !strings.Contains(r.stdout, "No lease") {
		t.Fatalf("release with nothing on file: %s", r.stdout)
	}

	// A selector lease with a short window attaches mock-1; --no-attach
	// only reserves mock-2, for its 8 s maximum.
	sel := leaseOf(e.runEnv(hub, "lease", "--platform", "mock", "--ttl", "6", "--json").stdout)
	if sel.Device != "mock-1@lh-hub" || sel.TTLSec != 6 || !strings.HasSuffix(sel.ID, "@lh-hub") {
		t.Fatalf("selector lease: %+v", sel)
	}
	r = e.runEnv(hub, "lease", "--platform", "mock", "--no-attach", "--json")
	var reserved daemon.LeaseResult
	_ = json.Unmarshal([]byte(r.stdout), &reserved)
	if reserved.Lease.Device != "mock-2@lh-hub" || reserved.Lease.TTLSec != 8 || (reserved.Device != nil && reserved.Device.Attached) {
		t.Fatalf("--no-attach: %s", r.stdout)
	}
	if r = e.runEnv(hub, "lease", "--platform", "mock"); r.code != 5 {
		t.Fatalf("every mock leased: exit %d\n%s", r.code, r.stderr)
	}
	// Used through the window: the uses before its end work, the first
	// after it is LEASE_EXPIRED (the use in between did not extend it), the
	// entry is dropped, and the next try is LEASE_REQUIRED: no new lease.
	// A use starts only with 3 s left: one CLI one-shot under -race on a
	// loaded machine can take more than a second.
	end, _ := time.Parse(time.RFC3339, sel.ExpiresAt)
	for time.Until(end) > 3*time.Second {
		e.okEnv(hub, "tapOn", "During", "--device", "mock-1")
		time.Sleep(time.Second)
	}
	time.Sleep(time.Until(end) + 1500*time.Millisecond)
	r = e.runEnv(hub, "tapOn", "After", "--device", "mock-1")
	if r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeLeaseExpired || strings.Contains(leaseFile(e.home), sel.ID) {
		t.Fatalf("past the window: exit %d\n%s\n%s", r.code, r.stderr, leaseFile(e.home))
	}
	if r = e.runEnv(hub, "tapOn", "Again", "--device", "mock-1"); r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeLeaseRequired {
		t.Fatalf("after the expired entry: exit %d\n%s", r.code, r.stderr)
	}
	// The hub ended it and detached the device: the second client leases
	// it and takes over.
	leaseOf(e.runEnv(home2, "lease", "--device", "mock-1", "--json").stdout)
	e.okEnv(home2, "tapOn", "Takeover", "--device", "mock-1")
	r = e.runEnv(home2, "release", "--all", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"released"`) {
		t.Fatalf("release --all: %s", r.stdout)
	}
	e.ok("release", "--device", "mock-2", "--target-hub="+addr, "--token", "lease-token")

	// The hub's recorded unix socket enforces leases for local device commands too.
	local := daemon.NewUnixClient(started.Daemon.SocketPath)
	ctx := context.Background()
	if _, err := local.Command(ctx, "mock-2", "tapOn", "Local", true); !isCode(err, daemon.CodeLeaseRequired) {
		t.Fatalf("local command without lease: %v", err)
	}
	grant, err := local.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-2", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	local.SetLease("mock-2", grant.Lease.ID)
	if _, err = local.Command(ctx, "mock-2", "tapOn", "Local", true); err != nil {
		t.Fatal(err)
	}
	if _, err = local.ReleaseLease(ctx, grant.Lease.ID); err != nil {
		t.Fatal(err)
	}

	// Reservations: reserve records one; before its start commands carry it
	// and get LEASE_NOT_STARTED, and another home cannot claim it; after it,
	// another home claims it by id and drives the device; release --lease
	// cancels one reservation and release --device the rest.
	t.Run("reserve-claim", func(t *testing.T) {
		e.okEnv(hub, "release", "--all")
		start := time.Now().Add(4 * time.Second).UTC().Truncate(time.Second).Add(time.Second)
		l := leaseOf(e.okEnv(hub, "reserve", "--device", "mock-1", "--start-at", start.Format(time.RFC3339), "--ttl", "1m", "--json").stdout)
		if l.State != daemon.LeaseScheduled || !strings.Contains(leaseFile(e.home), `"startsAt"`) {
			t.Fatalf("reserve: %+v\n%s", l, leaseFile(e.home))
		}
		if r := e.runEnv(hub, "tapOn", "Login", "--device", "mock-1"); r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeLeaseNotStarted ||
			!strings.Contains(leaseFile(e.home), l.ID) {
			t.Fatalf("before the start: exit %d\n%s", r.code, r.stderr)
		}
		if r := e.runEnv(home2, "claim", "--lease", l.ID); r.code != 5 || strings.Contains(leaseFile(filepath.Join(e.cwd, "home2")), l.ID) {
			t.Fatalf("claim before the start: exit %d\n%s", r.code, r.stderr)
		}
		if r := e.okEnv(hub, "device", "list"); !strings.Contains(r.stdout, "next ") {
			t.Fatalf("device list: %s", r.stdout)
		}
		time.Sleep(time.Until(start) + 1500*time.Millisecond)
		e.okEnv(home2, "claim", "--lease", l.ID, "--platform", "mock")
		e.okEnv(home2, "tapOn", "Login", "--device", "mock-1")
		r1 := leaseOf(e.okEnv(hub, "reserve", "--device", "mock-1", "--start-at", "10m", "--ttl", "1m", "--json").stdout)
		leaseOf(e.okEnv(hub, "reserve", "--device", "mock-1", "--start-at", "30m", "--ttl", "1m", "--json").stdout)
		e.okEnv(hub, "release", "--lease", r1.ID)
		if r := e.okEnv(hub, "release", "--device", "mock-1", "--json"); !strings.Contains(r.stdout, l.ID) {
			t.Fatalf("release --device: %s", r.stdout)
		}
		if r := e.runEnv(hub, "device", "list", "--platform", "mock", "--json"); strings.Contains(r.stdout, `"reservations"`) {
			t.Fatalf("reservations left: %s", r.stdout)
		}
	})

	// Usage errors.
	e.fails(2, "release", "--target-hub="+addr, "--token", "lease-token")
	e.fails(2, "lease", "--target-hub="+addr, "--token", "lease-token")
	e.fails(2, "lease", "--device", "mock-1", "--kind", "device", "--target-hub="+addr, "--token", "lease-token")

	// A daemon without leases: release does nothing and succeeds; lease is
	// a usage error.
	plain := serveTCP(t, e, "plainlease", "plain-token")
	if r = e.run("release", "--device", "mock-1", "--target-hub="+plain, "--token", "plain-token"); r.code != 0 || !strings.Contains(r.stdout, "no leases") {
		t.Fatalf("release on a plain daemon: exit %d\n%s", r.code, r.stdout)
	}
	if r = e.run("release", "--all", "--daemon", "nothing-runs-here"); r.code != 0 {
		t.Fatalf("release with no daemon: exit %d", r.code)
	}
	e.fails(2, "lease", "--device", "mock-1", "--target-hub="+plain, "--token", "plain-token")
	// ...and a plain daemon still attaches without a lease.
	e.ok("tapOn", "Plain", "--device", "mock-1", "--platform", "mock", "--target-hub="+plain, "--token", "plain-token")

	r = e.ok("maestro-d-hub", "stop")
	if !strings.Contains(r.stdout, "stopped") {
		t.Fatalf("hub stop: %s", r.stdout)
	}
	e.fails(3, "maestro-d-hub", "stop")
}

func regexpMatch(pattern, s string) bool {
	return regexp.MustCompile(pattern).MatchString(s)
}

// reportDirs lists the report directories a mock-1 session made under the
// default daemon's home.
func (e *e2e) reportDirs() []string {
	e.t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(e.home, "run", "default", "reports", "mock-1", "*"))
	return dirs
}

func TestE2E_ReportLifecycle(t *testing.T) {
	e := newE2E(t)

	// A failing step captures its failure artifacts; the daemon writes
	// report.json and junit-report.xml, never report.html or Allure.
	e.ok("tapOn", "Login", "--device", "mock-1", "--platform", "mock")
	e.fails(1, "assertTrue", "false", "--device", "mock-1")
	dirs := e.reportDirs()
	if len(dirs) != 1 {
		t.Fatalf("report dirs = %v, want 1", dirs)
	}
	kept := dirs[0]
	for _, name := range []string{"report.json", "junit-report.xml", "flows"} {
		if _, err := os.Stat(filepath.Join(kept, name)); err != nil {
			t.Errorf("attached session lacks %s: %v", name, err)
		}
	}
	for _, name := range []string{"report.html", "allure-results"} {
		if _, err := os.Stat(filepath.Join(kept, name)); err == nil {
			t.Errorf("daemon session wrote %s", name)
		}
	}
	if err := os.MkdirAll(filepath.Join(kept, "bundles", "abc"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Default: the report stays after detach, without its inputs.
	e.ok("detach", "--device", "mock-1")
	if _, err := os.Stat(filepath.Join(kept, "report.json")); err != nil {
		t.Fatalf("detach removed the kept report: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kept, "bundles")); err == nil {
		t.Error("detach left bundles/")
	}

	// --discard-reports removes the session's own report directory.
	time.Sleep(1100 * time.Millisecond) // a new second, so a new report dir
	e.ok("tapOn", "Login", "--device", "mock-1", "--platform", "mock", "--discard-reports")
	if got := e.reportDirs(); len(got) != 2 {
		t.Fatalf("report dirs while discarding session attached = %v, want 2", got)
	}
	e.ok("detach", "--device", "mock-1")
	if got := e.reportDirs(); len(got) != 1 || got[0] != kept {
		t.Errorf("after a discarding detach report dirs = %v, want only %s", got, kept)
	}

	// A caller's outputDir is never deleted.
	out := filepath.Join(t.TempDir(), "out")
	e.ok("tapOn", "Login", "--device", "mock-1", "--platform", "mock", "--output-dir", out, "--discard-reports")
	e.ok("detach", "--device", "mock-1")
	if _, err := os.Stat(filepath.Join(out, "report.json")); err != nil {
		t.Errorf("a caller's outputDir lost report.json: %v", err)
	}
}

// seed writes a file under the e2e home and ages it.
func (e *e2e) seed(rel string, ago time.Duration) string {
	e.t.Helper()
	p := filepath.Join(e.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	old := time.Now().Add(-ago)
	for d := p; d != e.home; d = filepath.Dir(d) {
		_ = os.Chtimes(d, old, old)
	}
	return p
}

func TestE2E_Housekeeping(t *testing.T) {
	e := newE2E(t)
	day := 24 * time.Hour
	oldBlob := e.seed("blobs/sha256/"+strings.Repeat("a", 64), 2*day)
	newBlob := e.seed("blobs/sha256/"+strings.Repeat("b", 64), time.Hour)
	deadLog := e.seed("run/old-daemon/daemon.log", 8*day)
	idleReport := e.seed("run/old-daemon2/reports/mock-9/20261101-000000/report.json", 4*day)
	keepBin := e.seed("drivers/keep", 30*day)

	// gc --dry-run lists and deletes nothing.
	r := e.ok("gc", "--dry-run", "--json")
	var out struct {
		GC daemon.SweepResult `json:"gc"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("gc --json: %v\n%s", err, r.stdout)
	}
	listed := map[string]bool{}
	for _, x := range out.GC.Removed {
		listed[x.Path] = true
	}
	if !out.GC.DryRun || !listed[oldBlob] || !listed[filepath.Join(e.home, "run", "old-daemon")] || listed[newBlob] {
		t.Fatalf("dry run listed %v", out.GC.Removed)
	}
	for _, p := range []string{oldBlob, deadLog, idleReport} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry run deleted %s", p)
		}
	}
	r = e.ok("gc", "--dry-run")
	if !strings.Contains(r.stdout, "Would delete "+oldBlob) || !strings.Contains(r.stdout, "path(s)") {
		t.Errorf("gc --dry-run text: %s", r.stdout)
	}

	// A running daemon sweeps at start: old blob, dead run dir and idle
	// report go; its own run dir, fresh blobs and drivers stay.
	e.okEnv([]string{daemon.EnvHousekeepingInterval + "=1s"}, "tapOn", "Login", "--device", "mock-1", "--platform", "mock")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(oldBlob); os.IsNotExist(err) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, p := range []string{oldBlob, filepath.Join(e.home, "run", "old-daemon"), filepath.Dir(idleReport)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("the daemon's sweep left %s", p)
		}
	}
	for _, p := range []string{newBlob, keepBin, filepath.Join(e.home, "run", "default", "daemon.json")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the daemon's sweep removed %s: %v", p, err)
		}
	}

	// While another sweep holds the home, gc says so and touches nothing
	// there. The daemon, still running, sweeps every second under this
	// lock: the test waits out a sweep in progress (a sweep that finds the
	// lock held skips the home, so none waits on the test).
	hk, err := os.OpenFile(filepath.Join(e.home, "housekeeping.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for {
		err = syscall.Flock(int(hk.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	r = e.ok("gc")
	if !strings.Contains(r.stdout, "Skipped the home") {
		t.Errorf("gc while locked: %s", r.stdout)
	}
	r = e.ok("gc", "--include-build-cache", "--dry-run")
	if !strings.Contains(r.stdout, "the build cache was still checked") {
		t.Errorf("gc --include-build-cache while locked: %s", r.stdout)
	}
	_ = syscall.Flock(int(hk.Fd()), syscall.LOCK_UN)
	_ = hk.Close()

	// gc --include-build-cache prunes the binary's build cache, not
	// MAESTRO_D_HOME's.
	runnerHome := t.TempDir()
	stale := filepath.Join(runnerHome, "cache", "wda-builds", "sim-ios20.0-iphone")
	if err := os.MkdirAll(filepath.Join(stale, "Build"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * day)
	_ = filepath.Walk(stale, func(p string, _ os.FileInfo, _ error) error { return os.Chtimes(p, old, old) })
	r = e.okEnv([]string{"MAESTRO_RUNNER_HOME=" + runnerHome}, "gc", "--include-build-cache")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("gc --include-build-cache left %s:\n%s", stale, r.stdout)
	}
	if !strings.Contains(r.stdout, "Deleted") {
		t.Errorf("gc output: %s", r.stdout)
	}
}
