package daemon

import (
	"context"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/emulator"
	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

// Deps are the pieces pkg/cli injects so this package never imports it.
type Deps struct {
	// NewDriver opens a driver on deviceID. cleanup tears it down (never
	// nil on success). Log output goes to the daemon's stdout.
	NewDriver func(ctx context.Context, deviceID string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error)
	// ListDevices discovers live devices; platform "" means all.
	ListDevices func(platform string) []DeviceInfo
	// NormalizeHierarchy turns a driver's raw hierarchy (XML or JSON) into
	// the cross-driver tree. Optional: without it the raw document is
	// returned parsed (JSON) or as a string (XML).
	NormalizeHierarchy func(raw []byte) (any, error)
	// Version is the binary version for daemon.json and reports.
	Version string

	// Device management: the catalog and the create and delete tools
	// behind `device catalog|create|delete`. A nil hook makes its
	// operation fail with INTERNAL.
	IOSCatalog         func() (*simulator.Catalog, error)
	AndroidCatalog     func(available bool) (*emulator.Catalog, error)
	CreateSimulator    func(name, deviceTypeID, runtimeID string) (string, error)
	CreateAVD          func(spec emulator.AVDSpec) error
	InstallSystemImage func(image string) error
	DeleteSimulator    func(udid string) error
	DeleteAVD          func(name string) error
}
