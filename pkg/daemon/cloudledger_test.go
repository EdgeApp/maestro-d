package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// envLedgerHelper makes the test binary a ledger writer for
// TestCloudLedger_ProcessesSerialise.
const envLedgerHelper = "MAESTRO_D_TEST_LEDGER_WRITER"

func TestCloudLedger_HelperProcess(t *testing.T) {
	prefix := os.Getenv(envLedgerHelper)
	if prefix == "" {
		t.Skip("helper process only")
	}
	for i := 0; i < 20; i++ {
		ref := fmt.Sprintf("%s-%02d", prefix, i)
		if err := addLedgerEntry(CloudEntry{Daemon: prefix, PID: os.Getpid(), ID: ref, Provider: "devicefarm", ProviderRef: ref, StartedAt: ref}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloudLedger_AddRemoveAndSurvivesRunFileCleanup(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	if entries, err := ReadLedger(); err != nil || entries != nil {
		t.Fatalf("empty: %v %v", entries, err)
	}
	e := CloudEntry{Daemon: "d1", PID: 1, ID: "df-1", Provider: "devicefarm", ProviderRef: "arn:1", StartedAt: "2026-11-20T00:00:00Z"}
	if err := addLedgerEntry(e); err != nil {
		t.Fatal(err)
	}
	// Re-adding the same ref replaces it.
	e.ID = "df-1b"
	if err := addLedgerEntry(e); err != nil {
		t.Fatal(err)
	}
	// Run-file cleanup of that daemon leaves the ledger alone.
	_ = os.MkdirAll(RunDir("d1"), 0o700)
	RemoveRunFiles("d1")
	if info, _ := LiveInfo("d1"); info != nil {
		t.Fatal("d1 alive")
	}
	entries, _ := ReadLedger()
	if len(entries) != 1 || entries[0].ID != "df-1b" {
		t.Fatalf("after cleanup: %+v", entries)
	}
	if !deadEntry(entries[0]) {
		t.Fatal("an entry of a daemon that is not running is not dead")
	}
	if err := removeLedgerEntry("arn:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LedgerPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an empty ledger is not removed: %v", err)
	}
	if err := removeLedgerEntry("arn:none"); err != nil {
		t.Fatal(err)
	}
	// A corrupt ledger is an error, not silently empty.
	_ = os.WriteFile(LedgerPath(), []byte("{"), 0o600)
	if _, err := ReadLedger(); err == nil {
		t.Fatal("a corrupt ledger read")
	}
	if err := addLedgerEntry(e); err == nil {
		t.Fatal("a corrupt ledger written over")
	}
	_ = os.WriteFile(LedgerPath(), nil, 0o600)
	if entries, err := ReadLedger(); err != nil || entries != nil {
		t.Fatalf("empty file: %v %v", entries, err)
	}
}

func TestCloudLedger_GoroutinesSerialise(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := "arn:" + strconv.Itoa(i)
			if err := addLedgerEntry(CloudEntry{Daemon: "d", PID: 1, ID: ref, ProviderRef: ref, StartedAt: ref}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if entries, _ := ReadLedger(); len(entries) != 20 {
		t.Fatalf("%d of 20 entries landed", len(entries))
	}
}

// Where the file lock guards nothing (Windows), one daemon's concurrent
// writes still keep each other's entries.
func TestCloudLedger_GoroutinesSerialiseWithoutAFileLock(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	saved := ledgerLock
	ledgerLock = func(path string, _ time.Duration) (*os.File, error) {
		return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	}
	defer func() { ledgerLock = saved }()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := "arn:" + strconv.Itoa(i)
			if err := addLedgerEntry(CloudEntry{Daemon: "d", PID: 1, ID: ref, ProviderRef: ref, StartedAt: ref}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if entries, _ := ReadLedger(); len(entries) != 40 {
		t.Fatalf("%d of 40 entries landed", len(entries))
	}
}

func TestCloudLedger_ProcessesSerialise(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	var wg sync.WaitGroup
	for _, p := range []string{"a", "b"} {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCloudLedger_HelperProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), envLedgerHelper+"="+p, EnvHome+"="+home)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("writer %s: %v\n%s", p, err, out)
			}
		}(p)
	}
	wg.Wait()
	entries, _ := ReadLedger()
	if len(entries) != 40 {
		t.Fatalf("%d of 40 entries landed", len(entries))
	}
}

func TestCloudLedger_WritesWhileASweepHoldsItsLock(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	_ = os.MkdirAll(Home(), 0o700)
	f, err := lockRunDir(filepath.Join(Home(), "housekeeping.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlockRunDir(f)
	if err := addLedgerEntry(CloudEntry{Daemon: "d", ID: "df-1", ProviderRef: "arn:1"}); err != nil {
		t.Fatalf("a ledger write during a sweep: %v", err)
	}
}

func TestStopOrphans_CloudPass(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	// A dead daemon left a mock device it booted and a cloud device, and
	// the ledger holds the cloud session.
	dead := "deadd"
	_ = os.MkdirAll(RunDir(dead), 0o700)
	if err := WriteInfo(&Info{Name: dead, PID: 999999}); err != nil {
		t.Fatal(err)
	}
	devs := []DeviceInfo{
		{ID: "mock-9", Platform: "mock", BootedBy: BootedByDaemon},
		{ID: "df-1", Platform: "android", Kind: "cloud", BootedBy: BootedByDaemon, Provider: "devicefarm", ProviderRef: "arn:1"},
	}
	if err := atomicWriteJSON(DevicesPath(dead), devs); err != nil {
		t.Fatal(err)
	}
	for _, e := range []CloudEntry{
		{Daemon: dead, PID: 999999, ID: "df-1", Provider: "devicefarm", ProviderRef: "arn:1"},
		{Daemon: "live", PID: os.Getpid(), ID: "df-live", Provider: "devicefarm", ProviderRef: "arn:live"},
	} {
		if err := addLedgerEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	// This process stands in for the live daemon "live".
	_ = os.MkdirAll(RunDir("live"), 0o700)
	_ = WriteInfo(&Info{Name: "live", PID: os.Getpid()})

	// Without credentials the session is reported and kept.
	res, err := StopOrphans(context.Background(), OrphanOptions{})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(res.Stopped) != 1 || res.Stopped[0] != "mock-9" || len(res.Unreleased) != 1 || res.Unreleased[0].ProviderRef != "arn:1" {
		t.Fatalf("without credentials: %+v", res)
	}
	if entries, _ := ReadLedger(); len(entries) != 2 {
		t.Fatalf("ledger: %+v", entries)
	}
	// A release that fails is kept too.
	res, _ = StopOrphans(context.Background(), OrphanOptions{ReleaseCloud: func(context.Context, CloudEntry) (*cloudprov.DeviceMinutes, error) { return nil, errors.New("denied") }})
	if len(res.Unreleased) != 1 || res.Unreleased[0].Error != "denied" {
		t.Fatalf("failed release: %+v", res)
	}
	// With credentials it is released and dropped; the live daemon's stays.
	var asked []string
	res, err = StopOrphans(context.Background(), OrphanOptions{ReleaseCloud: func(_ context.Context, e CloudEntry) (*cloudprov.DeviceMinutes, error) {
		asked = append(asked, e.ProviderRef)
		return &cloudprov.DeviceMinutes{Metered: 3, Total: 3, Estimated: true}, nil
	}})
	if err != nil || len(res.Released) != 1 || res.Released[0].ID != "df-1" || res.Released[0].DeviceMinutes.Total != 3 || strings.Join(asked, ",") != "arn:1" {
		t.Fatalf("with credentials: %+v %v %v", res, err, asked)
	}
	if entries, _ := ReadLedger(); len(entries) != 1 || entries[0].ID != "df-live" {
		t.Fatalf("ledger after: %+v", entries)
	}
}

func TestRemoteProxy(t *testing.T) {
	var got, bodies []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Host)
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		_, _ = io.WriteString(w, `{"value":"ok"}`)
	}))
	defer target.Close()
	p, err := newRemoteProxy(target.URL + "/remote-endpoint/TOKEN/wd/hub")
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if strings.Contains(p.url, "TOKEN") || !strings.HasPrefix(p.url, "http://127.0.0.1:") {
		t.Fatalf("proxy url %s", p.url)
	}
	resp, err := http.Post(p.url+"/session", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(got) != 1 || !strings.HasPrefix(got[0], "POST /remote-endpoint/TOKEN/wd/hub/session ") {
		t.Fatalf("forwarded %v (%d)", got, resp.StatusCode)
	}
	// A POST with no body is sent with {}.
	resp, err = http.Post(p.url+"/session/s1/element/e1/click", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if bodies[len(bodies)-1] != "{}" {
		t.Fatalf("empty POST forwarded with %q", bodies[len(bodies)-1])
	}
	// Another path is not forwarded.
	resp, _ = http.Get(strings.Replace(p.url, "/", "/x", 3) + "/session")
	if resp != nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("wrong token: %d", resp.StatusCode)
		}
	}
	// A dead endpoint answers a WebDriver error that names no URL.
	target.Close()
	resp, err = http.Get(p.url + "/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "remote-endpoint") || strings.Contains(string(body), "127.0.0.1") {
		t.Fatalf("dead endpoint: %d %s", resp.StatusCode, body)
	}
	if _, err := newRemoteProxy("not a url"); err == nil {
		t.Fatal("a non-URL endpoint")
	}
	(*remoteProxy)(nil).close()
}
