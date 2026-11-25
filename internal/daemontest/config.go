package daemontest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// DeviceConfig writes a private config.json sharing ids in the given order.
func DeviceConfig(t testing.TB, ids ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	WriteDevices(t, path, ids...)
	return path
}

func WriteDevices(t testing.TB, path string, ids ...string) {
	t.Helper()
	entries := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, map[string]string{"id": id})
	}
	data, err := json.Marshal(map[string]any{"hubDevices": entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
