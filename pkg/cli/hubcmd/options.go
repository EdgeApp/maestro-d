package hubcmd

import (
	"context"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm"
)

type daemonOpts struct {
	Name, DeviceConfig, HTTPAddr, Token, TLSCert, TLSKey string
	JSON, Verbose                                        bool
	StartTimeout                                         int
	PassEnv                                              []string
	explicit                                             map[string]bool
	hub                                                  hubOpts
	deps                                                 func() daemon.Deps
}
type dflag = common.Flag[daemonOpts]

// hubOpts are hub start's switches.
type hubOpts struct {
	name      string
	tokenFile string
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

func parseDaemonArgs(args []string, flags []dflag, stepKeys map[string]string) (*daemonOpts, *common.ParsedArgs, error) {
	o, parsed, err := common.ParseArgs(args, flags, stepKeys)
	if err != nil {
		return nil, nil, err
	}
	o.Name = DefaultHubDaemon
	o.explicit = parsed.Explicit
	return o, parsed, nil
}

type actionFactory func([]dflag, func(context.Context, *daemonOpts, []string) error) cli.ActionFunc

func actions(deps func() daemon.Deps, parse common.ArgParser[daemonOpts]) actionFactory {
	return func(flags []dflag, run func(context.Context, *daemonOpts, []string) error) cli.ActionFunc {
		return common.Action(flags, parse, func(ctx context.Context, o *daemonOpts, args []string) error {
			o.deps = deps
			return run(ctx, o, args)
		}, func(o *daemonOpts) bool { return o.JSON })
	}
}
