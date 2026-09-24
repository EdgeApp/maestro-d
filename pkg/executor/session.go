package executor

// maestro-d addition. This file is not part of upstream maestro-runner.
// It hosts a long-lived FlowRunner so individual steps can be executed one at
// a time (the daemon's one-shot CLI, REST and JS surfaces) while reusing the
// exact same dispatcher, script engine and report writers a YAML flow uses.
// Nothing in upstream's flow_runner.go changes; this file only reaches into
// the package-private pieces. See docs/daemon/UPSTREAM.md.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// SessionConfig is the flow-level configuration a Session applies once at
// creation, exactly as the `appId:` / `env:` header of a flow file would.
type SessionConfig struct {
	// Name labels the session's report flow (defaults to "session").
	Name string
	// AppID is the default app for launchApp/stopApp/killApp/clearState
	// steps that don't name one. Also exported as ${APP_ID}.
	AppID string
	// URL is the web driver equivalent of AppID.
	URL string
	// Env are session-scoped variables (flow `env:`); they take precedence
	// over RunnerConfig.Env just like a flow header does.
	Env map[string]string
	// FlowDir is the base directory for relative paths in steps (runFlow
	// file, runScript file, takeScreenshot path). Defaults to the current
	// working directory.
	FlowDir string
	// AlertMonitor asks the iOS driver to register its system-alert monitor
	// at session start. Upstream does this only when a flow contains a
	// launchApp step; a session doesn't know its steps ahead of time, so the
	// daemon opts in explicitly.
	AlertMonitor bool
	// CommandTimeout overrides the driver's default element-find timeout in
	// milliseconds (flow `commandTimeout:`). 0 keeps the driver default.
	CommandTimeout int
}

// StepOutcome is the result of executing one step through a Session. It is
// the daemon's wire-level result: everything a caller needs to decide
// pass/fail and to locate the artifacts written into the report directory.
type StepOutcome struct {
	// Index is the step's position in the session report (cmd-NNN).
	Index int `json:"index"`
	// Type is the YAML command name (tapOn, assertVisible, …).
	Type string `json:"type"`
	// Description is the step's human-readable YAML description.
	Description string `json:"description"`
	// Success is false when the step failed and was not optional.
	Success bool `json:"success"`
	// Skipped is true when the step's `platform:` gate did not match the
	// driver (Success is true in that case).
	Skipped bool `json:"skipped,omitempty"`
	// Optional mirrors the step's `optional: true`; a failed optional step
	// reports Success=true with the failure message preserved.
	Optional bool `json:"optional,omitempty"`
	// Message is the driver's status message.
	Message string `json:"message,omitempty"`
	// Element describes the element the step acted on, when any.
	Element *core.ElementInfo `json:"element,omitempty"`
	// Data carries step-specific payloads (screenshot bytes, extracted
	// text, cookies, …). Byte slices marshal as base64.
	Data any `json:"data,omitempty"`
	// DurationMs is wall-clock time for the step.
	DurationMs int64 `json:"durationMs"`
	// Artifacts are report-relative paths captured for this step.
	Artifacts report.CommandArtifacts `json:"artifacts"`
	// Error is the failure, when Success is false (or the optional step
	// failed). Cancelled reports whether the caller's context ended it and
	// Panicked whether the driver panicked (the daemon maps that to a device
	// error rather than a command failure).
	Error     *report.Error `json:"error,omitempty"`
	Cancelled bool          `json:"cancelled,omitempty"`
	Panicked  bool          `json:"panicked,omitempty"`
	// SubSteps are the nested results of compound steps (runFlow, repeat,
	// retry), straight from the report.
	SubSteps []report.Command `json:"subSteps,omitempty"`
}

// Err returns the outcome as a Go error (nil on success).
func (o *StepOutcome) Err() error {
	if o.Success {
		return nil
	}
	if o.Error != nil {
		return fmt.Errorf("%s", o.Error.Message)
	}
	if o.Message != "" {
		return fmt.Errorf("%s", o.Message)
	}
	return fmt.Errorf("%s failed", o.Type)
}

// Session executes steps one at a time against a driver, sharing a single
// script engine (variables, output, copied text), report and driver
// settings across all of them. Calls are serialised; a step runs to
// completion (or context cancellation) before the next one starts.
//
// The Session owns the report writers but not the driver: the caller that
// created the driver closes it after Close().
type Session struct {
	mu      sync.Mutex
	baseCtx context.Context
	fr      *FlowRunner
	cfg     SessionConfig
	closed  bool
	failed  bool
}

// NewSession creates the report skeleton, applies the flow-level prelude
// (variables, timeouts, alert monitor, WDA session) and returns a ready
// Session. ctx is the session's base context: a step runs under the
// per-call context passed to Execute, and the driver is reset to ctx after
// each step.
func NewSession(ctx context.Context, driver core.Driver, rc RunnerConfig, sc SessionConfig) (*Session, error) {
	if driver == nil {
		return nil, fmt.Errorf("session: driver is nil")
	}
	if sc.Name == "" {
		sc.Name = "session"
	}
	if sc.FlowDir == "" {
		if wd, err := os.Getwd(); err == nil {
			sc.FlowDir = wd
		}
	}
	if rc.OutputDir == "" {
		rc.OutputDir = filepath.Join(os.TempDir(), "maestro-d", sc.Name)
	}
	if err := os.MkdirAll(rc.OutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create output dir: %w", err)
	}

	// A synthetic zero-step flow gives the report its skeleton; steps are
	// appended to it as they execute.
	f := flow.Flow{
		SourcePath: filepath.Join(sc.FlowDir, sc.Name+".yaml"),
		Config: flow.Config{
			Name:           sc.Name,
			AppID:          sc.AppID,
			URL:            sc.URL,
			Env:            sc.Env,
			CommandTimeout: sc.CommandTimeout,
		},
	}
	index, details, err := report.BuildSkeleton([]flow.Flow{f}, report.BuilderConfig{
		OutputDir:     rc.OutputDir,
		Device:        rc.Device,
		App:           rc.App,
		CI:            rc.CI,
		RunnerVersion: rc.RunnerVersion,
		DriverName:    rc.DriverName,
	})
	if err != nil {
		return nil, fmt.Errorf("session: build report skeleton: %w", err)
	}
	if err := report.WriteSkeleton(rc.OutputDir, index, details); err != nil {
		return nil, fmt.Errorf("session: write report skeleton: %w", err)
	}
	iw := report.NewIndexWriter(rc.OutputDir, index)
	iw.Start()
	detail := &details[0]

	fr := &FlowRunner{
		ctx:         ctx,
		flow:        f,
		detail:      detail,
		driver:      driver,
		config:      rc,
		indexWriter: iw,
		flowWriter:  report.NewFlowWriter(detail, rc.OutputDir, iw),
		script:      NewScriptEngine(),
		totalFlows:  1,
	}

	// Prelude — mirrors FlowRunner.Run() up to the first step.
	driver.SetContext(ctx)
	fr.script.ImportSystemEnv()
	fr.script.SetVariables(rc.Env)
	fr.script.SetFlowDir(sc.FlowDir)
	if info := driver.GetPlatformInfo(); info != nil {
		fr.script.SetPlatform(info.Platform)
	}
	if fr.flow.Config.AppID != "" {
		fr.flow.Config.AppID = fr.script.ExpandVariables(fr.flow.Config.AppID)
	}
	if appID := fr.flow.Config.EffectiveAppID(); appID != "" {
		fr.script.SetVariable("APP_ID", appID)
	}
	for k, v := range fr.flow.Config.Env {
		fr.script.SetVariable(k, fr.script.ExpandVariables(v))
	}
	if sc.CommandTimeout > 0 {
		driver.SetFindTimeout(sc.CommandTimeout)
	}
	fr.script.SetConditionTimeout(rc.ConditionTimeout)
	fr.waitForIdleTimeout = rc.WaitForIdleTimeout
	_ = driver.SetWaitForIdleTimeout(rc.WaitForIdleTimeout)
	inner := core.Unwrap(driver)
	if configurer, ok := inner.(core.TypingFrequencyConfigurer); ok && rc.TypingFrequency > 0 {
		_ = configurer.SetTypingFrequency(rc.TypingFrequency)
	}
	if preparer, ok := inner.(core.FlowAware); ok {
		// The WDA driver enables its alert monitor when it sees a launchApp
		// step; feed it one when the session asks for the monitor.
		var prepare []flow.Step
		if sc.AlertMonitor {
			prepare = []flow.Step{&flow.LaunchAppStep{
				BaseStep: flow.BaseStep{StepType: flow.StepLaunchApp},
				AppID:    fr.flow.Config.EffectiveAppID(),
			}}
		}
		preparer.PrepareForFlow(prepare)
	}
	if ensurer, ok := inner.(core.SessionEnsurer); ok {
		// A session is what makes the driver's interactive commands work
		// (on WDA every tap, swipe and key press is a session-scoped
		// route). It is created with no app id on purpose: WDA launches
		// the app named in a session request, and attaching must leave
		// whatever is on screen alone. AppID stays the default for an
		// explicit launchApp step. Without a session nothing interactive
		// can succeed, so failing here is an attach failure, not a warning.
		if err := ensurer.EnsureSession(""); err != nil {
			return nil, fmt.Errorf("session: %w", err)
		}
	}
	resetConsoleLogs(driver)
	fr.flowWriter.Start()

	return &Session{baseCtx: ctx, fr: fr, cfg: sc}, nil
}

// Config returns the session's configuration.
func (s *Session) Config() SessionConfig { return s.cfg }

// OutputDir is the report directory the session writes to.
func (s *Session) OutputDir() string { return s.fr.config.OutputDir }

// Driver returns the underlying driver.
func (s *Session) Driver() core.Driver { return s.fr.driver }

// Execute runs one step and records it in the session report. ctx bounds
// the step; cancelling it fails the step with Cancelled=true and leaves the
// session usable. A step's own `timeout:` and `optional:` fields apply
// exactly as in a flow. Execute never panics: a panicking driver produces a
// failed outcome with Panicked=true.
func (s *Session) Execute(ctx context.Context, step flow.Step) *StepOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executeLocked(ctx, step)
}

// ExecuteAll runs steps in order, stopping at the first non-optional
// failure (or context cancellation) unless continueOnError is set. It
// returns every outcome produced.
func (s *Session) ExecuteAll(ctx context.Context, steps []flow.Step, continueOnError bool) []*StepOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	outs := make([]*StepOutcome, 0, len(steps))
	for i, step := range steps {
		if i > 0 && s.fr.config.StepDelay > 0 {
			select {
			case <-time.After(time.Duration(s.fr.config.StepDelay) * time.Millisecond):
			case <-ctx.Done():
			}
		}
		out := s.executeLocked(ctx, step)
		outs = append(outs, out)
		if !out.Success && !continueOnError {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	return outs
}

// RunFlow executes a parsed flow as one compound "runFlow" step: its
// onFlowStart hooks, steps and onFlowComplete hooks run inside the session
// (sharing variables), and each nested step lands in SubSteps.
func (s *Session) RunFlow(ctx context.Context, f flow.Flow) *StepOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closedOutcome(flow.StepRunFlow, "runFlow")
	}
	fr := s.fr
	name := f.Config.Name
	if name == "" && f.SourcePath != "" {
		name = filepath.Base(f.SourcePath)
	}
	if name == "" {
		name = "inline"
	}
	if f.Config.AppID != "" {
		f.Config.AppID = fr.script.ExpandVariables(f.Config.AppID)
	}
	desc := "runFlow: " + name
	idx := s.appendCommand(report.Command{Type: string(flow.StepRunFlow), YAML: desc, Label: name})
	fr.flowWriter.CommandStart(idx)
	start := time.Now()

	restore := s.enterStep(ctx)
	fr.depth++
	var result *core.CommandResult
	var panicked bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				logger.Error("session: panic in runFlow %s: %v\n%s", name, r, debug.Stack())
				result = &core.CommandResult{Success: false, Error: fmt.Errorf("driver panic: %v", r), Message: fmt.Sprintf("driver panic: %v", r)}
			}
		}()
		result = s.runFlowSteps(f)
	}()
	fr.depth--
	restore()
	subs := fr.subCommands
	fr.subCommands = nil

	if result == nil {
		result = &core.CommandResult{Success: false, Error: fmt.Errorf("no result"), Message: "no result"}
	}
	return s.finishStep(ctx, idx, desc, string(flow.StepRunFlow), false, result, start, panicked, subs, report.CommandArtifacts{})
}

// runFlowSteps runs a flow's hooks and steps through the nested dispatcher.
func (s *Session) runFlowSteps(f flow.Flow) (result *core.CommandResult) {
	fr := s.fr
	defer func() {
		for _, step := range f.Config.OnFlowComplete {
			fr.executeNestedStep(step) // cleanup: failures ignored, as upstream does
		}
	}()
	for _, step := range f.Config.OnFlowStart {
		r := fr.executeNestedStep(step)
		if !r.Success && !step.IsOptional() {
			return &core.CommandResult{Success: false, Error: fmt.Errorf("onFlowStart failed: %v", r.Error), Message: fmt.Sprintf("onFlowStart failed: %v", r.Error)}
		}
	}
	return fr.executeSubFlow(f)
}

// Screenshot returns a PNG of the current screen.
func (s *Session) Screenshot(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSessionClosed
	}
	defer s.enterStep(ctx)()
	return s.fr.driver.Screenshot()
}

// Hierarchy returns the driver's view hierarchy (XML or JSON, per driver).
func (s *Session) Hierarchy(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSessionClosed
	}
	defer s.enterStep(ctx)()
	return s.fr.driver.Hierarchy()
}

// State returns the driver's current state snapshot.
func (s *Session) State(ctx context.Context) *core.StateSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.enterStep(ctx)()
	return s.fr.driver.GetState()
}

// PlatformInfo returns the driver's platform information.
func (s *Session) PlatformInfo() *core.PlatformInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fr.driver.GetPlatformInfo()
}

// Vars returns a copy of the script engine's variables.
func (s *Session) Vars() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The engine imports the process environment (so ${HOME} works in
	// flows); hide entries that are still just that, they are noise — and
	// the daemon's environment is nobody else's business.
	vars := s.fr.script.Variables()
	for k, v := range vars {
		if ev, ok := os.LookupEnv(k); ok && ev == v {
			delete(vars, k)
		}
	}
	return vars
}

// SetVar sets a variable visible to ${VAR} expansion and scripts.
func (s *Session) SetVar(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fr.script.SetVariable(name, value)
}

// Eval evaluates JavaScript in the session's script engine and returns the
// exported result. Assignments to `output.*` become variables, as they do
// for evalScript.
func (s *Session) Eval(js string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSessionClosed
	}
	v, err := s.fr.script.js.Eval(js)
	s.fr.script.SyncOutputToVariables()
	return v, err
}

// Failed reports whether any non-optional step has failed in this session.
func (s *Session) Failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// Close finalises the report and releases the script engine. The driver is
// left to its owner. Close is idempotent.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	status := report.StatusPassed
	if s.failed {
		status = report.StatusFailed
	}
	s.fr.flowWriter.End(status)
	s.fr.indexWriter.End()
	s.fr.indexWriter.Close()
	s.fr.script.Close()
}

var errSessionClosed = fmt.Errorf("session is closed")

func (s *Session) closedOutcome(typ flow.StepType, desc string) *StepOutcome {
	return &StepOutcome{
		Type:        string(typ),
		Description: desc,
		Error:       &report.Error{Type: "unknown", Message: errSessionClosed.Error()},
		Message:     errSessionClosed.Error(),
	}
}

// enterStep points the runner and driver at the per-call context and
// returns a func that restores the base context.
func (s *Session) enterStep(ctx context.Context) func() {
	if ctx == nil {
		ctx = s.baseCtx
	}
	s.fr.ctx = ctx
	s.fr.driver.SetContext(ctx)
	return func() {
		s.fr.ctx = s.baseCtx
		s.fr.driver.SetContext(s.baseCtx)
	}
}

// appendCommand adds a report entry for a step about to run and returns its
// index. The FlowWriter reads Commands through the shared *FlowDetail, so
// the new entry is visible to CommandStart/CommandEnd immediately.
func (s *Session) appendCommand(cmd report.Command) int {
	idx := len(s.fr.detail.Commands)
	cmd.ID = fmt.Sprintf("cmd-%03d", idx)
	cmd.Index = idx
	cmd.Status = report.StatusPending
	s.fr.detail.Commands = append(s.fr.detail.Commands, cmd)
	return idx
}

func (s *Session) executeLocked(ctx context.Context, step flow.Step) *StepOutcome {
	if s.closed {
		return s.closedOutcome(step.Type(), step.Describe())
	}
	fr := s.fr
	desc := step.Describe()
	typ := string(step.Type())

	// Build the report entry the same way BuildSkeleton would so params and
	// selectors render identically.
	_, details, err := report.BuildSkeleton([]flow.Flow{{Steps: []flow.Step{step}}}, report.BuilderConfig{})
	var cmd report.Command
	if err == nil && len(details) == 1 && len(details[0].Commands) == 1 {
		cmd = details[0].Commands[0]
	} else {
		cmd = report.Command{Type: typ, YAML: desc, Label: step.Label()}
	}
	idx := s.appendCommand(cmd)
	fr.flowWriter.CommandStart(idx)
	start := time.Now()

	// Step-level platform gate, as executeStep applies it.
	if gate := step.PlatformGate(); gate != "" {
		if info := fr.driver.GetPlatformInfo(); info != nil && !strings.EqualFold(info.Platform, gate) {
			fr.flowWriter.CommandEnd(idx, report.StatusSkipped, nil, nil, report.CommandArtifacts{})
			return &StepOutcome{Index: idx, Type: typ, Description: desc, Success: true, Skipped: true,
				Message: fmt.Sprintf("skipped: platform gate %q != %q", gate, info.Platform)}
		}
	}

	var before report.CommandArtifacts
	if fr.config.Artifacts == ArtifactAlways {
		before = fr.captureArtifacts(idx, "before")
	}

	restore := s.enterStep(ctx)
	fr.subCommands = nil
	var result *core.CommandResult
	var panicked, compound bool
	var screenshotPath string
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				logger.Error("session: panic in %s: %v\n%s", desc, r, debug.Stack())
				result = &core.CommandResult{Success: false, Error: fmt.Errorf("driver panic: %v", r), Message: fmt.Sprintf("driver panic: %v", r)}
			}
		}()
		// Compound steps and takeScreenshot take the top-level route
		// executeStep uses: their sub-commands / artifact land on this entry
		// (the nested route would file them under a sub-command instead).
		switch st := step.(type) {
		case *flow.RepeatStep:
			fr.script.ExpandStep(st)
			compound = true
			result = fr.executeRepeat(st)
		case *flow.RetryStep:
			fr.script.ExpandStep(st)
			compound = true
			result = fr.executeRetry(st)
		case *flow.RunFlowStep:
			fr.script.ExpandStep(st)
			compound = true
			result = fr.executeRunFlow(st)
		case *flow.TakeScreenshotStep:
			fr.script.ExpandStep(st)
			result, screenshotPath = fr.executeTakeScreenshot(st, idx)
		default:
			s.applyDefaultAppID(step)
			result = fr.executeNestedStep(step)
		}
	}()
	restore()

	// Compound steps accumulate their sub-commands directly; every other
	// step is recorded by executeNestedStep as one sub-command we discard.
	var subs []report.Command
	if compound {
		subs = fr.subCommands
	}
	fr.subCommands = nil

	if result == nil {
		result = &core.CommandResult{Success: false, Error: fmt.Errorf("no result"), Message: "no result"}
	}
	if screenshotPath != "" {
		before.ScreenshotAfter = screenshotPath
	}
	return s.finishStep(ctx, idx, desc, typ, step.IsOptional(), result, start, panicked, subs, before)
}

// applyDefaultAppID fills the session appId into app-lifecycle steps that
// don't name one, as executeStep does for the main flow.
func (s *Session) applyDefaultAppID(step flow.Step) {
	appID := s.fr.flow.Config.EffectiveAppID()
	if appID == "" {
		return
	}
	switch st := step.(type) {
	case *flow.LaunchAppStep:
		if st.AppID == "" {
			st.AppID = appID
		}
	case *flow.StopAppStep:
		if st.AppID == "" {
			st.AppID = appID
		}
	case *flow.KillAppStep:
		if st.AppID == "" {
			st.AppID = appID
		}
	case *flow.ClearStateStep:
		if st.AppID == "" {
			st.AppID = appID
		}
	case *flow.SetPermissionsStep:
		if st.AppID == "" {
			st.AppID = appID
		}
	}
}

// finishStep records the result in the report and builds the outcome. seed
// carries artifacts captured before/during the step (a "before" screenshot,
// a takeScreenshot's own file) that the after-capture must not clobber.
func (s *Session) finishStep(ctx context.Context, idx int, desc, typ string, optional bool,
	result *core.CommandResult, start time.Time, panicked bool, subs []report.Command,
	seed report.CommandArtifacts) *StepOutcome {
	fr := s.fr
	dur := time.Since(start).Milliseconds()
	out := &StepOutcome{
		Index:       idx,
		Type:        typ,
		Description: desc,
		Optional:    optional,
		Message:     result.Message,
		Element:     result.Element,
		Data:        result.Data,
		DurationMs:  dur,
		Panicked:    panicked,
		SubSteps:    subs,
	}

	status := report.StatusPassed
	var errInfo *report.Error
	if result.Success {
		out.Success = true
	} else {
		errInfo = commandResultToError(result)
		if errInfo == nil {
			errInfo = &report.Error{Type: "unknown", Message: result.Message}
		}
		if panicked {
			errInfo.Type = "device"
		}
		if ctx != nil && ctx.Err() != nil {
			out.Cancelled = true
			if errInfo.Type == "unknown" || errInfo.Type == "" {
				errInfo.Type = "timeout"
			}
		}
		out.Error = errInfo
		if optional {
			out.Success = true
			status = report.StatusPassed
		} else {
			status = report.StatusFailed
			s.failed = true
		}
	}

	artifacts := seed
	if (!result.Success && fr.config.Artifacts != ArtifactNever) || fr.config.Artifacts == ArtifactAlways {
		after := fr.captureArtifacts(idx, "after")
		if artifacts.ScreenshotAfter == "" {
			artifacts.ScreenshotAfter = after.ScreenshotAfter
		}
		artifacts.ViewHierarchy = after.ViewHierarchy
	}
	out.Artifacts = artifacts

	fr.flowWriter.CommandEndWithSubs(idx, status, commandResultToElement(result), errInfo, artifacts, subs)
	if fr.config.OnStepComplete != nil {
		errMsg := ""
		if errInfo != nil {
			errMsg = errInfo.Message
		}
		fr.config.OnStepComplete(idx, desc, status != report.StatusFailed, dur, errMsg)
	}
	return out
}
