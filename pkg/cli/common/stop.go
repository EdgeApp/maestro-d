package common

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// StopDaemons shuts the named daemons down and waits for them to exit; with
// --all, one that is not running is skipped.
func StopDaemons(ctx context.Context, names []string, what string, timeoutSeconds int, jsonOutput, ignoreStopped bool) error {
	timeout := 30 * time.Second
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	var stopped []string
	for _, name := range names {
		client, err := daemon.Dial(name)
		if err != nil {
			return err
		}
		if client == nil {
			if ignoreStopped {
				continue
			}
			return daemon.Errorf(daemon.CodeDaemonUnavailable, "%s %q is not running", what, name)
		}
		if err := client.Shutdown(ctx); err != nil {
			if de, ok := err.(*daemon.Error); !ok || de.Code != daemon.CodeDaemonUnavailable {
				return err
			}
		}
		if !daemon.WaitForExit(client.Info, timeout) {
			return daemon.Errorf(daemon.CodeDaemonUnavailable, "%s %q (pid %d) did not exit within %v", what, name, client.Info.PID, timeout)
		}
		stopped = append(stopped, name)
		if !jsonOutput {
			fmt.Printf("%s %q stopped\n", what, name)
		}
	}
	if jsonOutput {
		PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "stopped": stopped})
	}
	return nil
}
