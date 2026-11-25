package cli

// daemon_wiring.go is the only bridge between pkg/cli and pkg/daemon: it
// adapts upstream's driver factory (CreateDriver + RunConfig) and device
// discovery (collectDevices) to the daemon.Deps interface. Nothing in the
// daemon package imports pkg/cli, so upstream rebases only touch this file
// when RunConfig or CreateDriver change shape.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/computer"
	"github.com/devicelab-dev/maestro-runner/pkg/emulator"
	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
	"github.com/devicelab-dev/maestro-runner/pkg/version"
)

// HubDriverDeps discovers local TVs before the hub accepts its first request.
func HubDriverDeps() daemon.Deps {
	cachedTVs()
	return daemonDeps()
}

// UpstreamVersion is the maestro-runner release this fork is based on.
const UpstreamVersion = "1.1.26"

// daemonDeps builds the dependencies the daemon server needs from the CLI's
// driver and device code.
func daemonDeps() daemon.Deps {
	return daemon.Deps{
		Version:            version.Version,
		NewDriver:          daemonNewDriver,
		ListDevices:        daemonListDevices,
		NormalizeHierarchy: NormalizeHierarchy,
		IOSCatalog:         simulator.ListCatalog,
		AndroidCatalog:     emulator.ListCatalog,
		CreateSimulator:    simulator.CreateSimulator,
		CreateAVD:          emulator.CreateAVD,
		InstallSystemImage: emulator.InstallSystemImage,
		DeleteSimulator:    simulator.DeleteSimulator,
		DeleteAVD:          emulator.DeleteAVD,
		InstallApp:         daemonInstallApp,
		UninstallApp:       daemonUninstallApp,
		AppID:              appIDOf,
		TVInfo:             physicalTVInfo,
		Providers:          map[string]func() (cloudprov.RemoteProvider, error){"devicefarm": deviceFarmProvider(devicefarm.Options{})},
		NewRemoteDriver:    newRemoteAppiumDriver,
	}
}

// deviceFarmProvider constructs the Device Farm provider from opts and the
// process's AWS credential chain.
func deviceFarmProvider(opts devicefarm.Options) func() (cloudprov.RemoteProvider, error) {
	return func() (cloudprov.RemoteProvider, error) {
		p, err := devicefarm.NewProvider(context.Background(), opts)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
}

// iosUDIDRe matches both 40-hex (pre-2018) and 8-16 (modern) UDIDs and
// simulator UUIDs.
var iosUDIDRe = regexp.MustCompile(`^([0-9A-Fa-f]{40}|[0-9A-Fa-f]{8}-[0-9A-Fa-f]{16}|[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})$`)

// guessPlatform infers a platform from a device id when --platform was not
// given: look the id up among visible devices, else fall back on its shape.
func guessPlatform(id string) string {
	if id == computer.DeviceID { // maestro-d: the computer device
		return computer.Platform
	}
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
	if platform == "" && isMockDevice(id) {
		platform = "mock"
	}
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
		OutputDir:          driverOutputDir(cfg),
		ShutdownAfter:      false,
		// An attached device streams and records: a physical iPhone's
		// capture starts before WebDriverAgent (ios_capture.go).
		ScreenCapture: true,
	}
	if rc.AppiumURL == "" {
		rc.AppiumURL = "http://127.0.0.1:4723"
	}
	// The CLI sends its own MAESTRO_TEAM_ID / DEVELOPMENT_TEAM as teamId; a
	// REST or JavaScript attach sends none, so the daemon's environment
	// supplies it. WebDriverAgent is signed with that team and its device
	// build is cached under it.
	if rc.TeamID == "" {
		rc.TeamID = teamIDFromEnv(os.LookupEnv)
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
	if v, ok := cfg.Extra["noFlutterFallback"].(bool); ok {
		rc.NoFlutterFallback = v
	}
	if v, ok := cfg.Extra["capsFile"].(string); ok {
		rc.CapsFile = v
	}
	if v, ok := cfg.Extra["newCommandTimeout"].(float64); ok {
		rc.NewCommandTimeout = int(v)
	}
	return rc
}

// teamIDFromEnv returns the Apple team the daemon was started with:
// MAESTRO_TEAM_ID, else DEVELOPMENT_TEAM, the variables the CLI's --team-id
// reads. Empty values are skipped.
func teamIDFromEnv(lookup func(string) (string, bool)) string {
	for _, name := range []string{"MAESTRO_TEAM_ID", "DEVELOPMENT_TEAM"} {
		if v, ok := lookup(name); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// daemonNewDriver opens a driver for one device through upstream's
// CreateDriver and describes it for the session report.
func daemonNewDriver(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
	if isComputerAttach(id, cfg) {
		return computerNewDriver(ctx, id, cfg)
	}
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
		case isTVDriver(driver):
			info.Kind = "tv"
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

// isTVDriver reports whether driver runs a physical Apple TV.
func isTVDriver(driver core.Driver) bool {
	tv, ok := driver.(interface{ IsTV() bool })
	return ok && tv.IsTV()
}

// envMockDevices lists mock devices for tests (MAESTRO_D_MOCK_DEVICES=
// mock-1,mock-2). The mock driver attaches any mock-N id but lists none;
// with this set, those ids are listed as ready mock devices, so lease
// selectors and id normalization see them. Read only when set.
const envMockDevices = "MAESTRO_D_MOCK_DEVICES"

// mockDevices returns the listed mock devices, if any.
func mockDevices() []daemon.DeviceInfo {
	v := os.Getenv(envMockDevices)
	if v == "" {
		return nil
	}
	var out []daemon.DeviceInfo
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, daemon.DeviceInfo{ID: id, Platform: "mock", Kind: "device", Name: id, State: "device", Ready: true})
		}
	}
	return out
}

// isMockDevice reports whether id is one of the listed mock devices.
func isMockDevice(id string) bool {
	for _, d := range mockDevices() {
		if d.ID == id {
			return true
		}
	}
	return false
}

// daemonListDevices adapts `maestro-runner devices` discovery.
func daemonListDevices(platform string) []daemon.DeviceInfo {
	platform = strings.ToLower(platform)
	mocks := mockDevices()
	if platform == "mock" {
		return mocks
	}
	if platform == "web" {
		return nil
	}
	if platform == computer.Platform {
		if mac, ok := computerEntry(); ok {
			return []daemon.DeviceInfo{mac}
		}
		return nil
	}
	entries := collectDevices(platform, true)
	out := make([]daemon.DeviceInfo, 0, len(entries)+len(mocks)+1)
	if platform == "" {
		out = append(out, mocks...)
		if mac, ok := computerEntry(); ok {
			out = append(out, mac)
		}
	}
	for _, e := range entries {
		out = append(out, daemon.DeviceInfo{
			ID: e.ID, Platform: e.Platform, Kind: e.Kind, Name: e.Name, Model: e.Model,
			OSVersion: e.OSVersion, State: e.State, Ready: e.Ready,
		})
	}
	return out
}

// driverOutputDir is where a daemon session's driver writes beside the
// report (the opt-in Android trace): the caller's outputDir, else the
// report directory the daemon chose.
func driverOutputDir(cfg daemon.AttachConfig) string {
	if cfg.OutputDir != "" {
		return cfg.OutputDir
	}
	return cfg.ReportDir
}
