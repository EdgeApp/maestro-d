package executor

// maestro-d: setTrafficShape and resetTrafficShape. The executor runs them,
// not the driver: they are an HTTP call from the host, not a device command.

import (
	"context"
	"errors"
	"fmt"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// TrafficShaper sends a device's configured shaping calls. The daemon
// gives each session one (netshape.Shaper) for its device.
type TrafficShaper interface {
	Set(ctx context.Context, v netshape.Values) (netshape.Result, error)
	Reset(ctx context.Context) (netshape.Result, error)
}

// UsageError is a step whose values turned out bad only once its variables
// were expanded. The daemon answers it with USAGE.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

func (fr *FlowRunner) executeTrafficShapeStep(step flow.Step) *core.CommandResult {
	if fr.shaper == nil {
		// Only a daemon session has a shaper: this is `maestro-d test`.
		return unsupportedResult(fmt.Sprintf("%s runs in a maestro-d daemon session (a one-shot command, REST or the JS client); maestro-d test does not shape traffic", step.Type()))
	}
	ctx := fr.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	var res netshape.Result
	var err error
	var what string
	switch s := step.(type) {
	case *flow.SetTrafficShapeStep:
		direction := fr.script.ExpandVariables(s.Direction)
		if verr := flow.ValidateTrafficShapeDirection(direction); verr != nil {
			return errorResult(&UsageError{Err: verr})
		}
		v := netshape.Values{Mbit: s.Mbit, LossPct: s.LossPct, LatencyMs: s.LatencyMs, JitterMs: s.JitterMs, Direction: direction}.WithDefaults()
		res, err = fr.shaper.Set(ctx, v)
		fr.undo.noteSet(err)
		what = fmt.Sprintf("traffic shape set (mbit %v, lossPct %v, latencyMs %v, jitterMs %v, %s)", v.Mbit, v.LossPct, v.LatencyMs, v.JitterMs, v.Direction)
	case *flow.ResetTrafficShapeStep:
		res, err = fr.shaper.Reset(ctx)
		if err == nil {
			fr.undo.shaped = false
		}
		what = "traffic shape reset"
	default:
		return errorResult(fmt.Errorf("not a traffic-shaping step: %s", step.Type()))
	}
	if errors.Is(err, netshape.ErrNotConfigured) {
		return unsupportedResult(err.Error())
	}
	if err != nil {
		return errorResult(err)
	}
	return &core.CommandResult{Success: true, Message: fmt.Sprintf("%s: %d", what, res.Status),
		Data: map[string]any{"status": res.Status, "durationMs": res.Duration.Milliseconds()}}
}

func errorResult(err error) *core.CommandResult {
	return &core.CommandResult{Success: false, Error: err, Message: err.Error()}
}
