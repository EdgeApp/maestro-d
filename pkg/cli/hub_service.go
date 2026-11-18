package cli

// hub_service.go: `maestro-d hub service install|uninstall|status`, the
// hub as a per-user launchd LaunchAgent on macOS
// (~/Library/LaunchAgents/app.edge.maestro-d.hub.plist). It is an agent,
// not a system daemon, because simulators and WebDriverAgent need the
// user's GUI session. launchd runs `hub start --foreground` at login and
// again after a crash (KeepAlive {SuccessfulExit: false}), but not after a
// clean `hub stop`. The tokens never go into the plist: install writes
// them to <home>/hub/secrets (mode 0600) and the plist names the files.
// While the service is installed, `hub start` starts it through launchd
// (kickstart), so there is never a second hub process. On Linux, run
// `hub start --foreground` under any supervisor (docs/daemon/hub.md).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
	"howett.net/plist"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// ServiceLabel is the launchd label of the hub service for daemon "hub" in
// the default home (~/.maestro-d).
const ServiceLabel = "app.edge.maestro-d.hub"

// serviceLabel is the launchd label of the hub service for the daemon name
// in this home. Another home or daemon name gets its own label, so two hubs
// on one machine are two services, and `hub start` for one never kickstarts
// the other. A home's part is its directory name plus a short hash of its
// full path, so two homes with one directory name still differ; a daemon
// name is already limited to characters launchd labels take.
func serviceLabel(name string) string {
	label := ServiceLabel
	home := filepath.Clean(daemon.Home())
	if u, err := os.UserHomeDir(); err != nil || home != filepath.Join(u, ".maestro-d") {
		sum := sha256.Sum256([]byte(home))
		label += "." + labelPart(filepath.Base(home)) + "-" + hex.EncodeToString(sum[:4])
	}
	if name != "" && name != DefaultHubDaemon {
		label += "." + name
	}
	return label
}

// labelPart keeps a home directory's name to letters, digits and dashes.
func labelPart(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			b[i] = '-'
		}
	}
	return strings.Trim(string(b), "-")
}

// Seams for tests.
var (
	serviceGOOS     = runtime.GOOS
	launchAgentsDir = func() string {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "LaunchAgents")
	}
	launchctl = func(args ...string) ([]byte, error) {
		return exec.Command("launchctl", args...).CombinedOutput()
	}
	serviceUID = os.Getuid
	// waitForHub waits until the named hub answers on its socket.
	waitForHub = func(ctx context.Context, name string, timeout time.Duration) (*daemon.Client, error) {
		deadline := time.Now().Add(timeout)
		for {
			if c, _ := daemon.Dial(name); c != nil && c.Ping(2*time.Second) == nil {
				return c, nil
			}
			if time.Now().After(deadline) {
				return nil, daemon.Errorf(daemon.CodeDaemonUnavailable, "hub %q did not start within %v; see %s", name, timeout,
					filepath.Join(daemon.RunDir(name), "daemon.log"))
			}
			select {
			case <-ctx.Done():
				return nil, daemon.WrapErr(daemon.CodeInterrupted, ctx.Err())
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
)

// servicePassEnv are the variables install records into the plist, when
// set: launchd starts the agent with a bare environment.
var servicePassEnv = []string{"PATH", daemon.EnvHome, "ANDROID_HOME", "ANDROID_SDK_ROOT", "DEVELOPER_DIR", "MAESTRO_TEAM_ID"}

func servicePlistPath(name string) string {
	return filepath.Join(launchAgentsDir(), serviceLabel(name)+".plist")
}

// serviceSecretsDir holds the tokens of the hub service for daemon name:
// <home>/hub/secrets for the default daemon, <home>/hub/daemons/<name>/secrets
// for another. The paths never nest, so two services in one home never share,
// shadow or delete each other's files, whatever their names.
func serviceSecretsDir(name string) string {
	if name == "" || name == DefaultHubDaemon {
		return filepath.Join(daemon.Home(), "hub", "secrets")
	}
	return filepath.Join(daemon.Home(), "hub", "daemons", name, "secrets")
}

func serviceTarget(name string) string {
	return fmt.Sprintf("gui/%d/%s", serviceUID(), serviceLabel(name))
}

// serviceInstalled reports whether the hub daemon name runs as a launchd
// service in this home.
func serviceInstalled(name string) bool {
	if serviceGOOS != "darwin" {
		return false
	}
	_, err := os.Stat(servicePlistPath(name))
	return err == nil
}

func serviceUnsupported() error {
	return daemon.Errorf(daemon.CodeUsage, "the hub service is a macOS LaunchAgent; on %s run `maestro-d hub start --foreground` under your supervisor (docs/daemon/hub.md shows a systemd unit)", serviceGOOS)
}

var hubServiceUninstallFlags = flagSet(one(hubDaemonFlag), daemonSelectFlags[1:], []dflag{
	boolFlag("purge", "Also delete the tokens install wrote under <home>/hub/secrets (<home>/hub/daemons/<daemon>/secrets for a daemon other than hub)", nil,
		func(o *daemonOpts, v bool) { o.hub.purge = v }),
})

var hubServiceStatusFlags = flagSet(one(hubDaemonFlag), daemonSelectFlags[1:])

var hubServiceCommand = &cli.Command{
	Name:  "service",
	Usage: "Run the hub as a launchd LaunchAgent (macOS): install, uninstall, status",
	Subcommands: []*cli.Command{
		{
			Name:      "install",
			Usage:     "Install and load the hub as a LaunchAgent that starts at login and restarts after a crash",
			ArgsUsage: " ",
			Description: flagsDoc(`Takes hub start's flags. The agent runs 'hub start --foreground' with
them; the tokens are written to <home>/hub/secrets (mode 0600) and passed
as --token-file/--peer-token-file, never written into the plist. PATH,
MAESTRO_D_HOME, ANDROID_HOME, ANDROID_SDK_ROOT, DEVELOPER_DIR and
MAESTRO_TEAM_ID are recorded when set. The hub's output goes to its
daemon.log.

  maestro-d hub service install --name lab-a --token-file ~/.hub-token --peer-token-file ~/.peer-token`, hubStartFlags),
			SkipFlagParsing: true,
			Action:          withArgs(hubStartFlags, runServiceInstall),
		},
		{
			Name:            "uninstall",
			Usage:           "Unload the LaunchAgent (stopping the hub) and delete its plist; --purge also deletes the tokens",
			ArgsUsage:       " ",
			Description:     flagsDoc("", hubServiceUninstallFlags),
			SkipFlagParsing: true,
			Action:          withArgs(hubServiceUninstallFlags, runServiceUninstall),
		},
		{
			Name:            "status",
			Usage:           "Print the LaunchAgent's launchd state, then hub status",
			ArgsUsage:       " ",
			Description:     flagsDoc("", hubServiceStatusFlags),
			SkipFlagParsing: true,
			Action:          withArgs(hubServiceStatusFlags, runServiceStatus),
		},
	},
}

// writeSecret writes one token file, mode 0600.
func writeSecret(path, value string) error {
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	return os.Chmod(path, 0o600)
}

// servicePlist builds the LaunchAgent's plist.
func servicePlist(o *daemonOpts, bin, tokenFile, peerTokenFile string) ([]byte, error) {
	args := append([]string{bin}, hubChildArgs(o)...)
	args = append(args, "--token-file", tokenFile, "--peer-token-file", peerTokenFile)
	env := map[string]string{}
	for _, k := range servicePassEnv {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	logPath := filepath.Join(daemon.RunDir(o.Name), "daemon.log")
	return plist.MarshalIndent(map[string]any{
		"Label":                serviceLabel(o.Name),
		"ProgramArguments":     args,
		"RunAtLoad":            true,
		"KeepAlive":            map[string]any{"SuccessfulExit": false},
		"EnvironmentVariables": env,
		"StandardOutPath":      logPath,
		"StandardErrorPath":    logPath,
		"ProcessType":          "Interactive",
	}, plist.XMLFormat, "\t")
}

func runServiceInstall(ctx context.Context, o *daemonOpts, args []string) error {
	if serviceGOOS != "darwin" {
		return serviceUnsupported()
	}
	token, peerToken, err := checkHubFlags(o)
	if err != nil {
		return err
	}
	if c, _ := daemon.Dial(o.Name); c != nil && !serviceInstalled(o.Name) {
		return daemon.Errorf(daemon.CodeUsage, "hub %q is already running (pid %d); stop it first (maestro-d hub stop)", o.Name, c.Info.PID)
	}
	dir := serviceSecretsDir(o.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	tokenFile, peerTokenFile := filepath.Join(dir, "token"), filepath.Join(dir, "peer-token")
	if err := writeSecret(tokenFile, token); err != nil {
		return err
	}
	if err := writeSecret(peerTokenFile, peerToken); err != nil {
		return err
	}
	bin, err := daemon.Binary()
	if err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	data, err := servicePlist(o, bin, tokenFile, peerTokenFile)
	if err != nil {
		return daemon.WrapErr(daemon.CodeInternal, err)
	}
	if err := os.MkdirAll(daemon.RunDir(o.Name), 0o700); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	if err := os.MkdirAll(launchAgentsDir(), 0o755); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	if serviceInstalled(o.Name) {
		_, _ = launchctl("bootout", serviceTarget(o.Name))
	}
	path := servicePlistPath(o.Name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	if out, err := launchctl("bootstrap", fmt.Sprintf("gui/%d", serviceUID()), path); err != nil {
		return daemon.Errorf(daemon.CodeDaemonUnavailable, "launchctl bootstrap %s: %v %s", path, err, strings.TrimSpace(string(out)))
	}
	c, err := waitForHub(ctx, o.Name, startTimeout(o))
	if err != nil {
		return err
	}
	if !o.JSON {
		fmt.Printf("hub service installed: %s\n", path)
	}
	return printHub(o, c.Info, "running under launchd")
}

func startTimeout(o *daemonOpts) time.Duration {
	if o.StartTimeout > 0 {
		return time.Duration(o.StartTimeout) * time.Second
	}
	return 30 * time.Second
}

// kickstartService starts the installed service's hub (hub start).
func kickstartService(ctx context.Context, o *daemonOpts) error {
	if c, _ := daemon.Dial(o.Name); c != nil && c.Ping(2*time.Second) == nil {
		return printHub(o, c.Info, "already running under launchd")
	}
	if out, err := launchctl("kickstart", serviceTarget(o.Name)); err != nil {
		return daemon.Errorf(daemon.CodeDaemonUnavailable, "launchctl kickstart %s: %v %s", serviceTarget(o.Name), err, strings.TrimSpace(string(out)))
	}
	if !o.JSON {
		progress("Started the hub service through launchd (kickstart %s)", serviceTarget(o.Name))
	}
	c, err := waitForHub(ctx, o.Name, startTimeout(o))
	if err != nil {
		return err
	}
	return printHub(o, c.Info, "running under launchd")
}

func runServiceUninstall(ctx context.Context, o *daemonOpts, args []string) error {
	if serviceGOOS != "darwin" {
		return serviceUnsupported()
	}
	hubName(o)
	path := servicePlistPath(o.Name)
	installed := serviceInstalled(o.Name)
	if installed {
		if out, err := launchctl("bootout", serviceTarget(o.Name)); err != nil && !strings.Contains(string(out), "No such process") {
			progress("launchctl bootout %s: %v %s", serviceTarget(o.Name), err, strings.TrimSpace(string(out)))
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return daemon.WrapErr(daemon.CodeUsage, err)
		}
	}
	if c, _ := daemon.Dial(o.Name); c != nil {
		daemon.WaitForExit(c.Info, startTimeout(o))
	}
	var purged []string
	if o.hub.purge {
		dir := serviceSecretsDir(o.Name)
		for _, f := range []string{filepath.Join(dir, "token"), filepath.Join(dir, "peer-token")} {
			if err := os.Remove(f); err == nil {
				purged = append(purged, f)
			}
		}
		_ = os.Remove(dir)
		if o.Name != DefaultHubDaemon {
			_ = os.Remove(filepath.Dir(dir)) // <home>/hub/daemons/<name>, when empty
		}
	}
	if o.JSON {
		printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "removed": installed, "plist": path, "purged": purged})
		return nil
	}
	if installed {
		fmt.Printf("hub service uninstalled: %s removed\n", path)
	} else {
		fmt.Println("hub service was not installed")
	}
	for _, f := range purged {
		fmt.Printf("deleted %s\n", f)
	}
	return nil
}

// launchdState picks state and pid out of `launchctl print`.
func launchdState(out string) (state, pid string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "state = "); ok && state == "" {
			state = v
		}
		if v, ok := strings.CutPrefix(line, "pid = "); ok && pid == "" {
			pid = v
		}
	}
	return state, pid
}

func runServiceStatus(ctx context.Context, o *daemonOpts, args []string) error {
	if serviceGOOS != "darwin" {
		return serviceUnsupported()
	}
	hubName(o)
	if !serviceInstalled(o.Name) {
		fmt.Println("hub service: not installed")
	} else {
		out, err := launchctl("print", serviceTarget(o.Name))
		state, pid := launchdState(string(out))
		switch {
		case err != nil:
			fmt.Printf("hub service: installed (%s), not loaded in launchd\n", servicePlistPath(o.Name))
		case pid != "":
			fmt.Printf("hub service: installed (%s), launchd state %s, pid %s\n", servicePlistPath(o.Name), state, pid)
		default:
			fmt.Printf("hub service: installed (%s), launchd state %s\n", servicePlistPath(o.Name), state)
		}
	}
	c, _ := daemon.Dial(o.Name)
	if c == nil {
		fmt.Printf("hub %q is not running\n", o.Name)
		return nil
	}
	st, err := c.Hub(ctx)
	if err != nil {
		return err
	}
	printHubStatus(os.Stdout, st.Hub)
	return nil
}
