package computer

import "time"

// fastTimings shortens every pause so tests run quickly; call the result to
// restore them.
func fastTimings() func() {
	saved := []time.Duration{loginSettle, loginTimeout, loginInterval, clickGap, defaultHold, keyGap, dragStep, defaultDrag, stillInterval, inputReady}
	loginSettle, loginTimeout, loginInterval, inputReady = time.Millisecond, 60*time.Millisecond, 5*time.Millisecond, 0
	clickGap, defaultHold, keyGap, dragStep, defaultDrag, stillInterval = 0, 2*time.Millisecond, 0, time.Millisecond, 4*time.Millisecond, 2*time.Millisecond
	return func() {
		loginSettle, loginTimeout, loginInterval, clickGap, defaultHold, keyGap, dragStep, defaultDrag, stillInterval, inputReady =
			saved[0], saved[1], saved[2], saved[3], saved[4], saved[5], saved[6], saved[7], saved[8], saved[9]
	}
}
