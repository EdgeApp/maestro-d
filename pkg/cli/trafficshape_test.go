package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// setTrafficShape with no flags sends the all-defaults shape.
func TestOneShot_SetTrafficShapeNeedsNoFlags(t *testing.T) {
	_, p := oneShotParse(t, "setTrafficShape", "--device", "d")
	v, err := buildStepValue(daemon.CommandSpecs["setTrafficShape"], p)
	if err != nil || v != nil {
		t.Fatalf("value %v err %v", v, err)
	}
	_, p = oneShotParse(t, "setTrafficShape", "--mbit", "1.5", "--direction", "download", "--device", "d")
	v, err = buildStepValue(daemon.CommandSpecs["setTrafficShape"], p)
	m, ok := v.(map[string]any)
	if err != nil || !ok || m["mbit"] != 1.5 || m["direction"] != "download" {
		t.Fatalf("value %#v err %v", v, err)
	}
	_, p = oneShotParse(t, "resetTrafficShape", "--device", "d")
	if v, err := buildStepValue(daemon.CommandSpecs["resetTrafficShape"], p); err != nil || v != nil {
		t.Fatalf("reset: %v %v", v, err)
	}
}

// hub start passes --network-config to its detached child (and so to the
// LaunchAgent hub service install writes) as an absolute path.
func TestHubChildArgs_NetworkConfig(t *testing.T) {
	args := strings.Join(hubChildArgs(&daemonOpts{NetworkConfig: "shapes.json"}), " ")
	abs, _ := filepath.Abs("shapes.json")
	if !strings.Contains(args, "--network-config "+abs) {
		t.Fatalf("child args %q", args)
	}
	if strings.Contains(strings.Join(hubChildArgs(&daemonOpts{}), " "), "network-config") {
		t.Fatal("an unset --network-config reached the child")
	}
}

// hub start passes --config to its detached child as an absolute path.
func TestHubChildArgs_Config(t *testing.T) {
	args := strings.Join(hubChildArgs(&daemonOpts{DeviceConfig: "lab.json"}), " ")
	abs, _ := filepath.Abs("lab.json")
	if !strings.Contains(args, "--config "+abs) || strings.Contains(args, "--network-config") {
		t.Fatalf("child args %q", args)
	}
	if strings.Contains(strings.Join(hubChildArgs(&daemonOpts{}), " "), "--config") {
		t.Fatal("an unset --config reached the child")
	}
}

// A hub reads the share list from its config at start: config.json's
// hubDevices, none from network.json, and a config it cannot use, or both
// flags at once, stops the start.
func TestConfigShare(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if share, err := configShare(&daemonOpts{}); err != nil || len(share) != 0 {
		t.Fatalf("no config: %v %v", share, err)
	}
	write(filepath.Join(daemon.Home(), "network.json"), `{"devices":{}}`)
	if share, err := configShare(&daemonOpts{}); err != nil || len(share) != 0 {
		t.Fatalf("network.json only: %v %v", share, err)
	}
	// A network.json shares nothing, so one the hub cannot use does not
	// stop it: the shaping commands report it, as before.
	write(filepath.Join(daemon.Home(), "network.json"), `{"devices":{"PHONE-A":{}}}`)
	if err := os.Chmod(filepath.Join(daemon.Home(), "network.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if share, err := configShare(&daemonOpts{}); err != nil || len(share) != 0 {
		t.Fatalf("a broken network.json: %v %v", share, err)
	}
	write(filepath.Join(daemon.Home(), "config.json"), `{"hubDevices":[{"id":"PHONE-A"},{"id":"TV-1","description":"Lab TV"}]}`)
	if share, err := configShare(&daemonOpts{}); err != nil || strings.Join(share, ",") != "PHONE-A,TV-1" {
		t.Fatalf("config.json: %v %v", share, err)
	}
	other := filepath.Join(t.TempDir(), "lab.json")
	write(other, `{"hubDevices":[{"id":"OTHER"}]}`)
	if share, err := configShare(&daemonOpts{DeviceConfig: other}); err != nil || strings.Join(share, ",") != "OTHER" {
		t.Fatalf("--config: %v %v", share, err)
	}
	both := map[string]bool{"config": true, "network-config": true}
	if _, err := configShare(&daemonOpts{DeviceConfig: other, NetworkConfig: other, explicit: both}); err == nil || !strings.Contains(err.Error(), "--config and --network-config") {
		t.Fatalf("both flags: %v", err)
	}
	if _, err := configShare(&daemonOpts{DeviceConfig: other, NetworkConfig: other}); err == nil || !strings.Contains(err.Error(), "MAESTRO_D_CONFIG and MAESTRO_D_NETWORK_CONFIG") {
		t.Fatalf("both variables: %v", err)
	}
	// A flag wins over the other's environment variable.
	o := &daemonOpts{DeviceConfig: other, NetworkConfig: "env-network.json", explicit: map[string]bool{"config": true}}
	if share, err := configShare(o); err != nil || strings.Join(share, ",") != "OTHER" || o.NetworkConfig != "" {
		t.Fatalf("--config over MAESTRO_D_NETWORK_CONFIG: %v %v %q", share, err, o.NetworkConfig)
	}
	o = &daemonOpts{DeviceConfig: "env-config.json", NetworkConfig: filepath.Join(daemon.Home(), "missing.json"), explicit: map[string]bool{"network-config": true}}
	if share, err := configShare(o); err != nil || len(share) != 0 || o.DeviceConfig != "" {
		t.Fatalf("--network-config over MAESTRO_D_CONFIG: %v %v %q", share, err, o.DeviceConfig)
	}
	// A named config.json that does not exist is a mistake, not an empty
	// config.
	missing := filepath.Join(t.TempDir(), "typo.json")
	_, err := configShare(&daemonOpts{DeviceConfig: missing, explicit: map[string]bool{"config": true}})
	if de, ok := err.(*daemon.Error); !ok || de.Code != daemon.CodeUsage || !strings.Contains(de.Message, "--config "+missing+" does not exist") {
		t.Fatalf("a missing --config: %v", err)
	}
	if _, err := configShare(&daemonOpts{DeviceConfig: missing}); err == nil || !strings.Contains(err.Error(), "MAESTRO_D_CONFIG "+missing) {
		t.Fatalf("a missing MAESTRO_D_CONFIG: %v", err)
	}
	write(other, `{"hubDevices":[{"id":"OTHER"},{"id":"OTHER"}]}`)
	_, err = configShare(&daemonOpts{DeviceConfig: other})
	if de, ok := err.(*daemon.Error); !ok || de.Code != daemon.CodeUsage || !strings.Contains(de.Message, "listed twice") {
		t.Fatalf("a broken config: %v", err)
	}
}
