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
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

// signalContext cancels on SIGINT/SIGTERM so an in-flight step is cancelled
// on the daemon and the CLI exits 130.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(ch)
	}()
	return ctx, cancel
}

// progress prints a status line to stderr (stdout is reserved for results).
func progress(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func printJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// finishErr turns any error into the documented exit code: the JSON failure
// envelope goes to stderr and the process exits with the code's status.
// Errors that are not *daemon.Error are reported as USAGE (exit 2).
func finishErr(err error, jsonMode bool) error {
	if err == nil {
		return nil
	}
	var de *daemon.Error
	if e, ok := err.(*daemon.Error); ok {
		de = e
	} else if e, ok := err.(cli.ExitCoder); ok {
		return e
	} else {
		de = daemon.WrapErr(daemon.CodeUsage, err)
	}
	if de.Code == daemon.CodeInterrupted {
		fmt.Fprintln(os.Stderr, "interrupted")
	} else if !jsonMode || de.Code != daemon.CodeCommandFailed {
		fmt.Fprintf(os.Stderr, "Error: %s\n", de.Message)
	}
	env := daemon.FailureEnvelope(de)
	// --json promises a JSON document on stdout; a failure that produced no
	// step result (DEVICE_IN_USE, USAGE, …) would otherwise leave it empty.
	if jsonMode && !jsonOnStdout {
		printJSON(os.Stdout, env)
	}
	printJSON(os.Stderr, env)
	return cli.Exit("", de.ExitCode())
}

// jsonOnStdout records that --json output already went to stdout, so a
// failure envelope is not printed there a second time.
var jsonOnStdout bool

func printJSONStdout(v any) {
	jsonOnStdout = true
	printJSON(os.Stdout, v)
}

// connectDaemon dials the named daemon, spawning it unless --no-spawn.
func connectDaemon(ctx context.Context, o *daemonOpts, spawn bool) (*daemon.Client, error) {
	opts := daemon.SpawnOptions{
		NoSpawn:      !spawn || o.NoSpawn,
		IdleTimeout:  o.IdleTimeout,
		HTTPAddr:     o.HTTPAddr,
		Token:        o.Token,
		StartTimeout: time.Duration(o.StartTimeout) * time.Second,
		Log: func(format string, args ...any) {
			if o.Verbose {
				progress(format, args...)
			}
		},
	}
	c, started, err := daemon.EnsureDaemon(ctx, o.Name, opts)
	if err != nil {
		return nil, err
	}
	if started && !o.JSON {
		progress("Started daemon %q (pid %d)", o.Name, c.Info.PID)
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
		return "", daemon.Errorf(daemon.CodeUsage, "no device attached to daemon %q; pass --device ID (and --platform / --team-id … for the first command)", o.Name)
	}
	return "", daemon.Errorf(daemon.CodeUsage, "daemon %q has %d devices attached; pass --device one of: %s",
		o.Name, len(attached), strings.Join(attached, ", "))
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
	if info != nil && info.Attached {
		if o.Attach.AppID != "" && info.AppID != "" && o.Attach.AppID != info.AppID {
			progress("warning: %s is attached with --app-id %s; ignoring --app-id %s (detach to change it)", id, info.AppID, o.Attach.AppID)
		} else if len(o.AttachSet) > 0 && o.Verbose {
			progress("note: %s is already attached; attach flags (%s) ignored", id, strings.Join(o.AttachSet, ", "))
		}
		return info, nil
	}
	if !o.JSON {
		progress("Attaching %s …", id)
	}
	start := time.Now()
	info, err = c.Attach(ctx, id, o.Attach)
	if err != nil {
		return nil, err
	}
	if !o.JSON {
		progress("Attached %s (%s, %s) in %s", id, info.Platform, info.Driver, time.Since(start).Round(time.Millisecond))
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
	if _, err := ensureAttached(ctx, c, o, id); err != nil {
		return nil, "", err
	}
	return c, id, nil
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
			printJSONStdout(r)
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
			printJSONStdout(r)
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
	fmt.Fprintf(w, "%-40s %-8s %-10s %-10s %-9s %s\n", "ID", "PLATFORM", "KIND", "STATE", "ATTACHED", "NAME")
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
		if d.OSVersion != "" {
			name += " " + d.OSVersion
		}
		if d.BootedBy == daemon.BootedByDaemon {
			name += " [booted by daemon]"
		}
		fmt.Fprintf(w, "%-40s %-8s %-10s %-10s %-9s %s\n", d.ID, d.Platform, d.Kind, state, att, strings.TrimSpace(name))
	}
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// daemonCommands are registered in cli.go.
func daemonCommands() []*cli.Command {
	return append([]*cli.Command{
		daemonStartCommand, daemonServeCommand, daemonStopCommand, daemonStatusCommand, daemonPsCommand,
		daemonDeviceCommand, daemonAttachCommand, daemonDetachCommand,
		daemonRunCommand, daemonGetCommand, daemonSetCommand, daemonEvalCommand, daemonCommandsCommand,
	}, oneShotCommands()...)
}

// withArgs wraps an action: parse the flag table (flags may appear anywhere
// on the line), run, and map errors to exit codes. All daemon commands use
// SkipFlagParsing so `get screenshot -o x.png` and `-o x.png get screenshot`
// both work; the flags are documented in each command's Description.
func withArgs(flags []dflag, fn func(ctx context.Context, o *daemonOpts, args []string) error) cli.ActionFunc {
	return func(c *cli.Context) error {
		args := c.Args().Slice()
		for _, a := range args {
			if a == "-h" || a == "--help" {
				cli.HelpPrinter(c.App.Writer, cli.CommandHelpTemplate, c.Command)
				return nil
			}
		}
		o, p, err := parseDaemonArgs(append(inheritedArgs(c, flags), args...), flags, nil)
		if err != nil {
			return finishErr(err, false)
		}
		if len(p.step) > 0 {
			return finishErr(daemon.Errorf(daemon.CodeUsage, "unknown flag --%s (see `maestro-d %s --help`)", p.step[0].key, c.Command.Name), false)
		}
		if len(p.base) > 0 {
			for k := range p.base {
				return finishErr(daemon.Errorf(daemon.CodeUsage, "unknown flag --%s (see `maestro-d %s --help`)", k, c.Command.Name), false)
			}
		}
		ctx, cancel := signalContext()
		defer cancel()
		return finishErr(fn(ctx, o, p.positional), o.JSON)
	}
}

// inheritedArgs re-encodes upstream global flags given before the command
// name (`maestro-d --device X --team-id T tapOn Login`) as leading
// arguments, so they act as defaults for the command's own flags.
func inheritedArgs(c *cli.Context, flags []dflag) []string {
	var out []string
	lineage := c.Lineage()
	if len(lineage) < 2 {
		return nil
	}
	for _, parent := range lineage[1:] {
		for _, f := range flags {
			if !parent.IsSet(f.name) {
				continue
			}
			switch f.kind {
			case kindBool:
				out = append(out, fmt.Sprintf("--%s=%t", f.name, parent.Bool(f.name)))
			case kindInt:
				out = append(out, fmt.Sprintf("--%s=%d", f.name, parent.Int(f.name)))
			case kindSlice:
				for _, v := range parent.StringSlice(f.name) {
					out = append(out, "--"+f.name+"="+v)
				}
			default:
				out = append(out, "--"+f.name+"="+parent.String(f.name))
			}
		}
	}
	return out
}

// flagsDoc appends the flag table to a command description.
func flagsDoc(desc string, flags []dflag) string {
	if desc != "" {
		desc += "\n\n"
	}
	return desc + "Flags (anywhere on the line):\n" + flagsUsage(flags)
}

var startFlags = flagSet(daemonSelectFlags, daemonSpawnFlags, one(deviceFlag), attachFlags)

var daemonStartCommand = &cli.Command{
	Name:      "start",
	Usage:     "Start the daemon in the background (and optionally attach a device)",
	ArgsUsage: " ",
	Description: flagsDoc(`Starts the named daemon if it is not already running. Commands start it
automatically, so this is only needed to pre-warm one, to open the REST
API (--http), or to attach a device up front:

  maestro-d start
  maestro-d start --http 127.0.0.1:7433 --token secret
  maestro-d start --device 00008110-… --platform ios --team-id ABCDE12345`, startFlags),
	SkipFlagParsing: true,
	Action: withArgs(startFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
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
			printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "daemon": st.Daemon, "device": dev})
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

var serveFlags = flagSet(daemonSelectFlags[:1], daemonSpawnFlags[1:4])

var daemonServeCommand = &cli.Command{
	Name:            "serve",
	Usage:           "Run the daemon in the foreground (what `start` spawns)",
	ArgsUsage:       " ",
	Description:     flagsDoc("", serveFlags),
	SkipFlagParsing: true,
	Action: withArgs(serveFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
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
		srv := daemon.New(daemon.Config{
			Name:        o.Name,
			HTTPAddr:    o.HTTPAddr,
			Token:       o.Token,
			IdleTimeout: idle,
			ReportsDir:  daemon.ReportsDir(o.Name),
			StartArgs:   os.Args[1:],
			Ready:       os.Stdout,
		}, daemonDeps())
		if err := srv.Run(context.Background()); err != nil {
			logger.Error("daemon exited: %v", err)
			return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
		}
		return nil
	}),
}

var stopFlags = flagSet(daemonSelectFlags, []dflag{
	boolFlag("all", "Stop every daemon of this user", nil, func(o *daemonOpts, v bool) { o.stopAll = v }),
	intFlag("timeout", "Seconds to wait for the daemon to exit (default 30)", nil, func(o *daemonOpts, v int) { o.StartTimeout = v }),
})

var daemonStopCommand = &cli.Command{
	Name:            "stop",
	Usage:           "Stop the daemon: detach all devices, shut down simulators it booted, exit",
	ArgsUsage:       " ",
	Description:     flagsDoc("", stopFlags),
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
					printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "stopped": []string{}})
				} else {
					fmt.Println("No daemons running.")
				}
				return nil
			}
		}
		timeout := 30 * time.Second
		if o.StartTimeout > 0 {
			timeout = time.Duration(o.StartTimeout) * time.Second
		}
		var stopped []string
		for _, name := range names {
			client, err := daemon.Dial(name)
			if err != nil {
				return err
			}
			if client == nil {
				if o.stopAll {
					continue
				}
				return daemon.Errorf(daemon.CodeDaemonUnavailable, "daemon %q is not running", name)
			}
			if err := client.Shutdown(ctx); err != nil {
				if de, ok := err.(*daemon.Error); !ok || de.Code != daemon.CodeDaemonUnavailable {
					return err
				}
			}
			if !daemon.WaitForExit(client.Info, timeout) {
				return daemon.Errorf(daemon.CodeDaemonUnavailable, "daemon %q (pid %d) did not exit within %v", name, client.Info.PID, timeout)
			}
			stopped = append(stopped, name)
			if !o.JSON {
				fmt.Printf("daemon %q stopped\n", name)
			}
		}
		if o.JSON {
			printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "stopped": stopped})
		}
		return nil
	}),
}

var statusFlags = flagSet(daemonSelectFlags)

var daemonStatusCommand = &cli.Command{
	Name:            "status",
	Usage:           "Show the daemon and its devices (exit 3 when not running)",
	ArgsUsage:       " ",
	Description:     flagsDoc("", statusFlags),
	SkipFlagParsing: true,
	Action: withArgs(statusFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		client, err := daemon.Dial(o.Name)
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
			printJSON(os.Stdout, st)
			return nil
		}
		d := st.Daemon
		fmt.Printf("daemon %q: pid %d, version %s, up since %s\n", d.Name, d.PID, d.Version, d.StartedAt)
		fmt.Printf("  socket:  %s\n", d.SocketPath)
		if d.HTTPAddr != "" {
			fmt.Printf("  http:    http://%s\n", d.HTTPAddr)
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
	Description:     flagsDoc("", daemonSelectFlags[1:2]),
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
			printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "daemons": entries})
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
	deviceListFlags  = flagSet(daemonSelectFlags, daemonSpawnFlags[:1], attachFlags[:1])
	deviceStartFlags = flagSet(daemonSelectFlags, daemonSpawnFlags[:1], attachFlags[:1], []dflag{
		intFlag("boot-timeout", "Seconds to wait for boot (default 180)", nil, func(o *daemonOpts, v int) { o.StartTimeout = v }),
	})
	deviceStopFlags = flagSet(daemonSelectFlags, daemonSpawnFlags[:1], []dflag{
		boolFlag("force", "Shut down even if the daemon did not boot it", nil, func(o *daemonOpts, v bool) { o.force = v }),
		boolFlag("orphans", "Shut down devices left behind by dead daemons", nil, func(o *daemonOpts, v bool) { o.orphans = v }),
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
			Description:     flagsDoc("", deviceListFlags),
			SkipFlagParsing: true,
			Action: withArgs(deviceListFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
				client, err := connectDaemon(ctx, o, true)
				if err != nil {
					return err
				}
				devs, err := client.Devices(ctx, o.Attach.Platform)
				if err != nil {
					return err
				}
				if o.JSON {
					printJSON(os.Stdout, daemon.DevicesResult{Envelope: daemon.Envelope{OK: true}, Devices: devs})
					return nil
				}
				printDevices(os.Stdout, devs)
				return nil
			}),
		},
		{
			Name:      "start",
			Usage:     "Boot a simulator (by name or UDID) or emulator (by AVD name or avd:<name>) through the daemon",
			ArgsUsage: "<name-or-udid>",
			Description: flagsDoc(`The daemon remembers what it booted and shuts it down again when it
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
					progress("Booting %s …", args[0])
				}
				dev, err := client.StartDevice(ctx, daemon.BootRequest{Platform: o.Attach.Platform, Name: args[0], BootTimeout: o.StartTimeout})
				if err != nil {
					return err
				}
				if o.JSON {
					printJSON(os.Stdout, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: *dev})
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
			Description: flagsDoc(`Only devices this daemon booted are shut down; pass --force for one that
was already running. --orphans shuts down devices recorded as booted by
daemons that are no longer alive.`, deviceStopFlags),
			SkipFlagParsing: true,
			Action: withArgs(deviceStopFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
				if o.orphans {
					stopped, err := daemon.StopOrphans()
					if err != nil {
						return daemon.WrapErr(daemon.CodeDeviceError, err)
					}
					if o.JSON {
						if stopped == nil {
							stopped = []string{}
						}
						printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "stopped": stopped})
					} else if len(stopped) == 0 {
						fmt.Println("No orphaned devices.")
					} else {
						fmt.Printf("stopped: %s\n", strings.Join(stopped, ", "))
					}
					return nil
				}
				if len(args) != 1 {
					return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d device stop <device-id> [--force] | --orphans")
				}
				client, err := connectDaemon(ctx, o, false)
				if err != nil {
					return err
				}
				if err := client.StopDevice(ctx, args[0], o.force); err != nil {
					return err
				}
				if o.JSON {
					printJSON(os.Stdout, daemon.Envelope{OK: true})
				} else {
					fmt.Printf("%s stopped\n", args[0])
				}
				return nil
			}),
		},
		deviceCatalogCommand,
		deviceCreateCommand,
		deviceDeleteCommand,
	},
}

var attachCmdFlags = flagSet(daemonSelectFlags, daemonSpawnFlags, one(deviceFlag), attachFlags)

var daemonAttachCommand = &cli.Command{
	Name:      "attach",
	Usage:     "Open a driver session on a device (installs the driver/app as `test` would)",
	ArgsUsage: " ",
	Description: flagsDoc(`Attaching is what the first one-shot command does implicitly; do it
explicitly to control the flags or to pay the driver start-up cost early.

  maestro-d attach --device 29271FDH200ABP --app-file app.apk
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
		dev, err := ensureAttached(ctx, client, o, o.Device)
		if err != nil {
			return err
		}
		if o.JSON {
			printJSON(os.Stdout, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: *dev})
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

var detachFlags = flagSet(daemonSelectFlags, daemonSpawnFlags[:1], one(deviceFlag), []dflag{
	boolFlag("all", "Detach every device", nil, func(o *daemonOpts, v bool) { o.stopAll = v }),
})

var daemonDetachCommand = &cli.Command{
	Name:            "detach",
	Usage:           "Close the driver session on a device (the device itself stays as it is)",
	ArgsUsage:       " ",
	Description:     flagsDoc("", detachFlags),
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
			printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "detached": ids})
		}
		return nil
	}),
}

var runFlags = flagSet(daemonSelectFlags, daemonSpawnFlags, one(deviceFlag), one(waitFlag), attachFlags, []dflag{
	boolFlag("continue-on-error", "Keep running remaining steps after a failure (step lists only)", nil, func(o *daemonOpts, v bool) { o.force = v }),
})

var daemonRunCommand = &cli.Command{
	Name:      "run",
	Usage:     "Run a flow file, or a YAML list of steps, on the attached device",
	ArgsUsage: "<flow.yaml | steps.yaml | ->",
	Description: flagsDoc(`A file with a config header (appId: … / ---) runs as a flow exactly like
'maestro-runner test' would, including onFlowStart/onFlowComplete hooks
and flow-scoped env. A bare YAML list of steps runs step by step in the
session; with --continue-on-error every step runs and the exit code
reflects the first failure. '-' reads stdin.

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

var getFlags = flagSet(daemonSelectFlags, daemonSpawnFlags, one(deviceFlag), attachFlags, []dflag{
	strFlag("out", "Write the screenshot to this file (default screenshot-<time>.png; '-' for stdout)", nil, func(o *daemonOpts, v string) { o.outPath = v }, "o"),
	boolFlag("raw", "Hierarchy as the driver's raw XML/JSON instead of the normalized tree", nil, func(o *daemonOpts, v bool) { o.force = v }),
	boolFlag("compact", "Hierarchy as a flat, greppable listing (as `hierarchy --compact`)", nil, func(o *daemonOpts, v bool) { o.compact = v }),
	strFlag("find", "Hierarchy elements matching a substring (as `hierarchy --find`)", nil, func(o *daemonOpts, v string) { o.find = v }),
})

var daemonGetCommand = &cli.Command{
	Name:      "get",
	Usage:     "Read device state: screenshot, hierarchy, state, info, vars",
	ArgsUsage: "screenshot | hierarchy | state | info | vars",
	Description: flagsDoc(`  maestro-d get screenshot -o now.png
  maestro-d get hierarchy | jq '.. | .text? // empty'
  maestro-d get hierarchy --find "Sign in"
  maestro-d get state      # foreground app, orientation, …
  maestro-d get info       # platform, OS version, device name, app version
  maestro-d get vars       # session variables (from -e and copyTextFrom/outputs)`, getFlags),
	SkipFlagParsing: true,
	Action: withArgs(getFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		what := args[0]
		if len(args) != 1 || !map[string]bool{"screenshot": true, "hierarchy": true, "state": true, "info": true, "vars": true}[what] {
			return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d get screenshot|hierarchy|state|info|vars")
		}
		client, id, err := session(ctx, o)
		if err != nil {
			return err
		}
		switch what {
		case "screenshot":
			png, err := client.Screenshot(ctx, id)
			if err != nil {
				return err
			}
			out := o.outPath
			if out == "-" {
				_, err = os.Stdout.Write(png)
				return err
			}
			if out == "" {
				out = fmt.Sprintf("screenshot-%s.png", time.Now().Format("20060102-150405"))
			}
			if err := os.WriteFile(out, png, 0o644); err != nil {
				return daemon.WrapErr(daemon.CodeUsage, err)
			}
			abs, _ := filepath.Abs(out)
			if o.JSON {
				printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "path": abs, "bytes": len(png)})
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
			printJSON(os.Stdout, v)
		case "state":
			v, err := client.State(ctx, id)
			if err != nil {
				return err
			}
			printJSON(os.Stdout, v)
		case "info":
			v, err := client.PlatformInfo(ctx, id)
			if err != nil {
				return err
			}
			printJSON(os.Stdout, v)
		case "vars":
			vars, err := client.Vars(ctx, id)
			if err != nil {
				return err
			}
			if o.JSON {
				printJSON(os.Stdout, daemon.VarsResult{Envelope: daemon.Envelope{OK: true}, Vars: vars})
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

var setFlags = flagSet(daemonSelectFlags, daemonSpawnFlags, one(deviceFlag), attachFlags)

var daemonSetCommand = &cli.Command{
	Name:            "set",
	Usage:           "Set session variables (usable as ${VAR} in later commands and scripts)",
	ArgsUsage:       "VAR=value …",
	Description:     flagsDoc("", setFlags),
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
			printJSON(os.Stdout, daemon.VarsResult{Envelope: daemon.Envelope{OK: true}, Vars: all})
		}
		return nil
	}),
}

var evalFlags = flagSet(daemonSelectFlags, daemonSpawnFlags, one(deviceFlag), one(waitFlag), attachFlags)

var daemonEvalCommand = &cli.Command{
	Name:      "eval",
	Usage:     "Evaluate JavaScript in the session (same engine as evalScript) and print the value",
	ArgsUsage: "<script | ->",
	Description: flagsDoc(`Session variables are in scope; assignments to output.X persist.

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
			printJSON(os.Stdout, daemon.EvalResult{Envelope: daemon.Envelope{OK: true}, Value: v})
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
	Description:     flagsDoc("", daemonSelectFlags[1:2]),
	SkipFlagParsing: true,
	Action: withArgs(daemonSelectFlags[1:2], func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) == 1 {
			spec, ok := daemon.CommandSpecs[args[0]]
			if !ok {
				return daemon.Errorf(daemon.CodeUsage, "unknown command %q", args[0])
			}
			if o.JSON {
				printJSON(os.Stdout, spec)
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
			printJSON(os.Stdout, specs)
			return nil
		}
		for _, t := range types {
			spec := daemon.CommandSpecs[string(t)]
			fmt.Printf("%-28s %s\n", t, spec.Doc)
		}
		return nil
	}),
}
