package hubcmd

// The standalone hub runtime and its local administration commands.

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm"
	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
	"github.com/devicelab-dev/maestro-runner/pkg/hub"
	"github.com/devicelab-dev/maestro-runner/pkg/hubd"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
	"github.com/devicelab-dev/maestro-runner/pkg/version"
)

// DefaultHubDaemon identifies the standalone server in its runtime record.
const DefaultHubDaemon = "hub"

var hubStartFlags = common.FlagSet(outputFlags, []dflag{
	common.StringFlag("name", "The hub's name in the mesh: [a-z0-9-]{1,32} (default: the short host name, lowercased)", []string{"MAESTRO_D_HUB_NAME"},
		func(o *daemonOpts, v string) { o.hub.name = v }),
	common.StringFlag("listen", "Address of the client and peer listener (default :7433, every interface)", []string{"MAESTRO_D_LISTEN"},
		func(o *daemonOpts, v string) { o.HTTPAddr = v }),
	common.StringFlag("advertise", "HOST:PORT peers are told to dial (default: the address each peer reaches this hub at)", nil,
		func(o *daemonOpts, v string) { o.hub.advertise = v }),
	{Name: "peer", Kind: common.Slice, Usage: "A hub to link with, HOST[:PORT] (https:// for TLS); repeatable",
		Apply: func(o *daemonOpts, v string) error { o.hub.peers = append(o.hub.peers, v); return nil }},
	common.BoolFlag("no-mdns", "Skip LAN discovery: link only with --peer hubs and the hubs that dial this one", nil,
		func(o *daemonOpts, v bool) { o.hub.noMDNS = v }),
	common.StringFlag("token", "Client bearer token (required, or --token-file)", []string{"MAESTRO_D_TOKEN"},
		func(o *daemonOpts, v string) { o.Token = v }),
	common.StringFlag("token-file", "Read the client token from this file (mode 0600)", nil,
		func(o *daemonOpts, v string) { o.hub.tokenFile = v }),
	common.StringFlag("peer-token", "The mesh's shared secret, sent on every peer link (required, or --peer-token-file)", []string{"MAESTRO_D_PEER_TOKEN"},
		func(o *daemonOpts, v string) { o.hub.peerToken = v }),
	common.StringFlag("peer-token-file", "Read the peer token from this file (mode 0600)", nil,
		func(o *daemonOpts, v string) { o.hub.peerTokenFile = v }),
	common.StringFlag("tls-cert", "Serve HTTPS and WSS with this PEM certificate (with --tls-key)", []string{"MAESTRO_D_TLS_CERT"},
		func(o *daemonOpts, v string) { o.TLSCert = v }),
	common.StringFlag("tls-key", "PEM private key for --tls-cert", []string{"MAESTRO_D_TLS_KEY"},
		func(o *daemonOpts, v string) { o.TLSKey = v }),
	common.StringFlag("peer-ca", "PEM trust root for peers' TLS (default: the system roots)", nil,
		func(o *daemonOpts, v string) { o.hub.peerCA = v }),
	{Name: "lease-max", Kind: common.Slice, Value: "8h", Usage: "The longest lease window, DURATION (8h, 90m, or seconds) for the whole hub, or DEVICE=DURATION for one device by id or name; repeatable",
		Apply: func(o *daemonOpts, v string) error { o.hub.leaseMax = append(o.hub.leaseMax, v); return nil }},
	common.StringFlag("reservation-lead", "How far ahead a reservation may start, DURATION (24h, 90m, or seconds); default 24h", nil,
		func(o *daemonOpts, v string) { o.hub.reservationLead = v }),
	common.BoolFlag("allow-remote-shell", "Let remote callers run runShell on this machine", nil,
		func(o *daemonOpts, v bool) { o.hub.remoteShell = v }),
	common.StringFlag("config", "The device config: config.json's hubDevices, each with an id, an optional description and optional trafficShaping calls (default <home>/config.json)", []string{"MAESTRO_D_CONFIG"},
		func(o *daemonOpts, v string) { o.DeviceConfig = v }),
	common.BoolFlag("devicefarm", "Launch AWS Device Farm devices for this mesh's cloud leases, with this hub's AWS credentials", nil,
		func(o *daemonOpts, v bool) { o.hub.devicefarm = v }),
	common.StringFlag("devicefarm-credentials-file", "With --devicefarm: an AWS shared credentials file (a regular file, mode 0600 or stricter)", nil,
		func(o *daemonOpts, v string) { o.hub.dfOpts.CredentialsFile = v }),
	common.StringFlag("devicefarm-profile", "With --devicefarm: the AWS profile to use", nil,
		func(o *daemonOpts, v string) { o.hub.dfOpts.Profile = v }),
	common.StringFlag("devicefarm-project", "With --devicefarm: the Device Farm project, by name or ARN (default maestro-d)", nil,
		func(o *daemonOpts, v string) { o.hub.dfOpts.Project = v }),
	common.IntFlag("cloud-max-devices", "With --devicefarm: how many cloud devices the hub holds at once, 0-20 (default 2, Device Farm's concurrency)", nil,
		func(o *daemonOpts, v int) { o.hub.cloudMax, o.hub.cloudMaxSet = v, true }),
	{Name: "pass-env", Kind: common.Slice, Usage: "A variable of this process's environment that sessions may import (repeatable); no other one is imported",
		Apply: func(o *daemonOpts, v string) error { o.PassEnv = append(o.PassEnv, v); return nil }},
	common.BoolFlag("foreground", "Run in the foreground instead of detaching (for launchd or another supervisor)", nil,
		func(o *daemonOpts, v bool) { o.hub.foreground = v }),
	common.IntFlag("start-timeout", "Seconds to wait for the detached hub to become ready (default 30)", nil,
		func(o *daemonOpts, v int) { o.StartTimeout = v }),
})

var hubStopFlags = common.FlagSet(outputFlags, []dflag{
	common.IntFlag("timeout", "Seconds to wait for the hub to exit (default 30)", nil, func(o *daemonOpts, v int) { o.StartTimeout = v }),
})

var hubStatusFlags = outputFlags

func Commands(deps func() daemon.Deps) []*cli.Command {
	withArgs := actions(deps, parseDaemonArgs)
	withConfig := actions(deps, common.ConfigParser(parseDaemonArgs, deps))
	return []*cli.Command{
		{
			Name:      "start",
			Usage:     "Start a hub: a daemon that shares this machine's devices, leases them to clients and links with other hubs",
			ArgsUsage: " ",
			Description: common.FlagHelp(`A hub listens on every interface (--listen, default :7433) and needs a
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

  maestro-d-hub start --name lab-a --token-file ~/.maestro-d/hub-token --peer-token-file ~/.maestro-d/peer-token
  maestro-d-hub start --no-mdns --peer https://hub-b.example.com:7433 --token T --peer-token P --tls-cert hub.pem --tls-key hub-key.pem`, hubStartFlags),
			SkipFlagParsing: true,
			Action:          withConfig(hubStartFlags, runHubStart),
		},
		{
			Name:            "stop",
			Usage:           "Stop the hub: its leases end, its devices are detached and its peer links close",
			ArgsUsage:       " ",
			Description:     common.FlagHelp("", hubStopFlags),
			SkipFlagParsing: true,
			Action:          withArgs(hubStopFlags, runHubStop),
		},
		{
			Name:            "status",
			Usage:           "Show the hub's name and version, and each peer's address, link state, RTT and device count",
			ArgsUsage:       " ",
			Description:     common.FlagHelp("Show this home's hub over its unix socket.", hubStatusFlags),
			SkipFlagParsing: true,
			Action:          withArgs(hubStatusFlags, runHubStatus),
		},
		devicesCommand(withArgs),
		serviceCommand(withArgs, withConfig),
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
	o.Name = DefaultHubDaemon
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
	if _, err = parseReservationLead(o.hub.reservationLead); err != nil {
		return "", "", err
	}
	if err = checkCloudFlags(o); err != nil {
		return "", "", err
	}
	if _, err = configShare(o); err != nil {
		return "", "", err
	}
	return token, peerToken, nil
}

// checkCloudFlags checks the Device Farm flags: they need --devicefarm,
// the cap is 0-20, and a credentials file must be a regular file no one
// else can read.
func checkCloudFlags(o *daemonOpts) error {
	df := o.hub.dfOpts
	if !o.hub.devicefarm && (df.CredentialsFile != "" || df.Profile != "" || df.Project != "" || o.hub.cloudMaxSet) {
		return daemon.Errorf(daemon.CodeUsage, "--devicefarm-credentials-file, --devicefarm-profile, --devicefarm-project and --cloud-max-devices need --devicefarm")
	}
	if !o.hub.cloudMaxSet {
		o.hub.cloudMax = defaultCloudMaxDevices
	}
	if o.hub.cloudMax < 0 || o.hub.cloudMax > 20 {
		return daemon.Errorf(daemon.CodeUsage, "--cloud-max-devices %d: give 0 to 20", o.hub.cloudMax)
	}
	if df.CredentialsFile == "" {
		return nil
	}
	abs, err := filepath.Abs(df.CredentialsFile)
	if err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	o.hub.dfOpts.CredentialsFile = abs
	fi, err := os.Stat(abs)
	if err != nil {
		return daemon.Errorf(daemon.CodeUsage, "--devicefarm-credentials-file: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return daemon.Errorf(daemon.CodeUsage, "--devicefarm-credentials-file %s is not a regular file", abs)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return daemon.Errorf(daemon.CodeUsage, "--devicefarm-credentials-file %s has mode %04o; make it 0600 (chmod 600) so no one else can read it", abs, perm)
	}
	return nil
}

// configShare reads the device config a hub starts with and returns the
// devices its hubDevices lists, which the hub shares. A config.json that
// cannot be used stops the start: it decides what the hub shares.
func configShare(o *daemonOpts) ([]string, error) {
	if o.DeviceConfig != "" {
		path, err := netshape.AbsoluteConfigPath(o.DeviceConfig)
		if err != nil {
			return nil, daemon.WrapErr(daemon.CodeUsage, err)
		}
		o.DeviceConfig = path
	}
	if err := common.CheckConfigPath(o.DeviceConfig, o.explicit); err != nil {
		return nil, err
	}
	cfg, err := netshape.LoadConfig(daemon.DeviceConfigPath(o.DeviceConfig))
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "%v", err)
	}
	return cfg.Shared, nil
}

// hubChildArgs is the command line of the detached hub: every flag but the
// tokens, which travel in its environment so they are in no process
// listing.
func hubChildArgs(o *daemonOpts) []string {
	args := []string{"start", "--foreground", "--name", o.hub.name, "--listen", o.HTTPAddr}
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
	for _, m := range o.hub.leaseMax {
		args = append(args, "--lease-max", m)
	}
	if o.hub.reservationLead != "" {
		args = append(args, "--reservation-lead", o.hub.reservationLead)
	}
	if o.hub.remoteShell {
		args = append(args, "--allow-remote-shell")
	}
	if o.hub.devicefarm {
		args = append(args, "--devicefarm")
		if f := o.hub.dfOpts.CredentialsFile; f != "" {
			args = append(args, "--devicefarm-credentials-file", f)
		}
		if p := o.hub.dfOpts.Profile; p != "" {
			args = append(args, "--devicefarm-profile", p)
		}
		if p := o.hub.dfOpts.Project; p != "" {
			args = append(args, "--devicefarm-project", p)
		}
		if o.hub.cloudMaxSet {
			args = append(args, "--cloud-max-devices", strconv.Itoa(o.hub.cloudMax))
		}
	}
	for _, n := range o.PassEnv {
		args = append(args, "--pass-env", n)
	}
	if o.DeviceConfig != "" {
		args = append(args, "--config", o.DeviceConfig)
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

func runHubStart(ctx context.Context, o *daemonOpts, args []string) (startErr error) {
	if o.hub.foreground {
		defer func() {
			if e, ok := startErr.(*daemon.Error); ok {
				fmt.Fprintf(os.Stdout, "FAILED pid=%d code=%s %s\n", os.Getpid(), e.Code, e.Message)
			}
		}()
	}
	o.Name = DefaultHubDaemon
	// An initial probe catches a starting hub before its ready record exists.
	probe, err := fsage.TryHubLock(daemon.Home())
	if errors.Is(err, fsage.ErrLocked) {
		e := daemon.Errorf(daemon.CodeHubRunning, "a hub is already running in %s", daemon.Home())
		return e
	}
	if err != nil {
		return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
	}
	fsage.UnlockFile(probe)
	warnLegacyService()

	if !o.hub.foreground && ServiceInstalled() {
		// The service owns the hub process: start it through launchd.
		return kickstartService(ctx, o)
	}
	token, peerToken, err := checkHubFlags(o)
	if err != nil {
		return err
	}
	if o.hub.foreground {
		return serveHub(o, token, peerToken)
	}
	timeout := 30 * time.Second
	if o.StartTimeout > 0 {
		timeout = time.Duration(o.StartTimeout) * time.Second
	}
	c, err := daemon.Spawn(ctx, o.Name, daemon.SpawnOptions{
		Hub: true, Args: hubChildArgs(o),
		Env:          []string{"MAESTRO_D_TOKEN=" + token, "MAESTRO_D_PEER_TOKEN=" + peerToken},
		StartTimeout: timeout,
		Log: func(format string, args ...any) {
			if o.Verbose {
				common.Progress(format, args...)
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
		common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "daemon": info})
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
	o.DeviceConfig, err = netshape.AbsoluteConfigPath(daemon.DeviceConfigPath(o.DeviceConfig))
	if err != nil {
		return err
	}
	runDir := daemon.HubRunDir()
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
	}
	if err := logger.Init(filepath.Join(runDir, "daemon.log")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot open daemon.log: %v\n", err)
	}
	defer logger.Close()
	if dir := filepath.Join(daemon.Home(), "hub", "daemons"); isDirectory(dir) {
		logger.Warn("legacy per-daemon hub state at %s is ignored", dir)
	}
	deps, err := hubDeps(o)
	if err != nil {
		return err
	}
	lead, err := parseReservationLead(o.hub.reservationLead)
	if err != nil {
		return err
	}
	policy := &hubd.Server{}
	mesh := newMesh(hub.Config{
		SelectorJournal: policy,
		Name:            o.hub.name, Token: token, PeerToken: peerToken, Peers: o.hub.peers, Advertise: o.hub.advertise,
		NoMDNS: o.hub.noMDNS, PeerCA: pool, Version: version.Version, Cloud: providerNames(deps), ReservationLead: lead,
		Logf: func(format string, args ...any) { logger.Info(format, args...) },
	})
	leaseMax, leaseMaxDevices, err := parseLeaseMax(o.hub.leaseMax)
	if err != nil {
		return err
	}
	srv := daemon.New(daemon.Config{
		HubProcess:       true,
		Name:             o.Name,
		HTTPAddr:         o.HTTPAddr,
		Token:            token,
		TLSCert:          o.TLSCert,
		TLSKey:           o.TLSKey,
		Hub:              policy,
		HubName:          o.hub.name,
		LeaseMax:         leaseMax,
		LeaseMaxDevices:  leaseMaxDevices,
		ReservationLead:  lead,
		Advertise:        o.hub.advertise,
		AllowRemoteShell: o.hub.remoteShell,
		PassEnv:          o.PassEnv,

		DeviceConfig:    o.DeviceConfig,
		StartArgs:       redactArgs(os.Args[1:]),
		Ready:           os.Stdout,
		Mesh:            mesh,
		CloudMaxDevices: o.hub.cloudMax,
	}, deps)
	if err := srv.Run(context.Background()); err != nil {
		logger.Error("hub exited: %v", err)
		if de, ok := err.(*daemon.Error); ok {
			return de
		}
		return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
	}
	return nil
}

// hubDeps are a hub's dependencies: a plain daemon's, with a cloud
// provider only when the hub was started with its credentials, so a hub
// never spends its operator's ambient AWS credentials by accident.
func hubDeps(o *daemonOpts) (daemon.Deps, error) {
	deps := daemon.Deps{}
	if o.deps != nil {
		deps = o.deps()
	}
	deps.Providers = nil
	if !o.hub.devicefarm {
		return deps, nil
	}
	p, err := hubDeviceFarm(o.hub.dfOpts)
	if err != nil {
		return deps, err
	}
	deps.Providers = map[string]func() (cloudprov.RemoteProvider, error){"devicefarm": func() (cloudprov.RemoteProvider, error) { return p, nil }}
	return deps, nil
}

// hubDeviceFarm is a hub's Device Farm provider, checked at start: one
// GetAccountSettings call. A failure stops the hub with the error, redacted.
func hubDeviceFarm(opts devicefarm.Options) (*devicefarm.Provider, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := devicefarm.NewProvider(ctx, opts)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "--devicefarm: %s", devicefarm.Redact(err.Error()))
	}
	acct, err := p.Account(ctx)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "--devicefarm: %s", p.Redact(err.Error()))
	}
	left := "?"
	if tm, ok := acct["trialMinutes"].(map[string]any); ok {
		left = fmt.Sprint(tm["remaining"])
	}
	logger.Info("cloud: devicefarm ready (trial minutes left %s)", left)
	return p, nil
}

// defaultCloudMaxDevices is how many cloud devices a hub holds at once:
// Device Farm's default concurrency per account.
const defaultCloudMaxDevices = 2

func isDirectory(dir string) bool { st, err := os.Stat(dir); return err == nil && st.IsDir() }

// providerNames lists deps' cloud providers, sorted, for the hub's hello.
func providerNames(deps daemon.Deps) []string {
	var out []string
	for n := range deps.Providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// hubClient reads the local hub record without starting a server.
func hubClient(ctx context.Context, o *daemonOpts) (*daemon.Client, error) {
	c, err := daemon.DialHub()
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, daemon.Errorf(daemon.CodeDaemonUnavailable, "hub %q is not running (maestro-d-hub start)", o.Name)
	}
	return c, nil
}

func runHubStop(ctx context.Context, o *daemonOpts, _ []string) error {
	c, err := hubClient(ctx, o)
	if err != nil {
		return err
	}
	if _, err = c.Hub(ctx); err != nil {
		return err
	}
	if err = c.Shutdown(ctx); err != nil {
		return err
	}
	if !daemon.WaitForExit(c.Info, startTimeout(o)) {
		return daemon.Errorf(daemon.CodeDaemonUnavailable, "hub pid %d did not exit", c.Info.PID)
	}
	if o.JSON {
		common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "stopped": []string{DefaultHubDaemon}})
	} else {
		fmt.Println("hub stopped")
	}
	return nil
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
		common.PrintJSON(os.Stdout, st)
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
	if h.ReservationLeadSec > 0 {
		fmt.Fprintf(w, "reservations start up to %v ahead\n", time.Duration(h.ReservationLeadSec)*time.Second)
	}
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
