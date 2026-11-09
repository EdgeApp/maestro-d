// Package daemon implements the maestro-d background process: a
// long-lived server that holds driver connections to one or more devices
// and executes individual YAML commands on request over a unix socket (and
// optionally TCP). The CLI, the REST API and the JavaScript library all
// speak the JSON protocol defined in this file.
//
// This package is not part of upstream maestro-runner. It depends on
// pkg/executor, pkg/flow, pkg/core, pkg/report, pkg/simulator and
// pkg/emulator, but never on pkg/cli: driver construction and device
// discovery are injected through Deps so pkg/cli can import this package.
package daemon

import (
	"fmt"
	"net/http"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/executor"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// APIVersion is bumped when the wire protocol changes incompatibly. A
// client refuses to talk to a daemon with a different major version.
const APIVersion = "1"

// Code identifies a failure class. Each code maps to exactly one CLI exit
// code and one HTTP status so all three surfaces agree.
type Code string

const (
	// CodeCommandFailed: the step ran and failed (assertion false, element
	// not found, timeout, script threw). Exit 1 / HTTP 422.
	CodeCommandFailed Code = "COMMAND_FAILED"
	// CodeUsage: unknown command, bad flags, unparsable step/YAML, unknown
	// device, bad request body. Exit 2 / HTTP 400.
	CodeUsage Code = "USAGE"
	// CodeDaemonUnavailable: could not connect to or spawn the daemon, API
	// version mismatch, daemon shutting down. Exit 3 / HTTP 503.
	CodeDaemonUnavailable Code = "DAEMON_UNAVAILABLE"
	// CodeDeviceError: driver/device failure — attach failed, device
	// disconnected, WDA/UIA2 died, driver panic. Exit 4 / HTTP 502.
	CodeDeviceError Code = "DEVICE_ERROR"
	// CodeBusy: the device has a step in flight and the caller asked not to
	// wait. Exit 5 / HTTP 409.
	CodeBusy Code = "BUSY"
	// CodeDeviceExternal: refusing to shut down a device the daemon did not
	// boot without force. Exit 5 / HTTP 409.
	CodeDeviceExternal Code = "DEVICE_EXTERNAL"
	// CodeDeviceInUse: another daemon has the device attached. Exit 5 /
	// HTTP 409; details name the owner.
	CodeDeviceInUse Code = "DEVICE_IN_USE"
	// CodeDeviceNotFound: no such device is connected or booted. Exit 4 /
	// HTTP 404.
	CodeDeviceNotFound Code = "DEVICE_NOT_FOUND"
	// CodeDeviceNotAttached: the device exists but has no driver; the CLI
	// auto-attaches instead of surfacing this. Exit 2 / HTTP 404.
	CodeDeviceNotAttached Code = "DEVICE_NOT_ATTACHED"
	// CodeInterrupted: the client cancelled (Ctrl-C) and the in-flight step
	// was cancelled with it. Exit 130.
	CodeInterrupted Code = "INTERRUPTED"
	// CodeInternal: a bug in the daemon. Exit 4 / HTTP 500.
	CodeInternal Code = "INTERNAL"
)

// ExitCode returns the CLI exit status for the code.
func (c Code) ExitCode() int {
	switch c {
	case "":
		return 0
	case CodeCommandFailed:
		return 1
	case CodeUsage, CodeDeviceNotAttached:
		return 2
	case CodeDaemonUnavailable:
		return 3
	case CodeDeviceError, CodeDeviceNotFound, CodeInternal:
		return 4
	case CodeBusy, CodeDeviceExternal, CodeDeviceInUse:
		return 5
	case CodeInterrupted:
		return 130
	}
	return 4
}

// HTTPStatus returns the HTTP status for the code.
func (c Code) HTTPStatus() int {
	switch c {
	case "":
		return http.StatusOK
	case CodeCommandFailed:
		return http.StatusUnprocessableEntity
	case CodeUsage:
		return http.StatusBadRequest
	case CodeDaemonUnavailable:
		return http.StatusServiceUnavailable
	case CodeDeviceError:
		return http.StatusBadGateway
	case CodeBusy, CodeDeviceExternal, CodeDeviceInUse:
		return http.StatusConflict
	case CodeDeviceNotFound, CodeDeviceNotAttached:
		return http.StatusNotFound
	case CodeInterrupted:
		return 499
	case CodeInternal:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// CodeFromHTTPStatus recovers a code from a status when a body has none.
func CodeFromHTTPStatus(status int) Code {
	switch status {
	case http.StatusUnprocessableEntity:
		return CodeCommandFailed
	case http.StatusBadRequest:
		return CodeUsage
	case http.StatusServiceUnavailable:
		return CodeDaemonUnavailable
	case http.StatusBadGateway:
		return CodeDeviceError
	case http.StatusConflict:
		return CodeBusy
	case http.StatusNotFound:
		return CodeDeviceNotFound
	}
	return CodeInternal
}

// ErrorBody is the `error` object of every failure envelope.
type ErrorBody struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// Step is the description of the step that failed, when one did.
	Step string `json:"step,omitempty"`
	// Details carries structured context: artifact paths, the owning
	// daemon for DEVICE_IN_USE, the attached device list for USAGE, …
	Details map[string]any `json:"details,omitempty"`
}

// Error is the Go-side form of ErrorBody. Every failure the client returns
// is an *Error, so callers can switch on Code and exit with ExitCode.
type Error struct {
	ErrorBody
	// Cause is the underlying Go error, when any (not serialised).
	Cause error `json:"-"`
}

func (e *Error) Error() string {
	if e.Step != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Step, e.Message, e.Code)
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

// Unwrap exposes Cause to errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// ExitCode is the CLI exit status for the error.
func (e *Error) ExitCode() int { return e.Code.ExitCode() }

// HTTPStatus is the HTTP status for the error.
func (e *Error) HTTPStatus() int { return e.Code.HTTPStatus() }

// Errorf builds an *Error.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{ErrorBody: ErrorBody{Code: code, Message: fmt.Sprintf(format, args...)}}
}

// WrapErr builds an *Error around err, keeping it as Cause. If err already
// is an *Error it is returned unchanged.
func WrapErr(code Code, err error) *Error {
	if err == nil {
		return nil
	}
	if de, ok := err.(*Error); ok {
		return de
	}
	return &Error{ErrorBody: ErrorBody{Code: code, Message: err.Error()}, Cause: err}
}

// WithDetails adds structured context and returns the same error.
func (e *Error) WithDetails(kv ...any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			e.Details[k] = kv[i+1]
		}
	}
	return e
}

// Envelope is the common shape of every JSON response: `ok`, `exitCode`
// and, on failure, `error`. Success payloads embed it.
type Envelope struct {
	OK       bool       `json:"ok"`
	ExitCode int        `json:"exitCode"`
	Error    *ErrorBody `json:"error,omitempty"`
}

// FailureEnvelope builds the envelope for an error.
func FailureEnvelope(e *Error) Envelope {
	return Envelope{OK: false, ExitCode: e.ExitCode(), Error: &e.ErrorBody}
}

// StepResult is the response body for one executed step: the envelope plus
// the executor.StepOutcome fields. A failed optional step is ok:true with
// `error` still populated so callers can see what went wrong.
type StepResult struct {
	Envelope
	Device     string                  `json:"device"`
	Step       string                  `json:"step"`
	Type       string                  `json:"type"`
	Index      int                     `json:"index"`
	DurationMs int64                   `json:"durationMs"`
	Message    string                  `json:"message,omitempty"`
	Skipped    bool                    `json:"skipped,omitempty"`
	Optional   bool                    `json:"optional,omitempty"`
	Element    *core.ElementInfo       `json:"element,omitempty"`
	Data       any                     `json:"data,omitempty"`
	Artifacts  report.CommandArtifacts `json:"artifacts"`
	SubSteps   []report.Command        `json:"subSteps,omitempty"`
	ReportDir  string                  `json:"reportDir,omitempty"`
}

// ResultFromOutcome converts an executor outcome into the wire result.
func ResultFromOutcome(device, reportDir string, o *executor.StepOutcome) StepResult {
	r := StepResult{
		Device:     device,
		Step:       o.Description,
		Type:       o.Type,
		Index:      o.Index,
		DurationMs: o.DurationMs,
		Message:    o.Message,
		Skipped:    o.Skipped,
		Optional:   o.Optional,
		Element:    o.Element,
		Data:       o.Data,
		Artifacts:  o.Artifacts,
		SubSteps:   o.SubSteps,
		ReportDir:  reportDir,
	}
	if o.Error != nil {
		body := &ErrorBody{Code: CodeCommandFailed, Message: o.Error.Message, Step: o.Description}
		if o.Panicked {
			body.Code = CodeDeviceError
		} else if o.Cancelled {
			body.Code = CodeInterrupted
		}
		details := map[string]any{}
		if o.Error.Type != "" {
			details["errorType"] = o.Error.Type
		}
		if o.Error.Details != "" {
			details["errorDetails"] = o.Error.Details
		}
		if o.Error.Suggestion != "" {
			details["suggestion"] = o.Error.Suggestion
		}
		if o.Artifacts.ScreenshotAfter != "" {
			details["screenshot"] = o.Artifacts.ScreenshotAfter
		}
		if o.Artifacts.ViewHierarchy != "" {
			details["hierarchy"] = o.Artifacts.ViewHierarchy
		}
		if reportDir != "" {
			details["reportDir"] = reportDir
		}
		if len(details) > 0 {
			body.Details = details
		}
		r.Error = body
	}
	if o.Success {
		r.OK = true
		r.ExitCode = 0
	} else {
		r.OK = false
		r.ExitCode = r.Error.Code.ExitCode()
	}
	return r
}

// Err returns the result's failure as an *Error (nil when ok).
func (r *StepResult) Err() *Error {
	if r.OK || r.Error == nil {
		return nil
	}
	return &Error{ErrorBody: *r.Error}
}

// StepsResult is the response for a batch (`/steps`) or a flow (`/flows`).
type StepsResult struct {
	Envelope
	Device  string       `json:"device"`
	Results []StepResult `json:"results"`
	// Passed/Failed/Skipped count top-level steps.
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// StepsResultFrom summarises a batch; the envelope reflects the first
// failure.
func StepsResultFrom(device string, results []StepResult) StepsResult {
	out := StepsResult{Envelope: Envelope{OK: true}, Device: device, Results: results}
	for i := range results {
		r := &results[i]
		switch {
		case r.Skipped:
			out.Skipped++
		case r.OK:
			out.Passed++
		default:
			out.Failed++
			if out.OK {
				out.OK = false
				out.ExitCode = r.ExitCode
				out.Error = r.Error
			}
		}
	}
	return out
}

// DeviceInfo describes one device the daemon knows about.
type DeviceInfo struct {
	ID        string `json:"id"`
	Platform  string `json:"platform"`       // android | ios | web | mock
	Kind      string `json:"kind,omitempty"` // device | emulator | simulator
	Name      string `json:"name,omitempty"`
	OSVersion string `json:"osVersion,omitempty"`
	State     string `json:"state,omitempty"` // device | offline | Booted | Shutdown | …
	Ready     bool   `json:"ready"`
	// BootedBy is "daemon" for devices this daemon booted, "external" for
	// everything else.
	BootedBy string `json:"bootedBy,omitempty"`
	BootedAt string `json:"bootedAt,omitempty"`
	// Attached is true while the daemon holds a driver for the device.
	Attached bool `json:"attached"`
	// AttachedAt / AppID / Driver / ReportDir describe the attachment.
	AttachedAt string `json:"attachedAt,omitempty"`
	AppID      string `json:"appId,omitempty"`
	Driver     string `json:"driver,omitempty"`
	ReportDir  string `json:"reportDir,omitempty"`
	// Busy is true while a step is in flight.
	Busy bool `json:"busy,omitempty"`
	// Owner names the daemon that has the device attached, when it is not
	// this one (ps / DEVICE_IN_USE).
	Owner string `json:"owner,omitempty"`
	// CreatedBy is "maestro-d" for a simulator or AVD that `device create`
	// made (recorded in <home>/created.json); only those are deleted
	// without force.
	CreatedBy string `json:"createdBy,omitempty"`
}

// AttachConfig is the body of POST /v1/devices/{id}/attach: the subset of
// upstream's RunConfig that describes how to open a driver on one device,
// plus the flow-level settings the session applies. Field names are the
// camelCase forms of the CLI flags.
type AttachConfig struct {
	Platform    string `json:"platform,omitempty"`
	Driver      string `json:"driver,omitempty"`
	AppFile     string `json:"appFile,omitempty"`
	AppID       string `json:"appId,omitempty"`
	URL         string `json:"url,omitempty"`
	TeamID      string `json:"teamId,omitempty"`
	WDABundleID string `json:"wdaBundleId,omitempty"`
	AppiumURL   string `json:"appiumUrl,omitempty"`
	// Env are session variables (-e KEY=VALUE).
	Env map[string]string `json:"env,omitempty"`
	// Artifacts: "on-failure" (default), "always", "never".
	Artifacts          string `json:"artifacts,omitempty"`
	NoAppInstall       bool   `json:"noAppInstall,omitempty"`
	NoDriverInstall    bool   `json:"noDriverInstall,omitempty"`
	DriverStartTimeout int    `json:"driverStartTimeout,omitempty"` // seconds
	WaitForIdleTimeout *int   `json:"waitForIdleTimeout,omitempty"` // ms
	ConditionTimeout   int    `json:"conditionTimeout,omitempty"`   // ms
	StepDelay          int    `json:"stepDelay,omitempty"`          // ms
	TypingFrequency    int    `json:"typingFrequency,omitempty"`
	CommandTimeout     int    `json:"commandTimeout,omitempty"` // ms
	// AlertMonitor enables the iOS system-alert monitor (default true).
	AlertMonitor *bool `json:"alertMonitor,omitempty"`
	// OutputDir overrides the report directory for this device.
	OutputDir string `json:"outputDir,omitempty"`
	// FlowDir is the base for relative paths in steps — the client's cwd.
	FlowDir string `json:"flowDir,omitempty"`
	// Extra holds driver-specific options the wiring may understand
	// (e.g. browser, headless, viewport for web).
	Extra map[string]any `json:"extra,omitempty"`
}

// BootRequest is the body of POST /v1/devices.
type BootRequest struct {
	// Platform is ios or android. To boot, it may be left out: an avd:<name>
	// id means android, and a known simulator name or UDID means ios.
	Platform string `json:"platform,omitempty"`
	// Name is an AVD name (or avd:<name>), a simulator name, or a simulator
	// UDID.
	Name        string `json:"name"`
	BootTimeout int    `json:"bootTimeout,omitempty"` // seconds
	// Create, when set, creates the simulator or AVD first.
	Create *CreateSpec `json:"create,omitempty"`
	// Boot boots what Create made (default true). Ignored without Create.
	Boot *bool `json:"boot,omitempty"`
}

// CreateSpec describes a simulator (type, runtime) or an AVD (image,
// profile, dataSize, installImage, acceptLicenses) for `device create`.
type CreateSpec struct {
	// Name defaults to BootRequest.Name.
	Name    string `json:"name,omitempty"`
	Type    string `json:"type,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	Image   string `json:"image,omitempty"`
	Profile string `json:"profile,omitempty"`
	// DataSize is the AVD's data partition (default 6G, at most 10G).
	DataSize       string `json:"dataSize,omitempty"`
	InstallImage   bool   `json:"installImage,omitempty"`
	AcceptLicenses bool   `json:"acceptLicenses,omitempty"`
}

// StopDeviceRequest is the body of DELETE /v1/devices/{id}.
type StopDeviceRequest struct {
	Force bool `json:"force,omitempty"`
	// Delete also deletes the simulator or AVD (`?delete=true`).
	Delete bool `json:"delete,omitempty"`
}

// StepsRequest is the body of POST /v1/devices/{dev}/steps.
type StepsRequest struct {
	// Steps are step objects in YAML data-model form ({"tapOn": "Login"}).
	Steps []any `json:"steps,omitempty"`
	// YAML is a YAML list of steps, used instead of Steps.
	YAML            string `json:"yaml,omitempty"`
	ContinueOnError bool   `json:"continueOnError,omitempty"`
	// Cwd resolves relative paths inside the steps (runFlow file, …).
	Cwd string `json:"cwd,omitempty"`
}

// FlowRequest is the body of POST /v1/devices/{dev}/flows.
type FlowRequest struct {
	File string            `json:"file,omitempty"`
	YAML string            `json:"yaml,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
}

// EvalRequest is the body of POST /v1/devices/{dev}/eval.
type EvalRequest struct {
	Script string `json:"script"`
}

// EvalResult is its response.
type EvalResult struct {
	Envelope
	Value any `json:"value"`
}

// VarsResult is the response of GET/PUT /v1/devices/{dev}/vars.
type VarsResult struct {
	Envelope
	Vars map[string]string `json:"vars"`
}

// DeviceResult wraps one device.
type DeviceResult struct {
	Envelope
	Device DeviceInfo `json:"device"`
}

// DevicesResult wraps a device list.
type DevicesResult struct {
	Envelope
	Devices []DeviceInfo `json:"devices"`
}

// DataResult wraps an arbitrary JSON payload (state, info, hierarchy).
type DataResult struct {
	Envelope
	Data any `json:"data"`
}

// StatusResult is GET /v1/status.
type StatusResult struct {
	Envelope
	Daemon  Info         `json:"daemon"`
	Devices []DeviceInfo `json:"devices"`
	// IdleShutdownAt is when the daemon will exit if nothing happens (RFC
	// 3339), empty when idle shutdown is disabled.
	IdleShutdownAt string `json:"idleShutdownAt,omitempty"`
}

// Info is the content of daemon.json.
type Info struct {
	Name       string   `json:"name"`
	PID        int      `json:"pid"`
	APIVersion string   `json:"apiVersion"`
	Version    string   `json:"version"`
	SocketPath string   `json:"socketPath"`
	HTTPAddr   string   `json:"httpAddr,omitempty"`
	StartedAt  string   `json:"startedAt"`
	StartArgs  []string `json:"startArgs,omitempty"`
	RunDir     string   `json:"runDir"`
	ReportsDir string   `json:"reportsDir"`
	// IdleTimeout is in seconds; 0 = never.
	IdleTimeout int `json:"idleTimeout"`
}

// Event is one entry on the /v1/events stream.
type Event struct {
	Seq  int64  `json:"seq"`
	Time string `json:"time"`
	// Type: step | nestedStep | device.attached | device.detached |
	// device.booted | device.stopped | daemon.shutdown
	Type       string `json:"type"`
	Device     string `json:"device,omitempty"`
	Depth      int    `json:"depth,omitempty"`
	Step       string `json:"step,omitempty"`
	Passed     bool   `json:"passed,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Error      string `json:"error,omitempty"`
	Message    string `json:"message,omitempty"`
}
