package xcodecache

import (
	"fmt"
	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// LastUsedFile is touched in a cache directory each time a runner uses it.
	LastUsedFile = ".last-used"
	// BuiltHereFile is touched in a WDA build directory when this host
	// compiled it, which proves the host can sign for its team.
	BuiltHereFile = ".built-here"
	// inUsePrefix starts the stamp a running runner keeps in its directory:
	// .in-use.<pid>-<seq>.
	inUsePrefix = ".in-use."
	// DefaultMaxAge is how long an unused cache directory is kept.
	DefaultMaxAge = 30 * 24 * time.Hour
)

// teamRe reads the team of a WDA build directory name such as
// device-ios26.3.1-teamABC or device-tvosunknown-teamABC-bundlecom.x.
var teamRe = regexp.MustCompile(`-team([^-]+)`)

// StampUse records that a runner uses dir: it touches .last-used and
// writes .in-use.<pid>-<seq>. The returned func removes the in-use stamp.
func StampUse(dir string, seq int64) func() {
	removeDeadStamps(dir)
	touch(filepath.Join(dir, LastUsedFile))
	inUse := filepath.Join(dir, fmt.Sprintf("%s%d-%d", inUsePrefix, os.Getpid(), seq))
	touch(inUse)
	return func() { _ = os.Remove(inUse) }
}

// MarkBuiltHere records that this host compiled the build in dir.
func MarkBuiltHere(dir string) { touch(filepath.Join(dir, BuiltHereFile)) }

// Touch updates (or creates) dir's .last-used stamp.
func Touch(dir string) { touch(filepath.Join(dir, LastUsedFile)) }

func touch(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		if f, cerr := os.Create(path); cerr == nil {
			_ = f.Close()
		}
	}
}

// Pruned is one cache directory PruneBuildCache deleted, or would delete.
type Pruned struct {
	Path   string
	Size   int64
	Reason string
}

// PruneBuildCache deletes the directories directly under root, other than
// keep, that no runner has used for maxAge and no live runner holds. A
// real-device WDA build (device-…) is deleted only when another build of
// its team has a .built-here newer than its last use: a host that cannot
// sign never compiles, so it never loses the device build it depends on.
// With dryRun nothing is deleted. root that does not exist is a no-op.
func PruneBuildCache(root, keep string, maxAge time.Duration, dryRun bool) []Pruned {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	builtHere := map[string]time.Time{} // team → newest .built-here
	for _, e := range entries {
		if t := modTime(filepath.Join(root, e.Name(), BuiltHereFile)); !t.IsZero() {
			if team := teamOf(e.Name()); team != "" && t.After(builtHere[team]) {
				builtHere[team] = t
			}
		}
	}
	var out []Pruned
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if !e.IsDir() || filepath.Clean(dir) == filepath.Clean(keep) {
			continue
		}
		used := LastUsed(dir)
		if time.Since(used) < maxAge || HeldByLiveRunner(dir) {
			continue
		}
		reason := fmt.Sprintf("build cache unused since %s", used.Format("2006-01-02"))
		if strings.HasPrefix(e.Name(), "device-") {
			team := teamOf(e.Name())
			if team == "" || !builtHere[team].After(used) {
				continue
			}
			reason += fmt.Sprintf("; team %s rebuilt here since", team)
		}
		size := fsage.Size(dir)
		if !dryRun {
			if err := os.RemoveAll(dir); err != nil {
				continue
			}
		}
		out = append(out, Pruned{Path: dir, Size: size, Reason: reason})
	}
	return out
}

// LastUsed is dir's .last-used time, else its newest file's.
func LastUsed(dir string) time.Time {
	if t := modTime(filepath.Join(dir, LastUsedFile)); !t.IsZero() {
		return t
	}
	return fsage.Newest(dir)
}

// removeDeadStamps deletes the in-use stamps of runners whose process is
// gone (a daemon killed before Stop).
func removeDeadStamps(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), inUsePrefix)
		if !ok {
			continue
		}
		if pid, ok := ownerPID(rest); ok && !pidAlive(pid) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// HeldByLiveRunner reports whether a live runner holds dir (an
// .in-use.<pid>-<seq> stamp whose process is alive).
func HeldByLiveRunner(dir string) bool {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), inUsePrefix)
		if !ok {
			continue
		}
		if pid, ok := ownerPID(rest); ok && pidAlive(pid) {
			return true
		}
	}
	return false
}

func teamOf(name string) string {
	if m := teamRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
