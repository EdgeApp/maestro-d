package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func splitInfo(t *testing.T, r result) daemon.Info {
	t.Helper()
	var out struct{ Daemon daemon.Info }
	if err := json.Unmarshal([]byte(r.stdout), &out); r.code != 0 || err != nil || out.Daemon.PID == 0 {
		t.Fatalf("missing daemon record: %v\n%s%s", err, r.stdout, r.stderr)
	}
	return out.Daemon
}

func TestE2E_ClientStandalone(t *testing.T) {
	e := newE2E(t)
	if err := os.Remove(filepath.Join(filepath.Dir(e.bin), "maestro-d-hub")); err != nil {
		t.Fatal(err)
	}
	mocks := []string{"MAESTRO_D_MOCK_DEVICES=mock-1"}
	if r := e.okEnv(mocks, "devices", "--json"); !strings.Contains(r.stdout, "mock-1") {
		t.Fatal(r.stdout)
	}
	e.okEnv(mocks, "tapOn", "x", "--device", "mock-1")
	fixture := filepath.Join(e.cwd, "flow")
	copyTree(t, "../../e2e/testdata/hub", fixture)
	flow := filepath.Join(fixture, "main.yaml")
	e.okEnv(mocks, "--platform", "mock", "--device", "mock-1", "test", flow)
	if r := e.ok("ps", "--json"); !strings.Contains(r.stdout, `"serve"`) {
		t.Fatal(r.stdout)
	}
	if r := e.runEnv([]string{daemon.EnvHubBin + "=" + filepath.Join(e.cwd, "absent")}, "hub", "status"); r.code != 2 || !strings.Contains(r.stderr, "install maestro-d-hub") {
		t.Fatalf("missing alias target: %s", r.stderr)
	}
}

func TestE2E_ServeLoopbackOnly(t *testing.T) {
	e := newE2E(t)
	mocks := []string{"MAESTRO_D_MOCK_DEVICES=mock-1"}
	info := splitInfo(t, e.okEnv(mocks, "start", "--daemon", "lb", "--http", "127.0.0.1:0", "--token", "t", "--json"))
	c := daemon.NewHTTPClient("http://"+info.HTTPAddr, "t", nil)
	ctx := context.Background()
	if devs, err := c.Devices(ctx, "mock"); err != nil || len(devs) != 1 {
		t.Fatalf("devices: %v %v", devs, err)
	}
	if _, err := c.Attach(ctx, "mock-1", daemon.AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Command(ctx, "mock-1", "tapOn", "x", true); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"serve", "--http", "0.0.0.0:0"}, {"serve", "--http", "192.168.1.20:0"}, {"serve", "--http", "lab.example:0"}, {"start", "--http", ":7433"}, {"serve", "--tls-cert", "c", "--tls-key", "k"}, {"start", "--tls-key", "k"}, {"serve", "--pass-env", "X"}} {
		r := e.fails(2, args...)
		if !strings.Contains(r.stderr, "maestro-d-hub start") || !strings.Contains(r.stderr, "USAGE") {
			t.Fatal(r.stderr)
		}
	}
}

func TestE2E_HubAlias(t *testing.T) {
	e := newE2E(t)
	mockEnv := func(home string) []string {
		return []string{daemon.EnvHome + "=" + home, "MAESTRO_D_MOCK_DEVICES=mock-1"}
	}
	home := filepath.Join(e.cwd, strings.Repeat("long", 22), "first")
	env := mockEnv(home)
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "config.json")
	const configText = `{"hubDevices":[{"id":"mock-1"}]}`
	if err := os.WriteFile(config, []byte(configText), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"start", "--name", "lab", "--listen", "127.0.0.1:0", "--no-mdns", "--token", "t", "--peer-token", "p", "--json"}
	start := func(env []string, bin string) daemon.Info {
		r := e.okEnv(env, append([]string{bin}, args...)...)
		if bin == "hub" && strings.Count(r.stderr, hubAliasNotice) != 1 {
			t.Fatal(r.stderr)
		}
		t.Cleanup(func() { e.runEnv(env, "maestro-d-hub", "stop") })
		return splitInfo(t, r)
	}
	info := start(env, "hub")
	if cmd, err := exec.Command("ps", "-p", fmt.Sprint(info.PID), "-o", "command=").Output(); err != nil || !strings.Contains(string(cmd), "maestro-d-hub start --foreground") {
		t.Fatalf("process: %s %v", cmd, err)
	} else {
		t.Logf("pid %d: %s", info.PID, cmd)
	}
	r := e.runEnv(env, "maestro-d-hub", "start")
	if r.code != 2 || !strings.Contains(r.stderr, "HUB_RUNNING") {
		t.Fatal(r)
	}
	if r := e.runEnv(env, "hub", "status", "--daemon", "hub"); r.code != 2 || !strings.Contains(r.stderr, "USAGE") {
		t.Fatal(r)
	}
	alias := e.okEnv(env, "hub", "status")
	direct := e.okEnv(env, "maestro-d-hub", "status")
	if alias.stdout != direct.stdout || strings.Count(alias.stderr, hubAliasNotice) != 1 {
		t.Fatal(alias, direct)
	}
	other := mockEnv(filepath.Join(filepath.Dir(home), "second"))
	otherInfo := start(other, "maestro-d-hub")
	if info.SocketPath == otherInfo.SocketPath || filepath.Dir(info.SocketPath) == info.RunDir {
		t.Fatal("long-home sockets", info, otherInfo)
	}
	if stop := e.okEnv(env, "hub", "stop"); strings.Count(stop.stderr, hubAliasNotice) != 1 {
		t.Fatal(stop)
	}
	e.okEnv(other, "maestro-d-hub", "status")
	if err := syscall.Kill(otherInfo.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !daemon.WaitForExit(&otherInfo, 5*time.Second) {
		t.Fatal("killed hub still live")
	}
	start(other, "maestro-d-hub")
	e.okEnv(other, "maestro-d-hub", "stop")

	t.Run("simultaneous detached starts", func(t *testing.T) {
		for round := 0; round < 3; round++ {
			raceEnv := mockEnv(filepath.Join(e.cwd, fmt.Sprintf("concurrent-%d", round)))
			ready := make(chan struct{})
			results := make(chan result, 2)
			for range 2 {
				go func() { <-ready; results <- e.runEnv(raceEnv, append([]string{"maestro-d-hub"}, args...)...) }()
			}
			close(ready)
			one, two := <-results, <-results
			t.Cleanup(func() { e.runEnv(raceEnv, "maestro-d-hub", "stop") })
			if one.code != 0 {
				one, two = two, one
			}
			if one.code != 0 || two.code != 2 || !strings.Contains(two.stderr, "HUB_RUNNING") {
				t.Fatalf("simultaneous starts: %v / %v", one, two)
			}
			winning := splitInfo(t, one)
			st, err := daemon.NewUnixClient(winning.SocketPath).Status(context.Background())
			if err != nil || st.Daemon.PID != winning.PID {
				t.Fatalf("winning process %d: %v %v", winning.PID, st, err)
			}
			e.okEnv(raceEnv, "maestro-d-hub", "stop")
		}
	})
	t.Run("ownership and live reports", func(t *testing.T) {
		plain := splitInfo(t, e.okEnv(env, "start", "--daemon", "hub", "--json"))
		t.Cleanup(func() { e.runEnv(env, "stop", "--daemon", "hub") })
		e.okEnv(env, "attach", "--daemon", "hub", "--device", "mock-1", "--platform", "mock")
		info = start(env, "maestro-d-hub")
		c := daemon.NewHTTPClient("http://"+info.HTTPAddr, "t", nil)
		ctx := context.Background()
		rows, err := c.Devices(ctx, "mock")
		if err != nil || len(rows) != 1 || rows[0].Available || rows[0].Owner == "" {
			t.Fatalf("owned device: %+v %v", rows, err)
		}
		_, err = c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1"})
		if !isCode(err, daemon.CodeDeviceInUse) || !strings.Contains(err.Error(), fmt.Sprint(plain.PID)) {
			t.Fatalf("lease: %v", err)
		}
		if _, err = c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock"}}); err == nil {
			t.Fatal("selector leased owned device")
		}
		e.okEnv(env, "detach", "--daemon", "hub", "--device", "mock-1")
		grant, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1", Attach: &daemon.AttachConfig{Platform: "mock"}})
		if err != nil {
			t.Fatal(err)
		}
		c.SetLease("mock-1", grant.Lease.ID)
		blocked := e.runEnv(env, "attach", "--daemon", "hub", "--device", "mock-1", "--platform", "mock")
		if blocked.code != 5 || !strings.Contains(blocked.stderr, "DEVICE_IN_USE") || !strings.Contains(blocked.stderr, fmt.Sprint(info.PID)) {
			t.Fatal(blocked)
		}
		local := daemon.NewUnixClient(info.SocketPath)
		st, err := local.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var live string
		for _, d := range st.Devices {
			if d.Attached {
				live = d.ReportDir
			}
		}
		if live == "" {
			t.Fatalf("missing live report: %+v", st)
		}
		idle := filepath.Join(filepath.Dir(live), "unused")
		if err = os.MkdirAll(idle, 0700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-8 * 24 * time.Hour)
		for _, root := range []string{live, idle} {
			if err = filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				return os.Chtimes(p, old, old)
			}); err != nil {
				t.Fatal(err)
			}
		}
		state := filepath.Join(home, "hub", "leases.json")
		if err = os.WriteFile(state, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"gc", "--dry-run"}, {"gc"}} {
			r := e.okEnv(env, args...)
			if !strings.Contains(r.stdout, idle) || strings.Contains(r.stdout, live) {
				t.Fatal(r.stdout)
			}
		}
		if _, err = os.Stat(idle); !os.IsNotExist(err) {
			t.Fatal("idle report retained")
		}
		if _, err = os.Stat(live); err != nil {
			t.Fatal(err)
		}
		for p, want := range map[string]string{config: configText, state: "{}"} {
			if data, err := os.ReadFile(p); err != nil || string(data) != want {
				t.Errorf("state changed: %s", p)
			}
		}
		if _, err = c.Command(ctx, "mock-1", "tapOn", "holder", true); err != nil {
			t.Fatal(err)
		}
		if _, err = c.ReleaseLease(ctx, grant.Lease.ID); err != nil {
			t.Fatal(err)
		}
		e.okEnv(env, "attach", "--daemon", "hub", "--device", "mock-1", "--platform", "mock")
		e.okEnv(env, "maestro-d-hub", "stop")
	})
	t.Run("upgrade preserves 3.x daemon", func(t *testing.T) {
		oldBin := os.Getenv("MAESTRO_D_TEST_BASE_BIN")
		if oldBin == "" {
			t.Skip("set MAESTRO_D_TEST_BASE_BIN to the 3.x client for the upgrade fixture")
		}
		base := *e
		base.bin = oldBin
		env := mockEnv(filepath.Join(e.cwd, "upgrade"))
		old := splitInfo(t, base.okEnv(env, "start", "--daemon", "hub", "--json"))
		t.Cleanup(func() { base.runEnv(env, "stop", "--daemon", "hub") })
		check := func() {
			r := e.runEnv(env, "maestro-d-hub", "stop")
			if r.code != 3 || !strings.Contains(r.stderr, "not running") {
				t.Fatal(r)
			}
			if !daemon.PIDAlive(old.PID) {
				t.Fatal("3.x daemon no longer running")
			}
			client := daemon.NewUnixClient(old.SocketPath)
			st, err := client.Status(context.Background())
			if err != nil || st.Daemon.PID != old.PID || st.Daemon.SocketPath != old.SocketPath {
				t.Fatalf("3.x record: %v %v", st, err)
			}
			if d, err := client.Devices(context.Background(), "mock"); err != nil || len(d) != 1 {
				t.Fatal(d, err)
			}
		}
		check()
		start(env, "maestro-d-hub")
		e.okEnv(env, "maestro-d-hub", "stop")
		check()
	})
}
