package emulator

// avdmanager.go (maestro-d): create and delete AVDs with avdmanager, install
// system images with sdkmanager, and size an AVD's data partition.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

const (
	// DefaultDataSize is the data partition `device create` gives an AVD.
	DefaultDataSize = "6G"
	// MaxDataSizeBytes is the largest data partition `device create` allows.
	MaxDataSizeBytes = int64(10) << 30
	// dataSizeKey is the config.ini key the emulator sizes userdata from.
	dataSizeKey = "disk.dataPartition.size"
)

// AVDSpec describes an AVD to create.
type AVDSpec struct {
	Name     string
	Image    string // system-images;android-35;google_apis;arm64-v8a
	Profile  string // pixel_8
	DataSize string // as ParseDataSize normalizes it, e.g. 8G
}

var avdNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidAVDName reports whether avdmanager accepts name.
func ValidAVDName(name string) bool { return avdNameRe.MatchString(name) }

var dataSizeRe = regexp.MustCompile(`^(?i)(\d+)\s*([KMG]?)B?$`)

// ParseDataSize reads a data partition size such as 6G, 8GB, 2048M or a
// byte count. It returns the size in bytes and the value to write to
// config.ini, and refuses zero and anything above 10G.
func ParseDataSize(s string) (int64, string, error) {
	m := dataSizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, "", fmt.Errorf("invalid data size %q: use a number with an optional K, M or G suffix, e.g. 8G", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0, "", fmt.Errorf("invalid data size %q: must be greater than zero", s)
	}
	unit := strings.ToUpper(m[2])
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30}[unit]
	if n > MaxDataSizeBytes>>shift {
		return 0, "", fmt.Errorf("data size %s is above the 10G limit", s)
	}
	return n << shift, m[1] + unit, nil
}

// avdHome is the directory holding <name>.ini and <name>.avd/.
func avdHome() string {
	if v := os.Getenv("ANDROID_AVD_HOME"); v != "" {
		return v
	}
	if v := os.Getenv("ANDROID_USER_HOME"); v != "" {
		return filepath.Join(v, "avd")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".android", "avd")
	}
	return filepath.Join(home, ".android", "avd")
}

// AVDConfigPath is the AVD's config.ini: the ini file's path= entry, else
// <avd home>/<name>.avd/config.ini.
func AVDConfigPath(name string) string {
	dir := filepath.Join(avdHome(), name+".avd")
	if data, err := os.ReadFile(filepath.Join(avdHome(), name+".ini")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "path="); ok && v != "" {
				dir = v
				break
			}
		}
	}
	return filepath.Join(dir, "config.ini")
}

// setConfigValue replaces key's line in an ini file, or appends it.
func setConfigValue(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	set := false
	for i, line := range lines {
		if k, _, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			lines[i] = key + "=" + value
			set = true
		}
	}
	if !set {
		lines = append(lines, key+"="+value)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// lastLines keeps a tool's error output short.
func lastLines(out []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// CreateAVD runs `avdmanager create avd -n NAME -k IMAGE -d PROFILE` and
// writes the data partition size into the new AVD's config.ini.
func CreateAVD(spec AVDSpec) error {
	if !ValidAVDName(spec.Name) {
		return fmt.Errorf("invalid AVD name %q: use letters, digits, '.', '_' or '-'", spec.Name)
	}
	avd, err := findAVDManager()
	if err != nil {
		return err
	}
	cmd := execCommand(avd, "create", "avd", "-n", spec.Name, "-k", spec.Image, "-d", spec.Profile)
	// avdmanager asks whether to make a custom hardware profile.
	cmd.Stdin = strings.NewReader("no\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("avdmanager create avd %s: %v: %s", spec.Name, err, lastLines(out, 5))
	}
	if spec.DataSize != "" {
		if err := setConfigValue(AVDConfigPath(spec.Name), dataSizeKey, spec.DataSize); err != nil {
			return fmt.Errorf("AVD %s was created, but setting %s failed: %w", spec.Name, dataSizeKey, err)
		}
	}
	logger.Info("Created AVD %s [%s, %s, data %s]", spec.Name, spec.Image, spec.Profile, spec.DataSize)
	return nil
}

// DeleteAVD runs `avdmanager delete avd -n NAME`.
func DeleteAVD(name string) error {
	avd, err := findAVDManager()
	if err != nil {
		return err
	}
	if out, err := execCommand(avd, "delete", "avd", "-n", name).CombinedOutput(); err != nil {
		return fmt.Errorf("avdmanager delete avd %s: %v: %s", name, err, lastLines(out, 5))
	}
	logger.Info("Deleted AVD %s", name)
	return nil
}

// InstallSystemImage runs `sdkmanager --install IMAGE`, answering yes to
// its license prompts. Callers only do this when the user accepted the
// licenses.
func InstallSystemImage(image string) error {
	sdk, err := findSDKManager()
	if err != nil {
		return err
	}
	cmd := execCommand(sdk, "--install", image)
	cmd.Stdin = strings.NewReader(strings.Repeat("y\n", 32))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sdkmanager --install %s: %v: %s", image, err, lastLines(out, 5))
	}
	logger.Info("Installed %s", image)
	return nil
}
