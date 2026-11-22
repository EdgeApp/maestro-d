package cli

// maestro-d: the computer device — the Mac this daemon runs on, driven over
// its own Screen Sharing (pkg/driver/computer). Its credentials live only in
// <home>/computer.json; no caller sends them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/computer"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb"
)

// computerConfigFile holds the computer's credentials, in daemon.Home().
const computerConfigFile = "computer.json"

// ComputerConfig is computer.json. maestro-d logs in with the macOS
// account (Screen Sharing's type-30 login), which lands on the user's
// desktop; a VNC-password connection opens on a login screen of its own,
// which on macOS 26 did not take the password. The VNC password is kept
// with the account so the file holds both of Screen Sharing's credentials.
type ComputerConfig struct {
	VNCPassword   string `json:"vncPassword"`
	MacOSUser     string `json:"macosUser"`
	MacOSPassword string `json:"macosPassword"`
}

// Seams for tests.
var (
	computerAddr         = "127.0.0.1:5900"
	computerGOOS         = runtime.GOOS
	computerConfigPath   = func() string { return filepath.Join(daemon.Home(), computerConfigFile) }
	computerHost         = hostFacts
	computerFullName     = fullName
	newComputerDriver    = computer.New
	computerRecognizer   computer.Recognizer // nil: the Vision helper
	computerProbeTimeout = 2 * time.Second
	// computerServiceOn reports whether macOS's Screen Sharing service is
	// enabled, so launchd, not some other process, holds its port.
	computerServiceOn = screenSharingEnabled
)

// loadComputerConfig reads computer.json, refusing a file others can read
// and naming any missing field. Nothing here logs the passwords.
func loadComputerConfig() (ComputerConfig, error) {
	path := computerConfigPath()
	st, err := os.Stat(path)
	if err != nil {
		return ComputerConfig{}, fmt.Errorf("%s: %w (it holds the computer's VNC and macOS passwords; see docs/macos.md)", path, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return ComputerConfig{}, fmt.Errorf("%s is readable by others (mode %04o); chmod 600 it", path, st.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ComputerConfig{}, err
	}
	var cfg ComputerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ComputerConfig{}, fmt.Errorf("%s: %v", path, err)
	}
	if cfg.MacOSUser == "" {
		if u, err := user.Current(); err == nil {
			cfg.MacOSUser = u.Username
		}
	}
	switch {
	case cfg.VNCPassword == "":
		return ComputerConfig{}, fmt.Errorf("%s: vncPassword is missing", path)
	case cfg.MacOSPassword == "":
		return ComputerConfig{}, fmt.Errorf("%s: macosPassword is missing", path)
	case cfg.MacOSUser == "":
		return ComputerConfig{}, fmt.Errorf("%s: macosUser is missing", path)
	}
	return cfg, nil
}

// hostInfo is the Mac's name, model and OS version, read once.
type hostInfo struct{ Name, Model, OSVersion string }

var (
	hostOnce   sync.Once
	hostCached hostInfo
)

func hostFacts() hostInfo {
	hostOnce.Do(func() {
		run := func(name string, args ...string) string {
			out, _ := exec.Command(name, args...).Output()
			return strings.TrimSpace(string(out))
		}
		hostCached = hostInfo{
			Name:      run("scutil", "--get", "ComputerName"),
			Model:     run("sysctl", "-n", "hw.model"),
			OSVersion: run("sw_vers", "-productVersion"),
		}
	})
	return hostCached
}

// fullName is the user's full name as the login window shows it, looked up
// through the directory search policy (`id -F`), so a network account's
// name is found too; "" when there is none.
func fullName(username string) string {
	out, err := exec.Command("id", "-F", username).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Listing states.
const (
	computerReady              = "ready"
	computerCredentialsMissing = "credentials-missing"
	computerLoginUnavailable   = "login-unavailable"
	computerScreenSharingOff   = "screen-sharing-off"
)

// computerEntry lists the Mac on darwin: probe Screen Sharing without
// logging in, and check computer.json is usable.
func computerEntry() (daemon.DeviceInfo, bool) {
	if computerGOOS != "darwin" {
		return daemon.DeviceInfo{}, false
	}
	h := computerHost()
	info := daemon.DeviceInfo{
		ID: computer.DeviceID, Platform: computer.Platform, Kind: computer.Kind,
		Name: h.Name, Model: h.Model, OSVersion: h.OSVersion,
	}
	// A closed port refuses at once; a Screen Sharing that has just ended
	// a session can take over a second to greet.
	types, err := probeScreenSharing()
	_, cfgErr := loadComputerConfig()
	switch {
	case err != nil:
		info.State = computerScreenSharingOff
	case cfgErr != nil:
		info.State = computerCredentialsMissing
	case !offersAccountLogin(types):
		info.State = computerLoginUnavailable
	default:
		info.State = computerReady
	}
	info.Ready = info.State == computerReady
	return info, true
}

// computerProbeTTL is how long listings reuse Screen Sharing's answer, so
// the many lookups of a busy daemon do not each open a connection to it.
var computerProbeTTL = 3 * time.Second

var probeCache struct {
	sync.Mutex
	addr  string
	at    time.Time
	types []byte
	err   error
}

// probeScreenSharing is Screen Sharing's offered security types, probed at
// most once per computerProbeTTL; concurrent listings share one probe.
func probeScreenSharing() ([]byte, error) {
	probeCache.Lock()
	defer probeCache.Unlock()
	if probeCache.addr == computerAddr && time.Since(probeCache.at) < computerProbeTTL {
		return probeCache.types, probeCache.err
	}
	var types []byte
	err := errScreenSharingOff
	if computerServiceOn() {
		_, types, err = rfb.Probe(context.Background(), computerAddr, computerProbeTimeout)
	}
	probeCache.addr, probeCache.at, probeCache.types, probeCache.err = computerAddr, time.Now(), types, err
	return types, err
}

var errScreenSharingOff = errors.New("Screen Sharing is not turned on")

// screenSharingEnabled reads launchd's overrides: Screen Sharing is on when
// its service, com.apple.screensharing, is enabled. Then launchd holds port
// 5900, and the macOS password goes only to macOS's own Screen Sharing;
// otherwise any local process could listen there.
func screenSharingEnabled() bool {
	out, err := exec.Command("launchctl", "print-disabled", "system").Output()
	return err == nil && serviceEnabled(string(out), "com.apple.screensharing")
}

// serviceEnabled reads one service's line of `launchctl print-disabled`:
// `"label" => enabled` (or `=> false` on older macOS, where the value says
// whether it is disabled).
func serviceEnabled(out, label string) bool {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=>")
		if !ok || strings.Trim(strings.TrimSpace(k), `"`) != label {
			continue
		}
		v = strings.TrimSpace(v)
		return v == "enabled" || v == "false"
	}
	return false
}

func offersAccountLogin(types []byte) bool {
	for _, t := range types {
		if t == rfb.SecurityAccount {
			return true
		}
	}
	return false
}

// isComputerAttach reports whether an attach is for the computer device.
func isComputerAttach(id string, cfg daemon.AttachConfig) bool {
	return strings.EqualFold(cfg.Platform, computer.Platform) || (cfg.Platform == "" && id == computer.DeviceID)
}

// computerNewDriver opens the computer driver from computer.json.
func computerNewDriver(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
	if id != computer.DeviceID {
		return nil, daemon.DriverInfo{}, nil, daemon.Errorf(daemon.CodeUsage, "the computer device's id is %q, not %q", computer.DeviceID, id)
	}
	if computerGOOS != "darwin" {
		return nil, daemon.DriverInfo{}, nil, daemon.Errorf(daemon.CodeUsage, "the computer device needs macOS")
	}
	cc, err := loadComputerConfig()
	if err != nil {
		return nil, daemon.DriverInfo{}, nil, daemon.Errorf(daemon.CodeUsage, "%v", err)
	}
	if !computerServiceOn() {
		return nil, daemon.DriverInfo{}, nil, daemon.Errorf(daemon.CodeDeviceError, "%v (System Settings → General → Sharing); maestro-d sends the macOS password only to macOS's own Screen Sharing", errScreenSharingOff)
	}
	idle := computer.DefaultIdleTimeout
	if v, ok := cfg.Extra["idleTimeoutSec"].(float64); ok {
		if v < 0 {
			return nil, daemon.DriverInfo{}, nil, daemon.Errorf(daemon.CodeUsage, "idleTimeoutSec must be 0 or more")
		}
		idle = time.Duration(v * float64(time.Second))
	}
	h := computerHost()
	d, err := newComputerDriver(ctx, computer.Config{
		Addr: computerAddr,
		Login: rfb.Login{
			Method: rfb.MethodAccount, User: cc.MacOSUser, Password: cc.MacOSPassword,
		},
		FullName:    computerFullName(cc.MacOSUser),
		DeviceName:  h.Name,
		OSVersion:   h.OSVersion,
		IdleTimeout: idle,
		Recognizer:  computerRecognizer,
	})
	if err != nil {
		// A rejected login, an account login Screen Sharing does not
		// offer, or a login screen that refused the password: the daemon
		// reports each as DEVICE_ERROR with this text.
		return nil, daemon.DriverInfo{}, nil, err
	}
	info := daemon.DriverInfo{
		DriverName: "computer",
		Device:     buildDeviceReport(d),
		App:        buildAppReport(d),
		Platform:   computer.Platform,
		Kind:       computer.Kind,
		Name:       h.Name,
	}
	return d, info, d.Close, nil
}
