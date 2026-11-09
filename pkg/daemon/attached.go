package daemon

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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
	cfg        AttachConfig
	session    *executor.Session
	cleanup    func()
	info       DeviceInfo
	reportDir  string
	attachedAt time.Time
	base       context.Context
	cancel     context.CancelFunc
	// busy is held for the duration of a step so `?wait=false` can report
	// BUSY without queueing.
	busy    sync.Mutex
	inFlite int32
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
	if owner := FindDeviceOwner(id, s.cfg.Name); owner != nil {
		e := Errorf(CodeDeviceInUse, "device %s is attached to daemon %q (pid %d)", id, owner.Name, owner.Info.PID)
		return nil, e.WithDetails("owner", owner.Name, "ownerPid", owner.Info.PID)
	}

	reportDir := cfg.OutputDir
	if reportDir == "" {
		reportDir = filepath.Join(s.cfg.ReportsDir, unsafePath.ReplaceAllString(id, "_"), time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return nil, WrapErr(CodeInternal, err)
	}
	// The blobs an attach materializes (a packed .app is a full unzipped
	// copy) are the session's, in a directory of this attach's own under
	// blobs/: an attach that fails removes that directory again, and
	// never another attach's blobs in a shared outputDir. blobs/ itself
	// goes too when this attach made it and it is left empty.
	blobsRoot := filepath.Join(reportDir, "blobs")
	_, statErr := os.Stat(blobsRoot)
	madeRoot := statErr != nil
	blobDir := filepath.Join(blobsRoot, uuid.New().String())
	keep := false
	defer func() {
		if keep {
			return
		}
		if err := os.RemoveAll(blobDir); err != nil {
			logger.Warn("attach %s: remove %s: %v", id, blobDir, err)
		}
		if madeRoot {
			_ = os.Remove(blobsRoot) // only when empty
		}
	}()
	if e := s.mapAttachBlobs(&cfg, blobDir); e != nil {
		return nil, e
	}

	base, cancel := context.WithCancel(context.Background())
	logger.Info("Attaching device %s (platform=%s driver=%s app=%s)", id, cfg.Platform, cfg.Driver, cfg.AppID)
	driver, dinfo, cleanup, err := s.deps.NewDriver(ctx, id, cfg)
	if err != nil {
		cancel()
		return nil, WrapErr(CodeDeviceError, fmt.Errorf("attach %s: %w", id, err))
	}
	if cleanup == nil {
		cleanup = func() {}
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
	sc := executor.SessionConfig{
		Name:           unsafePath.ReplaceAllString(id, "_"),
		AppID:          cfg.AppID,
		URL:            cfg.URL,
		FlowDir:        cfg.FlowDir,
		AlertMonitor:   alertMonitor,
		CommandTimeout: cfg.CommandTimeout,
		ImportEnv:      s.importsEnv(),
		PassEnv:        s.cfg.PassEnv,
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
	a := &attached{id: id, cfg: cfg, session: session, cleanup: cleanup, info: info,
		reportDir: reportDir, attachedAt: time.Now(), base: base, cancel: cancel}
	s.registry.MarkAttached(info)
	s.events.Publish(Event{Type: "device.attached", Device: id, Message: reportDir})
	logger.Info("Attached device %s (report %s)", id, reportDir)
	keep = true
	return a, nil
}

// baseCtx is cancelled on detach/shutdown.
func (a *attached) baseCtx() context.Context { return a.base }

// close ends the session and tears the driver down.
func (a *attached) close() {
	a.cancel()
	a.session.Close()
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("driver cleanup for %s panicked: %v", a.id, r)
			}
		}()
		a.cleanup()
	}()
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
			return "", forbidden("%s %s: %v", field, ref, err).WithDetails("field", field)
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
