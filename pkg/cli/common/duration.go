package common

import (
	"strconv"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// ParseLeaseDuration accepts integer seconds or a Go duration of at least one second.
func ParseLeaseDuration(flag, v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil {
		d = time.Duration(n) * time.Second
	} else if d, err = time.ParseDuration(v); err != nil {
		return 0, daemon.Errorf(daemon.CodeUsage, "--%s %q: want a duration (8h, 90m, 1h30m) or seconds", flag, v)
	}
	if d < time.Second {
		return 0, daemon.Errorf(daemon.CodeUsage, "--%s %q: at least 1s", flag, v)
	}
	return d.Round(time.Second), nil
}
