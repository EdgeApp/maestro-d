// Package xcodecache holds the disk hygiene shared by the iOS runners that
// drive `xcodebuild test-without-building` (wda and devicelab_ios): per-run
// result bundles that are deleted when the runner stops, and pruning of the
// build caches those runners leave behind.
package xcodecache

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// NoIndexStoreSetting keeps xcodebuild from writing Index.noindex, which no
// runner build ever reads.
const NoIndexStoreSetting = "COMPILER_INDEX_STORE_ENABLE=NO"

// ResultsDirName is the directory under a runner's build directory that
// holds the result bundles of its test runs.
const ResultsDirName = "results"

var runnerSeq atomic.Int64

// NextRunnerSeq numbers a runner within this process, so two runners that
// share a build directory never share a result bundle or in-use stamp.
func NextRunnerSeq() int64 { return runnerSeq.Add(1) }

// ResultBundlePath is where one test run of a runner writes its result
// bundle: <buildDir>/results/<pid>-<seq>-<attempt>.xcresult.
func ResultBundlePath(buildDir string, seq int64, attempt int) string {
	return filepath.Join(buildDir, ResultsDirName,
		fmt.Sprintf("%d-%d-%d.xcresult", os.Getpid(), seq, attempt))
}

// RemoveResultBundles deletes the given result bundles. A bundle the
// killed xcodebuild was still closing is retried once.
func RemoveResultBundles(paths []string) {
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			time.Sleep(500 * time.Millisecond)
			_ = os.RemoveAll(p)
		}
	}
}

// RemoveOrphanResults deletes the result bundles under buildDir/results
// whose owning process is no longer alive: runs that ended without Stop.
func RemoveOrphanResults(buildDir string) {
	dir := filepath.Join(buildDir, ResultsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, ok := ownerPID(e.Name())
		if !ok || pidAlive(pid) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// ownerPID reads the leading "<pid>-" of a result bundle or in-use stamp name.
func ownerPID(name string) (int, bool) {
	head, _, found := strings.Cut(name, "-")
	if !found {
		return 0, false
	}
	pid, err := strconv.Atoi(head)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}
