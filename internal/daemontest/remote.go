package daemontest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// RemoteDriver is a held cloud driver: a mock driver (without its
// screen capture) with counted keepalives, restarts and closes.
type RemoteDriver struct {
	core.Driver
	Pings, Restarts, Closes atomic.Int32
	PingErr, RestartErr     atomic.Pointer[error]
	// pingStarted, when set, is closed by the first ping.
	InPing atomic.Bool
	// installChecks counts AppInstalled calls; the first notYet of them
	// answer false (a negative notYet: always), and checkErr fails them.
	InstallChecks, NotYet atomic.Int32
	CheckErr              atomic.Pointer[error]
	// restartFails makes that many restarts fail first; restartDelay
	// makes each restart take that long.
	RestartFails atomic.Int32
	RestartDelay atomic.Int64
	RestartedAt  atomic.Int64 // when the last restart finished (UnixNano)
}

func (d *RemoteDriver) Keepalive() error {
	d.InPing.Store(true)
	defer d.InPing.Store(false)
	d.Pings.Add(1)
	if p := d.PingErr.Load(); p != nil {
		return *p
	}
	return nil
}
func (d *RemoteDriver) RestartSession() error {
	d.Restarts.Add(1)
	time.Sleep(time.Duration(d.RestartDelay.Load()))
	defer func() { d.RestartedAt.Store(time.Now().UnixNano()) }()
	if p := d.RestartErr.Load(); p != nil {
		return *p
	}
	if d.RestartFails.Add(-1) >= 0 {
		return errors.New("UiAutomation not connected")
	}
	return nil
}
func (d *RemoteDriver) AppInstalled(string) (bool, error) {
	n := d.InstallChecks.Add(1)
	if p := d.CheckErr.Load(); p != nil {
		return false, *p
	}
	ny := d.NotYet.Load()
	return ny >= 0 && n > ny, nil
}
func (d *RemoteDriver) Close() error { d.Closes.Add(1); return nil }

// Execute answers recording steps as the Appium driver does: it has no
// screen capture.
func (d *RemoteDriver) Execute(step flow.Step) *core.CommandResult {
	switch step.(type) {
	case *flow.StartRecordingStep, *flow.StopRecordingStep:
		err := fmt.Errorf("unsupported step type: %T", step)
		return &core.CommandResult{Success: false, Error: err, Message: err.Error()}
	}
	return d.Driver.Execute(step)
}

// Cloud is an in-process provider whose Allocate runs the hooks and
// records what it did.
type Cloud struct {
	*Provider
	Mu         sync.Mutex
	Refs       []string
	Released   []string
	Abandoned  []string
	State      string // Refresh's answer (default RUNNING)
	RefreshErr error  // Refresh's error, when set
	Expires    time.Duration
	AllocErr   error
	ReleaseFn  func(ref string) error
	// failAfterCreated fails Allocate after the session was recorded,
	// without stopping it (a stop the provider refused); hold, when set,
	// blocks Allocate after recording until it is closed or the request
	// is cancelled.
	FailAfterCreated error
	Hold             chan struct{}
}

func NewCloud() *Cloud {
	return &Cloud{Provider: NewProvider(), State: "RUNNING", Expires: 150 * time.Minute}
}

func (f *Cloud) Allocate(ctx context.Context, spec cloudprov.CloudSpec, h cloudprov.AllocateHooks) (*cloudprov.Allocation, error) {
	f.RecordCall("Allocate")
	if f.AllocErr != nil {
		return nil, f.AllocErr
	}
	ref := "arn:aws:devicefarm:us-west-2:1:session:p/" + h.DeviceID + "/00000"
	if h.Progress != nil {
		h.Progress("PENDING")
	}
	if err := h.Created(ref); err != nil {
		f.Mu.Lock()
		f.Released = append(f.Released, ref)
		f.Mu.Unlock()
		h.Abandoned(ref)
		return nil, cloudprov.Errorf(cloudprov.Other, "cannot record the cloud session; it was stopped (%v)", err)
	}
	f.Mu.Lock()
	f.Refs = append(f.Refs, ref)
	f.Mu.Unlock()
	if f.FailAfterCreated != nil {
		return nil, f.FailAfterCreated
	}
	if f.Hold != nil {
		select {
		case <-f.Hold:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		h.Abandoned(ref)
		return nil, cloudprov.Errorf(cloudprov.Other, "cancelled")
	}
	now := time.Now()
	return &cloudprov.Allocation{Ref: ref, Endpoint: "https://example.invalid/remote-endpoint/SECRETTOKEN/wd/hub",
		Caps: map[string]any{"platformName": "Android"}, Started: now, Expires: now.Add(f.Expires),
		Device: cloudprov.DeviceDesc{Model: "Google Pixel 8", Platform: spec.Platform, OSVersion: "14", FormFactor: "phone", Manufacturer: "Google",
			DashboardURL: "https://console.aws.amazon.com/devicefarm/home#/x", AppiumVersion: "2"}}, nil
}

func (f *Cloud) Refresh(context.Context, *cloudprov.Allocation) (string, string, error) {
	f.RecordCall("Refresh")
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.RefreshErr != nil {
		return "", "", f.RefreshErr
	}
	return f.State, "", nil
}

func (f *Cloud) Release(_ context.Context, a *cloudprov.Allocation) (*cloudprov.ReleaseResult, error) {
	f.RecordCall("Release")
	if f.ReleaseFn != nil {
		if err := f.ReleaseFn(a.Ref); err != nil {
			return nil, err
		}
	}
	f.Mu.Lock()
	f.Released = append(f.Released, a.Ref)
	f.Mu.Unlock()
	return &cloudprov.ReleaseResult{Minutes: &cloudprov.DeviceMinutes{Metered: 2, Total: 2}}, nil
}

func (f *Cloud) ReleaseRef(_ context.Context, ref string) (*cloudprov.DeviceMinutes, error) {
	f.RecordCall("ReleaseRef")
	if f.ReleaseFn != nil {
		if err := f.ReleaseFn(ref); err != nil {
			return nil, err
		}
	}
	f.Mu.Lock()
	f.Released = append(f.Released, ref)
	f.Mu.Unlock()
	return &cloudprov.DeviceMinutes{Metered: 4, Total: 4, Estimated: true}, nil
}

func (f *Cloud) ReleasedRefs() []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]string(nil), f.Released...)
}
