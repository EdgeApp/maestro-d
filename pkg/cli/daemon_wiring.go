package cli

// daemon_wiring.go is the only bridge between pkg/cli and pkg/daemon: it
// adapts upstream's driver factory (CreateDriver + RunConfig) and device
// discovery (collectDevices) to the daemon.Deps interface. Nothing in the
// daemon package imports pkg/cli, so upstream rebases only touch this file
// when RunConfig or CreateDriver change shape.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/emulator"
	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

// UpstreamVersion is the maestro-runner release this fork is based on.
const UpstreamVersion = "1.1.26"

// daemonDeps builds the dependencies the daemon server needs from the CLI's
// driver and device code.
func daemonDeps() daemon.Deps {
	return daemon.Deps{
		Version:            Version,
		NewDriver:          daemonNewDriver,
		ListDevices:        daemonListDevices,
		NormalizeHierarchy: NormalizeHierarchy,
	}
}

// iosUDIDRe matches both 40-hex (pre-2018) and 8-16 (modern) UDIDs and
// simulator UUIDs.
var iosUDIDRe = regexp.MustCompile(`^([0-9A-Fa-f]{40}|[0-9A-Fa-f]{8}-[0-9A-Fa-f]{16}|[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})$`)

// guessPlatform infers a platform from a device id when --platform was not
// given: look the id up among visible devices, else fall back on its shape.
func guessPlatform(id string) string {
	for _, d := range collectDevices("", true) {
		if d.ID == id {
			return d.Platform
		}
	}
	if iosUDIDRe.MatchString(id) {
		return "ios"
	}
	if strings.HasPrefix(id, "emulator-") || strings.Contains(id, ":") || id != "" {
		return "android"
	}
	return ""
}

// attachToRunConfig maps the daemon's AttachConfig onto upstream's RunConfig
// so CreateDriver behaves exactly as it does for `test`.
func attachToRunConfig(id string, cfg daemon.AttachConfig) *RunConfig {
	platform := strings.ToLower(cfg.Platform)
	if platform == "" && cfg.Driver != "mock" {
		platform = guessPlatform(id)
	}
	driver := cfg.Driver
	if driver == "" {
		driver = "uiautomator2" // upstream's flag default; resolveDriverName maps it per platform
	}
	rc := &RunConfig{
		Platform:           platform,
		Devices:            []string{id},
		Driver:             driver,
		AppFile:            cfg.AppFile,
		AppID:              cfg.AppID,
		AppiumURL:          cfg.AppiumURL,
		Env:                cfg.Env,
		TeamID:             cfg.TeamID,
		WDABundleID:        cfg.WDABundleID,
		NoAppInstall:       cfg.NoAppInstall,
		NoDriverInstall:    cfg.NoDriverInstall,
		DriverStartTimeout: cfg.DriverStartTimeout,
		ConditionTimeout:   cfg.ConditionTimeout,
		StepDelay:          cfg.StepDelay,
		TypingFrequency:    cfg.TypingFrequency,
		WaitForIdleTimeout: 200,
		Artifacts:          parseArtifactMode(cfg.Artifacts),
		OutputDir:          cfg.OutputDir,
		ShutdownAfter:      false,
	}
	// The web driver's start URL travels as the appId, which is how a flow
	// header names it; attach takes it as --url.
	if rc.AppID == "" && cfg.URL != "" {
		rc.AppID = cfg.URL
	}
	if rc.AppiumURL == "" {
		rc.AppiumURL = "http://127.0.0.1:4723"
	}
	if cfg.WaitForIdleTimeout != nil {
		rc.WaitForIdleTimeout = *cfg.WaitForIdleTimeout
	}
	// Web driver options travel in Extra (the JS/CLI pass them through).
	if v, ok := cfg.Extra["headed"].(bool); ok {
		rc.Headed = v
	}
	if v, ok := cfg.Extra["browser"].(string); ok {
		rc.Browser = v
	}
	if v, ok := cfg.Extra["userDataDir"].(string); ok {
		rc.UserDataDir = v
	}
	if v, ok := cfg.Extra["windowSize"].(string); ok {
		rc.WindowSize = v
	}
	if v, ok := cfg.Extra["androidTcpForward"].(bool); ok {
		rc.AndroidTCPForward = v
	}
	if v, ok := cfg.Extra["capsFile"].(string); ok {
		rc.CapsFile = v
	}
	if v, ok := cfg.Extra["newCommandTimeout"].(float64); ok {
		rc.NewCommandTimeout = int(v)
	}
	return rc
}

// daemonNewDriver opens a driver for one device through upstream's
// CreateDriver and describes it for the session report.
func daemonNewDriver(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
	rc := attachToRunConfig(id, cfg)
	if rc.Platform == "" && strings.ToLower(rc.Driver) != "mock" {
		return nil, daemon.DriverInfo{}, nil, fmt.Errorf("cannot infer platform for device %q; pass --platform", id)
	}
	if rc.CapsFile != "" {
		caps, err := loadCapabilities(rc.CapsFile)
		if err != nil {
			return nil, daemon.DriverInfo{}, nil, err
		}
		rc.Capabilities = caps
	}
	driver, cleanup, err := CreateDriver(rc)
	if err != nil {
		return nil, daemon.DriverInfo{}, nil, err
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	info := daemon.DriverInfo{
		DriverName: resolveDriverName(rc, rc.Platform),
		Device:     buildDeviceReport(driver),
		App:        buildAppReport(driver),
		Platform:   strings.ToLower(rc.Platform),
	}
	if pi := driver.GetPlatformInfo(); pi != nil {
		if pi.Platform != "" {
			info.Platform = strings.ToLower(pi.Platform)
		}
		info.Name = pi.DeviceName
		switch {
		case pi.IsSimulator && info.Platform == "ios":
			info.Kind = "simulator"
		case pi.IsSimulator:
			info.Kind = "emulator"
		default:
			info.Kind = "device"
		}
	}
	if info.Kind == "" || info.Kind == "device" {
		switch info.Platform {
		case "ios":
			if simulator.IsSimulator(id) {
				info.Kind = "simulator"
			}
		case "android":
			if emulator.IsEmulator(id) {
				info.Kind = "emulator"
			}
		}
	}
	if info.Kind == "" {
		info.Kind = "device"
	}
	return driver, info, cleanup, nil
}

// daemonListDevices adapts `maestro-runner devices` discovery.
func daemonListDevices(platform string) []daemon.DeviceInfo {
	platform = strings.ToLower(platform)
	if platform == "mock" || platform == "web" {
		return nil
	}
	entries := collectDevices(platform, true)
	out := make([]daemon.DeviceInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, daemon.DeviceInfo{
			ID: e.ID, Platform: e.Platform, Kind: e.Kind, Name: e.Name,
			OSVersion: e.OSVersion, State: e.State, Ready: e.Ready,
		})
	}
	return out
}
