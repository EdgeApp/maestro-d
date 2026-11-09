package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
)

func TestLeaseFile_RecordFindDropPrune(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	if loadLeases() != nil || findLease("h", "d") != nil {
		t.Fatal("empty home has no leases")
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	recordLease("http://a:7433", daemon.Lease{ID: "l1", Device: "d1", ExpiresAt: future, TTLSec: 600})
	recordLease("http://a:7433", daemon.Lease{ID: "l2", Device: "d2", ExpiresAt: future})
	recordLease("unix:hub", daemon.Lease{ID: "l3", Device: "d1", ExpiresAt: future})
	// Re-recording a device replaces its entry.
	recordLease("http://a:7433", daemon.Lease{ID: "l1b", Device: "d1", ExpiresAt: future})
	if r := findLease("http://a:7433", "d1"); r == nil || r.ID != "l1b" {
		t.Fatalf("find: %+v", r)
	}
	if len(leasesFor("http://a:7433")) != 2 || len(leasesFor("unix:hub")) != 1 {
		t.Fatalf("per hub: %+v", loadLeases())
	}
	st, err := os.Stat(leasesPath())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", st, err)
	}
	dropLease("http://a:7433", "l1b")
	if findLease("http://a:7433", "d1") != nil || findLease("unix:hub", "d1") == nil {
		t.Fatal("drop removes only that hub's entry")
	}
	// An entry whose recorded expiry is a week past is pruned on read.
	recordLease("http://a:7433", daemon.Lease{ID: "old", Device: "d9", ExpiresAt: time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)})
	if findLease("http://a:7433", "d9") != nil {
		t.Fatal("stale entry not pruned")
	}
	// An unreadable file is ignored.
	_ = os.WriteFile(leasesPath(), []byte("{"), 0o600)
	if loadLeases() != nil {
		t.Fatal("bad JSON")
	}
}

func TestHubStart_Helpers(t *testing.T) {
	o := &daemonOpts{Name: "hub", HTTPAddr: ":7433", TLSCert: "c.pem", TLSKey: "k.pem", PassEnv: []string{"A"},
		hub: hubOpts{share: []string{"x", "y"}, remoteShell: true}}
	args := strings.Join(hubChildArgs(o), " ")
	for _, want := range []string{"hub start --foreground --daemon hub --listen :7433", "--share-device x --share-device y", "--allow-remote-shell", "--pass-env A", "--tls-cert /"} {
		if !strings.Contains(args, want) {
			t.Errorf("child args %q lack %q", args, want)
		}
	}
	if strings.Contains(args, "token") {
		t.Fatalf("child args carry a token: %s", args)
	}
	got := redactArgs([]string{"hub", "start", "--token", "s3cret", "--token=s3cret", "--listen", ":1"})
	if strings.Contains(strings.Join(got, " "), "s3cret") {
		t.Fatalf("redact: %v", got)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	_ = os.WriteFile(f, []byte(" tok\n"), 0o600)
	o = &daemonOpts{hub: hubOpts{tokenFile: f}, explicit: map[string]bool{}}
	if tok, err := hubToken(o); err != nil || tok != "tok" {
		t.Fatalf("token file: %q %v", tok, err)
	}
	o.explicit["token"] = true
	if _, err := hubToken(o); err == nil {
		t.Fatal("--token and --token-file together")
	}
	o = &daemonOpts{hub: hubOpts{tokenFile: filepath.Join(dir, "missing")}}
	if _, err := hubToken(o); err == nil {
		t.Fatal("missing token file")
	}
	if _, err := hubToken(&daemonOpts{}); err == nil {
		t.Fatal("no token")
	}
	o = &daemonOpts{explicit: map[string]bool{}}
	hubName(o)
	if o.Name != DefaultHubDaemon {
		t.Fatal(o.Name)
	}
	if err := runHubStart(context.Background(), &daemonOpts{Token: "t", TLSCert: "c", explicit: map[string]bool{}}, nil); err == nil {
		t.Fatal("--tls-cert without --tls-key")
	}
}

func TestLease_Printing(t *testing.T) {
	if untilExpiry("nope") != "?" || untilExpiry(time.Now().Add(-time.Minute).Format(time.RFC3339)) != "expired" {
		t.Fatal("untilExpiry")
	}
	if !strings.HasSuffix(untilExpiry(time.Now().Add(90*time.Second).Format(time.RFC3339)), "s") {
		t.Fatal("time left")
	}
	if h := defaultHolder(); h == "" {
		t.Fatal("holder")
	}
	if (&daemonOpts{lease: leaseOpts{holder: "me"}}).holder() != "me" || daemon.HolderName("") != "another client" {
		t.Fatal("holder names")
	}
	var b bytes.Buffer
	printDevices(&b, []daemon.DeviceInfo{
		{ID: "a", Platform: "mock", Lease: &daemon.DeviceLease{Holder: "bob", ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339)}, Attached: true},
		{ID: "b", Platform: "mock", Available: true},
		{ID: "c", Platform: "mock"},
	})
	out := b.String()
	if !strings.Contains(out, "LEASE") || !strings.Contains(out, "bob (") || !strings.Contains(out, "available") {
		t.Fatalf("lease column:\n%s", out)
	}
	b.Reset()
	printDevices(&b, []daemon.DeviceInfo{{ID: "a", Platform: "mock"}})
	if strings.Contains(b.String(), "LEASE") {
		t.Fatal("no LEASE column off a hub")
	}
}

func TestMockDevices(t *testing.T) {
	t.Setenv(envMockDevices, "")
	if mockDevices() != nil || isMockDevice("mock-1") {
		t.Fatal("unset lists none")
	}
	if devs := daemonListDevices("mock"); len(devs) != 0 {
		t.Fatalf("mock listing: %+v", devs)
	}
	t.Setenv(envMockDevices, "mock-1, mock-2,")
	devs := daemonListDevices("mock")
	if len(devs) != 2 || devs[1].ID != "mock-2" || !devs[0].Ready || devs[0].Platform != "mock" {
		t.Fatalf("listed: %+v", devs)
	}
	if !isMockDevice("mock-2") || isMockDevice("mock-3") {
		t.Fatal("isMockDevice")
	}
	if rc := attachToRunConfig("mock-1", daemon.AttachConfig{}); rc.Platform != "mock" {
		t.Fatalf("a listed mock attaches as mock: %q", rc.Platform)
	}
	if devs := daemonListDevices("web"); devs != nil {
		t.Fatal("web")
	}
}

// leaseHub is an in-process hub with the mock driver, reached over TCP.
func leaseHub(t *testing.T) (*daemon.Server, string) {
	t.Helper()
	return leaseHubListing(t, nil)
}

// leaseHubListing is leaseHub listing mock-1 plus, while extra reports
// true, mock-7.
func leaseHubListing(t *testing.T, extra *atomic.Bool) (*daemon.Server, string) {
	t.Helper()
	t.Setenv(daemon.EnvHome, t.TempDir())
	deps := daemon.Deps{
		NewDriver: func(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
			return mock.New(mock.Config{Platform: "mock", DeviceID: id}), daemon.DriverInfo{DriverName: "mock", Platform: "mock"}, func() {}, nil
		},
		ListDevices: func(string) []daemon.DeviceInfo {
			devs := []daemon.DeviceInfo{{ID: "mock-1", Platform: "mock", Kind: "device", Ready: true}}
			if extra != nil && extra.Load() {
				devs = append(devs, daemon.DeviceInfo{ID: "mock-7", Platform: "mock", Kind: "device", Ready: true})
			}
			return devs
		},
	}
	s := daemon.New(daemon.Config{Name: "lh", Hub: true, HubName: "lh", Token: "t", HTTPAddr: ":7433", ReportsDir: t.TempDir()}, deps)
	hs := httptest.NewServer(s.TCPHandler())
	t.Cleanup(hs.Close)
	return s, hs.URL
}

// The CLI never takes a lease on its own: heldLease sends the one on
// file, drops one the hub plainly lost, and otherwise sends none, so the
// hub answers LEASE_REQUIRED naming `maestro-d lease`.
func TestHeldLease_UsesTheFileAndNeverTakesOne(t *testing.T) {
	var listed7 atomic.Bool
	_, url := leaseHubListing(t, &listed7)
	ctx := context.Background()
	o := &daemonOpts{Target: url, Token: "t", JSON: true}
	c := daemon.NewHTTPClient(url, "t", nil)
	wireLeases(c, o)
	if hub, err := isHub(ctx, c, o); err != nil || !hub {
		t.Fatalf("isHub: %v %v", hub, err)
	}
	// No lease on file: none is taken, and the hub refuses the device.
	id, info, err := heldLease(ctx, c, o, "mock-1")
	if err != nil || id != "mock-1" || info == nil || c.LeaseOf("mock-1") != "" {
		t.Fatalf("no lease: %q %+v %v", id, info, err)
	}
	if ls, _ := c.Leases(ctx); len(ls) != 0 {
		t.Fatalf("a lease was taken: %+v", ls)
	}
	_, err = c.Attach(ctx, "mock-1", daemon.AttachConfig{Platform: "mock"})
	var de *daemon.Error
	if !errors.As(err, &de) || de.Code != daemon.CodeLeaseRequired || !strings.Contains(de.Message, "maestro-d lease --device mock-1") ||
		!strings.Contains(de.Message, "md.lease()") {
		t.Fatalf("without a lease: %v", err)
	}
	// A lease on file is sent, and reused while the hub lists it.
	res, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1"})
	if err != nil {
		t.Fatal(err)
	}
	recordLease(leaseKey(c, o), res.Lease)
	if _, _, err := heldLease(ctx, c, o, "mock-1"); err != nil || c.LeaseOf("mock-1") != res.Lease.ID {
		t.Fatalf("on file: %v", err)
	}
	if _, err := c.Attach(ctx, "mock-1", daemon.AttachConfig{Platform: "mock"}); err != nil {
		t.Fatalf("with the lease: %v", err)
	}
	// Released behind our back: the lease on file is still sent, the hub
	// answers LEASE_EXPIRED, the entry is dropped, and no new lease is taken.
	if _, err := c.ReleaseLease(ctx, res.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := heldLease(ctx, c, o, "mock-1"); err != nil || c.LeaseOf("mock-1") != res.Lease.ID {
		t.Fatalf("stale: %v", err)
	}
	if _, err := c.Command(ctx, "mock-1", "tapOn", "x", true); !isCode(err, daemon.CodeLeaseExpired) {
		t.Fatalf("stale lease: %v", err)
	}
	if c.LeaseOf("mock-1") != "" || findLease(leaseKey(c, o), "mock-1") != nil {
		t.Fatal("the expired entry was kept")
	}
	if ls, _ := c.Leases(ctx); len(ls) != 0 {
		t.Fatalf("a lease was taken: %+v", ls)
	}
	// The hub leases only a device it lists.
	if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-7"}); !isCode(err, daemon.CodeDeviceNotFound) {
		t.Fatalf("unlisted lease: %v", err)
	}
	// A leased device that drops out of the listing (DEVICE_NOT_FOUND on
	// lookup) keeps its lease on file; a fresh client finds the hub from
	// the lease on file and sends it.
	listed7.Store(true)
	res, err = c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-7"})
	if err != nil {
		t.Fatal(err)
	}
	listed7.Store(false)
	recordLease(leaseKey(c, o), res.Lease)
	if id, info, err := heldLease(ctx, c, o, "mock-7"); err != nil || id != "mock-7" || info != nil || c.LeaseOf("mock-7") != res.Lease.ID {
		t.Fatalf("unlisted: %q %+v %v", id, info, err)
	}
	c2 := daemon.NewHTTPClient(url, "t", nil)
	wireLeases(c2, o)
	if hub, _ := isHub(ctx, c2, o); !hub || c2.LeaseOf("mock-7") == "" {
		t.Fatal("leases on file make a fresh client send them")
	}
	// The expiry hook drops the entry.
	c2.OnLeaseExpired("mock-7", c2.LeaseOf("mock-7"))
	if findLease(leaseKey(c2, o), "mock-7") != nil {
		t.Fatal("hook did not drop the entry")
	}
	if isCode(nil, daemon.CodeUsage) || !isCode(daemon.Errorf(daemon.CodeUsage, "x"), daemon.CodeUsage) {
		t.Fatal("isCode")
	}
}

// A blob upload starts with a HEAD, whose refusal has no body: the hub's
// X-Maestro-Error-Code header still makes it LEASE_REQUIRED or
// LEASE_EXPIRED (not BUSY, the bare 409's code), and an expired lease is
// dropped from leases.json.
func TestUploadBlob_LeaseRefusalsOnTheBodylessHEAD(t *testing.T) {
	_, url := leaseHub(t)
	ctx := context.Background()
	o := &daemonOpts{Target: url, Token: "t", JSON: true}
	c := daemon.NewHTTPClient(url, "t", nil)
	wireLeases(c, o)
	f := filepath.Join(t.TempDir(), "b")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	hex := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if _, err := c.UploadBlob(ctx, "mock-1", hex, f, 5); !isCode(err, daemon.CodeLeaseRequired) {
		t.Fatalf("without a lease: %v", err)
	}
	res, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1"})
	if err != nil {
		t.Fatal(err)
	}
	recordLease(leaseKey(c, o), res.Lease)
	if _, _, err := heldLease(ctx, c, o, "mock-1"); err != nil || c.LeaseOf("mock-1") != res.Lease.ID {
		t.Fatalf("on file: %v", err)
	}
	if _, err := c.ReleaseLease(ctx, res.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadBlob(ctx, "mock-1", hex, f, 5); !isCode(err, daemon.CodeLeaseExpired) {
		t.Fatalf("expired lease: %v", err)
	}
	if c.LeaseOf("mock-1") != "" || findLease(leaseKey(c, o), "mock-1") != nil {
		t.Fatal("the expired entry was kept")
	}
}

func TestLeaseDurations(t *testing.T) {
	for in, want := range map[string]time.Duration{"600": 10 * time.Minute, "8h": 8 * time.Hour, " 90m ": 90 * time.Minute, "1h30m": 90 * time.Minute, "1.5s": 2 * time.Second} {
		if got, err := parseLeaseDuration("ttl", in); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-5", "soon", "500ms"} {
		if _, err := parseLeaseDuration("ttl", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	hubMax, devs, err := parseLeaseMax([]string{"2h", "emulator-5554=10m", "Pixel 8=1800", "4h"})
	if err != nil || hubMax != 4*time.Hour || len(devs) != 2 || devs[0] != (daemon.LeaseMax{Device: "emulator-5554", Max: 10 * time.Minute}) ||
		devs[1] != (daemon.LeaseMax{Device: "Pixel 8", Max: 30 * time.Minute}) {
		t.Fatalf("lease-max: %v %+v %v", hubMax, devs, err)
	}
	for _, bad := range []string{"=10m", "x=", "x=never", "never"} {
		if _, _, err := parseLeaseMax([]string{bad}); err == nil {
			t.Errorf("--lease-max %q accepted", bad)
		}
	}
	o := &daemonOpts{}
	if err := ttlFlag("").apply(o, "2h"); err != nil || o.lease.ttl != 7200 {
		t.Fatalf("--ttl 2h: %d %v", o.lease.ttl, err)
	}
	if err := ttlFlag("").apply(o, "x"); err == nil {
		t.Fatal("--ttl x")
	}
	if secs(0) != "?" || secs(90) != "1m30s" {
		t.Fatal("secs")
	}
	args := strings.Join(hubChildArgs(&daemonOpts{hub: hubOpts{leaseMax: []string{"1h", "a=5m"}}}), " ")
	if !strings.Contains(args, "--lease-max 1h --lease-max a=5m") {
		t.Fatalf("child args: %s", args)
	}
	if err := runHubStart(context.Background(), &daemonOpts{Token: "t", hub: hubOpts{leaseMax: []string{"x=y"}}, explicit: map[string]bool{}}, nil); err == nil {
		t.Fatal("a bad --lease-max starts a hub")
	}
}

// leaseHelperEnv makes the test binary a helper process that records
// leases (TestLeaseFile_ConcurrentProcesses).
const leaseHelperEnv = "MAESTRO_D_TEST_LEASE_WRITER"

// Two processes sharing one home record leases at once: the advisory lock
// on leases.json.lock keeps every record.
func TestLeaseFile_ConcurrentProcesses(t *testing.T) {
	if who := os.Getenv(leaseHelperEnv); who != "" {
		future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		for i := 0; i < 40; i++ {
			recordLease("http://hub:7433", daemon.Lease{ID: fmt.Sprintf("%s-%d", who, i), Device: fmt.Sprintf("%s-dev%d", who, i), ExpiresAt: future})
		}
		return
	}
	home := t.TempDir()
	t.Setenv(daemon.EnvHome, home)
	var cmds []*exec.Cmd
	for _, who := range []string{"a", "b", "c"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseFile_ConcurrentProcesses$")
		cmd.Env = append(os.Environ(), leaseHelperEnv+"="+who, daemon.EnvHome+"="+home)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(leasesFor("http://hub:7433")); got != 120 {
		t.Fatalf("records kept: %d of 120", got)
	}
}
