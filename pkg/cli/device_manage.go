package cli

// device_manage.go: `device catalog`, `device create` and `device delete`,
// which manage iOS simulators and Android AVDs through the daemon.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/urfave/cli/v2"
)

// deviceManageOpts are the flags of the device management commands.
type deviceManageOpts struct {
	spec      daemon.CreateSpec
	noBoot    bool
	available bool
}

func manageStr(name, usage string, set func(s *daemon.CreateSpec, v string)) dflag {
	return strFlag(name, usage, nil, func(o *daemonOpts, v string) { set(&o.manage.spec, v) })
}

var (
	deviceCatalogFlags = flagSet(daemonSelectFlags, daemonSpawnFlags[:1], attachFlags[:1], []dflag{
		boolFlag("available", "Also list Android system images that sdkmanager can download", nil,
			func(o *daemonOpts, v bool) { o.manage.available = v }),
	})
	deviceCreateFlags = flagSet(daemonSelectFlags, daemonSpawnFlags[:1], attachFlags[:1], []dflag{
		manageStr("name", "Name of the new simulator or AVD (required)", func(s *daemon.CreateSpec, v string) { s.Name = v }),
		manageStr("type", "iOS: device type name or id (default: the newest iPhone the runtime supports)", func(s *daemon.CreateSpec, v string) { s.Type = v }),
		manageStr("runtime", "iOS: runtime, e.g. iOS-26-5 or 26.5 (default: the newest installed)", func(s *daemon.CreateSpec, v string) { s.Runtime = v }),
		manageStr("image", "Android: system image package (default: the newest installed google_apis image for this host's ABI)", func(s *daemon.CreateSpec, v string) { s.Image = v }),
		manageStr("profile", "Android: hardware profile (default pixel_8)", func(s *daemon.CreateSpec, v string) { s.Profile = v }),
		manageStr("data-size", "Android: data partition size (default 6G, at most 10G)", func(s *daemon.CreateSpec, v string) { s.DataSize = v }),
		{name: "boot", kind: kindBool, value: "true", usage: "Boot the device once created (default true; --no-boot only creates it)",
			apply: func(o *daemonOpts, v string) error {
				o.manage.noBoot = v == "false"
				return nil
			}},
		boolFlag("install-image", "Android: download the system image with sdkmanager when it is not installed (needs --accept-licenses)", nil,
			func(o *daemonOpts, v bool) { o.manage.spec.InstallImage = v }),
		boolFlag("accept-licenses", "Android: accept the SDK licenses for an image --install-image downloads", nil,
			func(o *daemonOpts, v bool) { o.manage.spec.AcceptLicenses = v }),
		intFlag("boot-timeout", "Seconds to wait for boot (default 120)", nil, func(o *daemonOpts, v int) { o.StartTimeout = v }),
	})
	deviceDeleteFlags = flagSet(daemonSelectFlags, daemonSpawnFlags[:1], []dflag{
		boolFlag("force", "Delete even if maestro-d did not create it", nil, func(o *daemonOpts, v bool) { o.force = v }),
	})
)

// connected wraps an action that needs the daemon, spawning it.
func connected(fn func(ctx context.Context, c *daemon.Client, o *daemonOpts, args []string, w io.Writer) error) func(ctx context.Context, o *daemonOpts, args []string) error {
	return func(ctx context.Context, o *daemonOpts, args []string) error {
		client, err := connectDaemon(ctx, o, true)
		if err != nil {
			return err
		}
		return fn(ctx, client, o, args, os.Stdout)
	}
}

var deviceCatalogCommand = &cli.Command{
	Name:      "catalog",
	Usage:     "List the iOS runtimes and device types, and the Android images and profiles, that device create can use",
	ArgsUsage: " ",
	Description: flagsDoc(`iOS comes from simctl; Android from sdkmanager and avdmanager (found under
ANDROID_HOME / ANDROID_SDK_ROOT, else on PATH). Android images are the
installed ones unless --available.

  maestro-d device catalog --platform ios
  maestro-d device catalog --platform android --available --json`, deviceCatalogFlags),
	SkipFlagParsing: true,
	Action:          withArgs(deviceCatalogFlags, connected(runDeviceCatalog)),
}

var deviceCreateCommand = &cli.Command{
	Name:      "create",
	Usage:     "Create an iOS simulator or an Android AVD, and boot it",
	ArgsUsage: " ",
	Description: flagsDoc(`Creates the device, records it in <home>/created.json (so device delete
accepts it without --force), and boots it unless --no-boot. A new AVD's
data partition is --data-size (written as disk.dataPartition.size).

  maestro-d device create --platform ios --name ci-phone --type "iPhone 16" --runtime iOS-26-5
  maestro-d device create --platform android --name ci-pixel --profile pixel_8 --data-size 8G --no-boot`, deviceCreateFlags),
	SkipFlagParsing: true,
	Action:          withArgs(deviceCreateFlags, connected(runDeviceCreate)),
}

var deviceDeleteCommand = &cli.Command{
	Name:      "delete",
	Usage:     "Delete a simulator or AVD (stopping it first)",
	ArgsUsage: "<id-or-name>",
	Description: flagsDoc(`Takes a UDID, a simulator name, avd:<name>, an AVD name or a running
emulator's serial. Only devices that device create made are deleted
without --force; anything else fails with DEVICE_EXTERNAL.`, deviceDeleteFlags),
	SkipFlagParsing: true,
	Action:          withArgs(deviceDeleteFlags, connected(runDeviceDelete)),
}

func runDeviceCatalog(ctx context.Context, c *daemon.Client, o *daemonOpts, args []string, w io.Writer) error {
	if len(args) != 0 {
		return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d device catalog [--platform ios|android] [--available]")
	}
	res, err := c.Catalog(ctx, o.Attach.Platform, o.manage.available)
	if err != nil {
		return err
	}
	if o.JSON {
		printJSON(w, res)
		return nil
	}
	printCatalog(w, res)
	return nil
}

func printCatalog(w io.Writer, res *daemon.CatalogResult) {
	if cat := res.IOS; cat != nil {
		fmt.Fprintln(w, "iOS runtimes")
		if len(cat.Runtimes) == 0 {
			fmt.Fprintln(w, "  (none installed)")
		}
		for _, r := range cat.Runtimes {
			note := ""
			if !r.Available {
				note = "  (unavailable)"
			}
			fmt.Fprintf(w, "  %-12s %s%s\n", r.Name, r.ID, note)
		}
		fmt.Fprintln(w, "iOS device types")
		for _, d := range cat.DeviceTypes {
			fmt.Fprintf(w, "  %-28s %s\n", d.Name, d.ID)
		}
	}
	if cat := res.Android; cat != nil {
		fmt.Fprintln(w, "Android system images")
		if len(cat.Images) == 0 {
			fmt.Fprintln(w, "  (none)")
		}
		for _, img := range cat.Images {
			state := "available"
			if img.Installed {
				state = "installed"
			}
			fmt.Fprintf(w, "  %-9s  %s\n", state, img.ID)
		}
		fmt.Fprintln(w, "Android profiles")
		fmt.Fprintf(w, "  %s\n", strings.Join(cat.Profiles, ", "))
	}
	for _, p := range []string{"ios", "android"} {
		if msg, ok := res.Errors[p]; ok {
			fmt.Fprintf(w, "%s: unavailable: %s\n", p, msg)
		}
	}
}

func runDeviceCreate(ctx context.Context, c *daemon.Client, o *daemonOpts, args []string, w io.Writer) error {
	if len(args) != 0 {
		return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d device create --platform ios|android --name NAME [flags]; unexpected %q", args[0])
	}
	spec := o.manage.spec
	boot := !o.manage.noBoot
	if !o.JSON {
		if boot {
			progress("Creating and booting %s …", spec.Name)
		} else {
			progress("Creating %s …", spec.Name)
		}
	}
	dev, err := c.CreateDevice(ctx, daemon.BootRequest{Platform: o.Attach.Platform, Name: spec.Name, Create: &spec,
		Boot: &boot, BootTimeout: o.StartTimeout})
	if err != nil {
		return err
	}
	if o.JSON {
		printJSON(w, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: *dev})
		return nil
	}
	if dev.Ready {
		fmt.Fprintf(w, "%s created and booted: %s (%s)\n", dev.ID, dev.Name, dev.Platform)
	} else {
		fmt.Fprintf(w, "%s created: %s (%s, shut down)\n", dev.ID, dev.Name, dev.Platform)
	}
	return nil
}

func runDeviceDelete(ctx context.Context, c *daemon.Client, o *daemonOpts, args []string, w io.Writer) error {
	if len(args) != 1 {
		return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d device delete <id-or-name> [--force]")
	}
	dev, err := c.DeleteDevice(ctx, args[0], o.force)
	if err != nil {
		return err
	}
	if o.JSON {
		printJSON(w, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: *dev})
		return nil
	}
	fmt.Fprintf(w, "%s deleted: %s (%s)\n", dev.ID, dev.Name, dev.Platform)
	return nil
}
