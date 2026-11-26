package cli

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func emulatorTarget(device string) bool {
	id, _, _ := strings.Cut(device, "@")
	return daemon.IsEmulatorSerial(id) || strings.HasPrefix(id, daemon.AVDPrefix)
}

// Prefer the target under which a grant was taken. After two AVDs swap
// serials, the same target is also the current serial of a different grant.
func targetLeaseRecords(records []leaseRecord, target string) ([]leaseRecord, error) {
	matches := func(id string) bool {
		local, _, _ := strings.Cut(id, "@")
		return id != "" && (id == target || local == target)
	}
	var original, current []leaseRecord
	var owner string
	for _, rec := range records {
		originalTarget := rec.OriginalDevice
		if originalTarget == "" {
			originalTarget = rec.Device
		}
		switch {
		case matches(originalTarget) || matches(rec.RequestedDevice):
			original = append(original, rec)
		case matches(rec.Device):
			current = append(current, rec)
		default:
			continue
		}
		_, hub, _ := strings.Cut(rec.Device, "@")
		if len(original)+len(current) > 1 && hub != owner {
			return nil, daemon.Errorf(daemon.CodeUsage, "qualify device %s with its hub name", target)
		}
		owner = hub
	}
	if len(original) != 0 {
		return original, nil
	}
	return current, nil
}

func targetLeaseRecord(hub, target string) (*leaseRecord, error) {
	records, err := targetLeaseRecords(leasesFor(hub), target)
	return currentLease(records, "", time.Now()), err
}

// Renew and release address the lease IDs even when the original AVD is offline.
func leaseRecordsForTarget(ctx context.Context, c *daemon.Client, hub, target string) ([]leaseRecord, error) {
	records := leasesFor(hub)
	if emulatorTarget(target) {
		matches, err := targetLeaseRecords(records, target)
		if err != nil || len(matches) != 0 {
			return matches, err
		}
	}
	id, _, err := lookupDevice(ctx, c, target)
	if err != nil {
		return nil, err
	}
	return targetLeaseRecords(records, id)
}

func resolveGrant(ctx context.Context, c *daemon.Client, o *daemonOpts, rec leaseRecord) (daemon.Lease, error) {
	result, err := c.Lease(ctx, rec.ID)
	if err != nil {
		if isCode(err, daemon.CodeLeaseExpired) {
			dropLease(leaseKey(c, o), rec.ID)
			if c.LeaseOf(rec.Device) == rec.ID {
				c.ClearLease(rec.Device)
			}
		}
		return daemon.Lease{}, err
	}
	l := result.Lease
	if l.ID != rec.ID {
		return l, daemon.Errorf(daemon.CodeInternal, "hub returned a different grant for %s", rec.ID)
	}
	if l.Device != rec.Device && c.LeaseOf(rec.Device) == rec.ID {
		c.ClearLease(rec.Device)
	}
	if l.Device == "" {
		return l, daemon.Errorf(daemon.CodeDeviceNotFound, "the emulator for lease %s is unavailable", rec.ID)
	}
	c.SetLease(l.Device, l.ID)
	if rec.Device != l.Device {
		refreshLease(leaseKey(c, o), l)
	}
	return l, nil
}

// A client clock on either side of a window boundary can select an ended
// grant or a scheduled one. Ask the hub before using another saved window.
func resolveSessionGrant(ctx context.Context, c *daemon.Client, o *daemonOpts, rec *leaseRecord) (*leaseRecord, daemon.Lease, error) {
	held, err := resolveGrant(ctx, c, o, *rec)
	if isCode(err, daemon.CodeLeaseExpired) || (err == nil && held.State == daemon.LeaseScheduled) {
		if alt := otherLeaseRecord(leaseKey(c, o), rec.Device, rec.ID); alt != nil {
			held, err = resolveGrant(ctx, c, o, *alt)
			rec = alt
			if err == nil && held.State != daemon.LeaseActive && held.State != daemon.LeaseScheduled {
				err = daemon.Errorf(daemon.CodeLeaseExpired, "lease %s is no longer active", rec.ID)
			}
		}
	}
	if err == nil && held.State == daemon.LeaseScheduled {
		err = daemon.Errorf(daemon.CodeLeaseNotStarted, "lease %s starts at %s", rec.ID, held.StartsAt).WithDetails("startsAt", held.StartsAt)
	}
	return rec, held, err
}

func rememberAttach(hub, lease string, cfg daemon.AttachConfig) {
	if cfg.AppFile != "" && !isBlobRef(cfg.AppFile) {
		if file, err := filepath.Abs(cfg.AppFile); err == nil {
			cfg.AppFile = file
		}
	}
	updateLeases(func(recs []leaseRecord) []leaseRecord {
		for i := range recs {
			if recs[i].ID == lease && recs[i].Hub == hub {
				recs[i].Attach = &cfg
				break
			}
		}
		return recs
	})
}
