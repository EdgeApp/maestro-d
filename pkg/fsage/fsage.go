// Package fsage answers how long a file tree has gone unused and how big it
// is, for the code that cleans up after maestro-d: the housekeeping sweep
// and the iOS runners' build-cache prune.
package fsage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errNewer = errors.New("newer file found")

// counts reports whether a file's time says anything about use. Directory
// times change when a child is deleted (as cleanup does), and *.lock files
// when a lock is merely probed, so neither counts.
func counts(d fs.DirEntry) bool {
	return !d.IsDir() && !strings.HasSuffix(d.Name(), ".lock")
}

// Newest is the latest modification time of a file under path; a tree
// with no such file falls back to path's own time, and a missing path
// gives the zero time.
func Newest(path string) time.Time {
	var newest time.Time
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !counts(d) {
			return nil
		}
		if fi, ierr := d.Info(); ierr == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		return nil
	})
	if newest.IsZero() {
		if fi, err := os.Stat(path); err == nil {
			newest = fi.ModTime()
		}
	}
	return newest
}

// IdleFor reports whether nothing under path changed within d. It stops
// at the first file newer than that, so a busy tree costs little.
func IdleFor(path string, d time.Duration) bool {
	cutoff := time.Now().Add(-d)
	sawFile := false
	err := filepath.WalkDir(path, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || !counts(e) {
			return nil
		}
		sawFile = true
		if fi, ierr := e.Info(); ierr == nil && fi.ModTime().After(cutoff) {
			return errNewer
		}
		return nil
	})
	if errors.Is(err, errNewer) {
		return false
	}
	if !sawFile {
		fi, serr := os.Stat(path)
		return serr == nil && fi.ModTime().Before(cutoff)
	}
	return true
}

// Size is the total size of the files under path.
func Size(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, ierr := d.Info(); ierr == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
