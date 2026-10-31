package core

import (
	"context"
	"regexp"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// ForegroundLookupLimit bounds a driver's foreground-app lookup. State is read
// holding the session's lock, so a wedged device must not hold it for long.
const ForegroundLookupLimit = 2 * time.Second

// ForegroundProbe runs one driver's foreground-app lookups. Each gets a context
// that ends at the limit, which the lookup hands to its device call so the call
// itself is cancelled rather than left running beside the next command. At most
// one runs at a time: a lookup whose call ignores its context still holds the
// probe, so polling a wedged device cannot pile up calls behind it. The zero
// value is ready to use.
type ForegroundProbe struct {
	busy atomic.Bool
	// limit overrides ForegroundLookupLimit (tests shorten it).
	limit time.Duration
}

// Lookup runs lookup within the limit. A lookup that fails, times out, or
// finds one still running returns "", leaving the state's fields absent.
func (p *ForegroundProbe) Lookup(lookup func(ctx context.Context) (string, error)) string {
	if !p.busy.CompareAndSwap(false, true) {
		return ""
	}
	limit := p.limit
	if limit <= 0 {
		limit = ForegroundLookupLimit
	}
	// Only Lookup cancels, so ctx ends early only at the limit, never as a
	// finished lookup's cleanup racing its own answer.
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	done := make(chan string, 1)
	go func() {
		id := p.run(ctx, lookup)
		// Free the probe before answering, so the next Lookup, which can
		// start as soon as this one returns, finds it free.
		p.busy.Store(false)
		done <- id
	}()
	select {
	case id := <-done:
		return id
	case <-ctx.Done():
		return ""
	}
}

// run calls lookup, turning an error, an answer after the limit, or a panic
// into "". The lookup runs off the request goroutine, where nothing else would
// recover a panic in a driver's device call, and one would end the daemon.
func (p *ForegroundProbe) run(ctx context.Context, lookup func(ctx context.Context) (string, error)) (id string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("foreground lookup panicked: %v\n%s", r, debug.Stack())
			id = ""
		}
	}()
	id, err := lookup(ctx)
	if err != nil || ctx.Err() != nil {
		return ""
	}
	return id
}

// Shell runs a device shell command.
type Shell interface {
	Shell(cmd string) (string, error)
}

// contextShell is a Shell that can also run a command under a context, which
// ends the command when the context does.
type contextShell interface {
	ShellContext(ctx context.Context, cmd string) (string, error)
}

// ShellWithin runs cmd under ctx when the shell supports it, and as a plain
// Shell call otherwise.
func ShellWithin(ctx context.Context, sh Shell, cmd string) (string, error) {
	if c, ok := sh.(contextShell); ok {
		return c.ShellContext(ctx, cmd)
	}
	return sh.Shell(cmd)
}

// AndroidForegroundApp reads an Android device's resumed package over sh
// within probe's limit, killing the shell at the limit when sh can, or returns
// "" without a shell or a resumed activity.
func AndroidForegroundApp(probe *ForegroundProbe, sh Shell) string {
	if sh == nil {
		return ""
	}
	return probe.Lookup(func(ctx context.Context) (string, error) {
		output, err := ShellWithin(ctx, sh, ForegroundDumpsysCommand)
		if err != nil {
			return "", err
		}
		return ForegroundPackageFromDumpsys(output), nil
	})
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
// appState compares it with appID, the session's own app: "foreground" when
// that app is in front, "background" when another one is (the app may have
// crashed: these lookups cannot tell). A driver that does not know its
// session's app passes "" and leaves appState absent, since the app in front
// says nothing about it.
func (s *StateSnapshot) SetForeground(id, appID string) {
	if id == "" {
		return
	}
	s.ForegroundApp = id
	switch appID {
	case "":
	case id:
		s.AppState = "foreground"
	default:
		s.AppState = "background"
	}
}
