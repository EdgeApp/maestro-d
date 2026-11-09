package cli

// hub.go: `maestro-d hub start|stop`. A hub is a named daemon (`hub` unless
// --daemon names another) that shares this machine's devices with clients
// on the network: it listens on every interface, needs a token, treats
// every TCP caller as remote, and leases its devices (see
// pkg/daemon/leases.go). It has no idle timeout.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// DefaultHubDaemon is the daemon name a hub runs under.
const DefaultHubDaemon = "hub"

// hubDaemonFlag names the hub's daemon. Unlike --daemon elsewhere it does
// not read MAESTRO_D: a shell set up for one local daemon still starts the
// hub under its own name.
var hubDaemonFlag = strFlag("daemon", "Daemon name the hub runs under (default hub)", nil,
	func(o *daemonOpts, v string) { o.Name = v }, "n")

var hubStartFlags = flagSet(one(hubDaemonFlag), daemonSelectFlags[1:], []dflag{
	strFlag("listen", "Address of the client listener (default :7433, every interface)", []string{"MAESTRO_D_LISTEN"},
		func(o *daemonOpts, v string) { o.HTTPAddr = v }),
	strFlag("token", "Client bearer token (required, or --token-file)", []string{"MAESTRO_D_TOKEN"},
		func(o *daemonOpts, v string) { o.Token = v }),
	strFlag("token-file", "Read the client token from this file (mode 0600)", nil,
		func(o *daemonOpts, v string) { o.hub.tokenFile = v }),
	strFlag("tls-cert", "Serve HTTPS with this PEM certificate (with --tls-key)", []string{"MAESTRO_D_TLS_CERT"},
		func(o *daemonOpts, v string) { o.TLSCert = v }),
	strFlag("tls-key", "PEM private key for --tls-cert", []string{"MAESTRO_D_TLS_KEY"},
		func(o *daemonOpts, v string) { o.TLSKey = v }),
	{name: "share-device", kind: kindSlice, usage: "Share only this device, by id or name (a running emulator's AVD name); repeatable. Default: every device",
		apply: func(o *daemonOpts, v string) error { o.hub.share = append(o.hub.share, v); return nil }},
	{name: "lease-max", kind: kindSlice, value: "8h", usage: "The longest lease window, DURATION (8h, 90m, or seconds) for the whole hub, or DEVICE=DURATION for one device by id or name; repeatable",
		apply: func(o *daemonOpts, v string) error { o.hub.leaseMax = append(o.hub.leaseMax, v); return nil }},
	boolFlag("allow-remote-shell", "Let remote callers run runShell on this machine", nil,
		func(o *daemonOpts, v bool) { o.hub.remoteShell = v }),
	{name: "pass-env", kind: kindSlice, usage: "A variable of this process's environment that sessions may import (repeatable); no other one is imported",
		apply: func(o *daemonOpts, v string) error { o.PassEnv = append(o.PassEnv, v); return nil }},
	boolFlag("foreground", "Run in the foreground instead of detaching (for launchd or another supervisor)", nil,
		func(o *daemonOpts, v bool) { o.hub.foreground = v }),
	intFlag("start-timeout", "Seconds to wait for the detached hub to become ready (default 20)", nil,
		func(o *daemonOpts, v int) { o.StartTimeout = v }),
})

var hubStopFlags = flagSet(one(hubDaemonFlag), daemonSelectFlags[1:], []dflag{
	intFlag("timeout", "Seconds to wait for the hub to exit (default 30)", nil, func(o *daemonOpts, v int) { o.StartTimeout = v }),
})

var hubCommand = &cli.Command{
	Name:  "hub",
	Usage: "Share this machine's devices with clients on the network (hub start|stop)",
	Subcommands: []*cli.Command{
		{
			Name:      "start",
			Usage:     "Start a hub: a daemon that shares this machine's devices and leases them to clients",
			ArgsUsage: " ",
			Description: flagsDoc(`A hub listens on every interface (--listen, default :7433) and needs a
token. Every TCP caller is remote, loopback included: it may not use this
machine's paths or runShell (unless --allow-remote-shell), and sessions
import none of this machine's environment except --pass-env names.
Clients lease a device explicitly before they drive it (maestro-d
lease); no command takes a lease on its own. A lease is a fixed window,
at most --lease-max (8h by default; or DEVICE=DURATION for one device):
only maestro-d renew moves its end.

  maestro-d hub start --token-file ~/.maestro-d/hub-token --share-device "iPhone 16"
  maestro-d hub start --listen :7443 --token T --tls-cert hub.pem --tls-key hub-key.pem`, hubStartFlags),
			SkipFlagParsing: true,
			Action:          withArgs(hubStartFlags, runHubStart),
		},
		{
			Name:            "stop",
			Usage:           "Stop the hub: its leases end and its devices are detached",
			ArgsUsage:       " ",
			Description:     flagsDoc("", hubStopFlags),
			SkipFlagParsing: true,
			Action: withArgs(hubStopFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
				hubName(o)
				return stopDaemons(ctx, o, []string{o.Name}, "hub")
			}),
		},
	},
}

// hubName defaults the daemon name to hub when --daemon was not given.
func hubName(o *daemonOpts) {
	if !o.explicit["daemon"] {
		o.Name = DefaultHubDaemon
	}
}

// hubToken resolves the client token: --token-file (which must not be
// readable by others), else --token / MAESTRO_D_TOKEN.
func hubToken(o *daemonOpts) (string, error) {
	if o.hub.tokenFile != "" {
		if o.explicit["token"] {
			return "", daemon.Errorf(daemon.CodeUsage, "give --token or --token-file, not both")
		}
		st, err := os.Stat(o.hub.tokenFile)
		if err != nil {
			return "", daemon.Errorf(daemon.CodeUsage, "--token-file: %v", err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			return "", daemon.Errorf(daemon.CodeUsage, "--token-file %s is readable by others (mode %04o); chmod 600 it", o.hub.tokenFile, st.Mode().Perm())
		}
		data, err := os.ReadFile(o.hub.tokenFile)
		if err != nil {
			return "", daemon.Errorf(daemon.CodeUsage, "--token-file: %v", err)
		}
		o.Token = strings.TrimSpace(string(data))
	}
	if o.Token == "" {
		return "", daemon.Errorf(daemon.CodeUsage, "hub start needs --token or --token-file: a hub's listener is open to the network")
	}
	return o.Token, nil
}

// hubChildArgs is the command line of the detached hub: every flag but the
// token, which travels in its environment so it is in no process listing.
func hubChildArgs(o *daemonOpts) []string {
	args := []string{"hub", "start", "--foreground", "--daemon", o.Name, "--listen", o.HTTPAddr}
	if o.TLSCert != "" {
		cert, _ := filepath.Abs(o.TLSCert)
		key, _ := filepath.Abs(o.TLSKey)
		args = append(args, "--tls-cert", cert, "--tls-key", key)
	}
	for _, d := range o.hub.share {
		args = append(args, "--share-device", d)
	}
	for _, m := range o.hub.leaseMax {
		args = append(args, "--lease-max", m)
	}
	if o.hub.remoteShell {
		args = append(args, "--allow-remote-shell")
	}
	for _, n := range o.PassEnv {
		args = append(args, "--pass-env", n)
	}
	return args
}

// redactArgs replaces token values in a command line, for daemon.json.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		switch {
		case a == "--token" && i+1 < len(out):
			out[i+1] = "***"
		case strings.HasPrefix(a, "--token="):
			out[i] = "--token=***"
		}
	}
	return out
}

func runHubStart(ctx context.Context, o *daemonOpts, args []string) error {
	hubName(o)
	if o.HTTPAddr == "" {
		o.HTTPAddr = fmt.Sprintf(":%d", DefaultHubPort)
	}
	token, err := hubToken(o)
	if err != nil {
		return err
	}
	if (o.TLSCert == "") != (o.TLSKey == "") {
		return daemon.Errorf(daemon.CodeUsage, "--tls-cert and --tls-key go together")
	}
	if _, _, err := parseLeaseMax(o.hub.leaseMax); err != nil {
		return err
	}
	if o.hub.foreground {
		return serveHub(o, token)
	}
	if c, err := daemon.Dial(o.Name); err != nil {
		return err
	} else if c != nil {
		if c.Info.Hub == "" {
			return daemon.Errorf(daemon.CodeUsage, "daemon %q is running and is not a hub; stop it first (maestro-d stop --daemon %s)", o.Name, o.Name)
		}
		return printHub(o, c.Info, "already running")
	}
	c, err := daemon.Spawn(ctx, o.Name, daemon.SpawnOptions{
		Args:         hubChildArgs(o),
		Env:          []string{"MAESTRO_D_TOKEN=" + token},
		StartTimeout: time.Duration(o.StartTimeout) * time.Second,
		Log: func(format string, args ...any) {
			if o.Verbose {
				progress(format, args...)
			}
		},
	})
	if err != nil {
		return err
	}
	return printHub(o, c.Info, "running")
}

func printHub(o *daemonOpts, info *daemon.Info, state string) error {
	if o.JSON {
		printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "daemon": info})
		return nil
	}
	scheme := "http"
	if info.TLS {
		scheme = "https"
	}
	fmt.Printf("hub %q %s (daemon %q, pid %d, %s://%s)\n", info.Hub, state, info.Name, info.PID, scheme, info.HTTPAddr)
	return nil
}

// serveHub runs the hub in this process until it is stopped.
func serveHub(o *daemonOpts, token string) error {
	runDir := daemon.RunDir(o.Name)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
	}
	if err := logger.Init(filepath.Join(runDir, "daemon.log")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot open daemon.log: %v\n", err)
	}
	defer logger.Close()
	leaseMax, leaseMaxDevices, err := parseLeaseMax(o.hub.leaseMax)
	if err != nil {
		return err
	}
	srv := daemon.New(daemon.Config{
		Name:             o.Name,
		HTTPAddr:         o.HTTPAddr,
		Token:            token,
		TLSCert:          o.TLSCert,
		TLSKey:           o.TLSKey,
		Hub:              true,
		ShareDevices:     o.hub.share,
		LeaseMax:         leaseMax,
		LeaseMaxDevices:  leaseMaxDevices,
		AllowRemoteShell: o.hub.remoteShell,
		PassEnv:          o.PassEnv,
		ReportsDir:       daemon.ReportsDir(o.Name),
		StartArgs:        redactArgs(os.Args[1:]),
		Ready:            os.Stdout,
	}, daemonDeps())
	if err := srv.Run(context.Background()); err != nil {
		logger.Error("hub exited: %v", err)
		return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
	}
	return nil
}
