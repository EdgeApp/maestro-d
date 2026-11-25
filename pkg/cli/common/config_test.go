package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFlagPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                               string
		explicit                           map[string]bool
		wantConfig, wantNetwork, wantError string
	}{
		{"explicit config", map[string]bool{"config": true}, path, "", ""},
		{"explicit network config", map[string]bool{"network-config": true}, "", "network.json", ""},
		{"two flags", map[string]bool{"config": true, "network-config": true}, path, "network.json", "--config and --network-config"},
		{"two environment values", nil, path, "network.json", "MAESTRO_D_CONFIG and MAESTRO_D_NETWORK_CONFIG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, network := path, "network.json"
			err := CheckConfigFlags(&config, &network, tc.explicit)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("config flags: %v", err)
			}
			if config != tc.wantConfig || network != tc.wantNetwork {
				t.Fatalf("selected %q, %q", config, network)
			}
		})
	}
	for _, explicit := range []bool{false, true} {
		config, network := path+"-missing", ""
		want := "MAESTRO_D_CONFIG"
		if explicit {
			want = "--config"
		}
		if err := CheckConfigFlags(&config, &network, map[string]bool{"config": explicit}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing config: %v", err)
		}
	}
}
