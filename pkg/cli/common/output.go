// Package common supplies command-line behavior shared by the client and hub.
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// SignalContext cancels on SIGINT/SIGTERM so an in-flight step is cancelled
// on the daemon and the CLI exits 130.
func SignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(ch)
	}()
	return ctx, cancel
}

// Progress prints a status line to stderr (stdout is reserved for results).
func Progress(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func PrintJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// FinishErr turns any error into the documented exit code: the JSON failure
// envelope goes to stderr and the process exits with the code's status.
// Errors that are not *daemon.Error are reported as USAGE (exit 2).
func FinishErr(err error, jsonMode bool) error {
	if err == nil {
		return nil
	}
	var de *daemon.Error
	if e, ok := err.(*daemon.Error); ok {
		de = e
	} else if e, ok := err.(cli.ExitCoder); ok {
		return e
	} else {
		de = daemon.WrapErr(daemon.CodeUsage, err)
	}
	if de.Code == daemon.CodeInterrupted {
		fmt.Fprintln(os.Stderr, "interrupted")
	} else if !jsonMode || de.Code != daemon.CodeCommandFailed {
		fmt.Fprintf(os.Stderr, "Error: %s\n", de.Message)
	}
	env := daemon.FailureEnvelope(de)
	// --json promises a JSON document on stdout; a failure that produced no
	// step result (DEVICE_IN_USE, USAGE, …) would otherwise leave it empty.
	if jsonMode && !jsonOnStdout {
		PrintJSON(os.Stdout, env)
	}
	PrintJSON(os.Stderr, env)
	return cli.Exit("", de.ExitCode())
}

// jsonOnStdout records that --json output already went to stdout, so a
// failure envelope is not printed there a second time.
var jsonOnStdout bool

func PrintJSONStdout(v any) {
	jsonOnStdout = true
	PrintJSON(os.Stdout, v)
}
