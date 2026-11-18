package capture

import "time"

// SetRename replaces the recorder's rename for a test.
func SetRename(f func(oldpath, newpath string) error) (restore func()) {
	old := rename
	rename = f
	return func() { rename = old }
}

// HoldRecorderLoops delays every new recorder's packet loop until release
// is closed, so a test can overrun its channel.
func HoldRecorderLoops(release <-chan struct{}) (restore func()) {
	old := startLoop
	startLoop = func(loop func()) {
		go func() {
			<-release
			loop()
		}()
	}
	return func() { startLoop = old }
}

// SetClock replaces a session's host clock for a test.
func SetClock(s *Session, now func() time.Time) { s.now = now }
