package emulator

// catalog.go (maestro-d): what `avdmanager create avd` can make here — the
// system images sdkmanager reports and the hardware profiles avdmanager
// knows — and the default image for the host's ABI.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultProfile is the hardware profile `device create` uses.
const DefaultProfile = "pixel_8"

// DefaultImageTag is the system-image tag the default image is chosen from.
const DefaultImageTag = "google_apis"

// Catalog lists system images and hardware profiles.
type Catalog struct {
	Images   []SystemImage `json:"images"`
	Profiles []string      `json:"profiles"`
}

// SystemImage is one sdkmanager system-image package.
type SystemImage struct {
	// ID is the package path, e.g. system-images;android-35;google_apis;arm64-v8a.
	ID          string `json:"id"`
	API         string `json:"api"` // android-35
	Tag         string `json:"tag"` // google_apis
	ABI         string `json:"abi"` // arm64-v8a
	Description string `json:"description,omitempty"`
	Installed   bool   `json:"installed"`
}

const sdkInstallHint = "install the Android SDK command-line tools (Android Studio > Settings > SDK Manager > SDK Tools > " +
	"Android SDK Command-line Tools, or `brew install --cask android-commandlinetools`) and set ANDROID_HOME"

// findSDKManager locates sdkmanager the way FindAVDManagerBinary finds
// avdmanager: under ANDROID_HOME / ANDROID_SDK_ROOT, then on PATH.
func findSDKManager() (string, error) {
	if home := getAndroidHome(); home != "" {
		for _, p := range []string{
			filepath.Join(home, "cmdline-tools", "latest", "bin", "sdkmanager"),
			filepath.Join(home, "tools", "bin", "sdkmanager"),
		} {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	if p, err := execLookPath("sdkmanager"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("sdkmanager not found; %s", sdkInstallHint)
}

// findAVDManager is FindAVDManagerBinary with an error that says how to
// install the tool.
func findAVDManager() (string, error) {
	p, err := FindAVDManagerBinary()
	if err != nil {
		return "", fmt.Errorf("avdmanager not found; %s", sdkInstallHint)
	}
	return p, nil
}

// ListCatalog runs `sdkmanager --list_installed` (or `--list` with
// available, which adds the images that can be downloaded) and
// `avdmanager list device -c`.
func ListCatalog(available bool) (*Catalog, error) {
	sdk, err := findSDKManager()
	if err != nil {
		return nil, err
	}
	avd, err := findAVDManager()
	if err != nil {
		return nil, err
	}
	flag := "--list_installed"
	if available {
		flag = "--list"
	}
	out, err := execCommand(sdk, flag).Output()
	if err != nil {
		return nil, fmt.Errorf("sdkmanager %s: %w", flag, err)
	}
	prof, err := execCommand(avd, "list", "device", "-c").Output()
	if err != nil {
		return nil, fmt.Errorf("avdmanager list device: %w", err)
	}
	return &Catalog{Images: parseSDKImages(string(out)), Profiles: parseProfiles(string(prof))}, nil
}

// parseSDKImages reads the system-image rows of `sdkmanager --list` or
// `--list_installed`. Rows under "Installed packages:" are installed; rows
// under "Available Packages:" are not, unless also installed.
func parseSDKImages(out string) []SystemImage {
	images := []SystemImage{}
	index := map[string]int{}
	installed := false
	for _, line := range strings.FieldsFunc(out, func(r rune) bool { return r == '\n' || r == '\r' }) {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Installed packages:"):
			installed = true
			continue
		case strings.HasPrefix(trimmed, "Available Packages:"), strings.HasPrefix(trimmed, "Available Updates:"):
			installed = false
			continue
		}
		cols := strings.Split(trimmed, "|")
		id := strings.TrimSpace(cols[0])
		if len(cols) < 2 || !strings.HasPrefix(id, "system-images;") {
			continue
		}
		parts := strings.Split(id, ";")
		if len(parts) != 4 {
			continue
		}
		img := SystemImage{ID: id, API: parts[1], Tag: parts[2], ABI: parts[3], Installed: installed}
		if len(cols) >= 3 {
			img.Description = strings.TrimSpace(cols[2])
		}
		if i, ok := index[id]; ok {
			images[i].Installed = images[i].Installed || installed
			continue
		}
		index[id] = len(images)
		images = append(images, img)
	}
	return images
}

// parseProfiles reads `avdmanager list device -c`: one profile id per line.
// avdmanager also prints warnings and errors about broken images there.
func parseProfiles(out string) []string {
	profiles := []string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Error:") || strings.HasPrefix(line, "Warning:") || strings.HasPrefix(line, "WARNING:") {
			continue
		}
		profiles = append(profiles, line)
	}
	return profiles
}

// HostABI is the system-image ABI that runs natively on a GOARCH.
func HostABI(goarch string) string {
	switch goarch {
	case "arm64":
		return "arm64-v8a"
	case "amd64":
		return "x86_64"
	case "386":
		return "x86"
	case "arm":
		return "armeabi-v7a"
	}
	return goarch
}

// Image returns the catalog entry for a package path.
func (c *Catalog) Image(id string) (SystemImage, bool) {
	for _, img := range c.Images {
		if img.ID == id {
			return img, true
		}
	}
	return SystemImage{}, false
}

// HasProfile reports whether avdmanager knows the hardware profile.
func (c *Catalog) HasProfile(id string) bool {
	for _, p := range c.Profiles {
		if p == id {
			return true
		}
	}
	return false
}

// DefaultImage is the installed google_apis image with the highest API
// level for the host's ABI.
func (c *Catalog) DefaultImage(goarch string) (SystemImage, bool) {
	abi := HostABI(goarch)
	var best SystemImage
	found := false
	for _, img := range c.Images {
		if !img.Installed || img.Tag != DefaultImageTag || img.ABI != abi {
			continue
		}
		if !found || apiLevel(img.API) > apiLevel(best.API) {
			best, found = img, true
		}
	}
	return best, found
}

// apiLevel turns "android-35" or "android-36.1" into a number; a code name
// ("android-Baklava") sorts below every numbered level.
func apiLevel(api string) float64 {
	v, err := strconv.ParseFloat(strings.TrimPrefix(api, "android-"), 64)
	if err != nil {
		return -1
	}
	return v
}
