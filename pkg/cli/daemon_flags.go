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

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// daemonOpts is everything a daemon command reads from its flags.
type daemonOpts struct {
	Name   string // daemon name (--daemon → MAESTRO_D → default)
	Device string // device id (--device → MAESTRO_DEVICE → only attached)

	JSON    bool
	NoWait  bool
	NoSpawn bool
	Verbose bool

	// Daemon spawn options (start, or auto-start from a one-shot).
	IdleTimeout  int // seconds; 0 = default, -1 = never
	HTTPAddr     string
	Token        string
	StartTimeout int // seconds

	// Attach configuration, sent as-is when the CLI has to attach.
	Attach daemon.AttachConfig
	// AttachSet lists the attach flags that were given explicitly, so a
	// one-shot against an already-attached device can warn on mismatch.
	AttachSet []string

	envPairs []string
	envFile  string

	// Command-specific switches.
	stopAll bool   // stop/detach --all
	force   bool   // device stop --force, run --continue-on-error, get hierarchy --raw
	orphans bool   // device stop --orphans
	outPath string // get screenshot -o
	compact bool   // get hierarchy --compact
	find    string // get hierarchy --find
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

type flagKind int

const (
	kindString flagKind = iota
	kindBool
	kindInt
	kindSlice
)

// dflag is one entry of the shared flag table.
type dflag struct {
	name    string
	aliases []string
	kind    flagKind
	env     []string
	usage   string
	value   string // default, shown in help; not applied
	// apply receives the string form ("true"/"false" for bools) once per
	// occurrence (slices) or once (everything else).
	apply func(o *daemonOpts, v string) error
}

func strFlag(name, usage string, env []string, apply func(o *daemonOpts, v string), aliases ...string) dflag {
	return dflag{name: name, aliases: aliases, kind: kindString, env: env, usage: usage,
		apply: func(o *daemonOpts, v string) error { apply(o, v); return nil }}
}

func boolFlag(name, usage string, env []string, apply func(o *daemonOpts, v bool), aliases ...string) dflag {
	return dflag{name: name, aliases: aliases, kind: kindBool, env: env, usage: usage,
		apply: func(o *daemonOpts, v string) error {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return daemon.Errorf(daemon.CodeUsage, "--%s expects true or false, got %q", name, v)
			}
			apply(o, b)
			return nil
		}}
}

func intFlag(name, usage string, env []string, apply func(o *daemonOpts, v int), aliases ...string) dflag {
	return dflag{name: name, aliases: aliases, kind: kindInt, env: env, usage: usage,
		apply: func(o *daemonOpts, v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return daemon.Errorf(daemon.CodeUsage, "--%s expects an integer, got %q", name, v)
			}
			apply(o, n)
			return nil
		}}
}

func attachStr(name, usage string, env []string, set func(a *daemon.AttachConfig, v string), aliases ...string) dflag {
	return strFlag(name, usage, env, func(o *daemonOpts, v string) { set(&o.Attach, v); o.markAttach(name) }, aliases...)
}

func attachBool(name, usage string, env []string, set func(a *daemon.AttachConfig, v bool)) dflag {
	return boolFlag(name, usage, env, func(o *daemonOpts, v bool) { set(&o.Attach, v); o.markAttach(name) })
}

func attachInt(name, usage string, env []string, set func(a *daemon.AttachConfig, v int)) dflag {
	return intFlag(name, usage, env, func(o *daemonOpts, v int) { set(&o.Attach, v); o.markAttach(name) })
}

func extra(a *daemon.AttachConfig, k string, v any) {
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	a.Extra[k] = v
}

// Flag groups. A command lists the groups it accepts.
var (
	// daemonSelectFlags pick the daemon and output mode.
	daemonSelectFlags = []dflag{
		// The flag is --daemon, not --name: tapOn, assertVisible, inputText and
		// friends have a `name` selector field, and a field wins over a table
		// flag, so `tapOn X --name ci` would silently become a selector.
		strFlag("daemon", "Daemon name (one daemon per name; each hosts many devices)", []string{daemon.EnvName},
			func(o *daemonOpts, v string) { o.Name = v }, "n"),
		boolFlag("json", "Print the full JSON result on stdout", nil, func(o *daemonOpts, v bool) { o.JSON = v }),
		boolFlag("verbose", "Print progress to stderr", []string{"MAESTRO_VERBOSE"}, func(o *daemonOpts, v bool) { o.Verbose = v }),
	}
	// daemonSpawnFlags control auto-start.
	daemonSpawnFlags = []dflag{
		boolFlag("no-spawn", "Fail instead of starting the daemon when it is not running", nil,
			func(o *daemonOpts, v bool) { o.NoSpawn = v }),
		intFlag("idle-timeout", "Seconds of inactivity before the daemon exits (0 = never; default 1800)", []string{"MAESTRO_D_IDLE_TIMEOUT"},
			func(o *daemonOpts, v int) {
				if v == 0 {
					o.IdleTimeout = -1
				} else {
					o.IdleTimeout = v
				}
			}),
		strFlag("http", "Also listen on this TCP address (host:port) for the REST API", []string{"MAESTRO_D_HTTP"},
			func(o *daemonOpts, v string) { o.HTTPAddr = v }),
		strFlag("token", "Bearer token required on the TCP listener", []string{"MAESTRO_D_TOKEN"},
			func(o *daemonOpts, v string) { o.Token = v }),
		intFlag("start-timeout", "Seconds to wait for a spawned daemon to become ready (default 20)", nil,
			func(o *daemonOpts, v int) { o.StartTimeout = v }),
	}
	// deviceFlag selects the device.
	deviceFlag = strFlag("device", "Device id / UDID (default: $MAESTRO_DEVICE, else the only attached device)", []string{daemon.EnvDevice},
		func(o *daemonOpts, v string) { o.Device = v }, "udid")
	// waitFlag is for step-running commands.
	waitFlag = boolFlag("no-wait", "Fail with BUSY instead of queueing behind a step already running on the device", nil,
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
		dflag{name: "env", aliases: []string{"e"}, kind: kindSlice, usage: "Session variable KEY=VALUE (repeatable); available to every step as ${KEY}",
			apply: func(o *daemonOpts, v string) error {
				if !strings.Contains(v, "=") {
					return daemon.Errorf(daemon.CodeUsage, "--env expects KEY=VALUE, got %q", v)
				}
				o.envPairs = append(o.envPairs, v)
				return nil
			}},
		strFlag("env-file", "File of KEY=VALUE lines to load as session variables", nil,
			func(o *daemonOpts, v string) { o.envFile = v }),
		attachStr("artifacts", "Artifact mode: all, none, on-failure (default)", []string{"MAESTRO_ARTIFACTS"},
			func(a *daemon.AttachConfig, v string) { a.Artifacts = v }),
		attachStr("output-dir", "Report/artifact directory (default ~/.maestro-d/run/<name>/reports/<device>/<ts>)", []string{"MAESTRO_OUTPUT"},
			func(a *daemon.AttachConfig, v string) { a.OutputDir = v }),
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
		dflag{name: "alert-monitor", kind: kindBool, value: "true", usage: "Auto-dismiss system alerts between steps (default true; --alert-monitor=false to disable)",
			apply: func(o *daemonOpts, v string) error {
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
		attachBool("follow-window", "Size the window to --window-size and let the page follow window resizes (web)", nil,
			func(a *daemon.AttachConfig, v bool) { extra(a, "followWindow", v) }),
		attachBool("android-tcp-forward", "Use adb TCP forwarding instead of the default transport (android)", nil,
			func(a *daemon.AttachConfig, v bool) { extra(a, "androidTcpForward", v) }),
	}
)

// flagSet joins groups into one ordered list.
func flagSet(groups ...[]dflag) []dflag {
	var out []dflag
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func one(f dflag) []dflag { return []dflag{f} }

// parsedArgs is the output of parseDaemonArgs for SkipFlagParsing commands.
type parsedArgs struct {
	// positional arguments in order.
	positional []string
	// step holds flags the table does not know, in order: they become the
	// command's YAML value (`--id foo` → {id: foo}).
	step []stepFlag
	// base holds BaseStep flags (optional, label, timeout) if given.
	base map[string]any
	// platformGate is --platform, which doubles as the step's platform gate.
	platformGate string
}

type stepFlag struct {
	key   string // as written, without dashes; may be dotted
	value string
	bare  bool // given without a value (→ true)
}

// isBoolType reports whether a command field's Go type is a bool.
func isBoolType(goType string) bool {
	return goType == "bool" || goType == "*bool"
}

// baseStepFlags are accepted by every one-shot command and merged into the
// value map; "platform" is shared with the attach flag of the same name.
var baseStepFlags = map[string]flagKind{"optional": kindBool, "label": kindString, "timeout": kindInt}

// parseDaemonArgs implements the one-shot grammar:
//
//	<cmd> [VALUE …] [--flag value | --flag=value | --bool] [-e K=V] [--] [VALUE …]
//
// Known flags (from the table) fill opts; unknown flags are collected as
// step fields. A flag without a following value is a boolean true; the
// following token is taken as the value unless it starts with "--" (a bare
// "-" or a negative number is still a value). stepKeys maps the command's
// field names to their Go types so that a bool field never swallows the
// token after it (`launchApp --clearState co.example`).
func parseDaemonArgs(args []string, flags []dflag, stepKeys map[string]string) (*daemonOpts, *parsedArgs, error) {
	byName := map[string]dflag{}
	for _, f := range flags {
		byName[f.name] = f
		for _, a := range f.aliases {
			byName[a] = f
		}
	}
	o := &daemonOpts{}
	p := &parsedArgs{base: map[string]any{}}
	explicit := map[string]bool{}

	usage := func(format string, a ...any) error { return daemon.Errorf(daemon.CodeUsage, format, a...) }
	takesValue := func(next []string) (string, bool) {
		if len(next) == 0 {
			return "", false
		}
		v := next[0]
		if v == "-" || !strings.HasPrefix(v, "-") || negNumRe.MatchString(v) {
			return v, true
		}
		return "", false
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.positional = append(p.positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" || negNumRe.MatchString(a) {
			p.positional = append(p.positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		value, hasValue := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, value, hasValue = k, v, true
		}
		// --no-<bool> negates a known bool flag.
		negated := false
		if f, ok := byName[strings.TrimPrefix(name, "no-")]; !hasValue && strings.HasPrefix(name, "no-") && ok && f.kind == kindBool {
			if _, direct := byName[name]; !direct {
				name, negated = strings.TrimPrefix(name, "no-"), true
			}
		}
		// A field of the command wins over a table flag of the same name
		// (`openBrowser --url`, `getConsoleLogs --output`); --platform is
		// both, and -e/--env feeds the step's env map when it has one.
		f, known := byName[name]
		if known && name != "platform" && (stepKeys[name] != "" || (f.name == "env" && stepKeys["env"] != "")) {
			known = false
			if f.name == "env" {
				name = "env"
			}
		}
		if known {
			switch {
			case f.kind == kindBool:
				if negated {
					value = "false"
				} else if !hasValue {
					value = "true"
				}
			case !hasValue:
				if v, ok := takesValue(args[i+1:]); ok {
					value, i = v, i+1
				} else {
					return nil, nil, usage("--%s needs a value", name)
				}
			}
			if err := f.apply(o, value); err != nil {
				return nil, nil, err
			}
			explicit[f.name] = true
			if f.name == "platform" {
				p.platformGate = value
			}
			continue
		}
		if kind, ok := baseStepFlags[name]; ok {
			switch kind {
			case kindBool:
				if !hasValue {
					value = "true"
				}
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, nil, usage("--%s expects true or false", name)
				}
				p.base[name] = b
			case kindInt:
				if !hasValue {
					if v, ok := takesValue(args[i+1:]); ok {
						value, i = v, i+1
					} else {
						return nil, nil, usage("--%s needs a value", name)
					}
				}
				n, err := strconv.Atoi(value)
				if err != nil {
					return nil, nil, usage("--%s expects an integer, got %q", name, value)
				}
				p.base[name] = n
			default:
				if !hasValue {
					if v, ok := takesValue(args[i+1:]); ok {
						value, i = v, i+1
					} else {
						return nil, nil, usage("--%s needs a value", name)
					}
				}
				p.base[name] = value
			}
			continue
		}
		// Unknown: a step field. Bool fields never take the next token.
		if !hasValue && !isBoolType(stepKeys[name]) {
			if v, ok := takesValue(args[i+1:]); ok {
				value, i, hasValue = v, i+1, true
			}
		}
		if !hasValue {
			p.step = append(p.step, stepFlag{key: name, bare: true})
			continue
		}
		p.step = append(p.step, stepFlag{key: name, value: value})
	}

	// Env-var fallbacks for known flags that were not given.
	for _, f := range flags {
		if explicit[f.name] {
			continue
		}
		for _, e := range f.env {
			if v := os.Getenv(e); v != "" {
				if f.kind == kindSlice {
					continue
				}
				if err := f.apply(o, v); err != nil {
					return nil, nil, err
				}
				break
			}
		}
	}
	if err := o.finish(); err != nil {
		return nil, nil, err
	}
	return o, p, nil
}

var negNumRe = regexp.MustCompile(`^-[0-9]+(\.[0-9]+)?$`)

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

// flagsUsage renders a table for the Description of SkipFlagParsing
// commands, whose flags urfave cannot list.
func flagsUsage(flags []dflag) string {
	var b strings.Builder
	for _, f := range flags {
		names := "--" + f.name
		for _, a := range f.aliases {
			if len(a) == 1 {
				names = "-" + a + ", " + names
			} else {
				names += ", --" + a
			}
		}
		switch f.kind {
		case kindString, kindSlice:
			names += " value"
		case kindInt:
			names += " N"
		}
		fmt.Fprintf(&b, "   %-32s %s\n", names, f.usage)
	}
	return b.String()
}
