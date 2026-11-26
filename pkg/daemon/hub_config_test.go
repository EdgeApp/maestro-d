package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpectedHubConfigPathDirectoryEntries(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Configs")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Devices.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	check := func(a, b string, same bool) {
		t.Helper()
		_, err := ExpectedHubConfigPath(a, b)
		if (err == nil) != same {
			t.Fatalf("match %q and %q: %v, want same=%t", a, b, err, same)
		}
	}
	variant := filepath.Join(dir, "devices.json")
	_, err := os.Stat(variant)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	insensitive := err == nil
	if !insensitive {
		if err := os.Link(path, variant); err != nil {
			t.Fatal(err)
		}
	}
	check(path, variant, insensitive)
	other := filepath.Join(dir, "other.json")
	if err := os.Link(path, other); err != nil {
		t.Fatal(err)
	}
	check(path, other, false)
	check(variant, path, insensitive)
	check(filepath.Join(root, "configs", "subdir", "new.json"), filepath.Join(dir, "subdir", "new.json"), insensitive)
}
