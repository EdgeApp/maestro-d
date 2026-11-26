package daemon

// appbuild.go: the build installApp last installed from a URL, kept per
// device and app id in <home>/builds/<device>/<app id>/ beside the ETag
// record, so clearState on a physical iPhone or iPad, which can clear an
// app only by reinstalling it, has the build to reinstall
// (core.AppBuildUser).

import (
	"os"
	"path/filepath"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// buildsDirName is the kept builds' directory in the daemon's home.
const buildsDirName = "builds"

// safeName is name as one path element: unsafe runs become _, and an
// empty name, . or .. becomes _.
func safeName(name string) string {
	name = unsafePath.ReplaceAllString(name, "_")
	if name == "" || name == "." || name == ".." {
		return "_"
	}
	return name
}

// buildDir is where the build kept for appID on device id lives.
func buildDir(id, appID string) string {
	return filepath.Join(Home(), buildsDirName, safeName(id), safeName(appID))
}

// keptBuild is the build kept for appID on device id, or "" for none.
func keptBuild(id, appID string) string {
	installsMu.Lock()
	defer installsMu.Unlock()
	dir := buildDir(id, appID)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Type().IsRegular() {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// keepBuild moves file, the build just installed for appID on device id,
// into that pair's build directory in place of the one kept before. A
// build that cannot be kept is logged: the install itself succeeded.
func keepBuild(id, appID, file string) {
	installsMu.Lock()
	defer installsMu.Unlock()
	dir := buildDir(id, appID)
	next := dir + ".next"
	_ = os.RemoveAll(next)
	if err := os.MkdirAll(next, 0o700); err != nil {
		logger.Warn("keep build of %s for %s: %v", appID, id, err)
		return
	}
	if err := moveFile(file, filepath.Join(next, filepath.Base(file))); err != nil {
		logger.Warn("keep build of %s for %s: %v", appID, id, err)
		_ = os.RemoveAll(next)
		return
	}
	_ = os.RemoveAll(dir)
	if err := renameFile(next, dir); err != nil {
		logger.Warn("keep build of %s for %s: %v", appID, id, err)
		_ = os.RemoveAll(next)
	}
}

// forgetInstall drops the ETag and the build kept for appID on device id,
// once the app is uninstalled: the next installApp installs it again.
func (s *Server) forgetInstall(id, appID string) {
	s.noteInstalled(id, appID, "")
	installsMu.Lock()
	defer installsMu.Unlock()
	_ = os.RemoveAll(buildDir(id, appID))
}

// renameFile is os.Rename, a variable so a test can stand in a move across
// volumes.
var renameFile = os.Rename

// moveFile renames src to dst, copying when they are on different volumes.
func moveFile(src, dst string) error {
	if renameFile(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := writeDownload(dst, in); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// giveBuildSource hands a driver that reinstalls from a build the builds
// kept for its device.
func giveBuildSource(id string, driver core.Driver) {
	if u, ok := core.Unwrap(driver).(core.AppBuildUser); ok {
		u.SetAppBuildSource(func(appID string) string { return keptBuild(id, appID) })
	}
}
