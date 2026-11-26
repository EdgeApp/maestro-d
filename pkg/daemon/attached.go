package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/executor"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// DriverInfo is what the injected driver factory reports about the driver
// it opened, for the session's report header.
type DriverInfo struct {
	DriverName string
	Device     report.Device
	App        report.App
	// Platform/Kind/Name refine the device registry entry.
	Platform string
	Kind     string
	Name     string
}

// attached is one device with a live driver: the executor.Session plus the
// bookkeeping the server needs.
type attached struct {
	id         string
	lease      <-chan struct{} // owning grant window, nil on a plain daemon
	cfg        AttachConfig
	session    *executor.Session
	cleanup    func()
	info       DeviceInfo
	reportDir  string
	release    func() // Ends this session's use of reportDir
	attachedAt time.Time
	base       context.Context
	cancel     context.CancelFunc
	// busy is held for the duration of a step so `?wait=false` can report
	// BUSY without queueing.
	busy    sync.Mutex
	inFlite int32
	// lastUsed is when a call last held the device (idledetach.go).
	lastUsed atomic.Int64
	// stopCapture ends the device.capture events (watchCapture).
	stopCapture func()
	// closing is closed when a detach's teardown has finished: until then
	// the id stays reserved, so a new attach waits (takeAttached).
	closing chan struct{}
}

var unsafePath = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// attachDevice opens the driver and session for one device.
func (s *Server) attachDevice(ctx context.Context, id string, cfg AttachConfig) (*attached, *Error) {
	if id == "" {
		return nil, Errorf(CodeUsage, "attach: device id is required")
	}
	if s.deps.NewDriver == nil {
		return nil, Errorf(CodeInternal, "attach: no driver factory configured")
	}
	if owner := s.cfg.FindDeviceOwner(id); owner != nil {
		e := Errorf(CodeDeviceInUse, "device %s is attached to daemon %q (pid %d)", id, owner.Name, owner.Info.PID)
		return nil, e.WithDetails("owner", owner.Name, "ownerPid", owner.Info.PID)
	}

	reportDir := cfg.OutputDir
	if reportDir == "" {
		name := time.Now().Format("20060102-150405")
		if leaseEnded(ctx) != nil {
			// Two holders can attach within one second. Each gets its own artifacts.
			name += "-" + uuid.New().String()
		}
		reportDir = filepath.Join(s.cfg.ReportsDir, unsafePath.ReplaceAllString(id, "_"), name)
	}
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return nil, WrapErr(CodeInternal, err)
	}
	// The blobs an attach materializes (a packed .app is a full unzipped
	// copy) are the session's, in a directory of this attach's own under
	// blobs/. The attach counts as a user of reportDir from here, before
	// anything is materialized, so another session's detach never removes
	// blobs/ or bundles/ under an attach still starting. An attach that
	// fails ends its use again, which removes its blobs/<uuid>.
	blobDir := filepath.Join(reportDir, "blobs", uuid.New().String())
	reportKey := canonicalDir(reportDir)
	s.addReportUser(reportKey)
	keep := false
	defer func() {
		if !keep {
			s.releaseReportDir(reportKey, blobDir, false)
		}
	}()
	if e := s.mapAttachBlobs(&cfg, blobDir); e != nil {
		return nil, e
	}
	cfg.ReportDir = reportDir

	rd := s.remote(id)
	if rd != nil {
		if e := cloudAttachConfig(rd, &cfg); e != nil {
			return nil, e
		}
	}
	if rd != nil && cfg.AppFile != "" {
		// An app to install goes through the provider, as the install
		// route does; the app id defaults to the file's.
		appID, e := s.installCloud(ctx, rd, cfg.AppFile, nil)
		if e != nil {
			return nil, e
		}
		if cfg.AppID == "" {
			cfg.AppID = appID
		}
		cfg.AppFile = ""
	} else if strings.HasPrefix(id, "df:") {
		return nil, Errorf(CodeUsage, "%s is a cloud model, not a device: launch it first (device create --provider devicefarm, or lease --location cloud)", id)
	}
	if e := s.verifyBinding(ctx, id); e != nil {
		return nil, e
	}
	base, cancel := context.WithCancel(context.Background())
	logger.Info("Attaching device %s (platform=%s driver=%s app=%s)", id, cfg.Platform, cfg.Driver, cfg.AppID)
	var (
		driver  core.Driver
		dinfo   DriverInfo
		cleanup func()
		err     error
	)
	if rd != nil {
		// A cloud device's driver is the one the daemon holds; detaching
		// keeps it, and the provider's session, for the next attach.
		driver, dinfo, cleanup = rd.driver, rd.dinfo, func() {}
	} else if driver, dinfo, cleanup, err = s.deps.NewDriver(ctx, id, cfg); err != nil {
		cancel()
		// A driver factory that already chose a code (USAGE for a bad
		// computer.json) keeps it; anything else is a device fault.
		var de *Error
		if errors.As(err, &de) {
			return nil, Errorf(de.Code, "attach %s: %s", id, de.Message)
		}
		return nil, WrapErr(CodeDeviceError, fmt.Errorf("attach %s: %w", id, err))
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	if leaseEnded(ctx) != nil && ctx.Err() != nil {
		// The lease (or its client) ended while a driver start that cannot
		// be cancelled ran: the driver is discarded, never attached.
		logger.Info("attach %s: cancelled while the driver started; discarding it", id)
		discardDriver(id, cleanup)
		cancel()
		return nil, Errorf(CodeInterrupted, "attach %s cancelled", id)
	}

	alertMonitor := true
	if cfg.AlertMonitor != nil {
		alertMonitor = *cfg.AlertMonitor
	}
	waitForIdle := 200
	if cfg.WaitForIdleTimeout != nil {
		waitForIdle = *cfg.WaitForIdleTimeout
	}
	rc := executor.RunnerConfig{
		OutputDir:          reportDir,
		Artifacts:          parseArtifacts(cfg.Artifacts),
		Device:             dinfo.Device,
		App:                dinfo.App,
		RunnerVersion:      s.deps.Version,
		DriverName:         dinfo.DriverName,
		Env:                cfg.Env,
		WaitForIdleTimeout: waitForIdle,
		ConditionTimeout:   cfg.ConditionTimeout,
		StepDelay:          cfg.StepDelay,
		TypingFrequency:    cfg.TypingFrequency,
		OnStepComplete: func(idx int, desc string, passed bool, durationMs int64, errMsg string) {
			s.events.Publish(Event{Type: "step", Device: id, Step: desc, Passed: passed, DurationMs: durationMs, Error: errMsg})
		},
		OnNestedStep: func(depth int, desc string, passed bool, durationMs int64, errMsg string) {
			s.events.Publish(Event{Type: "nestedStep", Device: id, Depth: depth, Step: desc, Passed: passed, DurationMs: durationMs, Error: errMsg})
		},
	}
	shaper := s.trafficShaper(id)
	sc := executor.SessionConfig{
		Name:           unsafePath.ReplaceAllString(id, "_"),
		AppID:          cfg.AppID,
		URL:            cfg.URL,
		FlowDir:        cfg.FlowDir,
		AlertMonitor:   alertMonitor,
		CommandTimeout: cfg.CommandTimeout,
		ImportEnv:      s.importsEnv(),
		PassEnv:        s.cfg.PassEnv,
		TrafficShaper:  shaper,
	}
	session, err := executor.NewSession(base, driver, rc, sc)
	if err != nil {
		cleanup()
		cancel()
		return nil, WrapErr(CodeDeviceError, err)
	}

	info := DeviceInfo{
		ID: id, Platform: dinfo.Platform, Kind: dinfo.Kind, Name: dinfo.Name,
		Attached: true, AttachedAt: nowRFC3339(), AppID: cfg.AppID, Driver: dinfo.DriverName, ReportDir: reportDir,
		Ready: true,
	}
	if info.Platform == "" {
		info.Platform = strings.ToLower(cfg.Platform)
	}
	if pi := driver.GetPlatformInfo(); pi != nil {
		if info.Platform == "" {
			info.Platform = strings.ToLower(pi.Platform)
		}
		if info.OSVersion == "" {
			info.OSVersion = pi.OSVersion
		}
	}
	// An emulator's record keeps its AVD's name, as discovery lists it,
	// not the driver's model string: status and a hub-booted emulator's
	// record outlive the session, and config.json names it avd:<name>.
	if name, ok := strings.CutPrefix(shaper.Alias, AVDPrefix); ok && name != "" {
		info.Kind, info.Name = "emulator", name
	}
	a := &attached{id: id, lease: leaseEnded(ctx), cfg: cfg, session: session, cleanup: cleanup, info: info,
		reportDir: reportDir, attachedAt: time.Now(), base: base, cancel: cancel}
	daemonOwned, discard := cfg.OutputDir == "", cfg.DiscardReports
	a.release = func() { s.releaseReportDir(reportKey, blobDir, daemonOwned && discard) }
	s.registry.MarkAttached(info)
	s.events.Publish(Event{Type: "device.attached", Device: id, Message: reportDir})
	a.stopCapture = s.watchCapture(id, driver)
	logger.Info("Attached device %s (report %s)", id, reportDir)
	keep = true
	return a, nil
}

// cloudAttachConfig checks an attach of a cloud device against the driver
// the daemon holds for it, and defaults the app id to the one it was
// launched with. A driver, Appium server, platform or capabilities file
// that differs from the held driver is USAGE.
func cloudAttachConfig(rd *remoteDevice, cfg *AttachConfig) *Error {
	rd.mu.Lock()
	gone := rd.ended
	rd.mu.Unlock()
	switch {
	case gone != "":
		return Errorf(CodeDeviceError, "cloud device %s is gone: %s", rd.id, gone).WithDetails("reason", gone)
	case cfg.Driver != "" && !strings.EqualFold(cfg.Driver, "appium"):
		return Errorf(CodeUsage, "cloud device %s runs the Appium driver the daemon holds for it, not %q", rd.id, cfg.Driver).WithDetails("field", "driver")
	case cfg.AppiumURL != "":
		return Errorf(CodeUsage, "cloud device %s uses its provider's Appium endpoint; appiumUrl does not apply", rd.id).WithDetails("field", "appiumUrl")
	case cfg.Platform != "" && !strings.EqualFold(cfg.Platform, rd.dinfo.Platform):
		return Errorf(CodeUsage, "cloud device %s is %s, not %s", rd.id, rd.dinfo.Platform, cfg.Platform).WithDetails("field", "platform")
	case cfg.Extra["capsFile"] != nil:
		return Errorf(CodeUsage, "cloud device %s was launched with its capabilities; a caps file does not apply", rd.id).WithDetails("field", "extra.capsFile")
	}
	if cfg.AppID == "" {
		cfg.AppID = rd.appID
	}
	return nil
}

// discardDriver runs a driver's cleanup, surviving a panic in it.
func discardDriver(id string, cleanup func()) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("driver cleanup for %s panicked: %v", id, r)
		}
	}()
	cleanup()
}

// baseCtx is cancelled on detach/shutdown.
func (a *attached) baseCtx() context.Context { return a.base }

// close ends the session, tears the driver down and ends the session's
// use of its report directory.
func (a *attached) close() {
	if a.stopCapture != nil {
		a.stopCapture()
	}
	a.cancel()
	// Undo what the session set on the device's network (traffic shape,
	// Android airplane mode), on a fresh context: the attach's is ended.
	a.session.Cleanup(context.Background())
	a.session.Close()
	discardDriver(a.id, a.cleanup)
	if a.release != nil {
		a.release()
	}
}

// canonicalDir is dir as an absolute path with symlinks resolved, so every
// spelling of one report directory counts its users together; dir as given
// when it cannot be resolved.
func canonicalDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Clean(dir)
}

// sessionInputDirs are the directories of session inputs (materialized
// blobs and flow bundles) a report directory holds.
var sessionInputDirs = []string{"blobs", "bundles"}

// reportUse is how the attached sessions of this daemon use one report
// directory: how many there are, and which session-input directories the
// daemon created there (a caller's outputDir may already have its own).
type reportUse struct {
	n       int
	created map[string]bool
}

// ActiveReportDirs returns the report directories still held by sessions or
// their detach cleanups. The returned slice is independent of future changes.
func (s *Server) ActiveReportDirs() []string {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	dirs := make([]string, 0, len(s.reportUsers))
	for dir := range s.reportUsers {
		dirs = append(dirs, dir)
	}
	return dirs
}

// addReportUser counts one more session using reportDir. The first one
// notes which session-input directories do not exist yet, so the daemon
// later removes only those it made.
func (s *Server) addReportUser(reportDir string) {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if s.reportUsers == nil {
		s.reportUsers = map[string]*reportUse{}
	}
	u := s.reportUsers[reportDir]
	if u == nil {
		u = &reportUse{created: map[string]bool{}}
		for _, sub := range sessionInputDirs {
			if _, err := os.Stat(filepath.Join(reportDir, sub)); os.IsNotExist(err) {
				u.created[sub] = true
			}
		}
		s.reportUsers[reportDir] = u
	}
	u.n++
}

// releaseReportDir ends one session's use of reportDir: it removes the
// session's own blobs/<uuid>, and when no session of this daemon uses the
// directory any more, the blobs/ and bundles/ the daemon created there
// (session inputs, not results). discard also removes the whole directory,
// which only a daemon-owned directory asks for.
func (s *Server) releaseReportDir(reportDir, blobDir string, discard bool) {
	if err := os.RemoveAll(blobDir); err != nil {
		logger.Warn("detach: remove %s: %v", blobDir, err)
	}
	// The last user's removal runs under reportMu, which addReportUser
	// also takes: an attach arriving meanwhile waits, then finds the
	// directory as the removal left it.
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	u := s.reportUsers[reportDir]
	if u != nil && u.n > 1 {
		u.n--
		return
	}
	delete(s.reportUsers, reportDir)
	if discard {
		if err := os.RemoveAll(reportDir); err != nil {
			logger.Warn("detach: discard %s: %v", reportDir, err)
		}
		return
	}
	for _, sub := range sessionInputDirs {
		path := filepath.Join(reportDir, sub)
		if u != nil && !u.created[sub] {
			continue // the caller's own directory of that name
		}
		if err := os.RemoveAll(path); err != nil {
			logger.Warn("detach: remove %s: %v", path, err)
		}
	}
}

func parseArtifacts(s string) executor.ArtifactMode {
	switch strings.ToLower(s) {
	case "always":
		return executor.ArtifactAlways
	case "never", "none":
		return executor.ArtifactNever
	default:
		return executor.ArtifactOnFailure
	}
}

// resolveSteps turns a StepsRequest into parsed steps.
func resolveSteps(req StepsRequest) ([]flow.Step, *Error) {
	src := "request"
	if req.Cwd != "" {
		src = filepath.Join(req.Cwd, "request.yaml")
	}
	if req.YAML != "" {
		steps, err := flow.ParseSteps([]byte(req.YAML), src)
		if err != nil {
			return nil, WrapErr(CodeUsage, err)
		}
		return steps, nil
	}
	steps := make([]flow.Step, 0, len(req.Steps))
	for i, raw := range req.Steps {
		st, err := stepFromValue(raw, src)
		if err != nil {
			return nil, WrapErr(CodeUsage, fmt.Errorf("step %d: %w", i, err))
		}
		steps = append(steps, st)
	}
	return steps, nil
}

// stepFromValue parses one step in YAML data-model form: a string (bare
// command) or a single-key map {command: value}.
func stepFromValue(raw any, src string) (flow.Step, error) {
	switch v := raw.(type) {
	case string:
		return buildStep(v, nil, src)
	case map[string]any:
		name := ""
		for k := range v {
			if flow.IsStepType(k) {
				name = k
				break
			}
		}
		if name == "" {
			return nil, fmt.Errorf("no known command in %v", keys(v))
		}
		return buildStep(name, v[name], src)
	default:
		return nil, fmt.Errorf("step must be a string or an object, got %T", raw)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// mapAttachBlobs replaces a blob: appFile or extra.capsFile with the
// session's copy of that blob.
func (s *Server) mapAttachBlobs(cfg *AttachConfig, dir string) *Error {
	mapRef := func(field, ref string) (string, *Error) {
		if _, _, err := executor.ParseBlobRef(ref); err != nil {
			return "", Forbidden("%s %s: %v", field, ref, err).WithDetails("field", field)
		}
		local, err := s.blobs.Materialize(ref, dir)
		if err != nil {
			return "", Errorf(CodeUsage, "%s %s: %v", field, ref, err).WithDetails("field", field)
		}
		return local, nil
	}
	if strings.HasPrefix(cfg.AppFile, "blob:") {
		local, e := mapRef("appFile", cfg.AppFile)
		if e != nil {
			return e
		}
		cfg.AppFile = local
	}
	if v, ok := cfg.Extra["capsFile"].(string); ok && strings.HasPrefix(v, "blob:") {
		local, e := mapRef("extra.capsFile", v)
		if e != nil {
			return e
		}
		extra := maps.Clone(cfg.Extra)
		extra["capsFile"] = local
		cfg.Extra = extra
	}
	return nil
}
