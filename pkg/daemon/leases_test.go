package daemon

import (
	"github.com/devicelab-dev/maestro-runner/internal/daemontest"

	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitFor polls cond until it holds, failing the test after 20 s: CI runs
// many packages at once under -race, and a teardown can take seconds there.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 20s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLeases_ClientForgetsExpiredLease(t *testing.T) {
	env := newCallerEnv(t, Config{Hub: NewTestHub(), Token: "t", DeviceConfig: daemontest.DeviceConfig(t, "mock-1")})
	ctx := context.Background()
	var told []string
	env.remote.OnLeaseExpired = func(device, lease, _ string) { told = append(told, device+"="+lease) }
	env.remote.SetLease("mock-1", "gone")
	_, err := env.remote.Command(ctx, "mock-1", "tapOn", "x", true)
	wantErrCode(t, err, CodeLeaseExpired)
	if env.remote.LeaseOf("mock-1") != "" || len(told) != 1 || told[0] != "mock-1=gone" {
		t.Fatalf("told %v, lease %q", told, env.remote.LeaseOf("mock-1"))
	}
	// Blob uploads carry the lease too.
	env.lease(t, "mock-1")
	f := filepath.Join(t.TempDir(), "b")
	_ = os.WriteFile(f, []byte("hello"), 0o644)
	hex := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if _, err := env.remote.UploadBlob(ctx, "mock-1", hex, f, 5); err != nil {
		t.Fatalf("upload with the lease %s: %v", env.remote.LeaseOf("mock-1"), err)
	}
	for in, want := range map[string]string{"/v1/devices/a%40b/x": "a@b", "/v1/devices/a?x": "a", "/v1/status": "", "/v1/devices/%zz": ""} {
		if got := pathDevice(in); got != want {
			t.Errorf("pathDevice(%q) = %q, want %q", in, got, want)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
