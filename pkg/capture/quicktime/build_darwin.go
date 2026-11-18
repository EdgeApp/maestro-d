//go:build darwin

package quicktime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/config"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

const (
	helperName   = "MaestroDCapture"
	helperSource = helperName + ".swift"
	helperPlist  = "Info.plist"
	// keepBuilds is how many helper builds the cache keeps.
	keepBuilds = 2
	// signIdentityEnv names a codesign identity to sign the helper with.
	signIdentityEnv = "MAESTRO_D_CAPTURE_SIGN_IDENTITY"
	buildTimeout    = 5 * time.Minute
)

// Where the helper's source and its builds live (seams for the tests).
var (
	helperSourceDir = func() string { return filepath.Join(config.GetDriversDir("ios"), helperName) }
	helperCacheDir  = func() string { return filepath.Join(config.GetCacheDir(), "capture-helper") }
)

var (
	buildMu  sync.Mutex
	builtApp string
)

// ensureHelperApp returns the helper app, building it on the process's
// first use (a failed build is tried again next time).
func ensureHelperApp() (string, error) {
	buildMu.Lock()
	defer buildMu.Unlock()
	if builtApp != "" {
		return builtApp, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	app, err := buildHelper(ctx, helperSourceDir(), helperCacheDir())
	if err != nil {
		return "", err
	}
	builtApp = app
	return app, nil
}

// identityLine is one valid identity in `security find-identity` output:
// `  1) <40-hex SHA-1> "<name>"`.
var identityLine = regexp.MustCompile(`^\s*\d+\)\s+([0-9A-Fa-f]{40})\s+"([^"]+)"`)

// signingIdentity picks the codesign identity: $MAESTRO_D_CAPTURE_SIGN_IDENTITY,
// else the first valid "Apple Development:" identity, else ad-hoc ("-"),
// with a label for the log.
func signingIdentity(ctx context.Context) (identity, label string) {
	if v := strings.TrimSpace(os.Getenv(signIdentityEnv)); v != "" {
		return v, v + " (" + signIdentityEnv + ")"
	}
	out, err := runCommand(ctx, "security", "find-identity", "-p", "codesigning", "-v")
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			m := identityLine.FindStringSubmatch(line)
			if m != nil && strings.HasPrefix(m[2], "Apple Development:") {
				return strings.ToUpper(m[1]), m[2]
			}
		}
	}
	return "-", "ad-hoc (no Apple Development identity; every rebuild asks for Camera access again)"
}

// cacheKey is sha256(source ‖ Info.plist ‖ swiftc --version ‖ identity),
// hex.
func cacheKey(source, plist, swiftc []byte, identity string) string {
	h := sha256.New()
	for _, part := range [][]byte{source, plist, swiftc, []byte(identity)} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildHelper returns <cache>/<key[:12]>/MaestroDCapture.app, building and
// signing it there when it is missing: the build goes to a temporary
// directory first and is renamed into place, and only the newest
// keepBuilds builds are kept.
func buildHelper(ctx context.Context, srcDir, cacheDir string) (string, error) {
	source, err := os.ReadFile(filepath.Join(srcDir, helperSource))
	if err != nil {
		return "", err
	}
	plist, err := os.ReadFile(filepath.Join(srcDir, helperPlist))
	if err != nil {
		return "", err
	}
	swiftc, err := runCommand(ctx, "xcrun", "swiftc", "--version")
	if err != nil {
		return "", fmt.Errorf("xcrun swiftc --version: %w: %s", err, strings.TrimSpace(string(swiftc)))
	}
	identity, label := signingIdentity(ctx)
	key := cacheKey(source, plist, swiftc, identity)
	dir := filepath.Join(cacheDir, key[:12])
	app := filepath.Join(dir, helperName+".app")
	if _, err := os.Stat(filepath.Join(app, "Contents", "MacOS", helperName)); err == nil {
		touch(dir)
		pruneBuilds(cacheDir, dir)
		return app, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(cacheDir, ".build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	built := filepath.Join(tmp, helperName+".app")
	macos := filepath.Join(built, "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(built, "Contents", helperPlist), plist, 0o644); err != nil {
		return "", err
	}
	logger.Info("building the capture helper (%s)", app)
	if out, err := runCommand(ctx, "xcrun", "swiftc", "-O",
		"-framework", "AVFoundation", "-framework", "CoreMediaIO", "-framework", "CoreMedia",
		"-o", filepath.Join(macos, helperName), filepath.Join(srcDir, helperSource)); err != nil {
		return "", fmt.Errorf("swiftc: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := runCommand(ctx, "codesign", "--force", "--sign", identity, "--timestamp=none", built); err != nil {
		return "", fmt.Errorf("codesign: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(built, app); err != nil {
		// Another process may have built the same key meanwhile.
		if _, serr := os.Stat(filepath.Join(app, "Contents", "MacOS", helperName)); serr != nil {
			return "", errors.Join(err, serr)
		}
	}
	touch(dir)
	pruneBuilds(cacheDir, dir)
	logger.Info("built the capture helper %s, signed with %s", app, label)
	return app, nil
}

func touch(dir string) {
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
}

// pruneBuilds keeps the newest keepBuilds builds (by modification time;
// keep is always kept) and removes the rest. In-progress builds (dot
// directories) are left alone.
func pruneBuilds(cacheDir, keep string) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	type build struct {
		path string
		mod  time.Time
	}
	var builds []build
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(cacheDir, e.Name())
		if p == keep {
			continue
		}
		if info, err := e.Info(); err == nil {
			builds = append(builds, build{p, info.ModTime()})
		}
	}
	sort.Slice(builds, func(i, j int) bool { return builds[i].mod.After(builds[j].mod) })
	for i, b := range builds {
		if i >= keepBuilds-1 {
			_ = os.RemoveAll(b.path)
		}
	}
}
