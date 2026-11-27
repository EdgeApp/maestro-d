package hubcmd

// hub_service.go: `maestro-d-hub service install|uninstall|status`, the
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

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// ServiceLabel is the launchd label of the hub service for daemon "hub" in
// the default home (~/.maestro-d).
const ServiceLabel = "app.edge.maestro-d.hub"

// Alternate homes get a distinct launchd label even when their basenames match.
func serviceLabel() string {
	label := ServiceLabel
	home := filepath.Clean(daemon.Home())
	if u, err := os.UserHomeDir(); err != nil || home != filepath.Join(u, ".maestro-d") {
		sum := sha256.Sum256([]byte(home))
		label += "." + labelPart(filepath.Base(home)) + "-" + hex.EncodeToString(sum[:4])
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
			if c, _ := daemon.DialHub(); c != nil && c.Ping(2*time.Second) == nil {
				return c, nil
			}
			if time.Now().After(deadline) {
				return nil, daemon.Errorf(daemon.CodeDaemonUnavailable, "hub %q did not start within %v; see %s", name, timeout,
					filepath.Join(daemon.HubRunDir(), "daemon.log"))
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

func servicePlistPath() string {
	return filepath.Join(launchAgentsDir(), serviceLabel()+".plist")
}

// serviceSecretsDir holds the singleton service credentials.
func serviceSecretsDir() string {
	return filepath.Join(daemon.Home(), "hub", "secrets")
}

func serviceTarget() string {
	return fmt.Sprintf("gui/%d/%s", serviceUID(), serviceLabel())
}

// ServiceInstalled reports whether the hub runs as a launchd
// service in this home.
func ServiceInstalled() bool {
	if serviceGOOS != "darwin" {
		return false
	}
	_, err := os.Stat(servicePlistPath())
	return err == nil
}

func warnLegacyService() {
	data, err := os.ReadFile(servicePlistPath())
	if err != nil {
		return
	}
	var agent struct{ ProgramArguments []string }
	if _, err := plist.Unmarshal(data, &agent); err != nil {
		return
	}
	a := agent.ProgramArguments
	if len(a) >= 3 && filepath.Base(a[0]) == "maestro-d" && a[1] == "hub" && a[2] == "start" {
		common.Progress("legacy LaunchAgent runs maestro-d hub start; update it with maestro-d-hub service install")
	}
}

func serviceUnsupported() error {
	return daemon.Errorf(daemon.CodeUsage, "the hub service is a macOS LaunchAgent; on %s run `maestro-d-hub start --foreground` under your supervisor (docs/daemon/hub.md shows a systemd unit)", serviceGOOS)
}

var hubServiceUninstallFlags = common.FlagSet(outputFlags, []dflag{
	common.BoolFlag("purge", "Also delete the tokens install wrote under <home>/hub/secrets", nil,
		func(o *daemonOpts, v bool) { o.hub.purge = v }),
})

var hubServiceStatusFlags = common.FlagSet(outputFlags)

func serviceCommand(withArgs, withConfig actionFactory) *cli.Command {
	return &cli.Command{
		Name:  "service",
		Usage: "Run the hub as a launchd LaunchAgent (macOS): install, uninstall, status",
		Subcommands: []*cli.Command{
			{
				Name:      "install",
				Usage:     "Install and load the hub as a LaunchAgent that starts at login and restarts after a crash",
				ArgsUsage: " ",
				Description: common.FlagHelp(`Takes hub start's flags. The agent runs 'hub start --foreground' with
them; the tokens are written to <home>/hub/secrets (mode 0600) and passed
as --token-file/--peer-token-file, never written into the plist. PATH,
MAESTRO_D_HOME, ANDROID_HOME, ANDROID_SDK_ROOT, DEVELOPER_DIR and
MAESTRO_TEAM_ID are recorded when set. The hub's output goes to its
daemon.log.

  maestro-d-hub service install --name lab-a --token-file ~/.hub-token --peer-token-file ~/.peer-token`, hubStartFlags),
				SkipFlagParsing: true,
				Action:          withConfig(hubStartFlags, runServiceInstall),
			},
			{
				Name:            "uninstall",
				Usage:           "Unload the LaunchAgent (stopping the hub) and delete its plist; --purge also deletes the tokens",
				ArgsUsage:       " ",
				Description:     common.FlagHelp("", hubServiceUninstallFlags),
				SkipFlagParsing: true,
				Action:          withArgs(hubServiceUninstallFlags, runServiceUninstall),
			},
			{
				Name:            "status",
				Usage:           "Print the LaunchAgent's launchd state, then hub status",
				ArgsUsage:       " ",
				Description:     common.FlagHelp("", hubServiceStatusFlags),
				SkipFlagParsing: true,
				Action:          withArgs(hubServiceStatusFlags, runServiceStatus),
			},
		},
	}
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
	logPath := filepath.Join(daemon.HubRunDir(), "daemon.log")
	return plist.MarshalIndent(map[string]any{
		"Label":                serviceLabel(),
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
	if c, _ := daemon.DialHub(); c != nil && !ServiceInstalled() {
		return daemon.Errorf(daemon.CodeUsage, "hub %q is already running (pid %d); stop it first (maestro-d-hub stop)", o.Name, c.Info.PID)
	}
	dir := serviceSecretsDir()
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
	bin, err := daemon.HubExecutable()
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
	if err := os.MkdirAll(daemon.HubRunDir(), 0o700); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	if err := os.MkdirAll(launchAgentsDir(), 0o755); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	if ServiceInstalled() {
		_, _ = launchctl("bootout", serviceTarget())
	}
	path := servicePlistPath()
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
	if out, err := launchctl("kickstart", serviceTarget()); err != nil {
		return daemon.Errorf(daemon.CodeDaemonUnavailable, "launchctl kickstart %s: %v %s", serviceTarget(), err, strings.TrimSpace(string(out)))
	}
	if !o.JSON {
		common.Progress("Started the hub service through launchd (kickstart %s)", serviceTarget())
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
	o.Name = DefaultHubDaemon
	path := servicePlistPath()
	installed := ServiceInstalled()
	if installed {
		if out, err := launchctl("bootout", serviceTarget()); err != nil && !strings.Contains(string(out), "No such process") {
			common.Progress("launchctl bootout %s: %v %s", serviceTarget(), err, strings.TrimSpace(string(out)))
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return daemon.WrapErr(daemon.CodeUsage, err)
		}
	}
	if c, _ := daemon.DialHub(); c != nil {
		daemon.WaitForExit(c.Info, startTimeout(o))
	}
	var purged []string
	if o.hub.purge {
		dir := serviceSecretsDir()
		for _, f := range []string{filepath.Join(dir, "token"), filepath.Join(dir, "peer-token")} {
			if err := os.Remove(f); err == nil {
				purged = append(purged, f)
			}
		}
		_ = os.Remove(dir)

	}
	if o.JSON {
		common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "removed": installed, "plist": path, "purged": purged})
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
	o.Name = DefaultHubDaemon
	if !ServiceInstalled() {
		fmt.Println("hub service: not installed")
	} else {
		out, err := launchctl("print", serviceTarget())
		state, pid := launchdState(string(out))
		switch {
		case err != nil:
			fmt.Printf("hub service: installed (%s), not loaded in launchd\n", servicePlistPath())
		case pid != "":
			fmt.Printf("hub service: installed (%s), launchd state %s, pid %s\n", servicePlistPath(), state, pid)
		default:
			fmt.Printf("hub service: installed (%s), launchd state %s\n", servicePlistPath(), state)
		}
	}
	c, _ := daemon.DialHub()
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
