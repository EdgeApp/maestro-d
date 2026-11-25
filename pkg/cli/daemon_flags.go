package cli

// daemon_flags.go holds the flag table shared by every maestro-d
// command. The lifecycle commands (start, attach, run, get, …) expose these
// as urfave flags so `--help` documents them; the one-shot YAML commands use
// SkipFlagParsing and read the same table through parseDaemonArgs so that
// `maestro-d tapOn Login --device X --team-id T` accepts exactly the
// flags `maestro-d attach` does.

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm"
)

// daemonOpts is everything a daemon command reads from its flags.
type daemonOpts struct {
	Name   string // daemon name (--daemon → MAESTRO_D → default)
	Device string // device id (--device → MAESTRO_DEVICE → only attached)

	JSON    bool
	NoWait  bool
	NoSpawn bool
	Verbose bool

	// gc
	DryRun            bool
	IncludeBuildCache bool

	// Daemon spawn options (start, or auto-start from a one-shot).
	IdleTimeout  int // seconds; 0 = default, -1 = never
	HTTPAddr     string
	Token        string
	StartTimeout int // seconds

	// Target is a TCP daemon or hub (--target-hub / MAESTRO_D_HUB) as
	// given: HOST[:PORT] or http(s)://HOST[:PORT]. Empty means the local
	// named daemon over its unix socket.
	Target string
	// CAFile adds a trust root for a self-signed hub (--ca / MAESTRO_D_CA).
	CAFile string

	// DeviceConfig and NetworkConfig are serve's and hub start's --config
	// and (older) --network-config.
	DeviceConfig  string
	NetworkConfig string

	// explicit records the table flags given on the command line (not
	// from the environment).
	explicit map[string]bool

	// Attach configuration, sent as-is when the CLI has to attach.
	Attach daemon.AttachConfig
	// AttachSet lists the attach flags that were given explicitly, so a
	// one-shot against an already-attached device can warn on mismatch.
	AttachSet []string

	envPairs []string
	envFile  string

	// Command-specific switches.
	stopAll bool             // stop/detach --all
	force   bool             // device stop --force, run --continue-on-error, get hierarchy --raw
	orphans bool             // device stop --orphans
	outPath string           // get screenshot -o, stream -o
	shot    shotFlags        // get screenshot --image, --quality, --region (maestro-d)
	compact bool             // get hierarchy --compact
	find    string           // get hierarchy --find
	manage  deviceManageOpts // device catalog / create
	// dfCreds are the Device Farm credentials --orphans releases with.
	dfCreds devicefarm.Options
	pack    packOpts // pack --flow, and a targeted run

	lease  leaseOpts  // lease, and the lease a targeted command takes
	stream streamOpts // stream
}

// leaseOpts are lease's switches.
type leaseOpts struct {
	// ttl is --ttl in seconds (0: the hub's default window).
	ttl      int
	holder   string
	noAttach bool
	all      bool
	// id is release's and claim's --lease; startAt is reserve's
	// --start-at as given.
	id      string
	startAt string
	sel     daemon.LeaseSelector
	// idleRelease and appiumVersion tune a cloud lease's launch.
	idleRelease   string
	appiumVersion string
}

// packOpts are pack's flow-bundle switches.
type packOpts struct {
	flow    bool
	base    string
	include []string
}

func (o *daemonOpts) markAttach(name string) { o.AttachSet = append(o.AttachSet, name) }

// finish applies env files / pairs and env-var fallbacks after all flags are
// read.
func (o *daemonOpts) finish() error {
	o.Name = daemon.ResolveName(o.Name)
	if o.Device == "" {
		o.Device = os.Getenv(daemon.EnvDevice)
	}
	env := map[string]string{}
	if o.envFile != "" {
		data, err := os.ReadFile(o.envFile)
		if err != nil {
			return daemon.Errorf(daemon.CodeUsage, "read --env-file: %v", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			if k, v, ok := strings.Cut(line, "="); ok {
				env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	for k, v := range parseEnvVars(o.envPairs) {
		env[k] = v
	}
	if len(env) > 0 {
		o.Attach.Env = env
		o.markAttach("env")
	}
	return nil
}

type dflag = common.Flag[daemonOpts]

// envFlag is -e/--env KEY=VALUE, collected into Attach.Env by finish.
var envFlag = dflag{Name: "env", Aliases: []string{"e"}, Kind: common.Slice, Usage: "Session variable KEY=VALUE (repeatable); available to every step as ${KEY}",
	Apply: func(o *daemonOpts, v string) error {
		if !strings.Contains(v, "=") {
			return daemon.Errorf(daemon.CodeUsage, "--env expects KEY=VALUE, got %q", v)
		}
		o.envPairs = append(o.envPairs, v)
		return nil
	}}

// includeFlag is --include GLOB, for pack --flow and a targeted run.
var includeFlag = dflag{Name: "include", Kind: common.Slice, Usage: "Also pack files matching GLOB (relative to the flow's directory) that flows open dynamically (repeatable)",
	Apply: func(o *daemonOpts, v string) error { o.pack.include = append(o.pack.include, v); return nil }}

func attachStr(name, usage string, env []string, set func(a *daemon.AttachConfig, v string), aliases ...string) dflag {
	return common.StringFlag(name, usage, env, func(o *daemonOpts, v string) { set(&o.Attach, v); o.markAttach(name) }, aliases...)
}

func attachBool(name, usage string, env []string, set func(a *daemon.AttachConfig, v bool)) dflag {
	return common.BoolFlag(name, usage, env, func(o *daemonOpts, v bool) { set(&o.Attach, v); o.markAttach(name) })
}

func attachInt(name, usage string, env []string, set func(a *daemon.AttachConfig, v int)) dflag {
	return common.IntFlag(name, usage, env, func(o *daemonOpts, v int) { set(&o.Attach, v); o.markAttach(name) })
}

func extra(a *daemon.AttachConfig, k string, v any) {
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	a.Extra[k] = v
}

// tokenFlag is the bearer token: required by a TCP listener that has one,
// and sent by a client that targets one.
var tokenFlag = common.StringFlag("token", "Bearer token: required on the TCP listener (serve/start), sent to a --target-hub", []string{"MAESTRO_D_TOKEN"},
	func(o *daemonOpts, v string) { o.Token = v })

// targetFlags point a client command at a TCP daemon or hub instead of the
// local named daemon. A targeted command never spawns a daemon.
var targetFlags = []dflag{
	{Name: "target-hub", Kind: common.OptionalValue, Env: []string{"MAESTRO_D_HUB"},
		Usage: "Talk to the daemon or hub at HOST[:PORT] (default port 7433; https:// for TLS) instead of the local one; bare means localhost:7433",
		Apply: func(o *daemonOpts, v string) error {
			if v == "" {
				v = "localhost"
			}
			o.Target = v
			return nil
		}},
	common.StringFlag("ca", "PEM file of a trust root for a --target-hub with a self-signed certificate", []string{"MAESTRO_D_CA"},
		func(o *daemonOpts, v string) { o.CAFile = v }),
	tokenFlag,
}

// Flag groups. A command lists the groups it accepts.
var (
	// daemonSelectFlags pick the daemon and output mode.
	daemonSelectFlags = []dflag{
		// The flag is --daemon, not --name: tapOn, assertVisible, inputText and
		// friends have a `name` selector field, and a field wins over a table
		// flag, so `tapOn X --name ci` would silently become a selector.
		common.StringFlag("daemon", "Daemon name (one daemon per name; each hosts many devices)", []string{daemon.EnvName},
			func(o *daemonOpts, v string) { o.Name = v }, "n"),
		common.BoolFlag("json", "Print the full JSON result on stdout", nil, func(o *daemonOpts, v bool) { o.JSON = v }),
		common.BoolFlag("verbose", "Print progress to stderr", []string{"MAESTRO_VERBOSE"}, func(o *daemonOpts, v bool) { o.Verbose = v }),
	}
	// daemonSpawnFlags control auto-start.
	daemonSpawnFlags = []dflag{
		common.BoolFlag("no-spawn", "Fail instead of starting the daemon when it is not running", nil,
			func(o *daemonOpts, v bool) { o.NoSpawn = v }),
		common.IntFlag("idle-timeout", "Seconds of inactivity before the daemon exits (0 = never; default 1800)", []string{"MAESTRO_D_IDLE_TIMEOUT"},
			func(o *daemonOpts, v int) {
				if v == 0 {
					o.IdleTimeout = -1
				} else {
					o.IdleTimeout = v
				}
			}),
		common.StringFlag("http", "Also listen on this TCP address (host:port) for the REST API", []string{"MAESTRO_D_HTTP"},
			func(o *daemonOpts, v string) { o.HTTPAddr = v }),
		tokenFlag,
		common.IntFlag("start-timeout", "Seconds to wait for a spawned daemon to become ready (default 20)", nil,
			func(o *daemonOpts, v int) { o.StartTimeout = v }),
	}
	// deviceFlag selects the device.
	deviceFlag = common.StringFlag("device", "Device id / UDID (default: $MAESTRO_DEVICE, else the only attached device)", []string{daemon.EnvDevice},
		func(o *daemonOpts, v string) { o.Device = v }, "udid")
	// waitFlag is for step-running commands.
	waitFlag = common.BoolFlag("no-wait", "Fail with BUSY instead of queueing behind a step already running on the device", nil,
		func(o *daemonOpts, v bool) { o.NoWait = v })
	// attachFlags configure the driver; the same set `maestro-runner test` takes.
	attachFlags = []dflag{
		attachStr("platform", "Platform of the device: ios, android, web, mock (inferred from the id when omitted)", []string{"MAESTRO_PLATFORM"},
			func(a *daemon.AttachConfig, v string) { a.Platform = v }, "p"),
		attachStr("driver", "Driver: uiautomator2, wda, appium, devicelab, playwright, mock", []string{"MAESTRO_DRIVER"},
			func(a *daemon.AttachConfig, v string) { a.Driver = v }, "d"),
		attachStr("app-file", "App binary (.apk, .app, .ipa) to install on attach", []string{"MAESTRO_APP_FILE"},
			func(a *daemon.AttachConfig, v string) { a.AppFile = v }),
		attachStr("app-id", "Default appId for launchApp/stopApp when a step omits it", []string{"MAESTRO_APP_ID"},
			func(a *daemon.AttachConfig, v string) { a.AppID = v }),
		attachStr("url", "Start URL (web); on one-shots that have a url field use `attach --url`", []string{"MAESTRO_URL"},
			func(a *daemon.AttachConfig, v string) { a.URL = v }),
		attachStr("team-id", "Apple Development Team ID for WDA code signing (iOS)", []string{"MAESTRO_TEAM_ID", "DEVELOPMENT_TEAM"},
			func(a *daemon.AttachConfig, v string) { a.TeamID = v }),
		attachStr("wda-bundle-id", "Custom WDA bundle identifier (iOS)", []string{"MAESTRO_WDA_BUNDLE_ID"},
			func(a *daemon.AttachConfig, v string) { a.WDABundleID = v }),
		attachStr("appium-url", "Appium server URL (appium driver)", []string{"APPIUM_URL"},
			func(a *daemon.AttachConfig, v string) { a.AppiumURL = v }),
		attachStr("caps", "Path to Appium capabilities JSON file", []string{"APPIUM_CAPS"},
			func(a *daemon.AttachConfig, v string) { extra(a, "capsFile", v) }),
		envFlag,
		common.StringFlag("env-file", "File of KEY=VALUE lines to load as session variables", nil,
			func(o *daemonOpts, v string) { o.envFile = v }),
		attachStr("artifacts", "Artifact mode: all, none, on-failure (default)", []string{"MAESTRO_ARTIFACTS"},
			func(a *daemon.AttachConfig, v string) { a.Artifacts = v }),
		attachStr("output-dir", "Report/artifact directory (default ~/.maestro-d/run/<name>/reports/<device>/<ts>)", []string{"MAESTRO_OUTPUT"},
			func(a *daemon.AttachConfig, v string) { a.OutputDir = v }),
		attachBool("discard-reports", "Delete the session's report directory at detach (default: kept, then swept after 3 days)", nil,
			func(a *daemon.AttachConfig, v bool) { a.DiscardReports = v }),
		attachBool("no-app-install", "Skip app installation even if --app-file is given", []string{"MAESTRO_NO_APP_INSTALL"},
			func(a *daemon.AttachConfig, v bool) { a.NoAppInstall = v }),
		attachBool("no-driver-install", "Skip driver installation (UIAutomator2, WDA)", []string{"MAESTRO_NO_DRIVER_INSTALL"},
			func(a *daemon.AttachConfig, v bool) { a.NoDriverInstall = v }),
		attachInt("driver-start-timeout", "Driver start timeout in seconds (0 = driver default)", []string{"MAESTRO_DRIVER_START_TIMEOUT"},
			func(a *daemon.AttachConfig, v int) { a.DriverStartTimeout = v }),
		attachInt("wait-for-idle-timeout", "Milliseconds to wait for the UI to settle after each action (default 200)", nil,
			func(a *daemon.AttachConfig, v int) { n := v; a.WaitForIdleTimeout = &n }),
		attachInt("condition-timeout", "Default timeout in ms for assertions / waitUntil (default 15000)", nil,
			func(a *daemon.AttachConfig, v int) { a.ConditionTimeout = v }),
		attachInt("step-delay", "Milliseconds to sleep between steps", nil,
			func(a *daemon.AttachConfig, v int) { a.StepDelay = v }),
		attachInt("typing-frequency", "Characters per second for inputText", nil,
			func(a *daemon.AttachConfig, v int) { a.TypingFrequency = v }),
		attachInt("command-timeout", "Hard timeout in ms for any single command on this device (0 = none)", nil,
			func(a *daemon.AttachConfig, v int) { a.CommandTimeout = v }),
		attachInt("new-command-timeout", "appium:newCommandTimeout in seconds (appium driver)", []string{"MAESTRO_NEW_COMMAND_TIMEOUT"},
			func(a *daemon.AttachConfig, v int) { extra(a, "newCommandTimeout", float64(v)) }),
		dflag{Name: "alert-monitor", Kind: common.Bool, Value: "true", Usage: "Auto-dismiss system alerts between steps (default true; --alert-monitor=false to disable)",
			Apply: func(o *daemonOpts, v string) error {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return daemon.Errorf(daemon.CodeUsage, "--alert-monitor expects true or false, got %q", v)
				}
				o.Attach.AlertMonitor = &b
				o.markAttach("alert-monitor")
				return nil
			}},
		attachBool("headed", "Show the browser window (web)", nil,
			func(a *daemon.AttachConfig, v bool) { extra(a, "headed", v) }),
		attachStr("browser", "Browser for the playwright driver: chromium, firefox, webkit", nil,
			func(a *daemon.AttachConfig, v string) { extra(a, "browser", v) }),
		attachStr("window-size", "Browser window size WIDTHxHEIGHT (web)", nil,
			func(a *daemon.AttachConfig, v string) { extra(a, "windowSize", v) }),
		attachStr("user-data-dir", "Browser profile directory (web)", nil,
			func(a *daemon.AttachConfig, v string) { extra(a, "userDataDir", v) }),
		attachBool("android-tcp-forward", "Use adb TCP forwarding instead of the default transport (android)", nil,
			func(a *daemon.AttachConfig, v bool) { extra(a, "androidTcpForward", v) }),
	}
)

// parseDaemonArgs adds the client environment and attach rules to the common flag grammar.
func parseDaemonArgs(args []string, flags []dflag, stepKeys map[string]string) (*daemonOpts, *common.ParsedArgs, error) {
	o, p, err := common.ParseArgs(args, flags, stepKeys)
	if err != nil {
		return nil, nil, err
	}
	o.explicit = p.Explicit
	if err := o.finish(); err != nil {
		return nil, nil, err
	}
	if err := o.checkTargeted(); err != nil {
		return nil, nil, err
	}
	return o, p, nil
}

// checkTargeted refuses the attach settings that name directories on the
// target's host: an explicit --output-dir or --user-data-dir is a usage
// error, and an outputDir that came only from MAESTRO_OUTPUT (a shell set
// up for local runs) is dropped with a notice. Appium capabilities are
// refused from either source: a hub attaches no Appium for a remote caller.
func (o *daemonOpts) checkTargeted() error {
	if o.Target == "" {
		return nil
	}
	for _, name := range []string{"output-dir", "user-data-dir"} {
		if o.explicit[name] {
			return daemon.Errorf(daemon.CodeUsage, "--%s names a directory on the daemon's host; it cannot be used with --target-hub", name)
		}
	}
	if v, _ := o.Attach.Extra["capsFile"].(string); v != "" {
		flag := "APPIUM_CAPS"
		if o.explicit["caps"] {
			flag = "--caps"
		}
		return daemon.Errorf(daemon.CodeUsage, "%s cannot be used with --target-hub: a hub attaches no Appium driver for a remote caller", flag)
	}
	if o.Attach.OutputDir != "" {
		fmt.Fprintf(os.Stderr, "note: ignoring MAESTRO_OUTPUT=%s with --target-hub (it names a directory on the daemon's host)\n", o.Attach.OutputDir)
		o.Attach.OutputDir = ""
		kept := o.AttachSet[:0]
		for _, n := range o.AttachSet {
			if n != "output-dir" {
				kept = append(kept, n)
			}
		}
		o.AttachSet = kept
	}
	return nil
}

// typedScalar converts a flag value string to the Go value the YAML parser
// would produce for it: true/false, integers, floats, and `{…}`/`[…]`
// documents; everything else stays a string.
func typedScalar(s string) (any, error) {
	switch {
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	case s == "null" || s == "~":
		return nil, nil
	case intRe.MatchString(s):
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return int(n), nil
		}
	case floatRe.MatchString(s):
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, nil
		}
	case strings.HasPrefix(s, "{") || strings.HasPrefix(s, "["):
		var v any
		if err := yaml.Unmarshal([]byte(s), &v); err != nil {
			return nil, daemon.Errorf(daemon.CodeUsage, "invalid YAML/JSON value %q: %v", s, err)
		}
		return v, nil
	}
	return s, nil
}

var (
	intRe   = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	floatRe = regexp.MustCompile(`^-?[0-9]+\.[0-9]+$`)
)

// setPath assigns v at a dotted path inside nested maps.
func setPath(m map[string]any, path string, v any) error {
	parts := strings.Split(path, ".")
	cur := m
	for i, part := range parts {
		if part == "" {
			return daemon.Errorf(daemon.CodeUsage, "bad flag name --%s", path)
		}
		if i == len(parts)-1 {
			cur[part] = v
			return nil
		}
		next, ok := cur[part].(map[string]any)
		if !ok {
			if _, exists := cur[part]; exists {
				return daemon.Errorf(daemon.CodeUsage, "--%s conflicts with an earlier --%s value", path, strings.Join(parts[:i+1], "."))
			}
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	return nil
}
