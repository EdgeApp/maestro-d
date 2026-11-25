package hubcmd

import (
	"context"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm"
)

type daemonOpts struct {
	Name, DeviceConfig, NetworkConfig, HTTPAddr, Token, TLSCert, TLSKey, Target, CAFile string
	JSON, Verbose                                                                       bool
	StartTimeout                                                                        int
	PassEnv                                                                             []string
	explicit                                                                            map[string]bool
	hub                                                                                 hubOpts
	deps                                                                                func() daemon.Deps
}
type dflag = common.Flag[daemonOpts]

// hubOpts are hub start's switches.
type hubOpts struct {
	name      string
	tokenFile string
	share     []string
	shareTV   []string
	// configShare is the hubDevices of the config the hub starts with.
	configShare []string
	// leaseMax holds each --lease-max [DEVICE=]DURATION as given.
	leaseMax []string
	// reservationLead is --reservation-lead as given ("": the default).
	reservationLead string
	remoteShell     bool
	foreground      bool
	advertise       string
	peers           []string
	noMDNS          bool
	peerToken       string
	peerTokenFile   string
	peerCA          string
	// purge: hub service uninstall --purge.
	purge bool
	// devicefarm: the hub launches AWS Device Farm devices for leases,
	// with dfOpts' credentials and project, at most cloudMax at once.
	devicefarm  bool
	dfOpts      devicefarm.Options
	cloudMax    int
	cloudMaxSet bool
}

var outputFlags = []dflag{
	common.BoolFlag("json", "Print the full JSON result on stdout", nil, func(o *daemonOpts, v bool) { o.JSON = v }),
	common.BoolFlag("verbose", "Print progress to stderr", []string{"MAESTRO_VERBOSE"}, func(o *daemonOpts, v bool) { o.Verbose = v }),
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

func parseDaemonArgs(args []string, flags []dflag, stepKeys map[string]string) (*daemonOpts, *common.ParsedArgs, error) {
	o, parsed, err := common.ParseArgs(args, flags, stepKeys)
	if err != nil {
		return nil, nil, err
	}
	o.Name = daemon.ResolveName(o.Name)
	o.explicit = parsed.Explicit
	return o, parsed, nil
}

type actionFactory func([]dflag, func(context.Context, *daemonOpts, []string) error) cli.ActionFunc

func actions(deps func() daemon.Deps) actionFactory {
	return func(flags []dflag, run func(context.Context, *daemonOpts, []string) error) cli.ActionFunc {
		return common.Action(flags, parseDaemonArgs, func(ctx context.Context, o *daemonOpts, args []string) error {
			o.deps = deps
			return run(ctx, o, args)
		}, func(o *daemonOpts) bool { return o.JSON })
	}
}
