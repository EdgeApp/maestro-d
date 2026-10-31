package core

import (
	"regexp"
	"time"
)

// ForegroundLookupLimit bounds a driver's foreground-app lookup. State is read
// holding the session's lock, so a wedged device must not hold it for long.
const ForegroundLookupLimit = 2 * time.Second

// WithinForegroundLimit runs a foreground lookup, giving up after the limit. A
// lookup that fails or times out returns "", leaving the state's field absent.
func WithinForegroundLimit(lookup func() (string, error)) string {
	return withinLimit(ForegroundLookupLimit, lookup)
}

func withinLimit(limit time.Duration, lookup func() (string, error)) string {
	done := make(chan string, 1)
	go func() {
		id, err := lookup()
		if err != nil {
			id = ""
		}
		done <- id
	}()
	select {
	case id := <-done:
		return id
	case <-time.After(limit):
		return ""
	}
}

var resumedActivity = regexp.MustCompile(`(?:topResumedActivity|mResumedActivity|ResumedActivity)[=:\s].*?\bu\d+\s+([A-Za-z0-9_.]+)/`)

// ForegroundPackageFromDumpsys finds the resumed package in the output of
// `dumpsys activity activities`, or "" when there is none.
func ForegroundPackageFromDumpsys(output string) string {
	match := resumedActivity.FindStringSubmatch(output)
	if match == nil {
		return ""
	}
	return match[1]
}

// ForegroundDumpsysCommand is the shell command ForegroundPackageFromDumpsys reads.
const ForegroundDumpsysCommand = "dumpsys activity activities | grep -E 'topResumedActivity|mResumedActivity'"

// SetForeground fills a snapshot's foreground fields when an id was found.
func (s *StateSnapshot) SetForeground(id string) {
	if id == "" {
		return
	}
	s.ForegroundApp = id
	s.AppState = "foreground"
}
