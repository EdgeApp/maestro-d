package cli

// lease.go: `maestro-d lease`, `renew` and `release`, and
// <home>/leases.json.
//
// On a hub every route that drives a device needs that device's lease
// (X-Maestro-Lease). The CLI keeps the leases it holds in leases.json under
// its own daemon.Home(), keyed by the hub's address and the device id as
// the hub returns it, and sends the matching one with every request. It
// never takes a lease on its own: a command that names a device with no
// lease on file goes without one, and the hub answers LEASE_REQUIRED,
// naming `maestro-d lease`. A lease is a fixed window; only `renew` moves
// its end. An entry the hub reports expired is dropped. Against a daemon
// that is not a hub none of this happens, and `release` does nothing and
// succeeds.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// leaseRecord is one entry of leases.json.
type leaseRecord struct {
	// Hub is the hub's address (its base URL, or unix:<daemon> for a hub
	// reached over its unix socket).
	Hub    string `json:"hub"`
	Device string `json:"device"`
	ID     string `json:"id"`
	Holder string `json:"holder,omitempty"`
	// ExpiresAt is the end of the window as the hub last reported it
	// (moved only by an explicit renew).
	ExpiresAt string `json:"expiresAt,omitempty"`
	TTLSec    int    `json:"ttlSec,omitempty"`
}

type leaseFile struct {
	Leases []leaseRecord `json:"leases"`
}

// leaseStaleAfter drops entries whose recorded expiry is this far past: a
// lease nobody used for that long is gone on any hub.
const leaseStaleAfter = 7 * 24 * time.Hour

// leasesMu serialises this process's read-modify-write of leases.json;
// an advisory lock on leases.json.lock serialises it with the other
// processes that share the home (parallel shards leasing devices of one
// hub), so no process saves over a lease another just recorded.
var leasesMu sync.Mutex

func leasesPath() string { return filepath.Join(daemon.Home(), "leases.json") }

func loadLeases() []leaseRecord {
	data, err := os.ReadFile(leasesPath())
	if err != nil {
		return nil
	}
	var f leaseFile
	if err := json.Unmarshal(data, &f); err != nil {
		progress("warning: ignoring unreadable %s: %v", leasesPath(), err)
		return nil
	}
	cutoff := time.Now().Add(-leaseStaleAfter)
	kept := f.Leases[:0]
	for _, r := range f.Leases {
		if t, err := time.Parse(time.RFC3339, r.ExpiresAt); err == nil && t.Before(cutoff) {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

func saveLeases(recs []leaseRecord) error {
	if err := os.MkdirAll(daemon.Home(), 0o700); err != nil {
		return err
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Hub != recs[j].Hub {
			return recs[i].Hub < recs[j].Hub
		}
		return recs[i].Device < recs[j].Device
	})
	if recs == nil {
		recs = []leaseRecord{}
	}
	data, err := json.MarshalIndent(leaseFile{Leases: recs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", leasesPath(), os.Getpid())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, leasesPath())
}

// updateLeases applies fn to the file's entries and writes them back,
// holding leases.json.lock from the read to the rename.
func updateLeases(fn func([]leaseRecord) []leaseRecord) {
	leasesMu.Lock()
	defer leasesMu.Unlock()
	if err := os.MkdirAll(daemon.Home(), 0o700); err != nil {
		progress("warning: %s: %v", leasesPath(), err)
		return
	}
	if lock, err := lockFile(leasesPath() + ".lock"); err != nil {
		progress("warning: lock %s: %v (updating it unlocked)", leasesPath(), err)
	} else {
		defer unlockFile(lock)
	}
	if err := saveLeases(fn(loadLeases())); err != nil {
		progress("warning: %s: %v", leasesPath(), err)
	}
}

// leasesFor returns the entries for one hub.
func leasesFor(hub string) []leaseRecord {
	leasesMu.Lock()
	defer leasesMu.Unlock()
	var out []leaseRecord
	for _, r := range loadLeases() {
		if r.Hub == hub {
			out = append(out, r)
		}
	}
	return out
}

func findLease(hub, device string) *leaseRecord {
	for _, r := range leasesFor(hub) {
		if r.Device == device {
			return &r
		}
	}
	return nil
}

func recordLease(hub string, l daemon.Lease) {
	updateLeases(func(recs []leaseRecord) []leaseRecord {
		out := recs[:0]
		for _, r := range recs {
			if r.Hub != hub || r.Device != l.Device {
				out = append(out, r)
			}
		}
		return append(out, leaseRecord{Hub: hub, Device: l.Device, ID: l.ID, Holder: l.Holder, ExpiresAt: l.ExpiresAt, TTLSec: l.TTLSec})
	})
}

// dropLease removes the entry for lease id on one hub.
func dropLease(hub, id string) {
	updateLeases(func(recs []leaseRecord) []leaseRecord {
		out := recs[:0]
		for _, r := range recs {
			if r.Hub != hub || r.ID != id {
				out = append(out, r)
			}
		}
		return out
	})
}

// leaseKey is the hub's address in leases.json.
func leaseKey(c *daemon.Client, o *daemonOpts) string {
	if o.Target != "" {
		return c.BaseURL()
	}
	return "unix:" + o.Name
}

// wireLeases makes c send the leases on file for its hub, and drop the
// ones the hub reports expired.
func wireLeases(c *daemon.Client, o *daemonOpts) {
	key := leaseKey(c, o)
	for _, r := range leasesFor(key) {
		c.SetLease(r.Device, r.ID)
	}
	c.OnLeaseExpired = func(device, lease string) {
		dropLease(key, lease)
		if !o.JSON {
			progress("Lease %s on %s has expired; take a new one with `maestro-d lease --device %s`", lease, device, device)
		}
	}
}

// isHub reports whether c talks to a hub, asking it (once) when neither
// its daemon.json nor a lease on file tells.
func isHub(ctx context.Context, c *daemon.Client, o *daemonOpts) (bool, error) {
	if c.Info == nil && len(leasesFor(leaseKey(c, o))) > 0 {
		return true, nil
	}
	if c.Info == nil {
		st, err := c.Status(ctx)
		if err != nil {
			return false, err
		}
		c.Info = &st.Daemon
	}
	return c.Info.Hub != "", nil
}

// defaultHolder names this client in a lease: user@host.
func defaultHolder() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	switch {
	case name != "" && host != "":
		return name + "@" + host
	case host != "":
		return host
	}
	return "maestro-d"
}

func (o *daemonOpts) holder() string {
	if o.lease.holder != "" {
		return o.lease.holder
	}
	return defaultHolder()
}

// lookupDevice normalizes a device id through GET /v1/devices/{id}: the id
// the daemon returns, with its record; an unlisted id (DEVICE_NOT_FOUND)
// is kept as given, with no record.
func lookupDevice(ctx context.Context, c *daemon.Client, id string) (string, *daemon.DeviceInfo, error) {
	info, err := c.Device(ctx, id)
	if err != nil {
		if de, ok := err.(*daemon.Error); ok && de.Code == daemon.CodeDeviceNotFound {
			return id, nil, nil
		}
		return id, nil, err
	}
	if info.ID != "" {
		id = info.ID
	}
	return id, info, nil
}

// heldLease makes c send the lease on file for device id, if this client
// holds one; it never takes a lease (with none, the hub answers
// LEASE_REQUIRED). A lease on file that has ended is still sent, so the
// hub answers LEASE_EXPIRED and the entry is dropped (wireLeases). It
// returns the normalized id and the device's record (nil when the daemon
// does not list it).
func heldLease(ctx context.Context, c *daemon.Client, o *daemonOpts, id string) (string, *daemon.DeviceInfo, error) {
	id, info, err := lookupDevice(ctx, c, id)
	if err != nil {
		return id, nil, err
	}
	if rec := findLease(leaseKey(c, o), id); rec != nil {
		c.SetLease(id, rec.ID)
	}
	return id, info, nil
}

func isCode(err error, code daemon.Code) bool {
	de, ok := err.(*daemon.Error)
	return ok && de.Code == code
}

// ---------------------------------------------------------------------------
// lease / release

var leaseFlags = flagSet(daemonSelectFlags, daemonSpawnFlags[:1], one(deviceFlag), attachFlags, targetFlags, []dflag{
	strFlag("kind", "Select a device of this kind: simulator, emulator, device", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Kind = v }),
	strFlag("name", "Select the device with this name (or id)", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Name = v }),
	strFlag("os", "Select an OS version, or a prefix of one (26 matches 26.5)", nil,
		func(o *daemonOpts, v string) { o.lease.sel.OSVersion = v }),
	strFlag("hub", "Select a device on this hub", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Hub = v }),
	boolFlag("boot", "Let the selector pick a shut-down simulator or AVD, which the hub boots for the lease", nil,
		func(o *daemonOpts, v bool) { o.lease.sel.Boot = v }),
	ttlFlag("The lease's window: a duration (8h, 90m) or seconds (default the hub's, 8h or the device's maximum); using the lease never extends it"),
	strFlag("holder", "Who holds the lease, as listings show it (default user@host)", nil,
		func(o *daemonOpts, v string) { o.lease.holder = v }),
	{name: "attach", kind: kindBool, value: "true", usage: "Attach the device once leased (default true; --no-attach only reserves it)",
		apply: func(o *daemonOpts, v string) error {
			o.lease.noAttach = v == "false"
			return nil
		}},
})

var leaseCommand = &cli.Command{
	Name:      "lease",
	Usage:     "Lease a device on a hub, by id or by selector, and attach it",
	ArgsUsage: " ",
	Description: flagsDoc(`A hub lends each device to one client at a time, for a fixed window: --ttl
(default 8 hours, or the device's maximum when that is shorter; the hub
refuses a window over the maximum). The lease is recorded in leases.json
and sent with every later command; using it never extends it. Only
`+"`maestro-d renew`"+` does. At the end of the window the hub ends the lease and
detaches the device (a running step is interrupted). No command takes a
lease on its own: without one, the hub answers LEASE_REQUIRED. Given
--device while this client already holds a live lease on it, lease prints
that one; --json prints the lease id for REST.

  maestro-d lease --device <udid> --target-hub=hub.lan
  maestro-d lease --platform ios --kind simulator --ttl 2h --target-hub=hub.lan --json`, leaseFlags),
	SkipFlagParsing: true,
	Action:          withArgs(leaseFlags, runLease),
}

func runLease(ctx context.Context, o *daemonOpts, args []string) error {
	o.lease.sel.Platform = o.Attach.Platform
	if o.Device != "" && (o.lease.sel != daemon.LeaseSelector{Platform: o.Attach.Platform}) {
		return daemon.Errorf(daemon.CodeUsage, "lease: give --device or a selector (--kind, --name, --os, --hub, --boot), not both")
	}
	if o.Device == "" && (o.lease.sel == daemon.LeaseSelector{}) {
		return daemon.Errorf(daemon.CodeUsage, "lease: give --device ID, or a selector: --platform, --kind, --name, --os, --hub")
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
	key := leaseKey(c, o)
	req := daemon.LeaseRequest{TTLSec: o.lease.ttl, Holder: o.holder()}
	if o.Device != "" {
		id, _, err := lookupDevice(ctx, c, o.Device)
		if err != nil {
			return err
		}
		if rec := findLease(key, id); rec != nil {
			if res, err := c.Lease(ctx, rec.ID); err == nil {
				recordLease(key, res.Lease)
				return printLease(o, res.Lease, nil)
			} else if !isCode(err, daemon.CodeLeaseExpired) {
				return err
			}
			dropLease(key, rec.ID)
		}
		req.Device = id
	} else {
		sel := o.lease.sel
		req.Select = &sel
	}
	res, err := c.AcquireLease(ctx, req)
	if err != nil {
		return err
	}
	l := res.Lease
	recordLease(key, l)
	c.SetLease(l.Device, l.ID)
	info := res.Device
	if !o.lease.noAttach {
		if info, err = ensureAttached(ctx, c, o, l.Device); err != nil {
			if _, rerr := c.ReleaseLease(ctx, l.ID); rerr == nil {
				dropLease(key, l.ID)
			}
			return err
		}
	}
	return printLease(o, l, info)
}

func printLease(o *daemonOpts, l daemon.Lease, info *daemon.DeviceInfo) error {
	if o.JSON {
		printJSON(os.Stdout, daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: l, Device: info})
		return nil
	}
	fmt.Printf("%s leased: lease %s, holder %s, expires %s (%s; window %s, max %s)\n", l.Device, l.ID, l.Holder, l.ExpiresAt,
		untilExpiry(l.ExpiresAt), secs(l.TTLSec), secs(l.MaxSec))
	return nil
}

// secs renders a number of seconds as a duration: 8h0m0s.
func secs(n int) string {
	if n <= 0 {
		return "?"
	}
	return (time.Duration(n) * time.Second).String()
}

// ttlFlag is --ttl: a lease window as a duration or seconds.
func ttlFlag(usage string) dflag {
	return dflag{name: "ttl", kind: kindString, usage: usage, apply: func(o *daemonOpts, v string) error {
		d, err := parseLeaseDuration("ttl", v)
		if err != nil {
			return err
		}
		o.lease.ttl = int(d / time.Second)
		return nil
	}}
}

var renewFlags = flagSet(daemonSelectFlags, one(deviceFlag), targetFlags, []dflag{
	ttlFlag("The new window from now: a duration (8h, 90m) or seconds (default the lease's own)"),
})

var renewCommand = &cli.Command{
	Name:      "renew",
	Usage:     "Renew this client's lease on a device (or every lease it holds on the hub): its window starts again now",
	ArgsUsage: " ",
	Description: flagsDoc(`A lease's window is fixed: using the device never extends it. renew sets
its end to now plus --ttl (default the lease's own window); a window over
the device's maximum is refused. Without --device it renews every lease
this client holds on the hub.

  maestro-d renew --device <udid> --target-hub=hub.lan
  maestro-d renew --device <udid> --ttl 30m --target-hub=hub.lan`, renewFlags),
	SkipFlagParsing: true,
	Action:          withArgs(renewFlags, runRenew),
}

func runRenew(ctx context.Context, o *daemonOpts, args []string) error {
	var c *daemon.Client
	var err error
	if o.Target != "" {
		c, err = connectTarget(o)
	} else if c, err = daemon.Dial(o.Name); err == nil && c == nil {
		return daemon.Errorf(daemon.CodeDaemonUnavailable, "daemon %q is not running; it has no leases", o.Name)
	}
	if err != nil {
		return err
	}
	key := leaseKey(c, o)
	var recs []leaseRecord
	if o.Device != "" {
		id, _, err := lookupDevice(ctx, c, o.Device)
		if err != nil {
			return err
		}
		rec := findLease(key, id)
		if rec == nil {
			return daemon.Errorf(daemon.CodeLeaseRequired, "no lease on %s on file for %s; take one with `maestro-d lease --device %s`",
				id, targetLabel(o), o.Device).WithDetails("hint", "maestro-d lease --device "+o.Device)
		}
		recs = append(recs, *rec)
	} else if recs = leasesFor(key); len(recs) == 0 {
		return daemon.Errorf(daemon.CodeLeaseRequired, "no lease on file for %s; take one with `maestro-d lease --device ID`", targetLabel(o))
	}
	renewed := []daemon.Lease{}
	for _, r := range recs {
		res, err := c.RenewLease(ctx, r.ID, o.lease.ttl)
		if err != nil {
			if isCode(err, daemon.CodeLeaseExpired) {
				dropLease(key, r.ID)
			}
			return err
		}
		recordLease(key, res.Lease)
		renewed = append(renewed, res.Lease)
		if !o.JSON {
			l := res.Lease
			fmt.Printf("%s renewed: lease %s, expires %s (%s)\n", l.Device, l.ID, l.ExpiresAt, untilExpiry(l.ExpiresAt))
		}
	}
	if o.JSON {
		if len(renewed) == 1 && o.Device != "" {
			printJSON(os.Stdout, daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: renewed[0]})
		} else {
			printJSON(os.Stdout, daemon.LeasesResult{Envelope: daemon.Envelope{OK: true}, Leases: renewed})
		}
	}
	return nil
}

// untilExpiry is the time left on a lease, for listings: "9m58s", or
// "expired".
func untilExpiry(expiresAt string) string {
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return "?"
	}
	d := time.Until(t).Round(time.Second)
	if d <= 0 {
		return "expired"
	}
	return d.String()
}

var releaseFlags = flagSet(daemonSelectFlags, one(deviceFlag), targetFlags, []dflag{
	boolFlag("all", "Release every lease this client holds on the hub", nil, func(o *daemonOpts, v bool) { o.lease.all = v }),
})

var releaseCommand = &cli.Command{
	Name:      "release",
	Usage:     "End this client's lease on a device (or --all): the hub detaches it",
	ArgsUsage: " ",
	Description: flagsDoc(`Against a daemon that has no leases (not a hub), release does nothing and
succeeds.

  maestro-d release --device <udid> --target-hub=hub.lan
  maestro-d release --all --target-hub=hub.lan`, releaseFlags),
	SkipFlagParsing: true,
	Action:          withArgs(releaseFlags, runRelease),
}

func runRelease(ctx context.Context, o *daemonOpts, args []string) error {
	if o.Device == "" && !o.lease.all {
		return daemon.Errorf(daemon.CodeUsage, "release: give --device ID or --all")
	}
	released := []string{}
	done := func(note string) error {
		if o.JSON {
			printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "released": released})
		} else if note != "" {
			fmt.Println(note)
		}
		return nil
	}
	var c *daemon.Client
	var err error
	if o.Target != "" {
		c, err = connectTarget(o)
	} else if c, err = daemon.Dial(o.Name); err == nil && c == nil {
		return done(fmt.Sprintf("daemon %q is not running; nothing to release", o.Name))
	}
	if err != nil {
		return err
	}
	if hub, err := isHub(ctx, c, o); err != nil {
		return err
	} else if !hub {
		return done(fmt.Sprintf("%s has no leases; nothing to release", targetLabel(o)))
	}
	key := leaseKey(c, o)
	var recs []leaseRecord
	if o.lease.all {
		recs = leasesFor(key)
	} else {
		id, _, err := lookupDevice(ctx, c, o.Device)
		if err != nil {
			return err
		}
		if rec := findLease(key, id); rec != nil {
			recs = append(recs, *rec)
		}
	}
	if len(recs) == 0 {
		return done("No lease on file to release.")
	}
	for _, r := range recs {
		if _, err := c.ReleaseLease(ctx, r.ID); err != nil && !isCode(err, daemon.CodeLeaseExpired) {
			return err
		}
		dropLease(key, r.ID)
		released = append(released, r.ID)
		if !o.JSON {
			fmt.Printf("%s released (lease %s)\n", r.Device, r.ID)
		}
	}
	return done("")
}

// parseLeaseDuration reads a lease window: a Go duration (8h, 90m, 1h30m)
// or a plain number of seconds, at least one second.
func parseLeaseDuration(flag, v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil {
		d = time.Duration(n) * time.Second
	} else if d, err = time.ParseDuration(v); err != nil {
		return 0, daemon.Errorf(daemon.CodeUsage, "--%s %q: want a duration (8h, 90m, 1h30m) or seconds", flag, v)
	}
	if d < time.Second {
		return 0, daemon.Errorf(daemon.CodeUsage, "--%s %q: a lease window is at least 1s", flag, v)
	}
	return d.Round(time.Second), nil
}

// parseLeaseMax reads hub start's --lease-max values: DURATION sets the
// hub's maximum window (the last one wins), DEVICE=DURATION one device's.
func parseLeaseMax(vals []string) (time.Duration, []daemon.LeaseMax, error) {
	var hubMax time.Duration
	var devs []daemon.LeaseMax
	for _, v := range vals {
		dev, dur, perDevice := "", v, false
		if i := strings.LastIndexByte(v, '='); i >= 0 {
			dev, dur, perDevice = strings.TrimSpace(v[:i]), v[i+1:], true
			if dev == "" {
				return 0, nil, daemon.Errorf(daemon.CodeUsage, "--lease-max %q: want DURATION or DEVICE=DURATION", v)
			}
		}
		d, err := parseLeaseDuration("lease-max", dur)
		if err != nil {
			return 0, nil, err
		}
		if perDevice {
			devs = append(devs, daemon.LeaseMax{Device: dev, Max: d})
		} else {
			hubMax = d
		}
	}
	return hubMax, devs, nil
}
