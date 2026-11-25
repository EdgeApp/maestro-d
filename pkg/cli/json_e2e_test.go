package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// JSON output is also the interface used by scripts which run the client
// one command at a time. Exercise one complete session with only mock devices.
func TestE2E_JSONDeviceSession(t *testing.T) {
	t.Setenv("MAESTRO_D_MOCK_DEVICES", "mock-1")
	e := newE2E(t)
	for _, tc := range []struct {
		args  []string
		field string
	}{
		{[]string{"start", "--device", "mock-1", "--platform", "mock"}, "device"},
		{[]string{"status"}, "daemon"},
		{[]string{"attach", "--device", "mock-1", "--platform", "mock"}, "device"},
		{[]string{"set", "TEST_VALUE=ready"}, "vars"},
		{[]string{"get", "vars"}, "vars"},
		{[]string{"eval", "TEST_VALUE"}, "value"},
		{[]string{"get", "artifacts"}, "artifacts"},
		{[]string{"get", "artifacts", "-o", t.TempDir()}, "dir"},
		{[]string{"detach", "--all"}, "ok"},
		{[]string{"stop", "--timeout", "5"}, "stopped"},
		{[]string{"stop", "--all"}, "stopped"},
	} {
		r := e.ok(append(tc.args, "--json")...)
		var data map[string]json.RawMessage
		if err := json.Unmarshal([]byte(r.stdout), &data); err != nil || string(data["ok"]) != "true" || data[tc.field] == nil {
			t.Fatalf("%v: %s (%v)", tc.args, r.stdout, err)
		}
	}
	listed := e.ok("commands", "--json")
	var specs []json.RawMessage
	if err := json.Unmarshal([]byte(listed.stdout), &specs); err != nil || len(specs) == 0 {
		t.Fatalf("commands: %v", err)
	}
	spec := e.ok("commands", "tapOn", "--json")
	if !json.Valid([]byte(spec.stdout)) || !strings.Contains(spec.stdout, "tapOn") {
		t.Fatalf("tapOn spec: %s", spec.stdout)
	}
}
