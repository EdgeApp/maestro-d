package executor

// maestro-d: steps the executor answers itself, before any driver, and the
// codes their failures carry to the daemon.

import (
	"errors"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// UnsupportedError is a step this host or device cannot do. The daemon
// answers it with UNSUPPORTED.
type UnsupportedError struct{ Msg string }

func (e *UnsupportedError) Error() string { return e.Msg }

// Outcome codes finishStep sets from a result's Go error; they are the
// daemon's error codes, named here as strings so the executor does not
// import the daemon.
const (
	codeUnsupported = "UNSUPPORTED"
	codeUsage       = "USAGE"
)

// outcomeCode maps a step's Go error to the code the daemon reports, or ""
// for an ordinary failure.
func outcomeCode(err error) string {
	var unsupported *UnsupportedError
	var config *netshape.ConfigError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &unsupported):
		return codeUnsupported
	case errors.As(err, &config):
		return codeUsage
	}
	return ""
}

func unsupportedResult(msg string) *core.CommandResult {
	return &core.CommandResult{Success: false, Error: &UnsupportedError{Msg: msg}, Message: msg}
}

// iosAirplaneMessage is the answer to an airplane-mode step on iOS.
const iosAirplaneMessage = "iOS has no airplane mode in maestro-d; simulate it with setTrafficShape (lossPct: 100)"

// executeAirplaneStep refuses setAirplaneMode and toggleAirplaneMode on iOS
// and tvOS, whichever driver runs the device: turning the radios off would
// break the driver's own connection. Other platforms run them as before.
func (fr *FlowRunner) executeAirplaneStep(step flow.Step) *core.CommandResult {
	if info := fr.driver.GetPlatformInfo(); info != nil && isApplePlatform(info.Platform) {
		return unsupportedResult(iosAirplaneMessage)
	}
	return fr.driver.Execute(step)
}

func isApplePlatform(platform string) bool {
	return strings.EqualFold(platform, "ios") || strings.EqualFold(platform, "tvos")
}
