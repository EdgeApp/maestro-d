package cli

import (
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
	testArgs := []string{"test", "flow/main.yaml", target, "--device", dev, "--output", "out", "--flatten"}
	if r := e.runEnv(env, testArgs...); r.code != daemon.CodeLeaseRequired.ExitCode() || !strings.Contains(r.stderr, "LEASE_REQUIRED") {
		t.Fatalf("missing lease: %+v", r)
	}
	for _, id := range []string{dev, "mock-1@hub-a"} {
		if r := e.runEnv(env, "lease", target, "--device", id, "--platform", "mock"); r.code != 0 {
			t.Fatal(r.stderr)
		}
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
	r := e.runEnv(env, "test", target, "--parallel", "2", "--device", dev+",mock-1@hub-a", "flow/main.yaml", "fail.yaml", "--output", "parallel", "--flatten")
	if r.code != 1 || !strings.Contains(r.stdout, "✗ FAIL") || !strings.Contains(r.stdout, "✓ PASS") {
		t.Fatalf("parallel flows: %+v", r)
	}
	raw, err := os.ReadFile(filepath.Join(e.cwd, "parallel", "report.json"))
	var index report.Index
	if err != nil || json.Unmarshal(raw, &index) != nil || len(index.Flows) != 2 || index.Flows[0].Device.ID != dev || index.Flows[1].Device.ID != "mock-1@hub-a" || index.Summary.Passed != 1 || index.Summary.Failed != 1 {
		t.Fatalf("parallel report: %v %s", err, raw)
	}
}
