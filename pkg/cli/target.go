package cli

import (
	"fmt"
	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func connectTarget(o *daemonOpts) (*daemon.Client, error) {
	return common.ConnectTarget(o.Target, o.CAFile, o.Token, o.Verbose)
}

// targetLabel names the daemon a command talks to, for messages.
func targetLabel(o *daemonOpts) string {
	if o.Target != "" {
		if base, _, err := common.ParseTarget(o.Target); err == nil {
			return base
		}
		return o.Target
	}
	return fmt.Sprintf("daemon %q", o.Name)
}
