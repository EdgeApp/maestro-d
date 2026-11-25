package hubcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

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
// hubDevices, none from network.json. An invalid config stops startup.
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
	// stop it.
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
