package daemon

import (
	"context"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
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

	// Apps: install a file (.apk, .app, .ipa) on a device, uninstall by id,
	// and read a file's app id (CFBundleIdentifier or package name).
	// platform may be "" when neither the file nor the registry tells.
	InstallApp   func(ctx context.Context, deviceID, platform, path string) error
	UninstallApp func(ctx context.Context, deviceID, platform, appID string) error
	AppID        func(path string) (string, error)

	// TVInfo reports whether id is a physical Apple TV this host has ever
	// listed, and its name. It answers from memory, never running devicectl,
	// and a TV once seen stays a TV, so the answer holds while devicectl
	// fails. nil means no TVs. A hub shares a TV only when --share-tv names
	// it.
	TVInfo func(id string) (name string, ok bool)

	// Providers maps a cloud provider's name ("devicefarm") to its
	// constructor (cloud.go). A plain daemon constructs a provider on first
	// use, from its own credentials; a hub is given only the providers it
	// was started with (`hub start --devicefarm`).
	Providers map[string]func() (cloudprov.RemoteProvider, error)
}
