package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestE2E_HubDevicesCLI(t *testing.T) {
	e := newE2E(t)
	env := []string{"MAESTRO_D_MOCK_DEVICES=mock-1,mock-2,mock-3,mock-spare"}
	const sim = "00000000-0000-4000-A000-000000000001"
	victim := "mock-1"
	bootLog := filepath.Join(e.cwd, "simulator-events")
	if runtime.GOOS == "darwin" {
		victim = sim
		// Substitute simctl discovery and power operations across hub and CLI processes.
		bin := filepath.Join(e.cwd, "sim-bin")
		if err := os.Mkdir(bin, 0700); err != nil {
			t.Fatal(err)
		}
		script := `#!/bin/sh
log="$MAESTRO_D_HOME/../simulator-events"
case "$1 $2 $3" in
  "simctl list devices") state=Shutdown; [ "$(tail -1 "$log" 2>/dev/null)" = boot ] && state=Booted
    echo '{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-18-0":[{"name":"G4 simulator","udid":"` + sim + `","isAvailable":true,"state":"'$state'"}]}}' ;;
  "simctl boot ` + sim + `") echo boot >> "$log" ;;
  "simctl shutdown ` + sim + `") echo shutdown >> "$log" ;;
  *) exit 1 ;;
esac
`
		for name, body := range map[string]string{"xcrun": script, "open": "#!/bin/sh\nexit 0\n"} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
		}
		env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	writeSharedMocks(t, e.home, victim, "mock-spare")
	activePath := filepath.Join(e.home, "config.json")
	start := func() daemon.Info {
		t.Helper()
		r := e.mustOK(env, "maestro-d-hub", "start", "--name", "devices-cli", "--listen", "127.0.0.1:0", "--no-mdns", "--token", "devices", "--peer-token", "peers", "--json")
		var out struct{ Daemon daemon.Info }
		if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out.Daemon.SocketPath == "" {
			t.Fatalf("hub startup: %s", r.stdout)
		}
		return out.Daemon
	}
	t.Cleanup(func() { e.runEnv(env, "maestro-d-hub", "stop") })
	info := start()
	cfg := func(args ...string) result {
		t.Helper()
		return e.runEnv(env, append([]string{"maestro-d-hub", "devices"}, args...)...)
	}
	ok := func(args ...string) result {
		t.Helper()
		r := cfg(args...)
		if r.code != 0 {
			t.Fatalf("devices %v: %s%s", args, r.stdout, r.stderr)
		}
		return r
	}
	rows := func() map[string]string {
		t.Helper()
		r := ok("--json")
		var out struct {
			Path    string
			Devices []struct{ ID, Description string }
		}
		if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out.Path != activePath {
			t.Fatalf("list --json: %s", r.stdout)
		}
		result := map[string]string{}
		for _, d := range out.Devices {
			result[d.ID] = d.Description
		}
		return result
	}
	ok("add", "mock-2", "--description", "rack 2", "--config", activePath)
	if rows()["mock-2"] != "rack 2" {
		t.Fatal("missing new device description")
	}
	alias := filepath.Join(e.cwd, "config-link.json")
	if err := os.Symlink(activePath, alias); err == nil {
		ok("set", "mock-2", "--description", "via symlink", "--config", alias)
		if rows()["mock-2"] != "via symlink" {
			t.Fatal("edit through config alias missed the active file")
		}
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	ok("set", "mock-2", "--description", "x")
	if rows()["mock-2"] != "x" {
		t.Fatal("description was not updated")
	}

	otherHome := filepath.Join(e.cwd, "other")
	writeSharedMocks(t, otherHome, "private")
	otherPath := filepath.Join(otherHome, "config.json")
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	before, other := read(activePath), read(otherPath)
	bad := cfg("remove", "mock-2", "--config", otherPath)
	if bad.code != 2 || !strings.Contains(bad.stderr, activePath) || !strings.Contains(bad.stderr, otherPath) || !bytes.Equal(before, read(activePath)) || !bytes.Equal(other, read(otherPath)) {
		t.Fatalf("config path mismatch: %s%s", bad.stdout, bad.stderr)
	}
	ok("remove", "mock-2")
	if _, found := rows()["mock-2"]; found {
		t.Fatal("removed device still listed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := daemon.NewHTTPClient(info.HTTPAddr, "devices", nil)
	reserve := func(id string) *daemon.LeaseResult {
		t.Helper()
		now := time.Now().UTC()
		res, err := client.Reserve(ctx, daemon.ReservationRequest{Device: id + "@devices-cli", Holder: "reserved-" + id, StartAt: now.Add(time.Hour).Format(time.RFC3339), ClientTime: now.Format(time.RFC3339), TTLSec: 300})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	req := daemon.LeaseRequest{Device: victim + "@devices-cli", TTLSec: 300, Holder: "boot-holder"}
	if runtime.GOOS == "darwin" {
		req.Device = ""
		req.Select = &daemon.LeaseSelector{Name: "G4 simulator", Boot: true}
	}
	active, err := client.AcquireLease(ctx, req)
	if err != nil || active.Lease.Booted != (runtime.GOOS == "darwin") {
		t.Fatalf("initial grant: %+v %v", active, err)
	}
	reserve(victim)
	spare := reserve("mock-spare")
	e.mustOK(env, "maestro-d-hub", "stop")
	before = read(activePath)
	bad = cfg("remove", victim)
	if bad.code != 5 || !strings.Contains(bad.stderr, "reserved-"+victim) || !strings.Contains(bad.stderr, "boot-holder") || !bytes.Equal(before, read(activePath)) {
		t.Fatalf("offline removal with a reservation: %s%s", bad.stdout, bad.stderr)
	}
	if runtime.GOOS == "darwin" && string(read(bootLog)) != "boot\n" {
		t.Fatalf("hub stopped its leased simulator: %s", read(bootLog))
	}
	ok("remove", victim, "--end-leases")
	if runtime.GOOS == "darwin" && string(read(bootLog)) != "boot\nshutdown\n" {
		t.Fatalf("offline boot cleanup: %s", read(bootLog))
	}
	if runtime.GOOS == "darwin" {
		t.Logf("simctl power operations across ordinary stop and forced offline removal: %q", string(read(bootLog)))
	}
	t.Logf("offline devices remove refused with both windows: %s; --end-leases removed grant %s and its reservation", bad.stderr, active.Lease.ID)
	r := ok("add", "mock-3")
	if !strings.Contains(r.stdout+r.stderr, "hub not running; edited "+activePath) {
		t.Fatalf("offline edit: %s%s", r.stdout, r.stderr)
	}
	info = start()
	client = daemon.NewHTTPClient(info.HTTPAddr, "devices", nil)
	devices, err := client.Devices(ctx, "mock")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range devices {
		ids = append(ids, d.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"mock-3@devices-cli", "mock-spare@devices-cli"}) {
		t.Fatalf("devices after restart: %v", ids)
	}
	grants, err := client.Leases(ctx)
	if err != nil || len(grants) != 1 || grants[0].ID != spare.Lease.ID {
		t.Fatalf("reservations after restart: %+v %v", grants, err)
	}
	t.Logf("After restart, the peer-facing list contains %v and only the unrelated reservation remains.", ids)
}
