package cli

// daemon.go: the maestro-d lifecycle and session commands. Every
// command talks to the daemon over its unix socket through daemon.Client;
// the one-shot YAML commands live in oneshot.go. Nothing here runs a driver
// in-process except `serve`.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

// connectDaemon dials the named daemon, spawning it unless --no-spawn. With
// --target-hub (or MAESTRO_D_HUB) it returns a TCP client for that daemon
// or hub instead, and never spawns.
func connectDaemon(ctx context.Context, o *daemonOpts, spawn bool) (*daemon.Client, error) {
	c, err := dialDaemon(ctx, o, spawn)
	if err != nil {
		return nil, err
	}
	wireLeases(c, o)
	return c, nil
}

func dialDaemon(ctx context.Context, o *daemonOpts, spawn bool) (*daemon.Client, error) {
	if o.Target != "" {
		return connectTarget(o)
	}
	if err := daemon.CheckLocalListen(o.HTTPAddr); err != nil {
		return nil, err
	}
	opts := daemon.SpawnOptions{
		NoSpawn:      !spawn || o.NoSpawn,
		IdleTimeout:  o.IdleTimeout,
		HTTPAddr:     o.HTTPAddr,
		Token:        o.Token,
		StartTimeout: time.Duration(o.StartTimeout) * time.Second,
		Log: func(format string, args ...any) {
			if o.Verbose {
				common.Progress(format, args...)
			}
		},
	}
	c, started, err := daemon.EnsureDaemon(ctx, o.Name, opts)
	if err != nil {
		return nil, err
	}
	if started && !o.JSON {
		common.Progress("Started daemon %q (pid %d)", o.Name, c.Info.PID)
	}
	return c, nil
}

// resolveDevice applies --device → $MAESTRO_DEVICE → the only attached device.
func resolveDevice(ctx context.Context, c *daemon.Client, o *daemonOpts) (string, error) {
	if o.Device != "" {
		return o.Device, nil
	}
	st, err := c.Status(ctx)
	if err != nil {
		return "", err
	}
	var attached []string
	for _, d := range st.Devices {
		if d.Attached {
			attached = append(attached, d.ID)
		}
	}
	switch len(attached) {
	case 1:
		return attached[0], nil
	case 0:
		return "", daemon.Errorf(daemon.CodeUsage, "no device attached to %s; pass --device ID (and --platform / --team-id … for the first command)", targetLabel(o))
	}
	return "", daemon.Errorf(daemon.CodeUsage, "%s has %d devices attached; pass --device one of: %s",
		targetLabel(o), len(attached), strings.Join(attached, ", "))
}

// leaseCell is a device's LEASE column on a hub: its holder and the time
// left, its next reservation, or available.
func leaseCell(d daemon.DeviceInfo) string {
	switch {
	case d.Lease != nil:
		return fmt.Sprintf("%s (%s)", daemon.HolderName(d.Lease.Holder), untilExpiry(d.Lease.ExpiresAt))
	case len(d.Reservations) > 0:
		next := d.Reservations[0]
		return fmt.Sprintf("next %s %s", daemon.HolderName(next.Holder), next.StartsAt)
	case d.Available:
		return "available"
	}
	return "-"
}

// ensureAttached attaches the device with the invocation's flags when the
// daemon does not hold it yet. When it is already attached the flags are
// ignored, with a warning if they differ in an obvious way.
func ensureAttached(ctx context.Context, c *daemon.Client, o *daemonOpts, id string) (*daemon.DeviceInfo, error) {
	info, err := c.Device(ctx, id)
	if err != nil {
		if de, ok := err.(*daemon.Error); !ok || de.Code != daemon.CodeDeviceNotFound {
			return nil, err
		}
	}
	return attachIfNeeded(ctx, c, o, id, info)
}

// attachIfNeeded is ensureAttached with the device's record already read
// (nil when the daemon does not list it).
func attachIfNeeded(ctx context.Context, c *daemon.Client, o *daemonOpts, id string, info *daemon.DeviceInfo) (*daemon.DeviceInfo, error) {
	var err error
	if info != nil && info.Attached {
		if o.Attach.AppID != "" && info.AppID != "" && o.Attach.AppID != info.AppID {
			common.Progress("warning: %s is attached with --app-id %s; ignoring --app-id %s (detach to change it)", id, info.AppID, o.Attach.AppID)
		} else if len(o.AttachSet) > 0 && o.Verbose {
			common.Progress("note: %s is already attached; attach flags (%s) ignored", id, strings.Join(o.AttachSet, ", "))
		}
		return info, nil
	}
	if !o.JSON {
		common.Progress("Attaching %s …", id)
	}
	start := time.Now()
	cfg := o.Attach
	if err := uploadAttachFiles(ctx, c, o, id, &cfg); err != nil {
		return nil, err
	}
	info, err = c.Attach(ctx, id, cfg)
	if err != nil {
		return nil, err
	}
	if !o.JSON {
		common.Progress("Attached %s (%s, %s) in %s", id, info.Platform, info.Driver, time.Since(start).Round(time.Millisecond))
	}
	return info, nil
}

// session connects, resolves the device and attaches it: the prelude of every
// device command.
func session(ctx context.Context, o *daemonOpts) (*daemon.Client, string, error) {
	c, err := connectDaemon(ctx, o, true)
	if err != nil {
		return nil, "", err
	}
	id, err := resolveDevice(ctx, c, o)
	if err != nil {
		return nil, "", err
	}
	if id, _, err = leaseAndAttach(ctx, c, o, id); err != nil {
		return nil, "", err
	}
	return c, id, nil
}

// leaseAndAttach sends the device's lease on file, if this client holds
// one (see lease.go; it never takes one: on a hub, without a lease the
// attach fails LEASE_REQUIRED), then attaches the device if needed.
func leaseAndAttach(ctx context.Context, c *daemon.Client, o *daemonOpts, id string) (string, *daemon.DeviceInfo, error) {
	lid, info, err := heldLease(ctx, c, o, id)
	if err != nil {
		return id, nil, err
	}
	info, err = attachIfNeeded(ctx, c, o, lid, info)
	return lid, info, err
}

// ---------------------------------------------------------------------------
// Result printing
// ---------------------------------------------------------------------------

func stepMark(ok, skipped bool) string {
	switch {
	case skipped:
		return colorize("–", "\033[2m")
	case ok:
		return colorize("✓", "\033[32m")
	}
	return colorize("✗", "\033[31m")
}

func colorize(s, code string) string {
	if !colorsEnabled {
		return s
	}
	return code + s + "\033[0m"
}

// printStepResult prints the human one-liner for a step, its sub-steps for
// compound commands, and any data the command produced.
func printStepResult(w io.Writer, r *daemon.StepResult) {
	line := fmt.Sprintf("%s %s", stepMark(r.OK, r.Skipped), r.Step)
	switch {
	case r.Skipped:
		line += " (" + strings.TrimPrefix(r.Message, "skipped: ") + ")"
		if r.Message == "" {
			line = strings.TrimSuffix(line, " ()") + " (skipped)"
		}
	case !r.OK && r.Error != nil:
		line += " — " + r.Error.Message
	case r.OK && r.Error != nil:
		line += " — optional, failed: " + r.Error.Message
	}
	line += fmt.Sprintf(" (%s)", fmtMs(r.DurationMs))
	fmt.Fprintln(w, line)
	printSubSteps(w, r.SubSteps, 1)
	if !r.OK && r.Error != nil {
		if s, _ := r.Error.Details["suggestion"].(string); s != "" {
			fmt.Fprintf(w, "  hint: %s\n", s)
		}
		if s, _ := r.Error.Details["screenshot"].(string); s != "" {
			fmt.Fprintf(w, "  screenshot: %s\n", s)
		}
	}
	printData(w, r.Data)
}

func printSubSteps(w io.Writer, subs []report.Command, depth int) {
	for _, s := range subs {
		desc := s.Label
		if desc == "" {
			desc = strings.TrimSpace(strings.SplitN(s.YAML, "\n", 2)[0])
		}
		if desc == "" {
			desc = s.Type
		}
		var dur int64
		if s.Duration != nil {
			dur = *s.Duration
		}
		line := fmt.Sprintf("%s%s %s", strings.Repeat("  ", depth), stepMark(s.Status != report.StatusFailed, s.Status == report.StatusSkipped), desc)
		if s.Status == report.StatusFailed && s.Error != nil {
			line += " — " + s.Error.Message
		}
		fmt.Fprintf(w, "%s (%s)\n", line, fmtMs(dur))
		printSubSteps(w, s.SubCommands, depth+1)
	}
}

// printData shows a command's return value (copyTextFrom, evalScript,
// getConsoleLogs …). Binary blobs (screenshot bytes) are elided.
func printData(w io.Writer, data any) {
	switch v := data.(type) {
	case nil:
	case string:
		if len(v) > 4096 && looksBase64(v) {
			fmt.Fprintf(w, "  data: <%d bytes>\n", len(v))
		} else {
			fmt.Fprintf(w, "  data: %s\n", v)
		}
	case bool, float64, int, int64:
		fmt.Fprintf(w, "  data: %v\n", v)
	default:
		b, err := json.Marshal(v)
		if err == nil {
			fmt.Fprintf(w, "  data: %s\n", b)
		}
	}
}

func looksBase64(s string) bool {
	for i := 0; i < len(s) && i < 64; i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
			return false
		}
	}
	return true
}

func fmtMs(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// emitStep prints a step result in the requested mode and returns the
// error that decides the exit code.
func emitStep(o *daemonOpts, r *daemon.StepResult, err error) error {
	if r != nil {
		if o.JSON {
			common.PrintJSONStdout(r)
		} else {
			printStepResult(os.Stdout, r)
		}
	}
	if err != nil {
		return err
	}
	if r != nil && r.Err() != nil {
		return r.Err()
	}
	return nil
}

func emitSteps(o *daemonOpts, r *daemon.StepsResult, err error) error {
	if r != nil {
		if o.JSON {
			common.PrintJSONStdout(r)
		} else {
			for i := range r.Results {
				printStepResult(os.Stdout, &r.Results[i])
			}
			fmt.Fprintf(os.Stdout, "%d passed, %d failed, %d skipped\n", r.Passed, r.Failed, r.Skipped)
		}
	}
	if err != nil {
		return err
	}
	if r != nil && !r.OK && r.Error != nil {
		return &daemon.Error{ErrorBody: *r.Error}
	}
	return nil
}

// printDevices renders the device table.
func printDevices(w io.Writer, devs []daemon.DeviceInfo) {
	if len(devs) == 0 {
		fmt.Fprintln(w, "No devices.")
		return
	}
	// A hub's listing carries lease state (every hub entry has maxSec):
	// show who holds each device.
	leases := false
	for _, d := range devs {
		leases = leases || d.Lease != nil || d.MaxSec > 0
	}
	// A cloud row shows its provider.
	cloud := false
	for _, d := range devs {
		cloud = cloud || d.Provider != ""
	}
	loc := func(d daemon.DeviceInfo) string {
		switch {
		case d.Location == daemon.LocationMesh && d.Machine != "":
			return "mesh (" + d.Machine + ")"
		case d.Location == daemon.LocationCloud:
			if d.Hub != "" {
				return "cloud (" + d.Provider + "@" + d.Hub + ")"
			}
			return "cloud (" + d.Provider + ")"
		case d.Location != "":
			return d.Location
		}
		return "-"
	}
	prov := func(d daemon.DeviceInfo) string {
		if !cloud {
			return ""
		}
		p := d.Provider
		if p == "" {
			p = "-"
		}
		return fmt.Sprintf("%-11s ", p)
	}
	provHdr := ""
	if cloud {
		provHdr = fmt.Sprintf("%-11s ", "PROVIDER")
	}
	// LOCATION is as wide as its longest value ("cloud (devicefarm@<hub>)").
	locW := 24
	for _, d := range devs {
		locW = max(locW, len(loc(d)))
	}
	// LEASE is as wide as its longest value ("next <holder> <start>").
	leaseW := 28
	for _, d := range devs {
		leaseW = max(leaseW, len(leaseCell(d)))
	}
	if leases {
		fmt.Fprintf(w, "%-40s %-8s %-10s %-10s %-9s %-*s %-*s %s%s\n", "ID", "PLATFORM", "KIND", "STATE", "ATTACHED", leaseW, "LEASE", locW, "LOCATION", provHdr, "NAME")
	} else {
		fmt.Fprintf(w, "%-40s %-8s %-10s %-10s %-9s %-*s %s%s\n", "ID", "PLATFORM", "KIND", "STATE", "ATTACHED", locW, "LOCATION", provHdr, "NAME")
	}
	for _, d := range devs {
		state := d.State
		if d.Busy {
			state += " (busy)"
		}
		att := "-"
		switch {
		case d.Attached:
			att = "yes"
		case d.Owner != "":
			att = "by " + d.Owner
		}
		name := d.Name
		switch {
		case name == "":
			name = d.Model
		case d.Model != "" && d.Model != d.Name:
			name += " (" + d.Model + ")"
		}
		if d.OSVersion != "" {
			name += " " + d.OSVersion
		}
		if d.BootedBy == daemon.BootedByDaemon && d.Provider == "" {
			name += " [booted by daemon]"
		}
		if leases {
			fmt.Fprintf(w, "%-40s %-8s %-10s %-10s %-9s %-*s %-*s %s%s\n", d.ID, d.Platform, d.Kind, state, att, leaseW, leaseCell(d), locW, loc(d), prov(d), strings.TrimSpace(name))
			continue
		}
		fmt.Fprintf(w, "%-40s %-8s %-10s %-10s %-9s %-*s %s%s\n", d.ID, d.Platform, d.Kind, state, att, locW, loc(d), prov(d), strings.TrimSpace(name))
	}
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// daemonCommands are registered in cli.go.
func daemonCommands() []*cli.Command {
	return append([]*cli.Command{
		daemonStartCommand, daemonServeCommand, daemonStopCommand, daemonStatusCommand, daemonPsCommand,
		daemonDeviceCommand, devicefarmCommand, daemonAttachCommand, daemonDetachCommand,
		daemonRunCommand, daemonGetCommand, daemonStreamCommand, daemonSetCommand, daemonEvalCommand, daemonCommandsCommand,
		installCommand, uninstallCommand, packCommand, hubAliasCommand, common.VersionCommand(), leaseCommand, reserveCommand, claimCommand, renewCommand, releaseCommand,
		gcCommand,
	}, oneShotCommands()...)
}

// withArgs wraps an action: parse the flag table (flags may appear anywhere
// on the line), run, and map errors to exit codes. All daemon commands use
// SkipFlagParsing so `get screenshot -o x.png` and `-o x.png get screenshot`
// both work; the flags are documented in each command's Description.
func withArgs(flags []dflag, fn func(context.Context, *daemonOpts, []string) error) cli.ActionFunc {
	return common.Action(flags, parseDaemonArgs, fn, func(o *daemonOpts) bool { return o.JSON })
}

// localDaemonAction gives the migration hint for flags removed from start and serve.
func localDaemonAction(flags []dflag, fn func(context.Context, *daemonOpts, []string) error) cli.ActionFunc {
	run := withArgs(flags, fn)
	return func(c *cli.Context) error {
		for _, arg := range c.Args().Slice() {
			key, _, _ := strings.Cut(arg, "=")
			switch key {
			case "--tls-cert", "--tls-key", "--pass-env":
				return common.FinishErr(daemon.Errorf(daemon.CodeUsage, "%s was removed from maestro-d %s; use maestro-d-hub start", key, c.Command.Name), false)
			}
		}
		return run(c)
	}
}

var startFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags, common.One(deviceFlag), attachFlags)

var daemonStartCommand = &cli.Command{
	Name:      "start",
	Usage:     "Start the daemon in the background (and optionally attach a device)",
	ArgsUsage: " ",
	Description: common.FlagHelp(`Starts the named daemon if it is not already running. Commands start it
automatically, so this is only needed to pre-warm one, to open the REST
API (--http), or to attach a device up front:

  maestro-d start
  maestro-d start --http 127.0.0.1:7433 --token secret
  maestro-d start --device 00008110-… --platform ios --team-id ABCDE12345`, startFlags),
	SkipFlagParsing: true,
	Action: localDaemonAction(startFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		client, err := connectDaemon(ctx, o, true)
		if err != nil {
			return err
		}
		st, err := client.Status(ctx)
		if err != nil {
			return err
		}
		var dev *daemon.DeviceInfo
		if o.Device != "" {
			if dev, err = ensureAttached(ctx, client, o, o.Device); err != nil {
				return err
			}
		}
		if o.JSON {
			common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "daemon": st.Daemon, "device": dev})
			return nil
		}
		fmt.Printf("daemon %q running (pid %d, socket %s", st.Daemon.Name, st.Daemon.PID, st.Daemon.SocketPath)
		if st.Daemon.HTTPAddr != "" {
			fmt.Printf(", http://%s", st.Daemon.HTTPAddr)
		}
		fmt.Println(")")
		return nil
	}),
}

var serveFlags = common.FlagSet(daemonSelectFlags[:1], daemonSpawnFlags[1:4], []dflag{
	common.StringFlag("config", "The device config: config.json's hubDevices, each with an id, an optional description and optional trafficShaping calls (default <home>/config.json, else <home>/network.json)", []string{"MAESTRO_D_CONFIG"},
		func(o *daemonOpts, v string) { o.DeviceConfig = v }),
	common.StringFlag("network-config", "Deprecated (use --config): an older network.json, each device's setTrafficShape and resetTrafficShape calls", []string{"MAESTRO_D_NETWORK_CONFIG"},
		func(o *daemonOpts, v string) { o.NetworkConfig = v }),
})

var daemonServeCommand = &cli.Command{
	Name:            "serve",
	Usage:           "Run the daemon in the foreground (what `start` spawns)",
	ArgsUsage:       " ",
	Description:     common.FlagHelp("", serveFlags),
	SkipFlagParsing: true,
	Action: localDaemonAction(serveFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if err := daemon.CheckLocalListen(o.HTTPAddr); err != nil {
			return err
		}
		idle := time.Duration(daemon.DefaultIdleSecs) * time.Second
		switch {
		case o.IdleTimeout < 0:
			idle = 0
		case o.IdleTimeout > 0:
			idle = time.Duration(o.IdleTimeout) * time.Second
		}
		runDir := daemon.RunDir(o.Name)
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
		}
		if err := logger.Init(filepath.Join(runDir, "daemon.log")); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot open daemon.log: %v\n", err)
		}
		defer logger.Close()
		if err := common.CheckConfigFlags(&o.DeviceConfig, &o.NetworkConfig, o.explicit); err != nil {
			return err
		}
		srv := daemon.New(daemon.Config{

			Name:          o.Name,
			HTTPAddr:      o.HTTPAddr,
			Token:         o.Token,
			IdleTimeout:   idle,
			ReportsDir:    daemon.ReportsDir(o.Name),
			DeviceConfig:  o.DeviceConfig,
			NetworkConfig: o.NetworkConfig,
			StartArgs:     os.Args[1:],
			Ready:         os.Stdout,
		}, daemonDeps())
		if err := srv.Run(context.Background()); err != nil {
			logger.Error("daemon exited: %v", err)
			return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
		}
		return nil
	}),
}

var stopFlags = common.FlagSet(daemonSelectFlags, []dflag{
	common.BoolFlag("all", "Stop every daemon of this user", nil, func(o *daemonOpts, v bool) { o.stopAll = v }),
	common.IntFlag("timeout", "Seconds to wait for the daemon to exit (default 30)", nil, func(o *daemonOpts, v int) { o.StartTimeout = v }),
})

var daemonStopCommand = &cli.Command{
	Name:            "stop",
	Usage:           "Stop the daemon: detach all devices, shut down simulators it booted, exit",
	ArgsUsage:       " ",
	Description:     common.FlagHelp("", stopFlags),
	SkipFlagParsing: true,
	Action: withArgs(stopFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		names := []string{o.Name}
		if o.stopAll {
			entries, err := daemon.ListDaemons()
			if err != nil {
				return daemon.WrapErr(daemon.CodeInternal, err)
			}
			names = names[:0]
			for _, e := range entries {
				if e.Alive {
					names = append(names, e.Name)
				}
			}
			if len(names) == 0 {
				if o.JSON {
					common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "stopped": []string{}})
				} else {
					fmt.Println("No daemons running.")
				}
				return nil
			}
		}
		return common.StopDaemons(ctx, names, "daemon", o.StartTimeout, o.JSON, o.stopAll)
	}),
}

var statusFlags = common.FlagSet(daemonSelectFlags, targetFlags)

var daemonStatusCommand = &cli.Command{
	Name:            "status",
	Usage:           "Show the daemon and its devices (exit 3 when not running)",
	ArgsUsage:       " ",
	Description:     common.FlagHelp("", statusFlags),
	SkipFlagParsing: true,
	Action: withArgs(statusFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		var client *daemon.Client
		var err error
		if o.Target != "" {
			client, err = connectTarget(o)
		} else {
			client, err = daemon.Dial(o.Name)
		}
		if err != nil {
			return err
		}
		if client == nil {
			return daemon.Errorf(daemon.CodeDaemonUnavailable, "daemon %q is not running", o.Name)
		}
		st, err := client.Status(ctx)
		if err != nil {
			return err
		}
		if o.JSON {
			common.PrintJSON(os.Stdout, st)
			return nil
		}
		d := st.Daemon
		fmt.Printf("daemon %q: pid %d, version %s, up since %s\n", d.Name, d.PID, d.Version, d.StartedAt)
		fmt.Printf("  socket:  %s\n", d.SocketPath)
		if d.Hub != "" {
			fmt.Printf("  hub:     %s (leases on)\n", d.Hub)
		}
		if d.HTTPAddr != "" {
			scheme := "http"
			if d.TLS {
				scheme = "https"
			}
			fmt.Printf("  %-8s %s://%s\n", scheme+":", scheme, d.HTTPAddr)
		}
		fmt.Printf("  reports: %s\n", d.ReportsDir)
		if d.IdleTimeout > 0 {
			fmt.Printf("  idle:    exits after %ds idle", d.IdleTimeout)
			if st.IdleShutdownAt != "" {
				fmt.Printf(" (at %s)", st.IdleShutdownAt)
			}
			fmt.Println()
		}
		fmt.Println()
		printDevices(os.Stdout, st.Devices)
		return nil
	}),
}

var daemonPsCommand = &cli.Command{
	Name:            "ps",
	Usage:           "List all daemons (running or stale) and the devices they hold",
	ArgsUsage:       " ",
	Description:     common.FlagHelp("", daemonSelectFlags[1:2]),
	SkipFlagParsing: true,
	Action: withArgs(daemonSelectFlags[1:2], func(ctx context.Context, o *daemonOpts, args []string) error {
		entries, err := daemon.ListDaemons()
		if err != nil {
			return daemon.WrapErr(daemon.CodeInternal, err)
		}
		if o.JSON {
			if entries == nil {
				entries = []daemon.DaemonEntry{}
			}
			common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "daemons": entries})
			return nil
		}
		if len(entries) == 0 {
			fmt.Println("No daemons.")
			return nil
		}
		fmt.Printf("%-16s %-8s %-8s %-25s %s\n", "NAME", "PID", "STATE", "STARTED", "DEVICES")
		for _, e := range entries {
			pid, started, state := "-", "-", "stale"
			if e.Info != nil {
				pid = fmt.Sprint(e.Info.PID)
				started = e.Info.StartedAt
			}
			if e.Alive {
				state = "running"
			}
			var devs []string
			for _, d := range e.Devices {
				s := d.ID
				if d.BootedBy == daemon.BootedByDaemon {
					s += "*"
				}
				devs = append(devs, s)
			}
			fmt.Printf("%-16s %-8s %-8s %-25s %s\n", e.Name, pid, state, started, strings.Join(devs, " "))
		}
		fmt.Println("\n* booted by the daemon (shut down when it stops)")
		return nil
	}),
}

var (
	deviceListFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags[:1], attachFlags[:1], targetFlags, queryFlags(true), []dflag{
		common.BoolFlag("available", "Only devices a lease could take now (ready, not leased, not attached)", nil,
			func(o *daemonOpts, v bool) { o.manage.query.Available = v }),
	})
	deviceStartFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags[:1], attachFlags[:1], targetFlags, common.One(manageHubFlag), []dflag{
		common.IntFlag("boot-timeout", "Seconds to wait for boot (default 180)", nil, func(o *daemonOpts, v int) { o.StartTimeout = v }),
	})
	deviceStopFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags[:1], targetFlags, []dflag{
		common.BoolFlag("force", "Shut down even if the daemon did not boot it", nil, func(o *daemonOpts, v bool) { o.force = v }),
		common.BoolFlag("orphans", "Shut down devices left behind by dead daemons, and release their cloud sessions", nil, func(o *daemonOpts, v bool) { o.orphans = v }),
		common.StringFlag("devicefarm-credentials-file", "With --orphans: release Device Farm sessions with this AWS shared credentials file", nil,
			func(o *daemonOpts, v string) { o.dfCreds.CredentialsFile = v }),
		common.StringFlag("devicefarm-profile", "With --orphans: release Device Farm sessions with this AWS profile", nil,
			func(o *daemonOpts, v string) { o.dfCreds.Profile = v }),
	})
)

var daemonDeviceCommand = &cli.Command{
	Name:  "device",
	Usage: "List, create, boot, shut down and delete devices, simulators and emulators",
	Subcommands: []*cli.Command{
		{
			Name:            "list",
			Aliases:         []string{"ls"},
			Usage:           "List connected devices and booted/available simulators, with attach state",
			ArgsUsage:       " ",
			Description:     common.FlagHelp("", deviceListFlags),
			SkipFlagParsing: true,
			Action: withArgs(deviceListFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
				client, err := connectDaemon(ctx, o, true)
				if err != nil {
					return err
				}
				q := o.manage.query
				q.Platform = o.Attach.Platform
				res, err := client.DeviceListQuery(ctx, q)
				if err != nil {
					return err
				}
				if o.JSON {
					res.Envelope = daemon.Envelope{OK: true}
					common.PrintJSON(os.Stdout, res)
					return nil
				}
				printDevices(os.Stdout, res.Devices)
				for _, u := range res.Unreachable {
					fmt.Fprintf(os.Stderr, "unreachable: hub %s (%s)\n", u.Hub, u.Error)
				}
				return nil
			}),
		},
		{
			Name:      "start",
			Usage:     "Boot a simulator (by name or UDID) or emulator (by AVD name or avd:<name>) through the daemon",
			ArgsUsage: "<name-or-udid>",
			Description: common.FlagHelp(`The daemon remembers what it booted and shuts it down again when it
stops (or on 'device stop'). Devices that were already running are
never shut down without --force.

--platform may be left out: avd:<name> means android, and a name or
UDID that 'device list' shows picks that device's platform.

  maestro-d device start "iPhone 15 Pro" --platform ios
  maestro-d device start avd:Pixel_7_API_33`, deviceStartFlags),
			SkipFlagParsing: true,
			Action: withArgs(deviceStartFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
				if len(args) != 1 {
					return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d device start <name-or-udid> [--platform ios|android]")
				}
				client, err := connectDaemon(ctx, o, true)
				if err != nil {
					return err
				}
				if !o.JSON {
					common.Progress("Booting %s …", args[0])
				}
				dev, err := client.StartDevice(ctx, daemon.BootRequest{Platform: o.Attach.Platform, Name: args[0], BootTimeout: o.StartTimeout, Hub: o.manage.hub})
				if err != nil {
					return err
				}
				if o.JSON {
					common.PrintJSON(os.Stdout, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: *dev})
					return nil
				}
				fmt.Printf("%s booted: %s (%s)\n", dev.ID, dev.Name, dev.Platform)
				return nil
			}),
		},
		{
			Name:      "stop",
			Usage:     "Shut down a simulator/emulator (detaching it first)",
			ArgsUsage: "<device-id> | --orphans",
			Description: common.FlagHelp(`Only devices this daemon booted are shut down; pass --force for one that
was already running. --orphans shuts down devices recorded as booted by
daemons that are no longer alive.`, deviceStopFlags),
			SkipFlagParsing: true,
			Action: withArgs(deviceStopFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
				if o.orphans {
					return runStopOrphans(ctx, o)
				}
				if len(args) != 1 {
					return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d device stop <device-id> [--force] | --orphans")
				}
				client, err := connectDaemon(ctx, o, false)
				if err != nil {
					return err
				}
				res, err := client.StopDeviceResult(ctx, args[0], o.force)
				if err != nil {
					return err
				}
				if o.JSON {
					if res.Device.ID == "" {
						common.PrintJSON(os.Stdout, daemon.Envelope{OK: true})
					} else {
						res.Envelope = daemon.Envelope{OK: true}
						common.PrintJSON(os.Stdout, res)
					}
				} else {
					fmt.Printf("%s stopped%s\n", args[0], minutesNote(res.Device.DeviceMinutes))
				}
				return nil
			}),
		},
		deviceCatalogCommand,
		deviceCreateCommand,
		deviceDeleteCommand,
	},
}

var attachCmdFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags, common.One(deviceFlag), attachFlags, targetFlags)

var daemonAttachCommand = &cli.Command{
	Name:      "attach",
	Usage:     "Open a driver session on a device (installs the driver/app as `test` would)",
	ArgsUsage: " ",
	Description: common.FlagHelp(`Attaching is what the first one-shot command does implicitly; do it
explicitly to control the flags or to pay the driver start-up cost early.

  maestro-d attach --device FAKEPIXEL0002 --app-file app.apk
  maestro-d attach --device 00008110-… --platform ios --team-id ABCDE12345 --app-id co.example.app
  maestro-d attach --device mock-1 --platform mock`, attachCmdFlags),
	SkipFlagParsing: true,
	Action: withArgs(attachCmdFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if o.Device == "" {
			return daemon.Errorf(daemon.CodeUsage, "--device ID is required")
		}
		client, err := connectDaemon(ctx, o, true)
		if err != nil {
			return err
		}
		_, dev, err := leaseAndAttach(ctx, client, o, o.Device)
		if err != nil {
			return err
		}
		if o.JSON {
			common.PrintJSON(os.Stdout, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: *dev})
			return nil
		}
		fmt.Printf("%s attached (%s, driver %s", dev.ID, dev.Platform, dev.Driver)
		if dev.AppID != "" {
			fmt.Printf(", app %s", dev.AppID)
		}
		fmt.Println(")")
		return nil
	}),
}

var detachFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags[:1], common.One(deviceFlag), targetFlags, []dflag{
	common.BoolFlag("all", "Detach every device", nil, func(o *daemonOpts, v bool) { o.stopAll = v }),
})

var daemonDetachCommand = &cli.Command{
	Name:            "detach",
	Usage:           "Close the driver session on a device (the device itself stays as it is)",
	ArgsUsage:       " ",
	Description:     common.FlagHelp("", detachFlags),
	SkipFlagParsing: true,
	Action: withArgs(detachFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		client, err := connectDaemon(ctx, o, false)
		if err != nil {
			return err
		}
		var ids []string
		if o.stopAll {
			st, err := client.Status(ctx)
			if err != nil {
				return err
			}
			for _, d := range st.Devices {
				if d.Attached {
					ids = append(ids, d.ID)
				}
			}
		} else {
			id, err := resolveDevice(ctx, client, o)
			if err != nil {
				return err
			}
			ids = []string{id}
		}
		for _, id := range ids {
			if err := client.Detach(ctx, id); err != nil {
				return err
			}
			if !o.JSON {
				fmt.Printf("%s detached\n", id)
			}
		}
		if o.JSON {
			if ids == nil {
				ids = []string{}
			}
			common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "detached": ids})
		}
		return nil
	}),
}

var runFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags, common.One(deviceFlag), common.One(waitFlag), attachFlags, targetFlags, []dflag{
	common.BoolFlag("continue-on-error", "Keep running remaining steps after a failure (step lists only)", nil, func(o *daemonOpts, v bool) { o.force = v }),
	includeFlag,
})

var daemonRunCommand = &cli.Command{
	Name:      "run",
	Usage:     "Run a flow file, or a YAML list of steps, on the attached device",
	ArgsUsage: "<flow.yaml | steps.yaml | ->",
	Description: common.FlagHelp(`A file with a config header (appId: … / ---) runs as a flow exactly like
'maestro-runner test' would, including onFlowStart/onFlowComplete hooks
and flow-scoped env. A bare YAML list of steps runs step by step in the
session; with --continue-on-error every step runs and the exit code
reflects the first failure. '-' reads stdin.

With --target-hub the file runs whole on the device's host: it is packed
with everything it uses (as 'maestro-d pack --flow', plus --include), the
files the hub lacks are uploaded, and the files its output fields write
(takeScreenshot, …) are fetched back beside the flow, where a local run
writes them.

  maestro-d run login.yaml -e USER=alice
  printf -- '- launchApp\n- tapOn: Login\n' | maestro-d run -`, runFlags),
	SkipFlagParsing: true,
	Action: withArgs(runFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) != 1 {
			return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d run <file|->")
		}
		src := args[0]
		var data []byte
		var err error
		if src == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(src)
		}
		if err != nil {
			return daemon.Errorf(daemon.CodeUsage, "read %s: %v", src, err)
		}
		cwd, _ := os.Getwd()
		client, id, err := session(ctx, o)
		if err != nil {
			return err
		}
		if o.Target != "" {
			return runBundle(ctx, o, client, id, src, data, cwd)
		}
		if isStepList(data) {
			res, err := client.Steps(ctx, id, daemon.StepsRequest{YAML: string(data), ContinueOnError: o.force, Cwd: cwd}, !o.NoWait)
			return emitSteps(o, res, err)
		}
		req := daemon.FlowRequest{Env: o.Attach.Env, Cwd: cwd}
		if src == "-" {
			req.YAML = string(data)
		} else {
			req.File, _ = filepath.Abs(src)
		}
		res, err := client.Flow(ctx, id, req, !o.NoWait)
		return emitStep(o, res, err)
	}),
}

// runBundle runs a flow file or step list on a targeted daemon: the file is
// on this machine, not the target's, so it is packed with its closure and
// uploaded, and posted as a bundle (with no working directory: a remote
// caller may not name one). stdin is packed as a root that sits in cwd.
func runBundle(ctx context.Context, o *daemonOpts, client *daemon.Client, id, src string, data []byte, cwd string) error {
	opts := packFlowOpts{Env: o.Attach.Env, Include: o.pack.include}
	root := src
	if src == "-" {
		tmp, err := os.MkdirTemp("", "maestro-d-stdin-")
		if err != nil {
			return daemon.WrapErr(daemon.CodeUsage, err)
		}
		defer os.RemoveAll(tmp)
		root = filepath.Join(tmp, "stdin.yaml")
		if err := os.WriteFile(root, data, 0o644); err != nil {
			return daemon.WrapErr(daemon.CodeUsage, err)
		}
		opts.Base = cwd
	}
	b, err := packFlow([]string{root}, opts)
	if err != nil {
		return err
	}
	if err := uploadBundle(ctx, client, id, b, false, o.JSON); err != nil {
		return err
	}
	if isStepList(data) {
		res, err := client.Steps(ctx, id, daemon.StepsRequest{Bundle: &b.Manifest, ContinueOnError: o.force}, !o.NoWait)
		if res != nil {
			if ferr := fetchBundleOutputs(ctx, client, id, b.Dir, res.Outputs, o.JSON); ferr != nil && err == nil {
				err = ferr
			}
		}
		return emitSteps(o, res, err)
	}
	res, err := client.Flow(ctx, id, daemon.FlowRequest{Bundle: &b.Manifest, Env: o.Attach.Env}, !o.NoWait)
	if res != nil {
		if ferr := fetchBundleOutputs(ctx, client, id, b.Dir, res.Outputs, o.JSON); ferr != nil && err == nil {
			err = ferr
		}
	}
	return emitStep(o, res, err)
}

// isStepList reports whether YAML is a bare list of steps (no config
// header), which `run` executes step by step instead of as a flow.
func isStepList(data []byte) bool {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return false
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = *node.Content[0]
	}
	return node.Kind == yaml.SequenceNode && !strings.Contains(string(data), "\n---")
}

var getFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags, common.One(deviceFlag), attachFlags, targetFlags, []dflag{
	common.StringFlag("out", "Write the screenshot to this file (default screenshot-<time>.png; '-' for stdout); for artifacts, download them into this directory", nil, func(o *daemonOpts, v string) { o.outPath = v }, "o"),
	common.BoolFlag("raw", "Hierarchy as the driver's raw XML/JSON instead of the normalized tree", nil, func(o *daemonOpts, v bool) { o.force = v }),
	common.BoolFlag("compact", "Hierarchy as a flat, greppable listing (as `hierarchy --compact`)", nil, func(o *daemonOpts, v bool) { o.compact = v }),
	common.StringFlag("find", "Hierarchy elements matching a substring (as `hierarchy --find`)", nil, func(o *daemonOpts, v string) { o.find = v }),
	common.StringFlag("image", "Screenshot image type: jpeg or png (default: from --out's extension, else the device's: JPEG on the computer, PNG elsewhere)", nil, func(o *daemonOpts, v string) { o.shot.image = v }),
	common.IntFlag("quality", "JPEG quality, 1-100 (default 80)", nil, func(o *daemonOpts, v int) { o.shot.quality = v }),
	common.StringFlag("region", "Capture only this rectangle of the screen, in screen pixels: x,y,width,height", nil, func(o *daemonOpts, v string) { o.shot.region = v }),
})

var daemonGetCommand = &cli.Command{
	Name:      "get",
	Usage:     "Read device state: screenshot, hierarchy, state, info, vars, artifacts",
	ArgsUsage: "screenshot | hierarchy | state | info | vars | artifacts",
	Description: common.FlagHelp(`  maestro-d get screenshot -o now.png
  maestro-d get hierarchy | jq '.. | .text? // empty'
  maestro-d get hierarchy --find "Sign in"
  maestro-d get state      # foreground app, orientation, …
  maestro-d get info       # platform, OS version, device name, app version
  maestro-d get vars       # session variables (from -e and copyTextFrom/outputs)
  maestro-d get artifacts  # the session's report files; --out DIR downloads them`, getFlags),
	SkipFlagParsing: true,
	Action: withArgs(getFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) != 1 || !map[string]bool{"screenshot": true, "hierarchy": true, "state": true, "info": true, "vars": true, "artifacts": true}[args[0]] {
			return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d get screenshot|hierarchy|state|info|vars|artifacts")
		}
		what := args[0]
		client, id, err := session(ctx, o)
		if err != nil {
			return err
		}
		switch what {
		case "screenshot":
			opts, err := o.shot.options(o.outPath) // maestro-d: screenshot_flags.go
			if err != nil {
				return err
			}
			png, contentType, err := client.ScreenshotWith(ctx, id, opts)
			if err != nil {
				return err
			}
			out := o.outPath
			if out == "-" {
				_, err = os.Stdout.Write(png)
				return err
			}
			if out == "" {
				out = fmt.Sprintf("screenshot-%s.%s", time.Now().Format("20060102-150405"), extensionFor(contentType))
			}
			if err := os.WriteFile(out, png, 0o644); err != nil {
				return daemon.WrapErr(daemon.CodeUsage, err)
			}
			abs, _ := filepath.Abs(out)
			if o.JSON {
				common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "path": abs, "bytes": len(png)})
			} else {
				fmt.Println(abs)
			}
		case "hierarchy":
			if o.force || o.compact || o.find != "" {
				raw, err := client.HierarchyRaw(ctx, id)
				if err != nil {
					return err
				}
				if o.force {
					os.Stdout.Write(raw)
					if len(raw) > 0 && raw[len(raw)-1] != '\n' {
						fmt.Println()
					}
					return nil
				}
				// Same renderer as `maestro-runner hierarchy`.
				text, err := formatHierarchy(raw, o.compact, o.find)
				if err != nil {
					return daemon.WrapErr(daemon.CodeDeviceError, err)
				}
				fmt.Println(text)
				return nil
			}
			v, err := client.Hierarchy(ctx, id)
			if err != nil {
				return err
			}
			common.PrintJSON(os.Stdout, v)
		case "state":
			v, err := client.State(ctx, id)
			if err != nil {
				return err
			}
			common.PrintJSON(os.Stdout, v)
		case "info":
			v, err := client.PlatformInfo(ctx, id)
			if err != nil {
				return err
			}
			common.PrintJSON(os.Stdout, v)
		case "artifacts":
			return getArtifacts(ctx, client, o, id)
		case "vars":
			vars, err := client.Vars(ctx, id)
			if err != nil {
				return err
			}
			if o.JSON {
				common.PrintJSON(os.Stdout, daemon.VarsResult{Envelope: daemon.Envelope{OK: true}, Vars: vars})
				return nil
			}
			keys := make([]string, 0, len(vars))
			for k := range vars {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("%s=%s\n", k, vars[k])
			}
		}
		return nil
	}),
}

var setFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags, common.One(deviceFlag), attachFlags, targetFlags)

var daemonSetCommand = &cli.Command{
	Name:            "set",
	Usage:           "Set session variables (usable as ${VAR} in later commands and scripts)",
	ArgsUsage:       "VAR=value …",
	Description:     common.FlagHelp("", setFlags),
	SkipFlagParsing: true,
	Action: withArgs(setFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) == 0 {
			return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d set VAR=value …")
		}
		vars := map[string]string{}
		for _, a := range args {
			k, v, ok := strings.Cut(a, "=")
			if !ok || k == "" {
				return daemon.Errorf(daemon.CodeUsage, "expected VAR=value, got %q", a)
			}
			vars[k] = v
		}
		client, id, err := session(ctx, o)
		if err != nil {
			return err
		}
		all, err := client.SetVars(ctx, id, vars)
		if err != nil {
			return err
		}
		if o.JSON {
			common.PrintJSON(os.Stdout, daemon.VarsResult{Envelope: daemon.Envelope{OK: true}, Vars: all})
		}
		return nil
	}),
}

var evalFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags, common.One(deviceFlag), common.One(waitFlag), attachFlags, targetFlags)

var daemonEvalCommand = &cli.Command{
	Name:      "eval",
	Usage:     "Evaluate JavaScript in the session (same engine as evalScript) and print the value",
	ArgsUsage: "<script | ->",
	Description: common.FlagHelp(`Session variables are in scope; assignments to output.X persist.

  maestro-d eval 'output.total = 3 * 4; output.total'
  maestro-d eval 'maestro.copiedText'`, evalFlags),
	SkipFlagParsing: true,
	Action: withArgs(evalFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) != 1 {
			return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d eval '<script>' | -")
		}
		script := args[0]
		if script == "-" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return daemon.WrapErr(daemon.CodeUsage, err)
			}
			script = string(data)
		}
		client, id, err := session(ctx, o)
		if err != nil {
			return err
		}
		v, err := client.Eval(ctx, id, script)
		if err != nil {
			return err
		}
		if o.JSON {
			common.PrintJSON(os.Stdout, daemon.EvalResult{Envelope: daemon.Envelope{OK: true}, Value: v})
			return nil
		}
		switch t := v.(type) {
		case string:
			fmt.Println(t)
		case nil:
			fmt.Println("null")
		default:
			b, _ := json.Marshal(t)
			fmt.Println(string(b))
		}
		return nil
	}),
}

var daemonCommandsCommand = &cli.Command{
	Name:            "commands",
	Usage:           "List every YAML command usable as a one-shot, with its fields",
	ArgsUsage:       "[command]",
	Description:     common.FlagHelp("", daemonSelectFlags[1:2]),
	SkipFlagParsing: true,
	Action: withArgs(daemonSelectFlags[1:2], func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) == 1 {
			spec, ok := daemon.CommandSpecs[args[0]]
			if !ok {
				return daemon.Errorf(daemon.CodeUsage, "unknown command %q", args[0])
			}
			if o.JSON {
				common.PrintJSON(os.Stdout, spec)
				return nil
			}
			fmt.Print(commandHelp(spec))
			return nil
		}
		types := flow.StepTypes()
		if o.JSON {
			specs := make([]daemon.CommandSpec, 0, len(types))
			for _, t := range types {
				specs = append(specs, daemon.CommandSpecs[string(t)])
			}
			common.PrintJSON(os.Stdout, specs)
			return nil
		}
		for _, t := range types {
			spec := daemon.CommandSpecs[string(t)]
			fmt.Printf("%-28s %s\n", t, spec.Doc)
		}
		return nil
	}),
}

// getArtifacts lists the session's report files, or downloads them all
// into --out DIR, keeping their relative paths.
func getArtifacts(ctx context.Context, client *daemon.Client, o *daemonOpts, id string) error {
	res, err := client.Artifacts(ctx, id)
	if err != nil {
		return err
	}
	if o.outPath == "" {
		if o.JSON {
			common.PrintJSON(os.Stdout, res)
			return nil
		}
		for _, a := range res.Artifacts {
			fmt.Printf("%10d  %s\n", a.Size, a.Path)
		}
		return nil
	}
	dir, _ := filepath.Abs(o.outPath)
	for _, a := range res.Artifacts {
		if target := filepath.Join(dir, filepath.FromSlash(a.Path)); filepath.IsAbs(filepath.FromSlash(a.Path)) || !strings.HasPrefix(target, dir+string(filepath.Separator)) {
			return daemon.Errorf(daemon.CodeInternal, "artifact path %q leaves %s", a.Path, dir)
		}
		if err := fetchArtifact(ctx, client, id, a.Path, filepath.Join(dir, filepath.FromSlash(a.Path))); err != nil {
			return err
		}
	}
	if o.JSON {
		common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "dir": dir, "artifacts": res.Artifacts})
		return nil
	}
	for _, a := range res.Artifacts {
		fmt.Println(filepath.Join(dir, filepath.FromSlash(a.Path)))
	}
	return nil
}
