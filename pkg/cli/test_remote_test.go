package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
	"github.com/urfave/cli/v2"
)

func TestTargetHubTestArguments(t *testing.T) {
	t.Setenv("MAESTRO_D_HUB", "")
	flags := append(append([]cli.Flag{}, GlobalFlags...), testCommand.Flags...)
	for _, tc := range []struct{ args, want []string }{
		{[]string{"test", "--target-hub", "login.yaml"}, []string{"test", "--target-hub=localhost:7433", "--", "login.yaml"}},
		{[]string{"--platform", "mock", "test", "one.yaml", "--target-hub=h:9", "--device", "a,b", "--parallel", "2", "two.yaml"}, []string{"--platform", "mock", "--device", "a,b", "test", "--target-hub=h:9", "--parallel", "2", "--", "one.yaml", "two.yaml"}},
		{[]string{"test", "local.yaml"}, []string{"test", "local.yaml"}},
		{[]string{"tapOn", "test", "--target-hub"}, []string{"tapOn", "test", "--target-hub"}},
	} {
		got := hubTestArgs(append([]string{"client"}, tc.args...), flags)
		if !reflect.DeepEqual(got[1:], tc.want) {
			t.Errorf("%v => %v, want %v", tc.args, got[1:], tc.want)
		}
	}
	t.Setenv("MAESTRO_D_HUB", "h:3")
	args := hubTestArgs([]string{"c", "test", "--device=a", "x.yaml", "--", "--literal.yaml"}, flags)
	if !reflect.DeepEqual(args, []string{"c", "--device=a", "test", "--", "x.yaml", "--literal.yaml"}) {
		t.Fatal(args)
	}
	if err := runHubTest(context.Background(), &RunConfig{Parallel: 2, Devices: []string{"a"}}, daemonOpts{}); !isCode(err, daemon.CodeUsage) {
		t.Fatal(err)
	}
}

func TestHubTestAttachOptions(t *testing.T) {
	cfg := RunConfig{
		Platform: "ios", AppFile: "checkout.app", AppID: "test.app", Env: map[string]string{"F": "1"},
		TeamID: "team", WDABundleID: "wda.bundle", DriverStartTimeout: 22, ConditionTimeout: 310,
		StepDelay: 8, TypingFrequency: 11, NoAppInstall: true, NoDriverInstall: true,
		NoFlutterFallback: true, AndroidTCPForward: true,
	}
	attach := hubTestAttachConfig(&cfg, "never")
	if attach.Driver != "" || attach.Artifacts != "never" || attach.WaitForIdleTimeout == nil || *attach.WaitForIdleTimeout != 0 {
		t.Fatalf("implicit driver, artifact mode or zero idle timeout lost: %+v", attach)
	}
	got := attachToRunConfig("SIM-UP", attach)
	if got.Platform != cfg.Platform || got.AppFile != cfg.AppFile || got.AppID != cfg.AppID || !reflect.DeepEqual(got.Env, cfg.Env) ||
		got.TeamID != cfg.TeamID || got.WDABundleID != cfg.WDABundleID || got.DriverStartTimeout != cfg.DriverStartTimeout ||
		got.ConditionTimeout != cfg.ConditionTimeout || got.StepDelay != cfg.StepDelay || got.TypingFrequency != cfg.TypingFrequency ||
		!got.NoAppInstall || !got.NoDriverInstall || !got.NoFlutterFallback || !got.AndroidTCPForward {
		t.Fatalf("hub driver settings: %+v", got)
	}
}

func TestE2E_TestOnHub(t *testing.T) {
	e := newE2E(t)
	a, b, _, _ := startTwoHubs(t, e, "flow-test")
	target := "--target-hub=" + a.HTTPAddr
	env := []string{"MAESTRO_D_TOKEN=res-tok"}
	dev := "mock-3@hub-b"
	// Query through the entry hub until the second hub is visible.
	c := daemon.NewHTTPClient("http://"+a.HTTPAddr, "res-tok", nil)
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if infos, err := c.Devices(context.Background(), "mock"); err == nil && len(infos) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub-b did not appear")
		}
	}
	copyTree(t, filepath.Join("..", "..", "e2e", "testdata", "hub"), filepath.Join(e.cwd, "flow"))
	testArgs := []string{"test", "flow/main.yaml", target, "--device", dev, "--output", "out", "--flatten", "--artifacts", "always"}
	if r := e.runEnv(env, testArgs...); r.code != daemon.CodeLeaseRequired.ExitCode() || !strings.Contains(r.stderr, "LEASE_REQUIRED") {
		t.Fatalf("missing lease: %+v", r)
	}
	for _, option := range []string{"--record", "--video=on-failure", "--update-screenshots"} {
		r := e.runEnv(env, append(testArgs, option)...)
		if r.code != daemon.CodeUsage.ExitCode() || !strings.Contains(r.stderr, "unavailable with test --target-hub") {
			t.Fatalf("%s: %+v", option, r)
		}
	}
	for _, id := range []string{dev, "mock-1@hub-a"} {
		if r := e.runEnv(env, "lease", target, "--device", id, "--platform", "mock", "--no-attach"); r.code != 0 {
			t.Fatal(r.stderr)
		}
	}
	if r := e.runEnv(env, append(testArgs, "--caps", "absent.json")...); r.code != daemon.CodeUsage.ExitCode() || !strings.Contains(r.stderr, "--caps is unavailable") {
		t.Fatalf("local driver option: %+v", r)
	}
	if r := e.runEnv(env, append(testArgs, "--driver", "appium")...); r.code != daemon.CodeForbidden.ExitCode() || !strings.Contains(r.stderr, "FORBIDDEN") {
		t.Fatalf("explicit driver on first attach: %+v", r)
	}
	if r := e.runEnv(env, testArgs...); r.code != 0 || !strings.Contains(r.stdout, "✓ PASS") || !strings.Contains(r.stdout, "mock-3") {
		t.Fatalf("remote flow: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "flow", "done.png")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(b.RunDir, "daemon.log"))
	if err != nil || !strings.Contains(string(data), `Running flow bundle "main.yaml" on mock-3`) {
		t.Fatalf("owner log: %v\n%s", err, data)
	}
	// One worker per grant, one successful flow and one failed assertion.
	if err := os.WriteFile(filepath.Join(e.cwd, "fail.yaml"), []byte("appId: mock\n---\n- assertTrue: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.runEnv(env, "test", target, "--parallel", "2", "--device", dev+",mock-1@hub-a", "flow/main.yaml", "fail.yaml", "--output", "parallel", "--flatten", "--artifacts", "always")
	if r.code != 1 || !strings.Contains(r.stdout, "✗ FAIL") || !strings.Contains(r.stdout, "✓ PASS") {
		t.Fatalf("parallel flows: %+v", r)
	}
	raw, err := os.ReadFile(filepath.Join(e.cwd, "parallel", "report.json"))
	var index report.Index
	if err != nil || json.Unmarshal(raw, &index) != nil || len(index.Flows) != 2 || index.Flows[0].Device.ID != dev || index.Flows[1].Device.ID != "mock-1@hub-a" || index.Summary.Passed != 1 || index.Summary.Failed != 1 {
		t.Fatalf("parallel report: %v %s", err, raw)
	}
	for _, tc := range []struct {
		dir, summary string
		flows        int
	}{
		{"out", `tests="1" failures="0"`, 1},
		{"parallel", `tests="2" failures="1"`, 2},
	} {
		dir := filepath.Join(e.cwd, tc.dir)
		xml, err := os.ReadFile(filepath.Join(dir, "junit-report.xml"))
		if err != nil || !strings.Contains(string(xml), tc.summary) {
			t.Fatalf("JUnit report in %s: %v %s", dir, err, xml)
		}
		results, err := filepath.Glob(filepath.Join(dir, "allure-results", "*-result.json"))
		if err != nil || len(results) != tc.flows {
			t.Fatalf("Allure results in %s: %v %v", dir, results, err)
		}
	}
	var totalScreenshots, hierarchies int
	names := map[string]string{}
	for _, entry := range index.Flows {
		dir := filepath.Join(e.cwd, "parallel")
		data, err := os.ReadFile(filepath.Join(dir, "flows", entry.ID+".json"))
		var detail report.FlowDetail
		if err != nil || json.Unmarshal(data, &detail) != nil {
			t.Fatalf("flow %s: %v", entry.ID, err)
		}
		refs := map[string][]*string{}
		collectHubReportAssets(detail.Commands, refs)
		if len(refs) == 0 {
			t.Fatalf("no report artifacts for %s", entry.ID)
		}
		for ref := range refs {
			name := filepath.Base(ref)
			if !strings.HasPrefix(ref, "assets/"+entry.ID+"/") || (names[name] != "" && names[name] != ref) {
				t.Fatalf("colliding asset %q in flow %s", ref, entry.ID)
			}
			names[name] = ref
			content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref)))
			if err != nil || len(content) == 0 {
				t.Fatalf("missing report asset %s: %v", ref, err)
			}
			if strings.HasSuffix(ref, ".png") {
				allure, err := os.ReadFile(filepath.Join(dir, "allure-results", name))
				if err != nil || !bytes.Equal(content, allure) {
					t.Fatalf("missing Allure attachment %s: %v", name, err)
				}
				totalScreenshots++
			} else if strings.HasSuffix(ref, ".xml") {
				hierarchies++
			}
		}
	}
	if totalScreenshots == 0 || hierarchies == 0 {
		t.Fatalf("downloaded %d screenshots and %d hierarchies", totalScreenshots, hierarchies)
	}
}
