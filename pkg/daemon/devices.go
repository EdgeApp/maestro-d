package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/emulator"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

const (
	BootedByDaemon   = "daemon"
	BootedByExternal = "external"
	defaultBootSecs  = 120
	// AVDPrefix marks a shut-down AVD's id in listings: avd:<name>.
	AVDPrefix = "avd:"
	// CreatedByMaestroD tags the devices `device create` made.
	CreatedByMaestroD = "maestro-d"
	createdFile       = "created.json"
)

// Registry tracks the devices a daemon booted or attached and persists them
// to devices.json so a crashed daemon leaves a record (`ps`, `device stop
// --orphans`). Live discovery (adb / simctl) is injected so this package
// stays independent of pkg/cli.
type Registry struct {
	mu      sync.Mutex
	name    string
	records map[string]*DeviceInfo
	sim     *simulator.Manager
	emu     *emulator.Manager
	list    func(platform string) []DeviceInfo
	// booting guards concurrent boots of the same name.
	booting map[string]bool
	// deps holds the catalog, create and delete hooks.
	deps Deps
	// boot starts a simulator or emulator and returns its id (a seam over
	// the simulator and emulator managers).
	boot func(platform, name string, timeout time.Duration) (string, error)
	// shutdown stops a simulator or emulator (a seam over shutdownDevice).
	shutdown func(DeviceInfo) error
}

// NewRegistry creates a registry for daemon name. list discovers live
// devices (may be nil for tests).
func NewRegistry(name string, list func(platform string) []DeviceInfo) *Registry {
	r := &Registry{
		name:    name,
		records: map[string]*DeviceInfo{},
		sim:     simulator.NewManager(),
		emu:     emulator.NewManager(),
		list:    list,
		booting: map[string]bool{},
	}
	r.boot = r.startNative
	r.shutdown = shutdownDevice
	return r
}

// startNative boots through upstream's simulator and emulator managers.
func (r *Registry) startNative(platform, name string, timeout time.Duration) (string, error) {
	if platform == "ios" {
		if simulator.IsSimulator(name) {
			return r.sim.Start(name, timeout)
		}
		return r.sim.StartByName(name, timeout)
	}
	return r.emu.Start(name, timeout)
}

// Live returns the devices currently visible to adb / simctl. Platform ""
// means all.
func (r *Registry) Live(platform string) []DeviceInfo {
	if r.list == nil {
		return nil
	}
	return r.list(platform)
}

// List merges live devices with the registry: every live device appears,
// tagged with bootedBy/attached from the records; records for devices that
// vanished are dropped.
func (r *Registry) List(platform string) []DeviceInfo {
	live := r.Live(platform)
	created := loadCreated()
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	out := make([]DeviceInfo, 0, len(live))
	for _, d := range live {
		seen[d.ID] = true
		if created.has(d.Platform, createdKey(d)) {
			d.CreatedBy = CreatedByMaestroD
		}
		if rec, ok := r.records[d.ID]; ok {
			d.BootedBy = rec.BootedBy
			d.BootedAt = rec.BootedAt
			d.Attached = rec.Attached
			d.AttachedAt = rec.AttachedAt
			d.AppID = rec.AppID
			d.Driver = rec.Driver
			d.ReportDir = rec.ReportDir
			d.Busy = rec.Busy
		} else {
			d.BootedBy = BootedByExternal
		}
		out = append(out, d)
	}
	// Records the live list can't see (mock devices, or a device that
	// dropped off adb while attached) still show so they can be detached.
	for id, rec := range r.records {
		if seen[id] {
			continue
		}
		if platform != "" && rec.Platform != platform {
			continue
		}
		if rec.Attached || rec.Platform == "mock" || rec.Platform == "web" {
			out = append(out, *rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Lookup returns the merged info for one device.
func (r *Registry) Lookup(id string) (DeviceInfo, bool) {
	for _, d := range r.List("") {
		if d.ID == id {
			return d, true
		}
	}
	return DeviceInfo{}, false
}

// Held returns the records of the devices this daemon has attached or
// booted, without asking adb or simctl.
func (r *Registry) Held() []DeviceInfo {
	var out []DeviceInfo
	for _, rec := range r.Records() {
		if rec.Attached || rec.BootedBy == BootedByDaemon {
			out = append(out, rec)
		}
	}
	return out
}

// Records returns the persisted records (a copy).
func (r *Registry) Records() []DeviceInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *Registry) snapshotLocked() []DeviceInfo {
	out := make([]DeviceInfo, 0, len(r.records))
	for _, rec := range r.records {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// persistLocked rewrites devices.json.
func (r *Registry) persistLocked() {
	if r.name == "" {
		return
	}
	if err := os.MkdirAll(RunDir(r.name), 0o700); err != nil {
		logger.Warn("devices.json: %v", err)
		return
	}
	if err := atomicWriteJSON(DevicesPath(r.name), r.snapshotLocked()); err != nil {
		logger.Warn("devices.json: %v", err)
	}
}

// MarkAttached records that the daemon holds a driver for the device.
func (r *Registry) MarkAttached(info DeviceInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[info.ID]
	if !ok {
		rec = &DeviceInfo{ID: info.ID, BootedBy: BootedByExternal}
		r.records[info.ID] = rec
	}
	if info.Platform != "" {
		rec.Platform = info.Platform
	}
	if info.Kind != "" {
		rec.Kind = info.Kind
	}
	if info.Name != "" {
		rec.Name = info.Name
	}
	rec.Attached = true
	rec.AttachedAt = nowRFC3339()
	rec.AppID = info.AppID
	rec.Driver = info.Driver
	rec.ReportDir = info.ReportDir
	rec.Busy = false
	r.persistLocked()
}

// MarkDetached clears the attachment; a record that was only there for
// the attachment is dropped.
func (r *Registry) MarkDetached(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[id]
	if !ok {
		return
	}
	rec.Attached = false
	rec.AttachedAt = ""
	rec.AppID = ""
	rec.Driver = ""
	rec.Busy = false
	if rec.BootedBy != BootedByDaemon {
		delete(r.records, id)
	}
	r.persistLocked()
}

// SetBusy flags a step in flight (not persisted).
func (r *Registry) SetBusy(id string, busy bool) {
	r.mu.Lock()
	if rec, ok := r.records[id]; ok {
		rec.Busy = busy
	}
	r.mu.Unlock()
}

// Boot starts a simulator or emulator and records it as daemon-booted. A
// device that is already running under that name/UDID is returned as-is
// (bootedBy external) rather than booted twice.
func (r *Registry) Boot(ctx context.Context, req BootRequest) (DeviceInfo, error) {
	if req.Name == "" {
		return DeviceInfo{}, Errorf(CodeUsage, "device start: a simulator/AVD name or UDID is required")
	}
	timeout := time.Duration(req.BootTimeout) * time.Second
	if timeout <= 0 {
		timeout = defaultBootSecs * time.Second
	}
	platform := strings.ToLower(req.Platform)
	if platform == "" {
		p, e := r.inferBootPlatform(req.Name)
		if e != nil {
			return DeviceInfo{}, e
		}
		platform = p
	}
	if platform != "ios" && platform != "android" {
		return DeviceInfo{}, Errorf(CodeUsage, "device start: platform must be ios or android (got %q)", req.Platform)
	}
	name := req.Name
	if platform == "android" {
		name = strings.TrimPrefix(name, AVDPrefix)
	}

	// Already running? A running emulator is listed under its AVD name.
	for _, d := range r.Live(platform) {
		if d.Ready && (d.ID == req.Name || strings.EqualFold(d.Name, name)) {
			logger.Info("device start: %s already running as %s", req.Name, d.ID)
			return d, nil
		}
	}

	key := platform + ":" + name
	r.mu.Lock()
	if r.booting[key] {
		r.mu.Unlock()
		return DeviceInfo{}, Errorf(CodeBusy, "device start: %s is already booting", req.Name)
	}
	r.booting[key] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.booting, key)
		r.mu.Unlock()
	}()

	var id string
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		id, err = r.boot(platform, name, timeout)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return DeviceInfo{}, Errorf(CodeInterrupted, "device start: cancelled while booting %s", req.Name)
	}
	if err != nil {
		if de, ok := err.(*Error); ok {
			return DeviceInfo{}, de
		}
		return DeviceInfo{}, WrapErr(CodeDeviceError, err)
	}

	info := DeviceInfo{ID: id, Platform: platform, Name: name, State: "Booted", Ready: true,
		BootedBy: BootedByDaemon, BootedAt: nowRFC3339()}
	if platform == "ios" {
		info.Kind = "simulator"
	} else {
		info.Kind = "emulator"
	}
	if live, ok := r.findLive(platform, id); ok {
		info.Name, info.OSVersion, info.State, info.Ready = live.Name, live.OSVersion, live.State, live.Ready
	}
	r.mu.Lock()
	if rec, ok := r.records[id]; ok {
		rec.BootedBy, rec.BootedAt = BootedByDaemon, info.BootedAt
		rec.Platform, rec.Kind, rec.Name = info.Platform, info.Kind, info.Name
	} else {
		cp := info
		r.records[id] = &cp
	}
	r.persistLocked()
	r.mu.Unlock()
	return info, nil
}

func (r *Registry) findLive(platform, id string) (DeviceInfo, bool) {
	for _, d := range r.Live(platform) {
		if d.ID == id {
			return d, true
		}
	}
	return DeviceInfo{}, false
}

// inferBootPlatform picks the platform of a device start without
// --platform: avd:<name> is android, and otherwise the id or name must match
// a listed device (a simulator by name or UDID, an AVD by name, a running
// emulator or device by serial).
func (r *Registry) inferBootPlatform(name string) (string, *Error) {
	if strings.HasPrefix(name, AVDPrefix) {
		return "android", nil
	}
	found := map[string]bool{}
	for _, d := range r.Live("") {
		if d.ID == name || (d.Name != "" && strings.EqualFold(d.Name, name)) {
			found[d.Platform] = true
		}
	}
	switch {
	case len(found) == 1:
		for p := range found {
			return p, nil
		}
	case len(found) > 1:
		return "", Errorf(CodeUsage, "device start: %q names both an iOS simulator and an Android device; pass --platform ios|android", name)
	}
	return "", Errorf(CodeUsage, "device start: no simulator, AVD or device is called %q; pass --platform ios|android, or avd:<name> for an AVD", name)
}

// Stop shuts a simulator/emulator down. External devices need force; a
// physical device can't be stopped at all. The caller must have detached
// the device first.
func (r *Registry) Stop(id string, force bool) error {
	info, ok := r.Lookup(id)
	if !ok {
		return Errorf(CodeDeviceNotFound, "device %s not found", id)
	}
	if strings.HasPrefix(info.ID, AVDPrefix) {
		return nil // a shut-down AVD: nothing to stop
	}
	if info.Attached {
		return Errorf(CodeBusy, "device %s is attached; detach it first", id)
	}
	if info.BootedBy != BootedByDaemon && !force {
		return Errorf(CodeDeviceExternal, "device %s was not started by this daemon; use --force to stop it", id).
			WithDetails("bootedBy", info.BootedBy)
	}
	if err := r.shutdown(info); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.records, id)
	r.persistLocked()
	r.mu.Unlock()
	return nil
}

// shutdownDevice shuts a simulator or emulator down regardless of who
// booted it.
func shutdownDevice(info DeviceInfo) error {
	switch {
	case info.Platform == "ios" && (info.Kind == "simulator" || simulator.IsSimulator(info.ID)):
		if err := simulator.ShutdownSimulator(info.ID, 60*time.Second); err != nil {
			return WrapErr(CodeDeviceError, err)
		}
	case info.Platform == "android" && (info.Kind == "emulator" || emulator.IsEmulator(info.ID)):
		if err := emulator.ShutdownEmulator(info.ID, 60*time.Second); err != nil {
			return WrapErr(CodeDeviceError, err)
		}
	case info.Platform == "mock" || info.Platform == "web":
		return nil
	default:
		return Errorf(CodeUsage, "device %s is a physical device and cannot be stopped", info.ID)
	}
	return nil
}

// ShutdownOwned stops every device this daemon booted that is still
// running. Errors are logged; the daemon is exiting anyway.
func (r *Registry) ShutdownOwned() {
	r.mu.Lock()
	var owned []DeviceInfo
	for _, rec := range r.records {
		if rec.BootedBy == BootedByDaemon {
			owned = append(owned, *rec)
		}
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, d := range owned {
		wg.Add(1)
		go func(d DeviceInfo) {
			defer wg.Done()
			logger.Info("Shutting down daemon-booted device %s", d.ID)
			if err := r.shutdown(d); err != nil {
				logger.Warn("shutdown %s: %v", d.ID, err)
			}
			r.mu.Lock()
			delete(r.records, d.ID)
			r.persistLocked()
			r.mu.Unlock()
		}(d)
	}
	wg.Wait()
}

// StopOrphans shuts down devices recorded as daemon-booted by daemons that
// are no longer alive and removes their run files. It returns the device
// IDs it stopped. Used by `device stop --orphans`; needs no daemon.
func StopOrphans() ([]string, error) {
	daemons, err := ListDaemons()
	if err != nil {
		return nil, err
	}
	var stopped []string
	var errs []error
	for _, d := range daemons {
		if d.Alive {
			continue
		}
		for _, dev := range d.Devices {
			if dev.BootedBy != BootedByDaemon {
				continue
			}
			if err := shutdownDevice(dev); err != nil {
				errs = append(errs, fmt.Errorf("%s (%s): %w", dev.ID, d.Name, err))
				continue
			}
			stopped = append(stopped, dev.ID)
		}
		RemoveRunFiles(d.Name)
	}
	return stopped, errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// created.json: the simulators and AVDs `device create` made.

// CreatedRecord is one entry of <home>/created.json. ID is the simulator's
// UDID, or the AVD's name (an emulator's serial changes with every boot).
type CreatedRecord struct {
	Platform  string `json:"platform"`
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	CreatedBy string `json:"createdBy"`
	CreatedAt string `json:"createdAt,omitempty"`
	Daemon    string `json:"daemon,omitempty"`
}

type createdRecords []CreatedRecord

// createdMu serialises this process's read-modify-write of created.json.
var createdMu sync.Mutex

// CreatedPath is created.json under Home(), shared by every daemon of that
// home and kept across restarts.
func CreatedPath() string { return filepath.Join(Home(), createdFile) }

func loadCreated() createdRecords {
	data, err := os.ReadFile(CreatedPath())
	if err != nil {
		return nil
	}
	var recs createdRecords
	if err := json.Unmarshal(data, &recs); err != nil {
		logger.Warn("created.json: %v", err)
		return nil
	}
	return recs
}

func (c createdRecords) has(platform, id string) bool {
	if id == "" {
		return false
	}
	for _, rec := range c {
		if rec.Platform == platform && rec.ID == id {
			return true
		}
	}
	return false
}

// recordCreated adds a record, replacing any with the same platform and id.
func recordCreated(rec CreatedRecord) error {
	createdMu.Lock()
	defer createdMu.Unlock()
	recs := loadCreated()
	out := createdRecords{}
	for _, r := range recs {
		if r.Platform != rec.Platform || r.ID != rec.ID {
			out = append(out, r)
		}
	}
	rec.CreatedBy = CreatedByMaestroD
	if rec.CreatedAt == "" {
		rec.CreatedAt = nowRFC3339()
	}
	out = append(out, rec)
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	return atomicWriteJSON(CreatedPath(), out)
}

// forgetCreated drops a record.
func forgetCreated(platform, id string) error {
	createdMu.Lock()
	defer createdMu.Unlock()
	recs := loadCreated()
	if !recs.has(platform, id) {
		return nil
	}
	out := createdRecords{}
	for _, r := range recs {
		if r.Platform != platform || r.ID != id {
			out = append(out, r)
		}
	}
	return atomicWriteJSON(CreatedPath(), out)
}

// createdKey is the created.json id of a listed device: a simulator's UDID,
// or the AVD name behind avd:<name> and behind a running emulator.
func createdKey(d DeviceInfo) string {
	switch {
	case d.Platform == "ios" && d.Kind == "simulator":
		return d.ID
	case d.Platform == "android" && strings.HasPrefix(d.ID, AVDPrefix):
		return strings.TrimPrefix(d.ID, AVDPrefix)
	case d.Platform == "android" && emulator.IsEmulator(d.ID):
		return d.Name
	}
	return ""
}
