package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestHubAliasLookup(t *testing.T) {
	name := "maestro-d-hub"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	home := t.TempDir()
	t.Setenv(daemon.EnvHome, home)
	sibling, bin, pathDir := t.TempDir(), filepath.Join(home, "bin"), t.TempDir()
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	paths := []string{filepath.Join(t.TempDir(), name), filepath.Join(sibling, name), filepath.Join(bin, name), filepath.Join(pathDir, name)}
	for _, file := range paths {
		if err := os.WriteFile(file, []byte("binary"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(daemon.EnvHubBin, paths[0])
	for _, file := range paths {
		got, err := findHubBinary(filepath.Join(sibling, "maestro-d"))
		if err != nil || got != file {
			t.Fatalf("lookup = %s, %v; want %s", got, err, file)
		}
		t.Setenv(daemon.EnvHubBin, "")
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := findHubBinary(filepath.Join(sibling, "maestro-d")); err == nil {
		t.Fatal("found absent hub")
	}
}
