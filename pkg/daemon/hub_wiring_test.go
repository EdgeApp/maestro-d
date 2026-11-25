package daemon_test

import (
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/hubd"
)

func init() {
	daemon.NewTestHub = func() daemon.Hub { return new(hubd.Server) }
}
