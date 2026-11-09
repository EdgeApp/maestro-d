package cli

// hub.go: `maestro-d hub start|stop|status|service`. A hub is a named
// daemon (`hub` unless --daemon names another) that shares this machine's
// devices with clients on the network: it listens on every interface,
// needs a token, treats every TCP caller as remote, and leases its devices
// (see pkg/daemon/leases.go). It has no idle timeout. Hubs link into a
// mesh (pkg/hub): they find each other with mDNS once, at launch, and dial
// every --peer; any hub then serves every hub's devices.

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/hub"
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
	strFlag("name", "The hub's name in the mesh: [a-z0-9-]{1,32} (default: the short host name, lowercased)", []string{"MAESTRO_D_HUB_NAME"},
		func(o *daemonOpts, v string) { o.hub.name = v }),
	strFlag("listen", "Address of the client and peer listener (default :7433, every interface)", []string{"MAESTRO_D_LISTEN"},
		func(o *daemonOpts, v string) { o.HTTPAddr = v }),
	strFlag("advertise", "HOST:PORT peers are told to dial (default: the address each peer reaches this hub at)", nil,
		func(o *daemonOpts, v string) { o.hub.advertise = v }),
	{name: "peer", kind: kindSlice, usage: "A hub to link with, HOST[:PORT] (https:// for TLS); repeatable",
		apply: func(o *daemonOpts, v string) error { o.hub.peers = append(o.hub.peers, v); return nil }},
	boolFlag("no-mdns", "Skip LAN discovery: link only with --peer hubs and the hubs that dial this one", nil,
		func(o *daemonOpts, v bool) { o.hub.noMDNS = v }),
	strFlag("token", "Client bearer token (required, or --token-file)", []string{"MAESTRO_D_TOKEN"},
		func(o *daemonOpts, v string) { o.Token = v }),
	strFlag("token-file", "Read the client token from this file (mode 0600)", nil,
		func(o *daemonOpts, v string) { o.hub.tokenFile = v }),
	strFlag("peer-token", "The mesh's shared secret, sent on every peer link (required, or --peer-token-file)", []string{"MAESTRO_D_PEER_TOKEN"},
		func(o *daemonOpts, v string) { o.hub.peerToken = v }),
	strFlag("peer-token-file", "Read the peer token from this file (mode 0600)", nil,
		func(o *daemonOpts, v string) { o.hub.peerTokenFile = v }),
	strFlag("tls-cert", "Serve HTTPS and WSS with this PEM certificate (with --tls-key)", []string{"MAESTRO_D_TLS_CERT"},
		func(o *daemonOpts, v string) { o.TLSCert = v }),
	strFlag("tls-key", "PEM private key for --tls-cert", []string{"MAESTRO_D_TLS_KEY"},
		func(o *daemonOpts, v string) { o.TLSKey = v }),
	strFlag("peer-ca", "PEM trust root for peers' TLS (default: the system roots)", nil,
		func(o *daemonOpts, v string) { o.hub.peerCA = v }),
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
	intFlag("start-timeout", "Seconds to wait for the detached hub to become ready (default 30)", nil,
		func(o *daemonOpts, v int) { o.StartTimeout = v }),
})

var hubStopFlags = flagSet(one(hubDaemonFlag), daemonSelectFlags[1:], []dflag{
	intFlag("timeout", "Seconds to wait for the hub to exit (default 30)", nil, func(o *daemonOpts, v int) { o.StartTimeout = v }),
})

var hubStatusFlags = flagSet(one(hubDaemonFlag), daemonSelectFlags[1:], targetFlags)

var hubCommand = &cli.Command{
	Name:  "hub",
	Usage: "Share this machine's devices with clients on the network, in a mesh of hubs (hub start|stop|status|service)",
	Subcommands: []*cli.Command{
		{
			Name:      "start",
			Usage:     "Start a hub: a daemon that shares this machine's devices, leases them to clients and links with other hubs",
			ArgsUsage: " ",
			Description: flagsDoc(`A hub listens on every interface (--listen, default :7433) and needs a
client token and a peer token. Every TCP caller is remote, loopback
included: it may not use this machine's paths or runShell (unless
--allow-remote-shell), and sessions import none of this machine's
environment except --pass-env names. Clients lease a device explicitly
before they drive it (maestro-d lease); no command takes a lease on its
own. A lease is a fixed window, at most --lease-max (8h by default; or
DEVICE=DURATION for one device): only maestro-d renew moves its end.

At launch the hub advertises itself with mDNS, browses the LAN for 3 s,
and dials every hub it found plus every --peer; after that it never
searches again. Each hub passes its peers on, so the mesh fills from one
seed. Any hub lists and drives every hub's devices: <id>@<hub>.

  maestro-d hub start --name lab-a --token-file ~/.maestro-d/hub-token --peer-token-file ~/.maestro-d/peer-token
  maestro-d hub start --no-mdns --peer https://hub-b.example.com:7433 --token T --peer-token P --tls-cert hub.pem --tls-key hub-key.pem`, hubStartFlags),
			SkipFlagParsing: true,
			Action:          withArgs(hubStartFlags, runHubStart),
		},
		{
			Name:            "stop",
			Usage:           "Stop the hub: its leases end, its devices are detached and its peer links close",
			ArgsUsage:       " ",
			Description:     flagsDoc("", hubStopFlags),
			SkipFlagParsing: true,
			Action: withArgs(hubStopFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
				hubName(o)
				return stopDaemons(ctx, o, []string{o.Name}, "hub")
			}),
		},
		{
			Name:      "status",
			Usage:     "Show the hub's name and version, and each peer's address, link state, RTT and device count",
			ArgsUsage: " ",
			Description: flagsDoc(`Without --target-hub it asks the local hub over its unix socket.

  maestro-d hub status
  maestro-d hub status --target-hub=hub-b.local --token T --json`, hubStatusFlags),
			SkipFlagParsing: true,
			Action:          withArgs(hubStatusFlags, runHubStatus),
		},
		hubServiceCommand,
	},
}

// hubName defaults the daemon name to hub when --daemon was not given.
func hubName(o *daemonOpts) {
	if !o.explicit["daemon"] {
		o.Name = DefaultHubDaemon
	}
}

// readSecretFile reads a token file that must not be readable by others.
func readSecretFile(flag, path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", daemon.Errorf(daemon.CodeUsage, "--%s: %v", flag, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", daemon.Errorf(daemon.CodeUsage, "--%s %s is readable by others (mode %04o); chmod 600 it", flag, path, st.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", daemon.Errorf(daemon.CodeUsage, "--%s: %v", flag, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// hubToken resolves the client token: --token-file (which must not be
// readable by others), else --token / MAESTRO_D_TOKEN.
func hubToken(o *daemonOpts) (string, error) {
	if o.hub.tokenFile != "" {
		if o.explicit["token"] {
			return "", daemon.Errorf(daemon.CodeUsage, "give --token or --token-file, not both")
		}
		t, err := readSecretFile("token-file", o.hub.tokenFile)
		if err != nil {
			return "", err
		}
		o.Token = t
	}
	if o.Token == "" {
		return "", daemon.Errorf(daemon.CodeUsage, "hub start needs --token or --token-file: a hub's listener is open to the network")
	}
	return o.Token, nil
}

// hubPeerToken resolves the peer token the same way.
func hubPeerToken(o *daemonOpts) (string, error) {
	if o.hub.peerTokenFile != "" {
		if o.explicit["peer-token"] {
			return "", daemon.Errorf(daemon.CodeUsage, "give --peer-token or --peer-token-file, not both")
		}
		t, err := readSecretFile("peer-token-file", o.hub.peerTokenFile)
		if err != nil {
			return "", err
		}
		o.hub.peerToken = t
	}
	if o.hub.peerToken == "" {
		return "", daemon.Errorf(daemon.CodeUsage, "hub start needs --peer-token or --peer-token-file: the secret every peer link of the mesh carries")
	}
	return o.hub.peerToken, nil
}

// checkHubFlags validates hub start's flags and fills the defaults.
func checkHubFlags(o *daemonOpts) (token, peerToken string, err error) {
	hubName(o)
	if o.HTTPAddr == "" {
		o.HTTPAddr = fmt.Sprintf(":%d", hub.DefaultPort)
	}
	if o.hub.name == "" {
		o.hub.name = daemon.DefaultHubName()
	}
	if !hub.ValidName(o.hub.name) {
		return "", "", daemon.Errorf(daemon.CodeUsage, "--name %q: a hub name is 1-32 of a-z, 0-9 and -", o.hub.name)
	}
	if token, err = hubToken(o); err != nil {
		return "", "", err
	}
	if peerToken, err = hubPeerToken(o); err != nil {
		return "", "", err
	}
	if (o.TLSCert == "") != (o.TLSKey == "") {
		return "", "", daemon.Errorf(daemon.CodeUsage, "--tls-cert and --tls-key go together")
	}
	if _, _, err = parseLeaseMax(o.hub.leaseMax); err != nil {
		return "", "", err
	}
	return token, peerToken, nil
}

// hubChildArgs is the command line of the detached hub: every flag but the
// tokens, which travel in its environment so they are in no process
// listing.
func hubChildArgs(o *daemonOpts) []string {
	args := []string{"hub", "start", "--foreground", "--daemon", o.Name, "--name", o.hub.name, "--listen", o.HTTPAddr}
	abs := func(p string) string {
		a, _ := filepath.Abs(p)
		return a
	}
	if o.TLSCert != "" {
		args = append(args, "--tls-cert", abs(o.TLSCert), "--tls-key", abs(o.TLSKey))
	}
	if o.hub.advertise != "" {
		args = append(args, "--advertise", o.hub.advertise)
	}
	for _, p := range o.hub.peers {
		args = append(args, "--peer", p)
	}
	if o.hub.noMDNS {
		args = append(args, "--no-mdns")
	}
	if o.hub.peerCA != "" {
		args = append(args, "--peer-ca", abs(o.hub.peerCA))
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
		case (a == "--token" || a == "--peer-token") && i+1 < len(out):
			out[i+1] = "***"
		case strings.HasPrefix(a, "--token="):
			out[i] = "--token=***"
		case strings.HasPrefix(a, "--peer-token="):
			out[i] = "--peer-token=***"
		}
	}
	return out
}

func runHubStart(ctx context.Context, o *daemonOpts, args []string) error {
	if !o.hub.foreground && serviceInstalled() {
		// The service owns the hub process: start it through launchd.
		hubName(o)
		return kickstartService(ctx, o)
	}
	token, peerToken, err := checkHubFlags(o)
	if err != nil {
		return err
	}
	if o.hub.foreground {
		return serveHub(o, token, peerToken)
	}
	if c, err := daemon.Dial(o.Name); err != nil {
		return err
	} else if c != nil {
		if c.Info.Hub == "" {
			return daemon.Errorf(daemon.CodeUsage, "daemon %q is running and is not a hub; stop it first (maestro-d stop --daemon %s)", o.Name, o.Name)
		}
		return printHub(o, c.Info, "already running")
	}
	timeout := 30 * time.Second
	if o.StartTimeout > 0 {
		timeout = time.Duration(o.StartTimeout) * time.Second
	}
	c, err := daemon.Spawn(ctx, o.Name, daemon.SpawnOptions{
		Args:         hubChildArgs(o),
		Env:          []string{"MAESTRO_D_TOKEN=" + token, "MAESTRO_D_PEER_TOKEN=" + peerToken},
		StartTimeout: timeout,
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

// peerCAPool reads --peer-ca.
func peerCAPool(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "--peer-ca: %v", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, daemon.Errorf(daemon.CodeUsage, "--peer-ca: %s holds no PEM certificate", path)
	}
	return pool, nil
}

// newMesh is the hub's mesh (tests replace the discoverer through it).
var newMesh = func(cfg hub.Config) *hub.Mesh { return hub.New(cfg) }

// serveHub runs the hub in this process until it is stopped.
func serveHub(o *daemonOpts, token, peerToken string) error {
	pool, err := peerCAPool(o.hub.peerCA)
	if err != nil {
		return err
	}
	runDir := daemon.RunDir(o.Name)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
	}
	if err := logger.Init(filepath.Join(runDir, "daemon.log")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot open daemon.log: %v\n", err)
	}
	defer logger.Close()
	mesh := newMesh(hub.Config{
		Name: o.hub.name, Token: token, PeerToken: peerToken, Peers: o.hub.peers, Advertise: o.hub.advertise,
		NoMDNS: o.hub.noMDNS, PeerCA: pool, Version: Version,
		Logf: func(format string, args ...any) { logger.Info(format, args...) },
	})
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
		HubName:          o.hub.name,
		ShareDevices:     o.hub.share,
		LeaseMax:         leaseMax,
		LeaseMaxDevices:  leaseMaxDevices,
		Advertise:        o.hub.advertise,
		AllowRemoteShell: o.hub.remoteShell,
		PassEnv:          o.PassEnv,
		ReportsDir:       daemon.ReportsDir(o.Name),
		StartArgs:        redactArgs(os.Args[1:]),
		Ready:            os.Stdout,
		Mesh:             mesh,
	}, daemonDeps())
	if err := srv.Run(context.Background()); err != nil {
		logger.Error("hub exited: %v", err)
		if de, ok := err.(*daemon.Error); ok {
			return de
		}
		return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
	}
	return nil
}

// hubClient connects hub status to its hub: --target-hub, else the local
// hub's unix socket (never spawning one).
func hubClient(ctx context.Context, o *daemonOpts) (*daemon.Client, error) {
	if o.Target != "" {
		return connectTarget(o)
	}
	hubName(o)
	c, err := daemon.Dial(o.Name)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, daemon.Errorf(daemon.CodeDaemonUnavailable, "hub %q is not running (maestro-d hub start)", o.Name)
	}
	return c, nil
}

func runHubStatus(ctx context.Context, o *daemonOpts, args []string) error {
	c, err := hubClient(ctx, o)
	if err != nil {
		return err
	}
	st, err := c.Hub(ctx)
	if err != nil {
		return err
	}
	if o.JSON {
		printJSON(os.Stdout, st)
		return nil
	}
	printHubStatus(os.Stdout, st.Hub)
	return nil
}

func printHubStatus(w io.Writer, h daemon.HubStatus) {
	mdns := "mDNS at launch"
	if !h.MDNS {
		mdns = "no mDNS"
	}
	fmt.Fprintf(w, "hub %s: maestro-d %s, API %s, listening on %s (%s)\n", h.Name, h.Version, h.APIVersion, h.Listen, mdns)
	if h.Addr != "" {
		fmt.Fprintf(w, "advertised as %s\n", h.Addr)
	}
	if len(h.Peers) == 0 {
		fmt.Fprintln(w, "No peers.")
		return
	}
	peers := append([]daemon.HubPeer(nil), h.Peers...)
	sort.SliceStable(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	fmt.Fprintf(w, "%-20s %-28s %-10s %-9s %-10s %s\n", "PEER", "ADDRESS", "STATE", "DIRECTION", "RTT", "DEVICES")
	for _, p := range peers {
		rtt, devs := "-", "-"
		if p.State == "linked" {
			rtt = fmt.Sprintf("%.1fms", p.RTTMs)
		}
		if p.Devices != nil {
			devs = fmt.Sprint(*p.Devices)
		}
		name, dir := p.Name, p.Direction
		if name == "" {
			name = "?"
		}
		if dir == "" {
			dir = "-"
		}
		line := fmt.Sprintf("%-20s %-28s %-10s %-9s %-10s %s", name, p.Addr, p.State, dir, rtt, devs)
		if p.Error != "" {
			line += "  (" + p.Error + ")"
		}
		fmt.Fprintln(w, line)
	}
}
