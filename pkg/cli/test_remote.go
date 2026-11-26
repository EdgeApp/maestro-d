package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/executor"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
	"github.com/devicelab-dev/maestro-runner/pkg/version"
	"github.com/urfave/cli/v2"
)

// The test runner uses flag.FlagSet, which stops at its first positional
// argument. A targeted test accepts the one-shot CLI's interspersed flags.
// The root accepts both global and test flags, including --device.
func hubTestArgs(argv []string, flags []cli.Flag) []string {
	takesValue := make(map[string]bool)
	for _, flag := range flags {
		f, ok := flag.(cli.DocGenerationFlag)
		for _, name := range flag.Names() {
			takesValue[name] = ok && f.TakesValue()
		}
	}
	global := make(map[string]bool)
	for _, flag := range GlobalFlags {
		for _, name := range flag.Names() {
			global[name] = true
		}
	}
	options := []string{argv[0]}
	var testOptions []string
	var paths []string
	test, targeted := false, os.Getenv("MAESTRO_D_HUB") != ""
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			paths = append(paths, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if test {
				paths = append(paths, arg)
			} else if arg == "test" {
				test = true
			} else {
				return argv // one-shot or other command, including "hub"
			}
			continue
		}
		name, value, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "target-hub" {
			targeted = true
			if value == "" {
				arg = "--target-hub=localhost:7433"
			}
		}
		dest := &testOptions
		if global[name] {
			dest = &options
		}
		*dest = append(*dest, arg)
		if name != "target-hub" && !assigned && takesValue[name] && i+1 < len(argv) {
			i++
			*dest = append(*dest, argv[i])
		}
	}
	if !test || !targeted {
		return argv
	}
	options = append(options, "test")
	options = append(options, testOptions...)
	return append(append(options, "--"), paths...)
}

// An omitted driver lets the hub select its device's native or provider driver.
func hubTestAttachConfig(cfg *RunConfig, artifacts string) daemon.AttachConfig {
	return daemon.AttachConfig{
		Platform: cfg.Platform, AppID: cfg.AppID, AppFile: cfg.AppFile,
		Env: cfg.Env, TeamID: cfg.TeamID, WDABundleID: cfg.WDABundleID,
		NoAppInstall: cfg.NoAppInstall, NoDriverInstall: cfg.NoDriverInstall,
		DriverStartTimeout: cfg.DriverStartTimeout, WaitForIdleTimeout: &cfg.WaitForIdleTimeout,
		ConditionTimeout: cfg.ConditionTimeout, StepDelay: cfg.StepDelay,
		TypingFrequency: cfg.TypingFrequency, Artifacts: artifacts,
		Extra: map[string]any{
			"androidTcpForward": cfg.AndroidTCPForward,
			"noFlutterFallback": cfg.NoFlutterFallback,
		},
	}
}

type hubFlowResult struct {
	device  report.Device
	started time.Time
	result  *daemon.StepResult
	err     error
}

func runHubTest(ctx context.Context, cfg *RunConfig, opts daemonOpts) error {
	workers := max(cfg.Parallel, 1)
	if len(cfg.Devices) != workers {
		return daemon.Errorf(daemon.CodeUsage, "test --target-hub requires one leased --device per worker (got %d for %d)", len(cfg.Devices), workers)
	}
	flows, err := validateAndParseFlows(cfg)
	if err != nil {
		return err
	}
	flows = executor.ExpandSuites(flows)
	if cfg.RetryFailed {
		flows, err = narrowToPreviousFailures(cfg.ReportBaseDir, flows)
		if err != nil || len(flows) == 0 {
			return err
		}
	}
	index, details, err := report.BuildSkeleton(flows, report.BuilderConfig{RunnerVersion: version.Version})
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	results := make([]hubFlowResult, len(flows))
	type target struct {
		client *daemon.Client
		device report.Device
	}
	targets := make([]target, workers)
	for worker, device := range cfg.Devices {
		workerOpts := opts
		workerOpts.Device = device
		client, err := connectDaemon(ctx, &workerOpts, false)
		if err != nil {
			return err
		}
		id, info, err := leaseAndAttach(ctx, client, &workerOpts, device, false)
		if err != nil {
			return err
		}
		// A restored grant can defer attachment until its first flow. Resolve the
		// inventory row for that grant before recording the target in the report.
		if !info.Attached {
			info, err = client.Device(ctx, id)
			if err != nil {
				return err
			}
		}
		if seen[id] {
			return daemon.Errorf(daemon.CodeUsage, "parallel workers require distinct leased devices: %s", id)
		}
		seen[id] = true
		deviceInfo := report.Device{
			ID: id, Name: info.Name, Platform: info.Platform, OSVersion: info.OSVersion,
			Model: info.Model, IsSimulator: info.Kind == "simulator" || info.Kind == "emulator",
		}
		if worker == 0 {
			index.Device = deviceInfo
		}
		targets[worker] = target{client, deviceInfo}
	}
	var wg sync.WaitGroup
	for worker, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := worker; i < len(flows); i += workers {
				results[i] = runHubFlow(ctx, target.client, target.device, flows[i], cfg, details[i].ID)
				if ctx.Err() != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return daemon.Errorf(daemon.CodeInterrupted, "interrupted")
	}
	return writeHubTestReport(cfg.OutputDir, index, details, results)
}

func runHubFlow(ctx context.Context, client *daemon.Client, device report.Device, f flow.Flow, cfg *RunConfig, flowID string) hubFlowResult {
	r := hubFlowResult{started: time.Now(), device: device}
	env := executor.FlowEnvironment(f.Config, cfg.Env)
	bundle, err := packFlow([]string{f.SourcePath}, packFlowOpts{Env: env})
	if err == nil {
		err = uploadBundle(ctx, client, device.ID, bundle, false, true)
	}
	if err != nil {
		r.err = err
		return r
	}
	r.result, r.err = client.Flow(ctx, device.ID, daemon.FlowRequest{
		Bundle: &bundle.Manifest, Env: f.Config.Env, DefaultAppID: f.Config.EffectiveAppID(),
		Test: &executor.TestFlowOptions{Env: cfg.Env, StepDelay: cfg.StepDelay, Artifacts: cfg.Artifacts},
	}, true)
	if r.result != nil {
		assignFlowCapture(r.result)
		err := fetchBundleOutputs(ctx, client, device.ID, bundle.Dir, r.result.Outputs, true)
		if r.err == nil {
			r.err = err
		}
		if err := fetchHubReportAssets(ctx, client, device.ID, cfg.OutputDir, flowID, r.result.SubSteps); err != nil {
			if r.err == nil {
				r.err = err
			} else {
				r.err = fmt.Errorf("%w (report artifacts: %v)", r.err, err)
			}
		}
	}
	return r
}

// The hub executes the bundle as one runFlow step. Its captures bracket the
// entire flow. Keep the real commands in the report and give its first and last
// command those boundary captures when they have no more specific diagnostics.
func assignFlowCapture(result *daemon.StepResult) {
	a := result.Artifacts
	if a == (report.CommandArtifacts{}) {
		return
	}
	if len(result.SubSteps) == 0 {
		status := report.StatusPassed
		if result.Error != nil {
			status = report.StatusFailed
		}
		result.SubSteps = []report.Command{{ID: "runFlow", Type: "runFlow", Label: result.Step, Status: status}}
	}
	first := &result.SubSteps[0].Artifacts
	last := first
	for i := range result.SubSteps {
		if result.SubSteps[i].Status != report.StatusSkipped {
			last = &result.SubSteps[i].Artifacts
		}
	}
	if first.ScreenshotBefore == "" {
		first.ScreenshotBefore = a.ScreenshotBefore
	}
	if last.ScreenshotAfter == "" {
		last.ScreenshotAfter = a.ScreenshotAfter
	}
	if last.ViewHierarchy == "" {
		last.ViewHierarchy = a.ViewHierarchy
	}
}

// Collect each referenced file once, including screenshots inside compound steps.
// Clear the remote references before any local renderer can read them.
func collectHubReportAssets(commands []report.Command, refs map[string][]*string) {
	for i := range commands {
		c := &commands[i]
		for _, field := range []*string{&c.Artifacts.ScreenshotBefore, &c.Artifacts.ScreenshotAfter, &c.Artifacts.ViewHierarchy} {
			if *field != "" {
				refs[*field] = append(refs[*field], field)
				*field = ""
			}
		}
		collectHubReportAssets(c.SubCommands, refs)
	}
}

func fetchHubReportAssets(ctx context.Context, client *daemon.Client, device, dir, flowID string, commands []report.Command) error {
	refs := make(map[string][]*string)
	collectHubReportAssets(commands, refs)
	for i, remote := range slices.Sorted(maps.Keys(refs)) {
		name, err := filepath.Localize(remote)
		if err != nil || !strings.HasPrefix(remote, "assets/") || strings.Contains(remote, `\`) {
			return daemon.Errorf(daemon.CodeInternal, "unexpected hub report artifact path %q", remote)
		}
		// Allure flattens its attachments. Include the local flow and file numbers
		// so multiple workers can return identically named screenshots.
		local := filepath.Join("assets", flowID, fmt.Sprintf("%s-%03d-%s", flowID, i, filepath.Base(name)))
		if err := fetchArtifact(ctx, client, device, remote, filepath.Join(dir, local)); err != nil {
			return err
		}
		for _, field := range refs[remote] {
			*field = filepath.ToSlash(local)
		}
	}
	return nil
}

func writeHubTestReport(dir string, index *report.Index, details []report.FlowDetail, results []hubFlowResult) error {
	end := time.Now()
	index.EndTime, index.LastUpdated = &end, end
	index.Summary.Pending = 0
	index.Status = report.StatusPassed
	run := executor.RunResult{TotalFlows: len(results), Duration: end.Sub(index.StartTime).Milliseconds()}
	var firstErr error
	for i, r := range results {
		entry, detail := &index.Flows[i], &details[i]
		status := report.StatusPassed
		if r.err != nil {
			status = report.StatusFailed
			index.Status = status
			message := r.err.Error()
			entry.Error = &message
			if firstErr == nil {
				firstErr = r.err
			}
			index.Summary.Failed++
		} else {
			index.Summary.Passed++
		}
		var duration int64
		detail.Commands = nil
		if r.result != nil {
			duration, detail.Commands = r.result.DurationMs, r.result.SubSteps
		}
		ended := r.started.Add(time.Duration(duration) * time.Millisecond)
		entry.Device, detail.Device = &r.device, &r.device
		entry.Status, entry.StartTime, entry.EndTime, entry.Duration, entry.Attempts = status, &r.started, &ended, &duration, 1
		detail.StartTime, detail.EndTime, detail.Duration = r.started, &ended, &duration
		counts := report.CommandSummary{Total: len(detail.Commands)}
		for _, cmd := range detail.Commands {
			switch cmd.Status {
			case report.StatusPassed:
				counts.Passed++
			case report.StatusFailed:
				counts.Failed++
			case report.StatusSkipped:
				counts.Skipped++
			}
		}
		entry.Commands = counts
		if r.result != nil && r.result.TestReport != nil {
			flowReport := r.result.TestReport
			counts = flowReport.Steps
			detail.StepLatency, detail.ConsoleLogs = flowReport.StepLatency, flowReport.ConsoleLogs
		}
		run.FlowResults = append(run.FlowResults, executor.FlowResult{
			ID: entry.ID, Name: entry.Name, SourceFile: entry.SourceFile, Status: status,
			Duration: duration, StepsTotal: counts.Total, StepsPassed: counts.Passed,
			StepsFailed: counts.Failed, StepsSkipped: counts.Skipped,
		})
	}
	run.Status, run.PassedFlows, run.FailedFlows = index.Status, index.Summary.Passed, index.Summary.Failed
	if err := report.WriteSkeleton(dir, index, details); err != nil {
		return err
	}
	if _, allureOK := generateCIReports(dir); allureOK {
		report.CopyAllureAttachments(dir, filepath.Join(dir, "allure-results"), details)
	}
	if err := printUnifiedOutput(dir, &run); err != nil {
		return err
	}
	if firstErr != nil {
		return firstErr
	}
	fmt.Printf("\nReport: %s\n", dir)
	return nil
}
