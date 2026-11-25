package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckConfigPath(path, nil); err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{false, true} {
		config := path + "-missing"
		want := "MAESTRO_D_CONFIG"
		if explicit {
			want = "--config"
		}
		if err := CheckConfigPath(config, map[string]bool{"config": explicit}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing config: %v", err)
		}
	}
}
