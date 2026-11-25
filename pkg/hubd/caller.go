package hubd

import (
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"strings"
)

// TCP callers on a hub, including loopback, must follow its remote policy.
func (s *Server) TCPCaller(addr string, clientOutputs bool) daemon.Caller {
	return daemon.Caller{Remote: true, Addr: addr, ClientOutputs: clientOutputs, AllowShell: s.cfg.AllowRemoteShell}
}

// checkRemoteAttach refuses the attach settings that would give a remote
// caller the hub's host: a web session, a report or flow directory, a
// browser profile, an app file that is not a blob ref, or Appium (its
// driver, server URL or capabilities file).
func checkRemoteAttach(cfg *daemon.AttachConfig) *daemon.Error {
	if strings.EqualFold(cfg.Platform, daemon.ComputerPlatform) {
		return daemon.Forbidden("a remote caller cannot attach this computer: the computer device is local only").
			WithDetails("field", "platform")
	}
	if strings.EqualFold(cfg.Platform, "web") || isWebDriver(cfg.Driver) {
		return daemon.Forbidden("a remote caller cannot attach a web session: hubs share simulators, emulators and phones, not the host's browser").
			WithDetails("field", "platform")
	}
	if cfg.OutputDir != "" {
		return daemon.Forbidden("a remote caller cannot set outputDir (a directory on the daemon's host)").WithDetails("field", "outputDir")
	}
	if cfg.FlowDir != "" {
		return daemon.Forbidden("a remote caller cannot set flowDir (a directory on the daemon's host)").WithDetails("field", "flowDir")
	}
	if cfg.AppFile != "" && !strings.HasPrefix(cfg.AppFile, "blob:") {
		return daemon.Forbidden("appFile %s is a path on the daemon's host; a remote caller must upload it (a blob: ref)", cfg.AppFile).
			WithDetails("field", "appFile")
	}
	if v, ok := cfg.Extra["browser"]; ok && v != "" && v != nil {
		return daemon.Forbidden("a remote caller cannot set extra.browser (a program on the daemon's host)").
			WithDetails("field", "extra.browser")
	}
	if v, ok := cfg.Extra["userDataDir"]; ok && v != "" && v != nil {
		return daemon.Forbidden("a remote caller cannot set extra.userDataDir (a browser profile on the daemon's host)").
			WithDetails("field", "extra.userDataDir")
	}
	// Appium capabilities name host files and programs (appium:app,
	// appium:chromedriverExecutable, …) and appiumUrl points the hub at
	// any address it can reach, so a remote caller gets no Appium at all.
	if isAppiumDriver(cfg.Driver) {
		return daemon.Forbidden("a remote caller cannot attach with the Appium driver (its capabilities reach the daemon's host)").
			WithDetails("field", "driver")
	}
	if cfg.AppiumURL != "" {
		return daemon.Forbidden("a remote caller cannot set appiumUrl (an Appium server the daemon would connect to)").
			WithDetails("field", "appiumUrl")
	}
	if v, ok := cfg.Extra["capsFile"]; ok && v != "" && v != nil {
		return daemon.Forbidden("a remote caller cannot set extra.capsFile (Appium capabilities reach the daemon's host)").
			WithDetails("field", "extra.capsFile")
	}
	return nil
}

// isAppiumDriver matches the driver names upstream's CreateDriver routes to
// Appium: it lower-cases the name, so any case of "appium" counts.
func isAppiumDriver(driver string) bool {
	return strings.EqualFold(strings.TrimSpace(driver), "appium")
}

func isWebDriver(driver string) bool {
	switch strings.ToLower(driver) {
	case "cdp", "playwright", "browser", "chrome", "chromium":
		return true
	}
	return false
}
