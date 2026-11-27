// Package daemontest provides driver fixtures for the local and hub server tests.
package daemontest

import (
	"context"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// SlowDriver is the mock driver with steps that take delay and stop
// early, failing, when their context ends (as a real driver's polling
// does).
type SlowDriver struct {
	*mock.Driver
	mu    sync.Mutex
	ctx   context.Context
	Delay time.Duration
}

func (d *SlowDriver) SetContext(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
}

func (d *SlowDriver) Execute(step flow.Step) *core.CommandResult {
	d.mu.Lock()
	ctx := d.ctx
	d.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-time.After(d.Delay):
		return d.Driver.Execute(step)
	case <-ctx.Done():
		return &core.CommandResult{Success: false, Error: ctx.Err(), Message: "cancelled"}
	}
}
