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
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
)

// leaseRecord is one entry of leases.json.
type leaseRecord struct {
	// Hub is the hub's address (its base URL, or unix:<daemon> for a hub
	// reached over its unix socket).
	Hub    string `json:"hub"`
	Device string `json:"device"`
	ID     string `json:"id"`
	Holder string `json:"holder,omitempty"`
	// StartsAt is the start of the window: later than now for a
	// reservation (maestro-d reserve). A record without it has started.
	StartsAt string `json:"startsAt,omitempty"`
	// ExpiresAt is the end of the window as the hub last reported it
	// (moved only by an explicit renew).
	ExpiresAt string `json:"expiresAt,omitempty"`
	TTLSec    int    `json:"ttlSec,omitempty"`
}

// startedAt reports whether r's window has started by now, and its start
// (zero when none is recorded: such a record has started, and ranks
// earliest).
func (r leaseRecord) startedAt(now time.Time) (bool, time.Time) {
	t, err := time.Parse(time.RFC3339, r.StartsAt)
	if r.StartsAt == "" || err != nil {
		return true, time.Time{}
	}
	return !t.After(now), t
}

// currentLease picks the record to use for device among recs: of the
// ones that have started, the one that started last; with none started,
// the reservation that starts first. The recorded expiresAt is not
// weighed: an ended lease is still sent, the hub answers LEASE_EXPIRED
// and the record is dropped (wireLeases), and a lease renewed from
// another home is still sent.
func currentLease(recs []leaseRecord, device string, now time.Time) *leaseRecord {
	var best *leaseRecord
	var bestStart time.Time
	bestStarted := false
	for i := range recs {
		r := recs[i]
		if r.Device != device {
			continue
		}
		started, start := r.startedAt(now)
		take := false
		switch {
		case best == nil:
			take = true
		case started != bestStarted:
			take = started
		case started:
			take = start.After(bestStart)
		default:
			take = start.Before(bestStart)
		}
		if take {
			best, bestStart, bestStarted = &r, start, started
		}
	}
	return best
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
		common.Progress("warning: ignoring unreadable %s: %v", leasesPath(), err)
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
		common.Progress("warning: %s: %v", leasesPath(), err)
		return
	}
	if lock, err := fsage.LockFile(leasesPath() + ".lock"); err != nil {
		common.Progress("warning: lock %s: %v (updating it unlocked)", leasesPath(), err)
	} else {
		defer fsage.UnlockFile(lock)
	}
	if err := saveLeases(fn(loadLeases())); err != nil {
		common.Progress("warning: %s: %v", leasesPath(), err)
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

// findLease returns the lease on file to use for device on one hub
// (currentLease).
func findLease(hub, device string) *leaseRecord {
	return currentLease(leasesFor(hub), device, time.Now())
}

// recordLease records l for one hub, replacing the record of the same
// lease id; the device's reservations stay (a lease now and a reservation
// later). A lease that has started also replaces the device's other
// started records: no two leases on a device overlap, so those have ended.
func recordLease(hub string, l daemon.Lease) {
	now := time.Now()
	started, _ := leaseRecord{StartsAt: l.StartsAt}.startedAt(now)
	updateLeases(func(recs []leaseRecord) []leaseRecord {
		out := recs[:0]
		for _, r := range recs {
			if r.Hub == hub && r.ID == l.ID {
				continue
			}
			if rs, _ := r.startedAt(now); started && rs && r.Hub == hub && r.Device == l.Device {
				continue
			}
			out = append(out, r)
		}
		return append(out, leaseRecord{Hub: hub, Device: l.Device, ID: l.ID, Holder: l.Holder, StartsAt: l.StartsAt,
			ExpiresAt: l.ExpiresAt, TTLSec: l.TTLSec})
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
	c.StoredLeases = func() []string {
		var ids []string
		for _, r := range leasesFor(key) {
			ids = append(ids, r.ID)
		}
		return ids
	}
	recs := leasesFor(key)
	now := time.Now()
	for _, r := range recs {
		if cur := currentLease(recs, r.Device, now); cur != nil {
			c.SetLease(cur.Device, cur.ID)
		}
	}
	// Which of a device's leases has started is judged by this machine's
	// clock, which may be up to a minute off the hub's: at a lease's end
	// and the next one's start, a refused request goes once more with the
	// other.
	c.OtherLease = func(device, failed string) string {
		var rest []leaseRecord
		for _, r := range leasesFor(key) {
			if r.ID != failed {
				rest = append(rest, r)
			}
		}
		if alt := currentLease(rest, device, time.Now()); alt != nil {
			return alt.ID
		}
		return ""
	}
	c.OnLeaseExpired = func(device, lease, hint string) {
		dropLease(key, lease)
		switch {
		case o.JSON:
		case hint != "":
			common.Progress("Lease %s on %s has ended, and the device is gone with it; take a new one with `%s`", lease, device, hint)
		default:
			common.Progress("Lease %s on %s has expired; take a new one with `maestro-d lease --device %s`", lease, device, device)
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

var leaseFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags[:1], common.One(deviceFlag), attachFlags, targetFlags, []dflag{
	common.StringFlag("kind", "Select a device of this kind: simulator, emulator, device", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Kind = v }),
	common.StringFlag("name", "Select the device with this name (or id)", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Name = v }),
	common.StringFlag("os", "Select an OS version, or a prefix of one (26 matches 26.5)", nil,
		func(o *daemonOpts, v string) { o.lease.sel.OSVersion = v }),
	common.StringFlag("hub", "Select a device on this hub", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Hub = v }),
	common.BoolFlag("boot", "Let the selector pick a shut-down simulator or AVD, which the hub boots for the lease", nil,
		func(o *daemonOpts, v bool) { o.lease.sel.Boot = v }),
	common.StringFlag("location", "cloud: launch a cloud device for the lease, on the hub that holds the provider's credentials (--name is then the model)", nil,
		func(o *daemonOpts, v string) { o.lease.sel.Location = v }),
	common.StringFlag("provider", "Cloud: the provider (default devicefarm)", nil, func(o *daemonOpts, v string) { o.lease.sel.Provider = v }),
	common.StringFlag("os-min", "Select an OS version at least this", nil, func(o *daemonOpts, v string) { o.lease.sel.OSVersionMin = v }),
	common.StringFlag("os-max", "Select an OS version at most this, at its own precision (17 admits 17.5)", nil, func(o *daemonOpts, v string) { o.lease.sel.OSVersionMax = v }),
	common.StringFlag("idle-release", "Cloud: release the device (and end the lease) after this long without a request (default 10m, 60s–150m)", nil,
		func(o *daemonOpts, v string) { o.lease.idleRelease = v }),
	common.StringFlag("appium-version", "Cloud: Appium 2 or 3 (default 3 on iOS 27 and later, else 2)", nil,
		func(o *daemonOpts, v string) { o.lease.appiumVersion = v }),
	ttlFlag("The lease's window: a duration (8h, 90m) or seconds (default the hub's, 8h or the device's maximum); using the lease never extends it"),
	common.StringFlag("holder", "Who holds the lease, as listings show it (default user@host)", nil,
		func(o *daemonOpts, v string) { o.lease.holder = v }),
	{Name: "attach", Kind: common.Bool, Value: "true", Usage: "Attach the device once leased (default true; --no-attach only reserves it)",
		Apply: func(o *daemonOpts, v string) error {
			o.lease.noAttach = v == "false"
			return nil
		}},
})

var leaseCommand = &cli.Command{
	Name:      "lease",
	Usage:     "Lease a device on a hub, by id or by selector, and attach it",
	ArgsUsage: " ",
	Description: common.FlagHelp(`A hub lends each device to one client at a time, for a fixed window: --ttl
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
		// A reservation on file does not stand in for a lease: the hub
		// is asked, and refuses a window that reaches it.
		if rec := findLease(key, id); rec != nil && startedRecord(*rec) {
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
		if strings.EqualFold(sel.Location, daemon.LocationCloud) {
			spec, err := leaseCloudSpec(o)
			if err != nil {
				return err
			}
			req.Cloud = spec
		} else if o.lease.idleRelease != "" || o.lease.appiumVersion != "" {
			return daemon.Errorf(daemon.CodeUsage, "--idle-release and --appium-version apply only to a cloud lease (--location cloud)")
		}
	}
	// Without an app to upload first, the hub attaches in the lease request
	// itself, under the lease: no gap in which a short TTL can run out.
	if !o.lease.noAttach && o.Attach.AppFile == "" {
		cfg := o.Attach
		req.Attach = &cfg
	}
	res, err := c.AcquireLease(ctx, req)
	if err != nil {
		return err
	}
	l := res.Lease
	recordLease(key, l)
	c.SetLease(l.Device, l.ID)
	info := res.Device
	if !o.lease.noAttach && (info == nil || !info.Attached) {
		if info, err = ensureAttached(ctx, c, o, l.Device); err != nil {
			if _, rerr := c.ReleaseLease(ctx, l.ID); rerr == nil {
				dropLease(key, l.ID)
			}
			return err
		}
	}
	return printLease(o, l, info)
}

// leaseCloudSpec is a cloud lease's launch settings from the flags.
func leaseCloudSpec(o *daemonOpts) (*cloudprov.CloudSpec, error) {
	spec := &cloudprov.CloudSpec{AppiumVersion: o.lease.appiumVersion, AppID: o.Attach.AppID}
	if o.lease.idleRelease != "" {
		sec, err := durationSeconds(o.lease.idleRelease)
		if err != nil {
			return nil, daemon.Errorf(daemon.CodeUsage, "--idle-release %q: %v", o.lease.idleRelease, err)
		}
		spec.IdleReleaseSec = sec
	}
	return spec, nil
}

// startedRecord reports whether r's window has started.
func startedRecord(r leaseRecord) bool {
	started, _ := r.startedAt(time.Now())
	return started
}

func printLease(o *daemonOpts, l daemon.Lease, info *daemon.DeviceInfo) error {
	if o.JSON {
		common.PrintJSON(os.Stdout, daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: l, Device: info})
		return nil
	}
	if l.State == daemon.LeaseScheduled {
		fmt.Printf("%s reserved: lease %s, holder %s, from %s (in %s) until %s (window %s, max %s)\n", l.Device, l.ID, l.Holder,
			l.StartsAt, untilExpiry(l.StartsAt), l.ExpiresAt, secs(l.TTLSec), secs(l.MaxSec))
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
	return dflag{Name: "ttl", Kind: common.String, Usage: usage, Apply: func(o *daemonOpts, v string) error {
		d, err := common.ParseLeaseDuration("ttl", v)
		if err != nil {
			return err
		}
		o.lease.ttl = int(d / time.Second)
		return nil
	}}
}

var renewFlags = common.FlagSet(daemonSelectFlags, common.One(deviceFlag), targetFlags, []dflag{
	ttlFlag("The new window from now: a duration (8h, 90m) or seconds (default the lease's own)"),
})

var renewCommand = &cli.Command{
	Name:      "renew",
	Usage:     "Renew this client's lease on a device (or every lease it holds on the hub): its window starts again now",
	ArgsUsage: " ",
	Description: common.FlagHelp(`A lease's window is fixed: using the device never extends it. renew sets
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
	} else {
		// Reservations that have not started are not renewed.
		for _, r := range leasesFor(key) {
			if startedRecord(r) {
				recs = append(recs, r)
			}
		}
		if len(recs) == 0 {
			return daemon.Errorf(daemon.CodeLeaseRequired, "no lease on file for %s; take one with `maestro-d lease --device ID`", targetLabel(o))
		}
	}
	renewed := []daemon.Lease{}
	var expired error
	for _, r := range recs {
		res, err := c.RenewLease(ctx, r.ID, o.lease.ttl)
		if err != nil {
			// Without --device, a lease on file that has ended (one a later
			// lease on its device followed) is dropped, and a reservation the
			// hub says has not started (this machine's clock may be ahead of
			// the hub's) is passed over; the rest are renewed, and it fails
			// only when nothing was.
			de, _ := err.(*daemon.Error)
			notStarted := de != nil && de.Code == daemon.CodeUsage && de.Details["startsAt"] != nil
			if isCode(err, daemon.CodeLeaseExpired) {
				dropLease(key, r.ID)
			} else if !notStarted {
				return err
			}
			if o.Device != "" {
				return err
			}
			expired = err
			continue
		}
		recordLease(key, res.Lease)
		renewed = append(renewed, res.Lease)
		if !o.JSON {
			l := res.Lease
			fmt.Printf("%s renewed: lease %s, expires %s (%s)\n", l.Device, l.ID, l.ExpiresAt, untilExpiry(l.ExpiresAt))
		}
	}
	if len(renewed) == 0 && expired != nil {
		return expired
	}
	if o.JSON {
		if len(renewed) == 1 && o.Device != "" {
			common.PrintJSON(os.Stdout, daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: renewed[0]})
		} else {
			common.PrintJSON(os.Stdout, daemon.LeasesResult{Envelope: daemon.Envelope{OK: true}, Leases: renewed})
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

var releaseFlags = common.FlagSet(daemonSelectFlags, common.One(deviceFlag), targetFlags, []dflag{
	common.BoolFlag("all", "Release every lease this client holds on the hub", nil, func(o *daemonOpts, v bool) { o.lease.all = v }),
	common.StringFlag("lease", "Release this one lease, by id (a reservation is cancelled)", nil, func(o *daemonOpts, v string) { o.lease.id = v }),
})

var releaseCommand = &cli.Command{
	Name:      "release",
	Usage:     "End this client's leases on a device (or one lease, or --all): the hub detaches it",
	ArgsUsage: " ",
	Description: common.FlagHelp(`--device releases every lease this client holds on that device, its
reservations included; --lease releases that one lease (a reservation is
cancelled, and nothing is detached); --all every lease on the hub. Against
a daemon that has no leases (not a hub), release does nothing and
succeeds.

  maestro-d release --device <udid> --target-hub=hub.lan
  maestro-d release --lease <lease id> --target-hub=hub.lan
  maestro-d release --all --target-hub=hub.lan`, releaseFlags),
	SkipFlagParsing: true,
	Action:          withArgs(releaseFlags, runRelease),
}

func runRelease(ctx context.Context, o *daemonOpts, args []string) error {
	switch n := btoi(o.Device != "") + btoi(o.lease.all) + btoi(o.lease.id != ""); {
	case n == 0:
		return daemon.Errorf(daemon.CodeUsage, "release: give --device ID, --lease ID or --all")
	case n > 1:
		return daemon.Errorf(daemon.CodeUsage, "release: give one of --device ID, --lease ID or --all")
	}
	released := []string{}
	done := func(note string) error {
		if o.JSON {
			common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "released": released})
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
	switch {
	case o.lease.all:
		recs = leasesFor(key)
	case o.lease.id != "":
		for _, r := range leasesFor(key) {
			if r.ID == o.lease.id {
				recs = []leaseRecord{r}
			}
		}
		if len(recs) == 0 {
			// Not on file (another client's, or a typo): the hub says which
			// device it holds and whether it has started, or that it has no
			// such lease, which is then the error.
			res, err := c.Lease(ctx, o.lease.id)
			if isCode(err, daemon.CodeLeaseExpired) {
				return daemon.Errorf(daemon.CodeLeaseExpired, "no lease %s on %s", o.lease.id, targetLabel(o)).WithDetails("lease", o.lease.id)
			}
			if err != nil {
				return err
			}
			l := res.Lease
			recs = []leaseRecord{{Hub: key, ID: l.ID, Device: l.Device, StartsAt: l.StartsAt, ExpiresAt: l.ExpiresAt}}
		}
	default:
		id, _, err := lookupDevice(ctx, c, o.Device)
		if err != nil {
			return err
		}
		for _, r := range leasesFor(key) {
			if r.Device == id {
				recs = append(recs, r)
			}
		}
	}
	if len(recs) == 0 {
		return done("No lease on file to release.")
	}
	minutes := map[string]*cloudprov.DeviceMinutes{}
	for _, r := range recs {
		res, err := c.ReleaseLease(ctx, r.ID)
		if err != nil && !isCode(err, daemon.CodeLeaseExpired) {
			return err
		}
		dropLease(key, r.ID)
		released = append(released, r.ID)
		note := ""
		if res != nil && res.DeviceMinutes != nil {
			minutes[r.ID] = res.DeviceMinutes
			note = minutesNote(res.DeviceMinutes)
		} else if err == nil && strings.HasPrefix(r.Device, "df-") {
			note = " (minutes in the device.released event)"
		}
		if !o.JSON {
			how := "released"
			if !startedRecord(r) {
				how = "reservation cancelled"
			}
			fmt.Printf("%s %s (lease %s)%s\n", r.Device, how, r.ID, note)
		}
	}
	if o.JSON && len(minutes) > 0 {
		common.PrintJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "released": released, "deviceMinutes": minutes})
		return nil
	}
	return done("")
}
