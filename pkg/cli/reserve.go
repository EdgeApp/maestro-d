package cli

// reserve.go: `maestro-d reserve` and `maestro-d claim`.
//
// reserve takes a reservation on a hub (POST /v1/reservations): a lease
// whose window starts later. It is recorded in leases.json like a lease,
// so a command on that device from this home after the start sends it;
// before the start the hub answers LEASE_NOT_STARTED. Nothing waits for
// the start. claim takes a lease by its id from wherever it came (a
// reservation made on another machine, a lease id handed over): it
// records it and attaches the device, as lease leaves one.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// parseStartAt reads --start-at: RFC 3339, or a duration (90m, 2h) or
// seconds from now, by this machine's clock.
func parseStartAt(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil {
		d = time.Duration(n) * time.Second
	} else if d, err = time.ParseDuration(v); err != nil {
		return time.Time{}, daemon.Errorf(daemon.CodeUsage, "--start-at %q: want an RFC 3339 time (2026-11-24T02:00:00Z) or a duration from now (90m, 2h)", v)
	}
	if d < 0 {
		return time.Time{}, daemon.Errorf(daemon.CodeUsage, "--start-at %q: the start is in the past", v)
	}
	return now.Add(d), nil
}

var reserveFlags = common.FlagSet(daemonSelectFlags, common.One(deviceFlag), targetFlags, []dflag{
	common.StringFlag("platform", "Select a device of this platform: ios, android, …", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Platform = v }, "p"),
	common.StringFlag("kind", "Select a device of this kind: simulator, device, tv", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Kind = v }),
	common.StringFlag("name", "Select the device with this name (or id)", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Name = v }),
	common.StringFlag("os", "Select an OS version, or a prefix of one (26 matches 26.5)", nil,
		func(o *daemonOpts, v string) { o.lease.sel.OSVersion = v }),
	common.StringFlag("os-min", "Select an OS version at least this", nil, func(o *daemonOpts, v string) { o.lease.sel.OSVersionMin = v }),
	common.StringFlag("os-max", "Select an OS version at most this, at its own precision (17 admits 17.5)", nil,
		func(o *daemonOpts, v string) { o.lease.sel.OSVersionMax = v }),
	common.StringFlag("hub", "Select a device on this hub", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Hub = v }),
	common.StringFlag("start-at", "When the window starts: an RFC 3339 time, or a duration from now (90m, 2h); required", nil,
		func(o *daemonOpts, v string) { o.lease.startAt = v }),
	ttlFlag("The window: a duration (8h, 90m) or seconds (default the hub's, 8h or the device's maximum)"),
	leaseDescriptionFlag,
	common.StringFlag("holder", "Who holds the reservation, as listings show it (default user@host)", nil,
		func(o *daemonOpts, v string) { o.lease.holder = v }),
})

var reserveCommand = &cli.Command{
	Name:      "reserve",
	Usage:     "Reserve a device on a hub for a window that starts later, by id or by selector",
	ArgsUsage: " ",
	Description: common.FlagHelp(`The hub holds the device for the window, and no other lease may reach into
it. The start is at most the hub's lead ahead (24 hours by default) and the
window at most the device's maximum (8 hours by default); this machine's
clock must be within a minute of the hub's. The reservation is recorded in
leases.json: from its start, commands on that device from this home send
it (before then the hub answers LEASE_NOT_STARTED, exit 5), and the first
one attaches the device. From another home or machine, claim it by its id
with `+"`maestro-d claim --lease <id>`"+`. Cancel it with `+"`maestro-d release --lease <id>`"+`.

  maestro-d reserve --device <udid> --start-at 2h --ttl 1h --target-hub=hub.lan
  maestro-d reserve --platform ios --kind device --start-at 2026-11-24T02:00:00Z --ttl 30m --json`, reserveFlags),
	SkipFlagParsing: true,
	Action:          withArgs(reserveFlags, runReserve),
}

func runReserve(ctx context.Context, o *daemonOpts, args []string) error {
	sel := o.lease.sel
	if o.Device != "" && (sel != daemon.LeaseSelector{}) {
		return daemon.Errorf(daemon.CodeUsage, "reserve: give --device or a selector (--platform, --kind, --name, --os, --hub), not both")
	}
	if o.Device == "" && (sel == daemon.LeaseSelector{}) {
		return daemon.Errorf(daemon.CodeUsage, "reserve: give --device ID, or a selector: --platform, --kind, --name, --os, --hub")
	}
	if o.lease.startAt == "" {
		return daemon.Errorf(daemon.CodeUsage, "reserve: --start-at is required (an RFC 3339 time, or a duration from now)")
	}
	now := time.Now()
	start, err := parseStartAt(o.lease.startAt, now)
	if err != nil {
		return err
	}
	c, err := connectDaemon(ctx, o, false)
	if err != nil {
		return err
	}
	if hub, err := isHub(ctx, c, o); err != nil {
		return err
	} else if !hub {
		return daemon.Errorf(daemon.CodeUsage, "%s is not a hub and has no reservations (start one with `maestro-d hub start`)", targetLabel(o))
	}
	req := daemon.ReservationRequest{StartAt: start.UTC().Format(time.RFC3339), ClientTime: time.Now().UTC().Format(time.RFC3339),
		TTLSec: o.lease.ttl, Holder: o.holder(), Description: o.lease.description}
	if o.Device != "" {
		id, _, err := lookupDevice(ctx, c, o.Device)
		if err != nil {
			return err
		}
		req.Device = id
	} else {
		req.Select = &sel
	}
	res, err := c.Reserve(ctx, req)
	if err != nil {
		// The hub asked answered "no route" itself: name it as the user
		// wrote it. A peer's refusal, which the mesh names, passes as is.
		if de, ok := err.(*daemon.Error); ok && de.Details["reason"] == "reservations-unsupported" && o.Target != "" &&
			de.Details["hub"] == c.BaseURL() {
			return daemon.ReservationsUnsupported(targetLabel(o))
		}
		return err
	}
	recordLease(leaseKey(c, o), res.Lease)
	return printLease(o, res.Lease, res.Device)
}

var claimFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags[:1], attachFlags, targetFlags, []dflag{
	common.StringFlag("lease", "The lease to claim, by id; required", nil, func(o *daemonOpts, v string) { o.lease.id = v }),
	{Name: "attach", Kind: common.Bool, Value: "true", Usage: "Attach the device once claimed (default true; --no-attach only records the lease)",
		Apply: func(o *daemonOpts, v string) error {
			o.lease.noAttach = v == "false"
			return nil
		}},
})

var claimCommand = &cli.Command{
	Name:      "claim",
	Usage:     "Take a lease by its id (a reservation once it has started), record it and attach the device",
	ArgsUsage: " ",
	Description: common.FlagHelp(`The lease id is all a client needs: a reservation made on another machine,
or a lease another client handed over. claim records it in leases.json, so
later commands on its device send it, and attaches the device (--no-attach
only records it). A reservation that has not started is LEASE_NOT_STARTED
(exit 5), and nothing is recorded: claim it from its start. Nothing waits
on the hub for that.

  maestro-d claim --lease <lease id> --target-hub=hub.lan
  maestro-d claim --lease <lease id> --team-id "$MAESTRO_TEAM_ID" --target-hub=hub.lan`, claimFlags),
	SkipFlagParsing: true,
	Action:          withArgs(claimFlags, runClaim),
}

func runClaim(ctx context.Context, o *daemonOpts, args []string) error {
	if o.lease.id == "" {
		return daemon.Errorf(daemon.CodeUsage, "claim: --lease ID is required")
	}
	c, err := connectDaemon(ctx, o, false)
	if err != nil {
		return err
	}
	if hub, err := isHub(ctx, c, o); err != nil {
		return err
	} else if !hub {
		return daemon.Errorf(daemon.CodeUsage, "%s is not a hub and has no leases (start one with `maestro-d hub start`)", targetLabel(o))
	}
	res, err := c.Lease(ctx, o.lease.id)
	if err != nil {
		return err
	}
	l := res.Lease
	if l.State == daemon.LeaseScheduled {
		return daemon.Errorf(daemon.CodeLeaseNotStarted, "lease %s on %s starts at %s (in %s); claim it then", l.ID, l.Device, l.StartsAt,
			untilExpiry(l.StartsAt)).WithDetails("lease", l.ID, "startsAt", l.StartsAt)
	}
	key := leaseKey(c, o)
	recordLease(key, l)
	c.SetLease(l.Device, l.ID)
	var info *daemon.DeviceInfo
	if !o.lease.noAttach {
		if info, err = ensureAttached(ctx, c, o, l.Device); err != nil {
			return err
		}
	}
	return printLease(o, l, info)
}
