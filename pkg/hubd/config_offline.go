package hubd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// OfflineConfig is used only while the caller owns the home's hub.lock.
// Discovery is fresh: a cached serial can identify yesterday's emulator.
type OfflineConfig struct {
	path         string
	config       *netshape.Config
	devices      []daemon.DeviceInfo
	reservations []reservationRecord
	now          time.Time
}

func OpenOfflineConfig(path string, deps daemon.Deps) (*OfflineConfig, error) {
	cfg, err := netshape.LoadConfig(path)
	if err != nil {
		return nil, daemon.WrapErr(daemon.CodeUsage, err)
	}
	f := &OfflineConfig{path: path, config: cfg, now: time.Now()}
	data, err := os.ReadFile(ReservationsPath("hub"))
	if !errors.Is(err, os.ErrNotExist) {
		var state reservationFile
		if err == nil {
			err = json.Unmarshal(data, &state)
		}
		if err != nil {
			return nil, daemon.Errorf(daemon.CodeUsage, "read %s: %v", ReservationsPath("hub"), err)
		}
		for _, r := range state.Reservations {
			if _, err := time.Parse(time.RFC3339, r.ExpiresAt); err != nil {
				return nil, daemon.Errorf(daemon.CodeUsage, "reservation %s: %v", r.ID, err)
			}
		}
		f.reservations = state.Reservations
	}
	if deps.ListDevices != nil {
		f.devices = deps.ListDevices("")
	}
	return f, nil
}

func (f *OfflineConfig) unfinished(r reservationRecord) bool {
	end, _ := time.Parse(time.RFC3339, r.ExpiresAt)
	return f.now.Before(end)
}

func (f *OfflineConfig) row(id string, entry netshape.Device) daemon.HubConfigDevice {
	d, listed := listedConfigDevice(id, f.devices)
	row := configDeviceRow(id, entry, listed)
	for _, r := range f.reservations {
		if f.unfinished(r) && (r.Device == id || r.Device == d.ID) {
			row.Reservations = append(row.Reservations, daemon.DeviceLease{Holder: r.Holder, StartsAt: r.StartsAt, ExpiresAt: r.ExpiresAt})
		}
	}
	return row
}

func (f *OfflineConfig) Listing() *daemon.HubConfigListing {
	out := &daemon.HubConfigListing{Envelope: daemon.Envelope{OK: true}, Path: f.path, Devices: []daemon.HubConfigDevice{}}
	for _, id := range f.config.Shared {
		out.Devices = append(out.Devices, f.row(id, f.config.Devices[id]))
	}
	return out
}

func (f *OfflineConfig) Put(id string, fields netshape.DeviceFields) (*daemon.HubConfigPutResult, error) {
	entry, err := netshape.UpsertDevice(f.path, id, fields)
	if err != nil {
		return nil, daemon.WrapErr(daemon.CodeInternal, err)
	}
	return &daemon.HubConfigPutResult{OK: true, Device: f.row(id, entry)}, nil
}

// A live serial wins over historical grants. An absent serial can only be
// identified by grants that agree about which AVD they verified.
func (f *OfflineConfig) serialAVD(serial string) string {
	if d, listed := listedConfigDevice(serial, f.devices); listed {
		if name := daemon.AVDEntryID(d); name != "" {
			return bootKey(daemon.DeviceInfo{ID: name})
		}
		return ""
	}
	var key string
	for _, r := range f.reservations {
		if r.Device == serial && r.AVD != "" && f.unfinished(r) {
			if key != "" && key != r.AVD {
				return ""
			}
			key = r.AVD
		}
	}
	return key
}

// Resolve every serial/stable pair that could refer to id. A different
// freshly identified AVD rules a pair out. An unknown serial could be any
// configured AVD, including one with a reservation under its stable name.
func (f *OfflineConfig) removalAliases(id string) ([]string, error) {
	aliases := []string{id}
	serial := daemon.IsEmulatorSerial(id)
	if !serial && !strings.HasPrefix(id, daemon.AVDPrefix) {
		return aliases, nil
	}
	candidates := slices.Clone(f.config.Shared)
	for _, r := range f.reservations {
		if f.unfinished(r) {
			candidates = append(candidates, r.Device)
			if name, ok := strings.CutPrefix(r.AVD, "android:"); ok {
				candidates = append(candidates, daemon.AVDPrefix+name)
			}
		}
	}
	for _, other := range candidates {
		var serialID, stableID string
		switch {
		case serial && strings.HasPrefix(other, daemon.AVDPrefix):
			serialID, stableID = id, other
		case !serial && daemon.IsEmulatorSerial(other):
			serialID, stableID = other, id
		default:
			continue
		}
		stable := f.serialAVD(serialID)
		if stable == "" {
			return nil, daemon.Errorf(daemon.CodeUsage, "cannot verify whether %s and %s name the same AVD while the hub is stopped", serialID, stableID)
		}
		matches := stable == bootKey(daemon.DeviceInfo{ID: stableID})
		if live, ok := listedConfigDevice(serialID, f.devices); ok {
			matches = daemon.ConfigNames(stableID, live)
		}
		if matches && !slices.Contains(aliases, other) {
			aliases = append(aliases, other)
		}
	}
	return aliases, nil
}

func (f *OfflineConfig) Remove(id string, endLeases bool) error {
	if _, ok := f.config.Lookup(id); !ok {
		return daemon.Errorf(daemon.CodeDeviceNotFound, "device %s is not in %s", id, f.path)
	}
	aliases, err := f.removalAliases(id)
	if err != nil {
		return err
	}
	targetAVD := ""
	if strings.HasPrefix(id, daemon.AVDPrefix) {
		targetAVD = bootKey(daemon.DeviceInfo{ID: id})
	} else if daemon.IsEmulatorSerial(id) {
		targetAVD = f.serialAVD(id)
	}
	var windows []string
	keep := make([]reservationRecord, 0, len(f.reservations))
	for _, r := range f.reservations {
		matches := slices.Contains(aliases, r.Device)
		if r.AVD != "" && targetAVD != "" {
			matches = r.AVD == targetAVD
		}
		if matches && f.unfinished(r) {
			windows = append(windows, fmt.Sprintf("%s (%s to %s)", daemon.HolderName(r.Holder), r.StartsAt, r.ExpiresAt))
		} else {
			keep = append(keep, r)
		}
	}
	if len(windows) != 0 {
		if !endLeases {
			return daemon.Errorf(daemon.CodeDeviceInUse, "device %s is in use by %s", id, strings.Join(windows, ", "))
		}
		if err := writeReservations(ReservationsPath("hub"), keep); err != nil {
			return daemon.WrapErr(daemon.CodeInternal, err)
		}
	}
	if err := netshape.RemoveDevices(f.path, id, aliases); err != nil {
		return daemon.WrapErr(daemon.CodeInternal, err)
	}
	return nil
}
