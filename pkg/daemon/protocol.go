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
	"path/filepath"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
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
	CodeUsage      Code = "USAGE"
	CodeHubRunning Code = "HUB_RUNNING"
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
	// CodeForbidden: a remote caller asked for something only a local
	// caller may do — a host path, runShell, ?cwd=, a web attach. Exit 2 /
	// HTTP 403; details name the denied step and field.
	CodeForbidden Code = "FORBIDDEN"
	// CodePeerUnavailable: the hub that owns the device cannot be reached
	// over the mesh. Exit 3 / HTTP 503; details.hub names it. Clients retry
	// it with the same request id.
	CodePeerUnavailable Code = "PEER_UNAVAILABLE"
	// CodeHubNameTaken: a peer's hello used a name already in the mesh.
	// Exit 5 / HTTP 409.
	CodeHubNameTaken Code = "HUB_NAME_TAKEN"
	// CodeHubVersion: a peer speaks another API version. Exit 5 / HTTP 409.
	CodeHubVersion Code = "HUB_VERSION"
	// CodeLeaseRequired: a mutating request on a hub without the device's
	// lease. Exit 5 / HTTP 409.
	CodeLeaseRequired Code = "LEASE_REQUIRED"
	// CodeLeaseExpired: the request's lease has expired or was released.
	// Exit 5 / HTTP 409.
	CodeLeaseExpired Code = "LEASE_EXPIRED"
	// CodeLeaseNotStarted: the request's lease is a reservation whose
	// window has not started; details.startsAt says when it does. Exit 5 /
	// HTTP 409.
	CodeLeaseNotStarted Code = "LEASE_NOT_STARTED"
	// CodeCaptureUnavailable: the device has no screen capture, or its
	// capture failed or produced no frames. Exit 4 / HTTP 503;
	// details.reason names why (pkg/capture's reasons: simulator,
	// unsupported-platform, no-frames, …). The X-Maestro-Error-Code header
	// tells it from DAEMON_UNAVAILABLE's 503.
	CodeCaptureUnavailable Code = "CAPTURE_UNAVAILABLE"
	// CodeUnsupported: this host or device cannot do what was asked — a
	// device with no traffic shaper configured on its host, or airplane
	// mode on iOS. Exit 2 / HTTP 501.
	CodeUnsupported Code = "UNSUPPORTED"
)

// Codes lists every code, for tables and tests.
var Codes = []Code{
	CodeCommandFailed, CodeUsage, CodeHubRunning, CodeDaemonUnavailable, CodeDeviceError, CodeBusy,
	CodeDeviceExternal, CodeDeviceInUse, CodeDeviceNotFound, CodeDeviceNotAttached,
	CodeInterrupted, CodeInternal, CodeForbidden, CodePeerUnavailable, CodeHubNameTaken,
	CodeHubVersion, CodeLeaseRequired, CodeLeaseExpired, CodeLeaseNotStarted, CodeCaptureUnavailable, CodeUnsupported,
}

// ExitCode returns the CLI exit status for the code.
func (c Code) ExitCode() int {
	switch c {
	case "":
		return 0
	case CodeCommandFailed:
		return 1
	case CodeUsage, CodeHubRunning, CodeDeviceNotAttached, CodeForbidden, CodeUnsupported:
		return 2
	case CodeDaemonUnavailable, CodePeerUnavailable:
		return 3
	case CodeDeviceError, CodeDeviceNotFound, CodeInternal, CodeCaptureUnavailable:
		return 4
	case CodeBusy, CodeDeviceExternal, CodeDeviceInUse, CodeHubNameTaken, CodeHubVersion, CodeLeaseRequired, CodeLeaseExpired, CodeLeaseNotStarted:
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
	case CodeHubRunning:
		return http.StatusConflict
	case CodeUsage:
		return http.StatusBadRequest
	case CodeDaemonUnavailable, CodePeerUnavailable, CodeCaptureUnavailable:
		return http.StatusServiceUnavailable
	case CodeDeviceError:
		return http.StatusBadGateway
	case CodeForbidden:
		return http.StatusForbidden
	case CodeBusy, CodeDeviceExternal, CodeDeviceInUse, CodeHubNameTaken, CodeHubVersion, CodeLeaseRequired, CodeLeaseExpired, CodeLeaseNotStarted:
		return http.StatusConflict
	case CodeDeviceNotFound, CodeDeviceNotAttached:
		return http.StatusNotFound
	case CodeInterrupted:
		return 499
	case CodeInternal:
		return http.StatusInternalServerError
	case CodeUnsupported:
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}

// HeaderErrorCode repeats an error envelope's code as a response header, so
// a client can tell failures apart on an answer without a body: a HEAD
// (LEASE_REQUIRED, LEASE_EXPIRED and BUSY are all 409).
const HeaderErrorCode = "X-Maestro-Error-Code"

// CodeFromHeader returns the code an answer's HeaderErrorCode names, or ""
// when it names none this client knows.
func CodeFromHeader(h http.Header) Code {
	c := Code(h.Get(HeaderErrorCode))
	for _, k := range Codes {
		if c == k {
			return c
		}
	}
	return ""
}

// CodeFromHTTPStatus recovers a code from a status when a body has none.
func CodeFromHTTPStatus(status int) Code {
	switch status {
	case http.StatusUnprocessableEntity:
		return CodeCommandFailed
	case http.StatusBadRequest, http.StatusUnauthorized:
		return CodeUsage
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusServiceUnavailable:
		return CodeDaemonUnavailable
	case http.StatusBadGateway:
		return CodeDeviceError
	case http.StatusConflict:
		return CodeBusy
	case http.StatusNotFound:
		return CodeDeviceNotFound
	case http.StatusNotImplemented:
		return CodeUnsupported
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
// detail returns a string detail of e ("" and false when e is nil or has
// none).
func (e *Error) Detail(key string) (string, bool) {
	if e == nil {
		return "", false
	}
	v, ok := e.Details[key].(string)
	return v, ok
}

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
	Device     string                   `json:"device"`
	Step       string                   `json:"step"`
	Type       string                   `json:"type"`
	Index      int                      `json:"index"`
	DurationMs int64                    `json:"durationMs"`
	Message    string                   `json:"message,omitempty"`
	Skipped    bool                     `json:"skipped,omitempty"`
	Optional   bool                     `json:"optional,omitempty"`
	Element    *core.ElementInfo        `json:"element,omitempty"`
	Data       any                      `json:"data,omitempty"`
	Artifacts  report.CommandArtifacts  `json:"artifacts"`
	SubSteps   []report.Command         `json:"subSteps,omitempty"`
	TestReport *executor.TestFlowReport `json:"testReport,omitempty"`
	ReportDir  string                   `json:"reportDir,omitempty"`
	// Outputs name the files a remote call's output fields wrote, as
	// paths under the report directory (outputs/<path>), fetched with
	// GET …/artifacts/{path}. Absent for local calls.
	Outputs []string `json:"outputs,omitempty"`
	// Warnings are notes the caller should see, such as a cloud session
	// that ends soon.
	Warnings []string `json:"warnings,omitempty"`
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
		TestReport: o.TestReport,
		ReportDir:  reportDir,
		Outputs:    o.Outputs,
	}
	if o.Forbidden != nil {
		d := o.Forbidden
		details := map[string]any{"reason": d.Reason}
		for k, v := range map[string]string{"step": d.Step, "command": d.Command, "field": d.Field, "path": d.Path} {
			if v != "" {
				details[k] = v
			}
		}
		r.Error = &ErrorBody{Code: CodeForbidden, Message: d.Reason, Step: o.Description, Details: details}
	} else if o.Error != nil {
		body := &ErrorBody{Code: CodeCommandFailed, Message: o.Error.Message, Step: o.Description}
		if o.Panicked {
			body.Code = CodeDeviceError
		} else if o.Cancelled {
			body.Code = CodeInterrupted
		} else if o.Code != "" { // maestro-d: UNSUPPORTED or USAGE from the executor
			body.Code = Code(o.Code)
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

// ForRemote rewrites the daemon's own report-directory paths in the
// result's messages to report-relative ones (outputs/<path>, as the
// result's outputs and the artifacts route name them), so a remote caller
// is told the file it can fetch rather than a path on the daemon's host.
func (r *StepResult) ForRemote(reportDir string) {
	if reportDir == "" {
		return
	}
	prefix := filepath.Clean(reportDir) + string(filepath.Separator)
	r.Message = strings.ReplaceAll(r.Message, prefix, "")
	if r.Error != nil {
		r.Error.Message = strings.ReplaceAll(r.Error.Message, prefix, "")
	}
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
	// Outputs collects every result's outputs.
	Outputs []string `json:"outputs,omitempty"`
}

// StepsResultFrom summarises a batch; the envelope reflects the first
// failure.
func StepsResultFrom(device string, results []StepResult) StepsResult {
	out := StepsResult{Envelope: Envelope{OK: true}, Device: device, Results: results}
	for i := range results {
		r := &results[i]
		out.Outputs = append(out.Outputs, r.Outputs...)
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
	// ID is the device's id; on a hub it is qualified, <localId>@<hub>.
	ID string `json:"id"`
	// Hub and LocalID, on a hub, split ID: the hub that owns the device
	// and the device's id on that hub's machine.
	Hub        string `json:"hub,omitempty"`
	LocalID    string `json:"localId,omitempty"`
	Platform   string `json:"platform"`       // android | ios | web | mock
	Kind       string `json:"kind,omitempty"` // device | emulator | simulator | tv
	Name       string `json:"name,omitempty"`
	Model      string `json:"model,omitempty"`      // hardware: "Pixel 8" (ro.product.model), "iPhone14,2"
	DeviceType string `json:"deviceType,omitempty"` // configured virtual hardware profile
	OSVersion  string `json:"osVersion,omitempty"`
	State      string `json:"state,omitempty"` // device | offline | Booted | Shutdown | …
	Ready      bool   `json:"ready"`
	// BootedBy is "daemon" for devices this daemon booted, "external" for
	// everything else.
	BootedBy string `json:"bootedBy,omitempty"`
	BootedAt string `json:"bootedAt,omitempty"`
	// Attached is true while the daemon holds a driver for the device.
	Attached bool `json:"attached"`
	// AttachReused is set on attach responses. A reused driver kept its original options.
	AttachReused *bool `json:"attachReused,omitempty"`
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
	// Lease is the device's lease on a hub, when it is leased.
	Lease *DeviceLease `json:"lease,omitempty"`
	// Reservations, on a hub, are the device's reservations that have not
	// started, by start: holder and window, never the lease id.
	Reservations []DeviceLease `json:"reservations,omitempty"`
	// Available is true on a hub for a ready device that is neither leased
	// nor attached.
	Available bool `json:"available,omitempty"`
	// MaxSec, on a hub, is the longest lease window the device takes.
	MaxSec int `json:"maxSec,omitempty"`
	// Machine and Addresses, on a hub, name the machine the device is
	// attached to: its host name, and the owning hub's advertised address
	// plus the host's non-loopback IPs. A client compares them with its
	// own to find the devices on its own machine, whichever hub it asked.
	Machine   string   `json:"machine,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	// TrafficShaping is true when the device's host has both traffic-shaping
	// calls configured for it in its config.json.
	TrafficShaping bool `json:"trafficShaping,omitempty"`
	// Location is where the device is, relative to the caller's computer:
	// local, mesh or cloud. It is computed per request (location.go).
	Location string `json:"location,omitempty"`
	// Provider names the cloud provider that holds the device
	// ("devicefarm"); empty for every other device.
	Provider string `json:"provider,omitempty"`

	// The rest describe a cloud device (remote.go).
	// ProviderRef is the provider's session reference (a Device Farm
	// session ARN); DashboardURL its page in the provider's console.
	ProviderRef  string `json:"providerRef,omitempty"`
	DashboardURL string `json:"dashboardUrl,omitempty"`
	// ExpiresAt is when the provider ends the session (RFC 3339).
	ExpiresAt string `json:"expiresAt,omitempty"`
	// IdleReleaseSec is how long the device may go without a request
	// before it is released; IdleReleaseAt when that happens if none comes.
	IdleReleaseSec int    `json:"idleReleaseSec,omitempty"`
	IdleReleaseAt  string `json:"idleReleaseAt,omitempty"`
	// EndedReason says why a cloud device's session ended (state ended).
	EndedReason string `json:"endedReason,omitempty"`
	// DeviceMinutes is the device time a released session used.
	DeviceMinutes *cloudprov.DeviceMinutes `json:"deviceMinutes,omitempty"`
	FormFactor    string                   `json:"formFactor,omitempty"`
	Manufacturer  string                   `json:"manufacturer,omitempty"`
	// AppiumVersion is the Appium major version a cloud device's provider
	// runs ("2" or "3").
	AppiumVersion string `json:"appiumVersion,omitempty"`
	// Description is the free text the device's host gives it in its
	// config.json, if any.
	Description string `json:"description,omitempty"`
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
	// OutputDir overrides the report directory for this device. A caller
	// that names one owns it: the daemon never deletes it.
	OutputDir string `json:"outputDir,omitempty"`
	// DiscardReports deletes the daemon-owned report directory at detach.
	// Without it the directory stays, and the housekeeping sweep deletes it
	// once it has been idle for reportMaxAge.
	DiscardReports bool `json:"discardReports,omitempty"`
	// ReportDir is the report directory the daemon chose for this session,
	// set before the driver is made so a driver can write beside the
	// report (the Android trace). Never part of the wire format.
	ReportDir string `json:"-"`
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
	// Hub, on a hub, names the hub whose machine boots or creates the
	// device (default: the one asked, or the suffix of a qualified Name).
	Hub string `json:"hub,omitempty"`
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
	// Cloud, instead of the fields above, launches a cloud device
	// (direct mode: a plain daemon and a local caller only).
	Cloud *cloudprov.CloudSpec `json:"cloud,omitempty"`
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
	// Bundle, used instead of Steps or YAML, is an uploaded flow bundle
	// whose root file is the step list; its relative references resolve
	// inside the bundle.
	Bundle *BundleManifest `json:"bundle,omitempty"`
}

// FlowRequest is the body of POST /v1/devices/{dev}/flows.
type FlowRequest struct {
	Test *executor.TestFlowOptions `json:"test,omitempty"`
	// DefaultAppID is inherited from a suite when its child has no app ID or URL.
	DefaultAppID string            `json:"defaultAppId,omitempty"`
	File         string            `json:"file,omitempty"`
	YAML         string            `json:"yaml,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Cwd          string            `json:"cwd,omitempty"`
	// Bundle, used instead of File or YAML, is an uploaded flow bundle;
	// its root file runs as the flow.
	Bundle *BundleManifest `json:"bundle,omitempty"`
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
	// Unreachable, on a hub, names the peers whose devices are missing.
	Unreachable []UnreachablePeer `json:"unreachable,omitempty"`
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
	// Cloud lists the cloud providers this daemon launches devices on.
	Cloud []CloudStatus `json:"cloud,omitempty"`
}

// CloudStatus is one cloud provider a daemon or hub launches devices on.
type CloudStatus struct {
	Provider string `json:"provider"`
	// MaxDevices, on a hub, is how many cloud devices it holds at most
	// (--cloud-max-devices); 0 on a plain daemon, which has no cap.
	MaxDevices int `json:"maxDevices,omitempty"`
	// Held is how many of the provider's devices it holds now.
	Held int `json:"held"`
}

// Info is the content of daemon.json.
type Info struct {
	Name       string   `json:"name"`
	PID        int      `json:"pid"`
	APIVersion string   `json:"apiVersion"`
	Version    string   `json:"version"`
	SocketPath string   `json:"socketPath"`
	HTTPAddr   string   `json:"httpAddr,omitempty"`
	WebAddr    string   `json:"webAddr,omitempty"`
	StartedAt  string   `json:"startedAt"`
	StartArgs  []string `json:"startArgs,omitempty"`
	RunDir     string   `json:"runDir"`
	ReportsDir string   `json:"reportsDir"`
	// IdleTimeout is in seconds; 0 = never.
	IdleTimeout int `json:"idleTimeout"`
	// TLS is true when the TCP listener serves HTTPS.
	TLS bool `json:"tls,omitempty"`
	// Hub is the hub's name when the daemon is a hub (`hub start`): it has
	// leases, and every TCP caller is remote.
	Hub string `json:"hub,omitempty"`
}

// ConfigLocalOnlyMessage is the refusal at both the mesh and the hub handler.
const ConfigLocalOnlyMessage = "device config can be changed only on the hub's own machine, over its unix socket"

// Event is one entry on the /v1/events stream.
type Event struct {
	// PreviouslyShared records public visibility at an unshare transition.
	PreviouslyShared bool   `json:"-"`
	Seq              int64  `json:"seq"`
	Time             string `json:"time"`
	// Type: step | nestedStep | device.attached | device.detached |
	// device.booted | device.stopped | device.capture | daemon.shutdown |
	// lease.granted | lease.released | lease.expired. device.capture's
	// Message is capturing, unavailable or lost, and Error its reason.
	// lease.expired is a lease whose window ran out; every other end of a
	// lease (a release, its device stopped or deleted, its attach failing,
	// the hub stopping) is lease.released.
	Type       string `json:"type"`
	Device     string `json:"device,omitempty"`
	Depth      int    `json:"depth,omitempty"`
	Step       string `json:"step,omitempty"`
	Passed     bool   `json:"passed,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Error      string `json:"error,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	// DeviceMinutes, on device.released, is the device time the cloud
	// session used.
	DeviceMinutes *cloudprov.DeviceMinutes `json:"deviceMinutes,omitempty"`
}
