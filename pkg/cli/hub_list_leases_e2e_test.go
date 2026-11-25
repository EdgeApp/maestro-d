package cli

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestE2E_HolderListThroughMesh(t *testing.T) {
	e := newE2E(t)
	pool := []string{"mock-1", "mock-2", "mock-3"}
	a, _, _, envB := startMockHubPair(t, e, "listing", nil, pool)
	holder := []string{"MAESTRO_D_HOME=" + filepath.Join(e.cwd, "holder"), "MAESTRO_D_TOKEN=res-tok"}
	stranger := []string{"MAESTRO_D_HOME=" + filepath.Join(e.cwd, "stranger"), "MAESTRO_D_TOKEN=res-tok"}
	target := "--target-hub=" + a.HTTPAddr
	ids := func(env []string) []string {
		t.Helper()
		r := e.mustOK(env, "device", "list", target, "--platform", "mock", "--json")
		var result daemon.DevicesResult
		if err := json.Unmarshal([]byte(r.stdout), &result); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, device := range result.Devices {
			ids = append(ids, device.ID)
		}
		slices.Sort(ids)
		return ids
	}
	await := func(env []string, want []string) {
		t.Helper()
		for until := time.Now().Add(15 * time.Second); ; time.Sleep(150 * time.Millisecond) {
			got := ids(env)
			if slices.Equal(got, want) {
				return
			}
			if time.Now().After(until) {
				t.Fatalf("listed %v, want %v", got, want)
			}
		}
	}
	all := []string{"mock-1@hub-b", "mock-2@hub-b", "mock-3@hub-b"}
	await(stranger, all)
	for _, id := range all[:2] {
		e.mustOK(holder, "lease", target, "--device", id, "--no-attach")
	}
	writeSharedMocks(t, envValue(envB, daemon.EnvHome), "mock-3")
	await(stranger, all[2:])
	for _, tc := range []struct{ env, want []string }{{holder, all}, {stranger, all[2:]}, {holder, all}} {
		if got := ids(tc.env); !slices.Equal(got, tc.want) {
			t.Fatalf("listed %v, want %v", got, tc.want)
		}
	}
}
