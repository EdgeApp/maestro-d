package wda

import (
	"errors"
	"fmt"
	"os"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// errPhysicalAddMedia says what a physical iPhone can and cannot do about
// media, so the hint never sends anyone to a driver that refuses the device.
var errPhysicalAddMedia = errors.New("addMedia is not supported on a physical iPhone: " +
	"iOS gives the host no way to write to a device's Photos library, and the devicelab iOS driver " +
	"(which adds media on-device) runs only on simulators. " +
	"Run the flow on an iOS simulator, or put the media in the iPhone's Photos library before the flow " +
	"(AirDrop, iCloud Photos, or a Finder sync)")

// addMedia adds photos/videos to the target's Photos library.
//
// On the WDA driver this is simulator-only: `xcrun simctl addmedia` writes real
// PHAssets the app's picker can select. A physical iPhone has no host-side path
// to its Photos library (an Apple restriction), and WebDriverAgent has no
// endpoint for it. The devicelab iOS runner's PhotoKit path cannot stand in
// either: this fork's devicelab iOS driver starts only on simulators, so a
// physical iPhone is always on WDA. (#131)
func (d *Driver) addMedia(step *flow.AddMediaStep) *core.CommandResult {
	if err := core.ValidateMediaFiles(step.Files); err != nil {
		return errorResult(err, err.Error())
	}
	for _, f := range step.Files {
		if _, err := os.Stat(f); err != nil {
			return errorResult(err, fmt.Sprintf("Media file not found: %s", f))
		}
	}

	if d.info == nil || !d.info.IsSimulator {
		err := errPhysicalAddMedia
		return errorResult(err, err.Error())
	}

	args := append([]string{"simctl", "addmedia", d.udid}, step.Files...)
	out, err := execCommand("xcrun", args...).CombinedOutput()
	if err != nil {
		return errorResult(fmt.Errorf("simctl addmedia failed: %w", err),
			fmt.Sprintf("Failed to add media: %v: %s", err, string(out)))
	}
	return successResult(fmt.Sprintf("Added %d media file(s) to the simulator", len(step.Files)), nil)
}
