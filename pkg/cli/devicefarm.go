package cli

// devicefarm.go: what the CLI does with AWS Device Farm itself: the held
// driver for a cloud device (newRemoteAppiumDriver), releasing a dead
// daemon's cloud sessions (device stop --orphans), and the devicefarm
// command group.

import (
	"context"
	"fmt"
	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm"
	appiumdriver "github.com/devicelab-dev/maestro-runner/pkg/driver/appium"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// newRemoteAppiumDriver opens a WebDriver session on a cloud device's
// Appium endpoint (the daemon's loopback proxy in front of it) with the
// provider's capabilities. It calls the Appium driver directly: never local
// discovery, adb, CreateDriver or createAppiumDriver, which would pin a udid
// the cloud endpoint refuses.
func newRemoteAppiumDriver(_ context.Context, endpoint string, caps map[string]any) (daemon.RemoteDriver, daemon.DriverInfo, error) {
	d, err := appiumdriver.NewDriver(endpoint, caps)
	if err != nil {
		return nil, daemon.DriverInfo{}, err
	}
	platform := "android"
	if p, _ := caps["platformName"].(string); strings.EqualFold(p, "ios") {
		platform = "ios"
	}
	info := daemon.DriverInfo{DriverName: "appium", Platform: platform, Kind: "cloud",
		Device: report.Device{Platform: platform, IsSimulator: false}}
	return remoteAppium{d}, info, nil
}

// remoteAppium is the held Appium driver of a cloud device. A cloud device
// has no screen capture, so recording steps answer that themselves rather
// than the Appium driver's "unsupported step type".
type remoteAppium struct{ *appiumdriver.Driver }

func (d remoteAppium) Execute(step flow.Step) *core.CommandResult {
	switch step.(type) {
	case *flow.StartRecordingStep, *flow.StopRecordingStep:
		err := &capture.Unavailable{Reason: capture.ReasonUnsupportedPlatform,
			Message: "a cloud device has no screen capture, so it cannot record (the provider keeps its own session video)"}
		return &core.CommandResult{Success: false, Error: err, Message: err.Error()}
	}
	return d.Driver.Execute(step)
}

// runStopOrphans is device stop --orphans: it shuts down the simulators and
// emulators dead daemons booted, and releases the cloud sessions the cloud
// ledger holds for them with this process's AWS credentials (or
// --devicefarm-credentials-file / --devicefarm-profile).
func runStopOrphans(ctx context.Context, o *daemonOpts) error {
	var p cloudprov.RemoteProvider
	var perr error
	release := func(ctx context.Context, e daemon.CloudEntry) (*cloudprov.DeviceMinutes, error) {
		if e.Provider != "devicefarm" {
			return nil, fmt.Errorf("no %s credentials", e.Provider)
		}
		if p == nil && perr == nil {
			p, perr = devicefarm.NewProvider(ctx, o.dfCreds)
		}
		if perr != nil {
			return nil, fmt.Errorf("%s", devicefarm.Redact(perr.Error()))
		}
		m, err := p.ReleaseRef(ctx, e.ProviderRef)
		if err != nil {
			return nil, fmt.Errorf("%s", p.Redact(err.Error()))
		}
		return m, nil
	}
	res, err := daemon.StopOrphans(ctx, daemon.OrphanOptions{ReleaseCloud: release})
	if err != nil && res == nil {
		return daemon.WrapErr(daemon.CodeDeviceError, err)
	}
	if res.Stopped == nil {
		res.Stopped = []string{}
	}
	if o.JSON {
		ok := err == nil && len(res.Unreleased) == 0
		out := map[string]any{"ok": ok, "exitCode": 0, "stopped": res.Stopped}
		if !ok {
			out["exitCode"] = daemon.CodeDeviceError.ExitCode()
		}
		if len(res.Released) > 0 {
			out["released"] = res.Released
		}
		if len(res.Unreleased) > 0 {
			out["unreleased"] = res.Unreleased
		}
		// The one document on stdout: the failure that follows goes to
		// stderr only.
		printJSONStdout(out)
	} else {
		printOrphans(os.Stdout, res)
	}
	if err != nil {
		return daemon.WrapErr(daemon.CodeDeviceError, err)
	}
	if len(res.Unreleased) > 0 {
		return daemon.Errorf(daemon.CodeDeviceError, "%d cloud session(s) left running; run device stop --orphans again with credentials", len(res.Unreleased))
	}
	return nil
}

func printOrphans(w io.Writer, res *daemon.OrphanResult) {
	if len(res.Stopped)+len(res.Released)+len(res.Unreleased) == 0 {
		fmt.Fprintln(w, "No orphaned devices.")
		return
	}
	if len(res.Stopped) > 0 {
		fmt.Fprintf(w, "stopped: %s\n", strings.Join(res.Stopped, ", "))
	}
	for _, r := range res.Released {
		fmt.Fprintf(w, "released: %s%s\n", r.ID, minutesNote(r.DeviceMinutes))
	}
	for _, u := range res.Unreleased {
		fmt.Fprintf(w, "unreleased: %s (daemon %s, %s session %s): %s\n", u.ID, u.Daemon, u.Provider, u.ProviderRef, u.Error)
	}
}

var devicefarmSessionsFlags = flagSet(daemonSelectFlags, daemonSpawnFlags[:1], targetFlags, one(manageHubFlag))

// devicefarmCommand is the devicefarm group: the provider's account,
// uploads and sessions, without a device.
var devicefarmCommand = &cli.Command{
	Name:  "devicefarm",
	Usage: "AWS Device Farm: remote access sessions, account and uploads",
	Subcommands: []*cli.Command{
		{
			Name:            "sessions",
			Usage:           "List the project's remote access sessions (name = device id, status, result, device minutes)",
			ArgsUsage:       " ",
			Description:     flagsDoc("", devicefarmSessionsFlags),
			SkipFlagParsing: true,
			Action: withArgs(devicefarmSessionsFlags, connected(func(ctx context.Context, c *daemon.Client, o *daemonOpts, args []string, w io.Writer) error {
				res, err := c.ProviderSessions(ctx, "devicefarm", o.manage.hub)
				if err != nil {
					return err
				}
				if o.JSON {
					printJSON(w, res)
					return nil
				}
				printSessions(w, res)
				return nil
			})),
		},
	},
}

func printSessions(w io.Writer, res *daemon.ProviderSessionsResult) {
	if len(res.Sessions) == 0 {
		fmt.Fprintln(w, "No sessions.")
		return
	}
	fmt.Fprintf(w, "%-14s %-11s %-9s %-28s %-8s %s\n", "NAME", "STATUS", "RESULT", "MODEL", "MINUTES", "CREATED")
	for _, s := range res.Sessions {
		mins := "-"
		if s.DeviceMinutes != nil {
			mins = fmt.Sprintf("%g", s.DeviceMinutes.Total)
		}
		fmt.Fprintf(w, "%-14s %-11s %-9s %-28s %-8s %s\n", s.Name, s.Status, s.Result, s.Model, mins, s.Created)
	}
}
