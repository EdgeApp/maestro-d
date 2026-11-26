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

// Find the held grant before asking discovery about a possibly recycled serial.
func targetLeaseRecord(hub, target string) (*leaseRecord, error) {
	records := leasesFor(hub)
	matches := func(id string) bool {
		local, _, _ := strings.Cut(id, "@")
		return id == target || local == target
	}
	var device string
	for _, rec := range records {
		if matches(rec.Device) || (rec.OriginalDevice != "" && matches(rec.OriginalDevice)) || (rec.RequestedDevice != "" && matches(rec.RequestedDevice)) {
			if device != "" && device != rec.Device {
				return nil, daemon.Errorf(daemon.CodeUsage, "qualify device %s with its hub name", target)
			}
			device = rec.Device
		}
	}
	return currentLease(records, device, time.Now()), nil
}

// Renew and release address the lease ID, even when the original AVD is offline.
func leaseRecordTarget(ctx context.Context, c *daemon.Client, hub, target string) (string, error) {
	if emulatorTarget(target) {
		rec, err := targetLeaseRecord(hub, target)
		if err != nil {
			return "", err
		}
		if rec != nil {
			return rec.Device, nil
		}
	}
	id, _, err := lookupDevice(ctx, c, target)
	return id, err
}

func resolveGrant(ctx context.Context, c *daemon.Client, o *daemonOpts, rec leaseRecord) (daemon.Lease, error) {
	result, err := c.Lease(ctx, rec.ID)
	if err != nil {
		if isCode(err, daemon.CodeLeaseExpired) {
			dropLease(leaseKey(c, o), rec.ID)
		}
		return daemon.Lease{}, err
	}
	l := result.Lease
	if l.ID != rec.ID {
		return l, daemon.Errorf(daemon.CodeLeaseExpired, "hub returned a different grant for %s", rec.ID)
	}
	if l.Device != rec.Device {
		c.ClearLease(rec.Device)
	}
	if l.Device == "" {
		return l, daemon.Errorf(daemon.CodeDeviceNotFound, "the emulator for lease %s is unavailable", rec.ID)
	}
	c.SetLease(l.Device, l.ID)
	if rec.Device != l.Device {
		recordLease(leaseKey(c, o), l)
	}
	return l, nil
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
