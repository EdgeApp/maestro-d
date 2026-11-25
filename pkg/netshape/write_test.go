package netshape

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
)

func TestEditDevice(t *testing.T) {
	const shape = `{
	"resetTrafficShape": { "method": "DELETE", "url": "https://example.test/r" },
	"setTrafficShape":{"method":"POST","url":"https://example.test/s","headers":{"Authorization":"a\u0026b<c>"},"body":{"n":9.00e+1}}
}`
	path := filepath.Join(t.TempDir(), ConfigFileName)
	data := `{"hubDevices":[{"id":"one","description":"original","trafficShaping":` + shape + `},{"id":"two"}]}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	get := func() []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	description := "  updated  "
	device, err := UpsertDevice(path, " one ", DeviceFields{Description: &description})
	if err != nil || device.Description != "updated" || !device.Shapeable() {
		t.Fatalf("updated device %+v: %v", device, err)
	}
	if device, err = UpsertDevice(path, "one", DeviceFields{}); err != nil || device.Description != "updated" {
		t.Fatalf("omitted description: %+v %v", device, err)
	}
	if _, err := UpsertDevice(path, "three", DeviceFields{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || !slices.Equal(cfg.Shared, []string{"one", "two", "three"}) {
		t.Fatalf("upsert order: %+v %v", cfg, err)
	}
	if !bytes.Contains(get(), []byte(shape)) {
		t.Fatalf("shaping bytes changed: %s", get())
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	description = " "
	if _, err := UpsertDevice(path, "one", DeviceFields{Description: &description}); err != nil || bytes.Contains(get(), []byte("description")) {
		t.Fatalf("clearing description: %v %s", err, get())
	}
	if err := RemoveDevice(path, "two"); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil || !slices.Equal(cfg.Shared, []string{"one", "three"}) || !bytes.Contains(get(), []byte(shape)) {
		t.Fatalf("remove: %+v %v", cfg, err)
	}
	for _, id := range []string{"one", "three"} {
		if err := RemoveDevice(path, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(get()); got != "{\n  \"hubDevices\": []\n}\n" {
		t.Fatalf("empty config: %s", got)
	}
}

func TestEditDeviceThroughConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	target := filepath.Join(dir, "shared", "maestro.json")
	if err := os.Symlink(filepath.Join("shared", "maestro.json"), path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := UpsertDevice(path, "first", DeviceFields{}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertDevice(target, "second", DeviceFields{}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDevice(path, "first"); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || !slices.Equal(cfg.Shared, []string{"second"}) {
		t.Fatalf("changes through both names: %v, %v", cfg, err)
	}
	if _, err := os.Readlink(path); err != nil {
		t.Fatalf("config link was replaced: %v", err)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected lock beside link: %v", err)
	}
	if _, err := os.Stat(target + ".lock"); err != nil {
		t.Fatalf("missing target lock: %v", err)
	}
}

func TestConfigPathThroughLinkedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix parent traversal")
	}
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("releases/v1", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/v1", "current"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("releases/existing.json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("current/../new.json", "config-link"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"current/../existing.json", "current/../sub/missing.json", "config-link"} {
		if _, err := UpsertDevice(key, "intended", DeviceFields{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"existing.json", "sub/missing.json", "new.json"} {
		cfg, err := LoadConfig(filepath.Join("releases", target))
		if err != nil || !slices.Equal(cfg.Shared, []string{"intended"}) {
			t.Fatalf("%s: %v, %v", target, cfg, err)
		}
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unintended file %s: %v", target, err)
		}
	}
}

func TestEditDeviceErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new-home", ConfigFileName)
	if _, err := UpsertDevice(path, "created", DeviceFields{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", " \t", "\xff"} {
		if _, err := UpsertDevice(path, id, DeviceFields{}); err == nil {
			t.Fatal("upsert accepted an invalid id")
		}
		if err := RemoveDevice(path, id); err == nil {
			t.Fatal("remove accepted an invalid id")
		}
	}
	if err := RemoveDevice(path, "unknown"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown device: %v", err)
	}
	for _, bad := range []string{`{`, `{"devices":{}}`, `{"hubDevices":[{"id":"d"},{"id":"d"}]}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := UpsertDevice(path, "d", DeviceFields{}); err == nil {
			t.Fatal("upsert accepted malformed config")
		}
		if err := RemoveDevice(path, "d"); err == nil {
			t.Fatal("remove accepted malformed config")
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != bad {
			t.Fatalf("malformed config changed: %s %v", got, err)
		}
	}
	if _, err := UpsertDevice(filepath.Join(path, "impossible"), "d", DeviceFields{}); err == nil {
		t.Fatal("upsert into a regular file succeeded")
	}
	if err := os.Mkdir(filepath.Join(dir, "locked.json.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertDevice(filepath.Join(dir, "locked.json"), "d", DeviceFields{}); err == nil {
		t.Fatal("upsert with inaccessible lock succeeded")
	}
	if _, err := UpsertDevice(dir, "d", DeviceFields{}); err == nil {
		t.Fatal("reading a directory succeeded")
	}
	if err := writeConfigFile(dir, []byte("{}")); err == nil {
		t.Fatal("replacing a directory succeeded")
	}
	if err := writeConfigFile(filepath.Join(path, "impossible"), nil); err == nil {
		t.Fatal("temp file in regular file succeeded")
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(dir), ".config-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary files after rename error: %v %v", temps, err)
	}
}

func TestConcurrentDeviceWrites(t *testing.T) {
	const childEnv = "MAESTRO_TEST_CONFIG_WRITER"
	const count = 12
	write := func(path, prefix string) error {
		for i := range count {
			if _, err := UpsertDevice(path, fmt.Sprintf("%s-%d", prefix, i), DeviceFields{}); err != nil {
				return err
			}
		}
		return nil
	}
	if prefix := os.Getenv(childEnv); prefix != "" {
		if err := write(os.Getenv(childEnv+"_PATH"), prefix); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), ConfigFileName)
	lock, err := fsage.LockFile(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	var children []*exec.Cmd
	var outputs [2]bytes.Buffer
	for i := range outputs {
		cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentDeviceWrites$")
		cmd.Env = append(os.Environ(), childEnv+"="+fmt.Sprintf("child%d", i), childEnv+"_PATH="+path)
		cmd.Stdout = &outputs[i]
		cmd.Stderr = &outputs[i]
		if err := cmd.Start(); err != nil {
			fsage.UnlockFile(lock)
			t.Fatal(err)
		}
		children = append(children, cmd)
	}
	done := make(chan error, 2)
	for i := range 2 {
		go func() { done <- write(path, fmt.Sprintf("goroutine%d", i)) }()
	}
	fsage.UnlockFile(lock)
	for i, cmd := range children {
		if err := cmd.Wait(); err != nil {
			t.Errorf("writer: %v %s", err, outputs[i].String())
		}
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	cfg, err := LoadConfig(path)
	if err != nil || len(cfg.Devices) != 4*count {
		t.Fatalf("lost concurrent edit: %+v, %v", cfg, err)
	}
}
