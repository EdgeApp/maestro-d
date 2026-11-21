package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/computer"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb/rfbtest"
)

// computerEnv points the computer device's seams at a temp config and a
// fake Screen Sharing, and restores them afterwards.
func computerEnv(t *testing.T, cfg rfbtest.Config, json string, mode os.FileMode) (*rfbtest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "computer.json")
	if json != "" {
		if err := os.WriteFile(path, []byte(json), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	s := rfbtest.Start(t, cfg)
	oldAddr, oldGOOS, oldPath, oldHost, oldName, oldNew, oldRec, oldTTL, oldOn := computerAddr, computerGOOS, computerConfigPath, computerHost, computerFullName, newComputerDriver, computerRecognizer, computerProbeTTL, computerServiceOn
	computerAddr = s.Addr()
	computerProbeTTL = 0
	computerServiceOn = func() bool { return true }
	computerGOOS = "darwin"
	computerConfigPath = func() string { return path }
	computerHost = func() hostInfo { return hostInfo{Name: "Demo Mac", Model: "Mac00,0", OSVersion: "26.6.1"} }
	computerFullName = func(string) string { return "Paul Puey" }
	computerRecognizer = computer.RecognizerFunc(func(context.Context, []byte) ([]computer.TextBox, error) {
		return []computer.TextBox{{Text: "Finder", X: 0.03, Y: 0.004, W: 0.03, H: 0.01}}, nil
	})
	t.Cleanup(func() {
		computerAddr, computerGOOS, computerConfigPath, computerHost, computerFullName, newComputerDriver, computerRecognizer, computerProbeTTL, computerServiceOn = oldAddr, oldGOOS, oldPath, oldHost, oldName, oldNew, oldRec, oldTTL, oldOn
	})
	return s, path
}

const goodConfig = `{"vncPassword": "vnc-pw", "macosUser": "paul", "macosPassword": "mac-pw"}`

func TestLoadComputerConfig(t *testing.T) {
	_, path := computerEnv(t, rfbtest.Config{}, goodConfig, 0o600)
	cfg, err := loadComputerConfig()
	if err != nil || cfg.MacOSUser != "paul" || cfg.MacOSPassword != "mac-pw" || cfg.VNCPassword != "vnc-pw" {
		t.Fatalf("%+v %v", cfg, err)
	}
	// macosUser defaults to the daemon's user.
	_ = os.WriteFile(path, []byte(`{"vncPassword": "v", "macosPassword": "m"}`), 0o600)
	if cfg, err := loadComputerConfig(); err != nil || cfg.MacOSUser == "" {
		t.Errorf("default user: %+v %v", cfg, err)
	}
	for body, want := range map[string]string{
		`{"macosPassword": "m"}`: "vncPassword is missing",
		`{"vncPassword": "v"}`:   "macosPassword is missing",
		`{nope`:                  "invalid character",
	} {
		_ = os.WriteFile(path, []byte(body), 0o600)
		_, err := loadComputerConfig()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", body, err)
		}
		if err != nil && (strings.Contains(err.Error(), "mac-pw") || strings.Contains(err.Error(), `"m"`)) {
			t.Errorf("error carries a password: %v", err)
		}
	}
	_ = os.WriteFile(path, []byte(goodConfig), 0o600)
	_ = os.Chmod(path, 0o644)
	if _, err := loadComputerConfig(); err == nil || !strings.Contains(err.Error(), "readable by others (mode 0644)") {
		t.Errorf("0644: %v", err)
	}
	_ = os.Remove(path)
	if _, err := loadComputerConfig(); err == nil || !strings.Contains(err.Error(), "docs/macos.md") {
		t.Errorf("missing: %v", err)
	}
}

func TestServiceEnabled(t *testing.T) {
	out := `disabled services = {
		"com.apple.ftp-proxy" => disabled
		"com.apple.screensharing" => enabled
		"com.apple.smbd" => disabled
	}
`
	for label, want := range map[string]bool{"com.apple.screensharing": true, "com.apple.smbd": false, "com.apple.missing": false} {
		if got := serviceEnabled(out, label); got != want {
			t.Errorf("%s: %v", label, got)
		}
	}
	// Older macOS: the value says whether the service is disabled.
	if !serviceEnabled(`"com.apple.screensharing" => false`, "com.apple.screensharing") || serviceEnabled(`"com.apple.screensharing" => true`, "com.apple.screensharing") {
		t.Error("true/false values")
	}
	screenSharingEnabled() // runs launchctl; either answer is fine here
}

// With the Screen Sharing service off, whatever listens on the port is not
// macOS's: the Mac lists as screen-sharing-off and the attach sends nothing.
func TestComputer_ServiceOffSendsNoPassword(t *testing.T) {
	s, _ := computerEnv(t, rfbtest.Config{}, goodConfig, 0o600)
	computerServiceOn = func() bool { return false }
	if e, _ := computerEntry(); e.State != "screen-sharing-off" || e.Ready {
		t.Errorf("entry: %+v", e)
	}
	_, _, _, err := computerNewDriver(context.Background(), "mac", daemon.AttachConfig{})
	var de *daemon.Error
	if !errors.As(err, &de) || de.Code != daemon.CodeDeviceError || !strings.Contains(err.Error(), "Screen Sharing is not turned on") {
		t.Errorf("attach: %v", err)
	}
	if n := s.Logins(); n != 0 {
		t.Errorf("%d logins reached the listener", n)
	}
}

func TestComputerEntry_ReusesTheProbe(t *testing.T) {
	s, _ := computerEnv(t, rfbtest.Config{}, goodConfig, 0o600)
	computerProbeTTL = time.Hour
	if e, _ := computerEntry(); e.State != "ready" {
		t.Fatalf("first listing: %+v", e)
	}
	s.Close()
	if e, _ := computerEntry(); e.State != "ready" {
		t.Errorf("a listing within the TTL probed again: %+v", e)
	}
	computerProbeTTL = 0
	if e, _ := computerEntry(); e.State != "screen-sharing-off" {
		t.Errorf("a listing after the TTL reused the probe: %+v", e)
	}
}

func TestComputerEntry_States(t *testing.T) {
	_, path := computerEnv(t, rfbtest.Config{}, goodConfig, 0o600)
	e, ok := computerEntry()
	if !ok || e.ID != "mac" || e.Platform != "macos" || e.Kind != "computer" || e.Name != "Demo Mac" || e.Model != "Mac00,0" || e.OSVersion != "26.6.1" || e.State != "ready" || !e.Ready {
		t.Fatalf("%+v", e)
	}
	_ = os.Chmod(path, 0o644)
	if e, _ := computerEntry(); e.State != "credentials-missing" || e.Ready {
		t.Errorf("insecure config: %+v", e)
	}
	_ = os.Chmod(path, 0o600)
	computerAddr = "127.0.0.1:1"
	if e, _ := computerEntry(); e.State != "screen-sharing-off" || e.Ready {
		t.Errorf("no server: %+v", e)
	}
	computerGOOS = "linux"
	if _, ok := computerEntry(); ok {
		t.Error("listed on linux")
	}
}

func TestComputerEntry_LoginUnavailable(t *testing.T) {
	computerEnv(t, rfbtest.Config{Types: []byte{2}}, goodConfig, 0o600)
	if e, _ := computerEntry(); e.State != "login-unavailable" || e.Ready {
		t.Errorf("%+v", e)
	}
}

func TestDaemonListDevices_Computer(t *testing.T) {
	computerEnv(t, rfbtest.Config{}, goodConfig, 0o600)
	t.Setenv(envMockDevices, "")
	found := func(list []daemon.DeviceInfo) bool {
		for _, d := range list {
			if d.ID == "mac" {
				return true
			}
		}
		return false
	}
	if l := daemonListDevices("macos"); len(l) != 1 || l[0].ID != "mac" {
		t.Errorf("macos filter: %+v", l)
	}
	if found(daemonListDevices("mock")) || found(daemonListDevices("web")) {
		t.Error("mac under another platform filter")
	}
	computerGOOS = "linux"
	if l := daemonListDevices("macos"); len(l) != 0 {
		t.Errorf("linux: %+v", l)
	}
}

func TestComputerAttach(t *testing.T) {
	s, _ := computerEnv(t, rfbtest.Config{User: "paul", Password: "mac-pw"}, goodConfig, 0o600)
	if !isComputerAttach("mac", daemon.AttachConfig{}) || !isComputerAttach("x", daemon.AttachConfig{Platform: "macOS"}) || isComputerAttach("mock-1", daemon.AttachConfig{}) {
		t.Error("isComputerAttach")
	}
	drv, info, cleanup, err := daemonNewDriver(context.Background(), "mac", daemon.AttachConfig{Extra: map[string]any{"idleTimeoutSec": float64(20)}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if info.Platform != "macos" || info.Kind != "computer" || info.DriverName != "computer" || info.Name != "Demo Mac" {
		t.Errorf("%+v", info)
	}
	if it, ok := drv.(interface{ IdleTimeout() time.Duration }); !ok || it.IdleTimeout() != 20*time.Second {
		t.Error("idle timeout not passed")
	}
	if s.Logins() != 1 {
		t.Errorf("logins %d", s.Logins())
	}
	for id, cfg := range map[string]daemon.AttachConfig{
		"other": {Platform: "macos"},
		"mac":   {Extra: map[string]any{"idleTimeoutSec": float64(-1)}},
	} {
		_, _, _, err := daemonNewDriver(context.Background(), id, cfg)
		var de *daemon.Error
		if !errors.As(err, &de) || de.Code != daemon.CodeUsage {
			t.Errorf("%s %+v: %v", id, cfg, err)
		}
	}
}

func TestComputerAttach_Failures(t *testing.T) {
	computerEnv(t, rfbtest.Config{User: "paul", Password: "other"}, goodConfig, 0o600)
	_, _, _, err := daemonNewDriver(context.Background(), "mac", daemon.AttachConfig{})
	var af *rfb.AuthFailed
	if !errors.As(err, &af) || strings.Contains(err.Error(), "mac-pw") {
		t.Errorf("wrong password: %v", err)
	}
	computerGOOS = "linux"
	if _, _, _, err := daemonNewDriver(context.Background(), "mac", daemon.AttachConfig{}); err == nil || !strings.Contains(err.Error(), "needs macOS") {
		t.Errorf("linux: %v", err)
	}
	computerGOOS = "darwin"
	computerConfigPath = func() string { return filepath.Join(t.TempDir(), "none.json") }
	_, _, _, err = daemonNewDriver(context.Background(), "mac", daemon.AttachConfig{})
	var de *daemon.Error
	if !errors.As(err, &de) || de.Code != daemon.CodeUsage {
		t.Errorf("no config: %v", err)
	}
}

func TestComputerPlatformRouting(t *testing.T) {
	if guessPlatform("mac") != "macos" {
		t.Error("guessPlatform(mac)")
	}
	if guessPlatform("mac2") == "macos" {
		t.Error("guessPlatform(mac2)")
	}
	// The daemon may infer a platform from the file (.apk → android); the
	// computer refuses before any of that runs adb or simctl.
	for _, err := range []error{
		daemonInstallApp(context.Background(), "mac", "", "/tmp/x.app"),
		daemonInstallApp(context.Background(), "mac", "android", "/tmp/x.apk"),
		daemonInstallApp(context.Background(), "mac", "ios", "/tmp/x.app"),
		daemonInstallApp(context.Background(), "other", "macos", "/tmp/x.app"),
		daemonUninstallApp(context.Background(), "mac", "", "com.x"),
		daemonUninstallApp(context.Background(), "mac", "android", "com.x"),
	} {
		var de *daemon.Error
		if !errors.As(err, &de) || de.Code != daemon.CodeUsage || !strings.Contains(err.Error(), "installs no apps") {
			t.Errorf("install on mac: %v", err)
		}
	}
}

func TestFullNameAndHostFacts(t *testing.T) {
	if runtime := computerGOOS; runtime != "darwin" {
		t.Skip("macOS only")
	}
	if h := hostFacts(); h.OSVersion == "" || h.Model == "" {
		t.Errorf("%+v", h)
	}
	u := os.Getenv("USER")
	if u != "" && fullName(u) == "" {
		t.Errorf("no full name for %s", u)
	}
	if fullName("no-such-user-xyz") != "" {
		t.Error("full name for a missing user")
	}
}
