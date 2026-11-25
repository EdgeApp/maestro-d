package cli

import (
	"context"
	"fmt"
	"os"
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
		id, info, err := leaseAndAttach(ctx, client, &workerOpts, device)
		if err != nil {
			return err
		}
		if seen[id] {
			return daemon.Errorf(daemon.CodeUsage, "parallel workers require distinct leased devices: %s", id)
		}
		seen[id] = true
		deviceInfo := report.Device{
			ID: id, Name: info.Name, Platform: info.Platform, OSVersion: info.OSVersion,
			Model: info.Model, IsSimulator: info.Kind == "simulator",
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
				results[i] = runHubFlow(ctx, target.client, target.device, flows[i], cfg.Env)
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

func runHubFlow(ctx context.Context, client *daemon.Client, device report.Device, f flow.Flow, env map[string]string) hubFlowResult {
	r := hubFlowResult{started: time.Now(), device: device}
	bundle, err := packFlow([]string{f.SourcePath}, packFlowOpts{Env: env})
	if err == nil {
		err = uploadBundle(ctx, client, device.ID, bundle, false, true)
	}
	if err != nil {
		r.err = err
		return r
	}
	r.result, r.err = client.Flow(ctx, device.ID, daemon.FlowRequest{Bundle: &bundle.Manifest, Env: env}, true)
	if r.result != nil {
		err := fetchBundleOutputs(ctx, client, device.ID, bundle.Dir, r.result.Outputs, true)
		if r.err == nil {
			r.err = err
		}
	}
	return r
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
	generateCIReports(dir)
	if err := printUnifiedOutput(dir, &run); err != nil {
		return err
	}
	if firstErr != nil {
		return firstErr
	}
	fmt.Printf("\nReport: %s\n", dir)
	return nil
}
