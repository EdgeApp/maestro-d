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
	"github.com/devicelab-dev/maestro-runner/pkg/executor"
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

func TestHubRecordingMustBelongToTheCurrentTest(t *testing.T) {
	result := &daemon.StepResult{Index: 2, TestReport: &executor.TestFlowReport{Recordings: []string{"assets/test-1/recordings/another-flow.mp4"}}}
	err := fetchHubReportAssets(t.Context(), nil, "mock", t.TempDir(), "flow-000", result)
	if err == nil || !strings.Contains(err.Error(), "unexpected hub recording path") {
		t.Fatalf("other flow's recording: %v", err)
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
		// The hub's attachment names the driver, as a local run's report does.
		raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
		var index report.Index
		if err != nil || json.Unmarshal(raw, &index) != nil || index.MaestroRunner.Driver != "mock" || index.MaestroRunner.Version == "" {
			t.Fatalf("runner metadata in %s: %v %s", dir, err, raw)
		}
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

	t.Run("top-level execution matches local test semantics", func(t *testing.T) {
		const hooks = `appId: ${APP_ID}
env:
  HOOK_VAR: from-header
onFlowStart:
  - assertTrue: ${HOOK_VAR == 'from-header' && APP_ID == 'expected'}
onFlowComplete:
  - runFlow:
      when:
        true: ${HOOK_VAR == 'from-header' && APP_ID == 'expected'}
      commands:
        - takeScreenshot: completed-hook
---
- assertTrue: ${APP_ID == 'expected'}
- assertTrue: true
`
		args := []string{"test", "paced.yaml", target, "--device", dev, "-e", "APP_ID=expected", "--output", "paced", "--flatten"}
		for _, tc := range []struct {
			header string
			flags  []string
		}{
			{"stepDelay: 150\n", []string{"--step-delay", "0"}},
			{"", []string{"--step-delay", "150"}},
		} {
			write(t, filepath.Join(e.cwd, "paced.yaml"), []byte(tc.header+hooks))
			if r := e.runEnv(env, append(args, tc.flags...)...); r.code != 0 {
				t.Fatal(r)
			}
			raw, err := os.ReadFile(filepath.Join(e.cwd, "paced", "report.json"))
			var index report.Index
			if err != nil || json.Unmarshal(raw, &index) != nil || len(index.Flows) != 1 || index.Flows[0].Duration == nil || *index.Flows[0].Duration < 150 {
				t.Fatalf("missing top-level delay: %s %v", raw, err)
			}
			if err := os.Remove(filepath.Join(e.cwd, "completed-hook.png")); err != nil {
				t.Fatalf("completion hook did not see the flow environment: %v", err)
			}
		}
		write(t, filepath.Join(e.cwd, "paced.yaml"), []byte("- assertTrue: false\n- takeScreenshot: unreachable-step\n"))
		if r := e.runEnv(env, args...); r.code != 1 {
			t.Fatal(r)
		}
		raw, err := os.ReadFile(filepath.Join(e.cwd, "paced", "report.json"))
		var index report.Index
		if err != nil || json.Unmarshal(raw, &index) != nil || len(index.Flows) != 1 {
			t.Fatalf("failed report: %s %v", raw, err)
		}
		counts := index.Flows[0].Commands
		if counts.Total != 2 || counts.Failed != 1 || counts.Skipped != 1 {
			t.Fatalf("failed and skipped commands: %+v", counts)
		}
		if _, err := os.Stat(filepath.Join(e.cwd, "unreachable-step.png")); !os.IsNotExist(err) {
			t.Fatalf("trailing step ran: %v", err)
		}
	})
	t.Run("explicit app installation on an attached lease", func(t *testing.T) {
		write(t, filepath.Join(e.cwd, "install.yaml"), []byte("- assertTrue: true\n"))
		args := []string{"test", "install.yaml", target, "--device", dev, "--platform", "mock", "--output", "install-report", "--flatten", "--app-file", "new-build.apk", "--condition-timeout", "21", "--wait-for-idle-timeout", "22", "--typing-frequency", "23"}
		if r := e.runEnv(env, args...); r.code == 0 || !strings.Contains(r.stderr, "new-build.apk") {
			t.Fatalf("missing app: %+v", r)
		}
		if r := e.runEnv(env, append(args, "--no-app-install")...); r.code != 0 {
			t.Fatal(r)
		}
		write(t, filepath.Join(e.cwd, "new-build.apk"), []byte("fixture for the mock installer"))
		if r := e.runEnv(env, args...); r.code != 0 {
			t.Fatal(r)
		}
		log, err := os.ReadFile(filepath.Join(b.RunDir, "daemon.log"))
		if err != nil || !strings.Contains(string(log), "Installing new-build.apk") {
			t.Fatalf("owner did not install the requested app: %v", err)
		}
	})
	t.Run("leaf counts latency and invocation capture policy", func(t *testing.T) {
		write(t, filepath.Join(e.cwd, "compound.yaml"), []byte(`- repeat:
    times: 3
    commands:
      - assertTrue: true
- assertTrue: false
- assertTrue: true
`))
		r := e.runEnv(env, "test", "compound.yaml", target, "--device", dev, "--output", "compound", "--flatten", "--artifacts", "never")
		if r.code != 1 || !strings.Contains(r.stdout, "3 steps passing") || !strings.Contains(r.stdout, "1 steps failing") || !strings.Contains(r.stdout, "1 steps skipped") {
			t.Fatalf("execution summary: %+v", r)
		}
		dir := filepath.Join(e.cwd, "compound")
		raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
		var index report.Index
		if err != nil || json.Unmarshal(raw, &index) != nil || len(index.Flows) != 1 || index.Flows[0].Commands.Total != 3 {
			t.Fatalf("top-level command index: %s %v", raw, err)
		}
		raw, err = os.ReadFile(filepath.Join(dir, index.Flows[0].DataFile))
		var detail report.FlowDetail
		if err != nil || json.Unmarshal(raw, &detail) != nil || detail.StepLatency == nil || *detail.StepLatency != report.ComputeStepLatency(detail.Commands) {
			t.Fatalf("command latency: %s %v", raw, err)
		}
		refs := map[string][]*string{}
		collectHubReportAssets(detail.Commands, refs)
		if len(refs) != 0 {
			t.Fatalf("unexpected report artifacts under --artifacts never: %v", refs)
		}
	})
	t.Run("recordings arrive in the client report", func(t *testing.T) {
		for _, tc := range []struct {
			last string
			code int
		}{{"true", 0}, {"false", 1}} {
			write(t, filepath.Join(e.cwd, "movies.yaml"), []byte(`- startRecording: test-movie
- assertTrue: true
- stopRecording
- startRecording: test-movie
- assertTrue: `+tc.last+"\n"))
			output := "movies-" + tc.last
			r := e.runEnv(env, "test", "movies.yaml", target, "--device", dev, "--output", output, "--flatten", "--step-delay", "150", "--artifacts", "never")
			if r.code != tc.code {
				t.Fatal(r)
			}
			for _, name := range []string{"test-movie.mp4", "test-movie-2.mp4"} {
				file := filepath.Join(e.cwd, output, "assets", "flow-000", "recordings", name)
				data, err := os.ReadFile(file)
				if err != nil || len(data) < 12 || string(data[4:8]) != "ftyp" {
					t.Fatalf("downloaded recording %s: %d bytes, %v", name, len(data), err)
				}
				t.Logf("%s/%s: %d MP4 bytes", output, name, len(data))
			}
		}
	})
	t.Run("suite cases retain inputs and retry independently", func(t *testing.T) {
		writeCase := func(name, source string) {
			t.Helper()
			write(t, filepath.Join(e.cwd, name), []byte(source))
		}
		writeCase("suite.yaml", `appId: suite.app
env:
  SUITE: inherited
  OVERRIDE: suite
---
- runFlow: failing.yaml
- runFlow:
    file: passing.yaml
    env:
      OVERRIDE: step
`)
		writeCase("failing.yaml", "name: First case\n---\n- assertTrue: false\n")
		writeCase("passing.yaml", `name: Second case
env:
  OVERRIDE: child
---
- launchApp
- assertTrue: ${SUITE == 'inherited' && OVERRIDE == 'step' && APP_ID == 'suite.app'}
`)
		args := []string{"test", "suite.yaml", target, "--device", dev, "--output", "suite-report", "--flatten"}
		check := func(failed int, names ...string) {
			t.Helper()
			dir := filepath.Join(e.cwd, "suite-report")
			raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
			var index report.Index
			if err != nil || json.Unmarshal(raw, &index) != nil || index.Summary.Failed != failed || len(index.Flows) != len(names) {
				t.Fatalf("suite report: %v %s", err, raw)
			}
			for i, name := range names {
				if index.Flows[i].Name != name {
					t.Fatalf("case %d: %+v", i, index.Flows[i])
				}
			}
			xml, err := os.ReadFile(filepath.Join(dir, "junit-report.xml"))
			if err != nil || bytes.Count(xml, []byte("<testcase ")) != len(names) {
				t.Fatalf("suite JUnit: %v %s", err, xml)
			}
		}
		if r := e.runEnv(env, args...); r.code != 1 {
			t.Fatal(r)
		}
		check(1, "First case", "Second case")
		writeCase("failing.yaml", "name: First case\n---\n- assertTrue: true\n")
		if r := e.runEnv(env, append(args, "--retry-failed")...); r.code != 0 {
			t.Fatal(r)
		}
		check(0, "First case")
	})
}
