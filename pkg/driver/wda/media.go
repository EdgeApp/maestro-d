package wda

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/config"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// addMedia adds photos/videos to the target's Photos library.
//
// On a simulator `xcrun simctl addmedia` writes real PHAssets the app's
// picker can select. A real device has no host-side path to its Photos
// database, so each file goes to the bundled WebDriverAgent's /wda/addMedia,
// which adds it on the device through PhotoKit. (#131)
func (d *Driver) addMedia(step *flow.AddMediaStep) *core.CommandResult {
	if err := core.ValidateMediaFiles(step.Files); err != nil {
		return errorResult(err, err.Error())
	}
	for _, f := range step.Files {
		if _, err := os.Stat(f); err != nil {
			return errorResult(err, fmt.Sprintf("Media file not found: %s", f))
		}
	}

	if d.info != nil && d.info.IsSimulator {
		args := append([]string{"simctl", "addmedia", d.udid}, step.Files...)
		out, err := execCommand("xcrun", args...).CombinedOutput()
		if err != nil {
			return errorResult(fmt.Errorf("simctl addmedia failed: %w", err),
				fmt.Sprintf("Failed to add media: %v: %s", err, string(out)))
		}
		return successResult(fmt.Sprintf("Added %d media file(s) to the simulator", len(step.Files)), nil)
	}

	fullAccess := false
	for _, f := range step.Files {
		data, err := os.ReadFile(f)
		if err != nil {
			return errorResult(err, fmt.Sprintf("Failed to read media file %s: %v", f, err))
		}
		mime, _ := core.MediaMIMEType(f)
		access, err := d.client.AddMedia(filepath.Base(f), mime, data)
		fullAccess = fullAccess || access == "full"
		if err != nil {
			if strings.Contains(err.Error(), "Unhandled endpoint") {
				return errorResult(err, staleWDAAddMediaMessage(d.wdaBuildDir))
			}
			return errorResult(err, fmt.Sprintf("Failed to add media %s: %v", filepath.Base(f), err))
		}
	}
	msg := fmt.Sprintf("Added %d media file(s) to the device through PhotoKit", len(step.Files))
	if fullAccess {
		msg += "; WebDriverAgentRunner holds full Photos access, since a runner built with Xcode 26 or earlier " +
			"has no add-only usage description and iOS asks for the whole library instead"
	}
	return successResult(msg, nil)
}

// staleWDAAddMediaMessage explains a WebDriverAgent without /wda/addMedia,
// naming buildDir, the build this device runs, when it is known. The cache
// key does not change when the bundled source does, so a device build made
// before the route existed is reused until it is deleted: hosts that cannot
// sign a new build (no signing identity) depend on reusing one.
func staleWDAAddMediaMessage(buildDir string) string {
	target := fmt.Sprintf("the device's build under %s (device-ios<version>-team<team>)",
		filepath.Join(config.GetCacheDir(), "wda-builds"))
	if buildDir != "" {
		target = buildDir
	}
	return fmt.Sprintf("addMedia on a real device needs the WebDriverAgent bundled with maestro-d, "+
		"and this device's WebDriverAgent has no /wda/addMedia: its build predates on-device addMedia. "+
		"Delete %s so the next attach rebuilds it. "+
		"A WebDriverAgent installed by `maestro-d wda update` (upstream) never has this route.", target)
}
