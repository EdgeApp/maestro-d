package hubd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// capsConfig is a config.json written as text, so the capabilities keep
// their key order, number literals and spacing as an operator wrote them.
const capsConfig = `{"hubDevices": [
  {"id": "mock-1", "description": "Lab iPad",
   "capabilities": {"teamstation": {"class": "ipad", "features": ["camera"]}, "anything": [1, 2], "big": 123456789012345678901234567890, "tiny": 1e400}},
  {"id": "mock-2", "capabilities": "just a string"},
  {"id": "mock-3", "capabilities": null},
  {"id": "emulator-5554", "description": "Its own"},
  {"id": "avd:pix", "capabilities": {"from": "avd"}},
  {"id": "emulator-5556", "capabilities": [true]},
  {"id": "avd:other", "capabilities": {"from": "other avd"}}
]}`

const mock1Caps = `{"teamstation":{"class":"ipad","features":["camera"]},"anything":[1,2],"big":123456789012345678901234567890,"tiny":1e400}`

func writeCapsConfig(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(daemon.Home(), "config.json"), []byte(capsConfig), 0o600); err != nil {
		t.Fatal(err)
	}
}

// compact is the JSON a wire answer carries for raw: the same tokens,
// without the spacing.
func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A device's capabilities come back as written, separate from its
// description, in the listing, on its own, in the attach and lease
// answers; a null one is left out. A running emulator takes its serial's
// entry first and its AVD's after.
func TestServer_DeviceCapabilities(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	writeCapsConfig(t)

	res := env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", TTLSec: 30, Attach: &daemon.AttachConfig{Platform: "mock"}})
	if res.Device == nil || compact(t, res.Device.Capabilities) != mock1Caps || res.Device.Description != "Lab iPad" {
		t.Fatalf("lease answer: %+v", res.Device)
	}
	if d, err := env.remote.Device(t.Context(), "mock-1"); err != nil || compact(t, d.Capabilities) != mock1Caps {
		t.Fatalf("device: %v %+v", err, d)
	}
	listed, err := env.remote.Devices(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range listed {
		if d.ID == "mock-1" && compact(t, d.Capabilities) != mock1Caps {
			t.Fatalf("listed: %+v", d)
		}
	}
	devs := []daemon.DeviceInfo{
		{ID: "mock-1", Platform: "mock"},
		{ID: "mock-2", Platform: "mock"},
		{ID: "mock-3", Platform: "mock"},
		{ID: "emulator-5554", Platform: "android", Kind: "emulator", Name: "pix"},
		{ID: "emulator-5556", Platform: "android", Kind: "emulator", Name: "other"},
		{ID: "mock-9", Platform: "mock", Capabilities: json.RawMessage(`{"stale":true}`)},
	}
	env.s.ApplyDeviceConfig(devs)
	want := []string{mock1Caps, `"just a string"`, "", `{"from":"avd"}`, `[true]`, ""}
	for i, d := range devs {
		got := ""
		if d.Capabilities != nil {
			got = compact(t, d.Capabilities)
		}
		if got != want[i] {
			t.Errorf("%s: capabilities %s, want %s", d.ID, got, want[i])
		}
	}
	if b, _ := json.Marshal(devs[2]); strings.Contains(string(b), "capabilities") {
		t.Errorf("a null capabilities is serialized: %s", b)
	}
}
