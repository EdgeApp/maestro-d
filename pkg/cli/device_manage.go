package cli

// device_manage.go: `device catalog`, `device create` and `device delete`,
// which manage iOS simulators and Android AVDs through the daemon.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/urfave/cli/v2"
)

// deviceManageOpts are the flags of the device management commands.
type deviceManageOpts struct {
	spec      daemon.CreateSpec
	noBoot    bool
	available bool
	// hub names the hub whose machine catalogs, creates or boots (--hub).
	hub string
	// query filters device list and device catalog (location, provider,
	// kind, name, OS range, available).
	query daemon.DeviceQuery
	// cloud is device create's cloud launch (--provider and the flags
	// after it); capsFile and idleRelease are read when it runs.
	cloud       cloudprov.CloudSpec
	capsFile    string
	idleRelease string
}

// cloudCreateFlags are device create's flags for a cloud device.
var cloudCreateFlags = []dflag{
	common.StringFlag("provider", "Launch a cloud device on this provider (devicefarm) instead of creating a simulator or AVD; --name is then the model (exact name, catalog id or regular expression)", nil,
		func(o *daemonOpts, v string) { o.manage.cloud.Provider = v }),
	common.StringFlag("os-version", "Cloud: the exact OS version", nil, func(o *daemonOpts, v string) { o.manage.cloud.OSVersion = v }),
	common.StringFlag("os-min", "Cloud: the lowest OS version", nil, func(o *daemonOpts, v string) { o.manage.cloud.OSVersionMin = v }),
	common.StringFlag("os-max", "Cloud: the highest OS version, at its own precision (17 admits 17.5)", nil, func(o *daemonOpts, v string) { o.manage.cloud.OSVersionMax = v }),
	common.StringFlag("app", "Cloud: an .apk or .ipa to install when the session starts (a path, or an upload ARN)", nil,
		func(o *daemonOpts, v string) { o.manage.cloud.App = v }),
	{Name: "auxiliary-app", Kind: common.Slice, Usage: "Cloud: another app to install, at most 3; repeatable",
		Apply: func(o *daemonOpts, v string) error {
			o.manage.cloud.AuxiliaryApps = append(o.manage.cloud.AuxiliaryApps, v)
			return nil
		}},
	common.StringFlag("app-id", "Cloud: the app's bundle id or package, the default for attach", nil, func(o *daemonOpts, v string) { o.manage.cloud.AppID = v }),
	common.StringFlag("appium-version", "Cloud: Appium 2 or 3 (default 3 on iOS 27 and later, else 2)", nil,
		func(o *daemonOpts, v string) { o.manage.cloud.AppiumVersion = v }),
	common.StringFlag("caps", "Cloud: a JSON file of capabilities merged into the WebDriver session's", nil, func(o *daemonOpts, v string) { o.manage.capsFile = v }),
	common.StringFlag("idle-release", "Cloud: release the device after this long without a request (Go duration or seconds; default 10m, 60s–150m)", nil,
		func(o *daemonOpts, v string) { o.manage.idleRelease = v }),
}

// cloudSpec builds a cloud launch from the flags: --name is the model,
// --boot-timeout the launch's boot timeout.
func cloudSpec(o *daemonOpts) (*cloudprov.CloudSpec, error) {
	spec := o.manage.cloud
	spec.Platform = o.Attach.Platform
	spec.Name = o.manage.spec.Name
	spec.BootTimeoutSec = o.StartTimeout
	if o.manage.idleRelease != "" {
		sec, err := durationSeconds(o.manage.idleRelease)
		if err != nil {
			return nil, daemon.Errorf(daemon.CodeUsage, "--idle-release %q: %v", o.manage.idleRelease, err)
		}
		spec.IdleReleaseSec = sec
	}
	if o.manage.capsFile != "" {
		data, err := os.ReadFile(o.manage.capsFile)
		if err != nil {
			return nil, daemon.Errorf(daemon.CodeUsage, "--caps: %v", err)
		}
		if err := json.Unmarshal(data, &spec.Caps); err != nil {
			return nil, daemon.Errorf(daemon.CodeUsage, "--caps %s: %v", o.manage.capsFile, err)
		}
	}
	abs := func(p string) string {
		if p == "" || strings.HasPrefix(p, "arn:") || strings.HasPrefix(p, "blob:") {
			return p
		}
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	spec.App = abs(spec.App)
	for i := range spec.AuxiliaryApps {
		spec.AuxiliaryApps[i] = abs(spec.AuxiliaryApps[i])
	}
	return &spec, nil
}

// durationSeconds reads a Go duration ("30m") or whole seconds ("1800").
// An explicit value under one second is an error: the daemon reads 0 as
// "the default", which would hide a typo such as 0 or 500ms.
func durationSeconds(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		d, derr := time.ParseDuration(v)
		if derr != nil {
			return 0, fmt.Errorf("not a duration or a number of seconds")
		}
		n = int(d / time.Second)
	}
	if n < 1 {
		return 0, fmt.Errorf("must be at least one second")
	}
	return n, nil
}

// minutesNote is ", N device minutes" for a released cloud device.
func minutesNote(m *cloudprov.DeviceMinutes) string {
	if m == nil {
		return ""
	}
	if m.Estimated {
		return fmt.Sprintf(" (about %g device minutes so far; Device Farm also bills its cleanup, so the billed figure is higher: devicefarm sessions shows it once the session completes)", m.Total)
	}
	return fmt.Sprintf(" (%g device minutes, %g metered)", m.Total, m.Metered)
}

// queryFlags are the filters device list and device catalog share.
func queryFlags(kind bool) []dflag {
	out := []dflag{
		common.StringFlag("location", "Only devices in these locations: local, mesh, cloud (comma list)", nil, func(o *daemonOpts, v string) {
			o.manage.query.Location = strings.Split(v, ",")
		}),
		common.StringFlag("provider", "Only this cloud provider's devices (devicefarm)", nil, func(o *daemonOpts, v string) { o.manage.query.Provider = v }),
		common.StringFlag("name", "Only devices whose name or id matches this regular expression (case-insensitive)", nil,
			func(o *daemonOpts, v string) { o.manage.query.Name = v }),
		common.StringFlag("os-min", "Only devices whose OS version is at least this (e.g. 16)", nil, func(o *daemonOpts, v string) { o.manage.query.OSVersionMin = v }),
		common.StringFlag("os-max", "Only devices whose OS version is at most this, at its own precision (17 admits 17.5)", nil,
			func(o *daemonOpts, v string) { o.manage.query.OSVersionMax = v }),
	}
	if kind {
		out = append(out, common.StringFlag("kind", "Only this kind: device, emulator, simulator, tv, cloud", nil,
			func(o *daemonOpts, v string) { o.manage.query.Kind = v }))
	}
	return out
}

// manageHubFlag is --hub on device catalog, create and start.
var manageHubFlag = common.StringFlag("hub", "On a hub: the hub whose machine does it (default: the hub you talk to)", nil,
	func(o *daemonOpts, v string) { o.manage.hub = v })

func manageStr(name, usage string, set func(s *daemon.CreateSpec, v string)) dflag {
	return common.StringFlag(name, usage, nil, func(o *daemonOpts, v string) { set(&o.manage.spec, v) })
}

var (
	deviceCatalogFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags[:1], attachFlags[:1], targetFlags, common.One(manageHubFlag), queryFlags(false), []dflag{
		common.BoolFlag("available", "Also list Android system images that sdkmanager can download; with --location cloud, only models available now", nil,
			func(o *daemonOpts, v bool) { o.manage.available = v }),
	})
	deviceCreateFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags[:1], attachFlags[:1], targetFlags, common.One(manageHubFlag), []dflag{
		manageStr("name", "Name of the new simulator or AVD (required)", func(s *daemon.CreateSpec, v string) { s.Name = v }),
		manageStr("type", "iOS: device type name or id (default: the newest iPhone the runtime supports)", func(s *daemon.CreateSpec, v string) { s.Type = v }),
		manageStr("runtime", "iOS: runtime, e.g. iOS-26-5 or 26.5 (default: the newest installed)", func(s *daemon.CreateSpec, v string) { s.Runtime = v }),
		manageStr("image", "Android: system image package (default: the newest installed google_apis image for this host's ABI)", func(s *daemon.CreateSpec, v string) { s.Image = v }),
		manageStr("profile", "Android: hardware profile (default pixel_8)", func(s *daemon.CreateSpec, v string) { s.Profile = v }),
		manageStr("data-size", "Android: data partition size (default 6G, at most 10G)", func(s *daemon.CreateSpec, v string) { s.DataSize = v }),
		{Name: "boot", Kind: common.Bool, Value: "true", Usage: "Boot the device once created (default true; --no-boot only creates it)",
			Apply: func(o *daemonOpts, v string) error {
				o.manage.noBoot = v == "false"
				return nil
			}},
		common.BoolFlag("install-image", "Android: download the system image with sdkmanager when it is not installed (needs --accept-licenses)", nil,
			func(o *daemonOpts, v bool) { o.manage.spec.InstallImage = v }),
		common.BoolFlag("accept-licenses", "Android: accept the SDK licenses for an image --install-image downloads", nil,
			func(o *daemonOpts, v bool) { o.manage.spec.AcceptLicenses = v }),
		common.IntFlag("boot-timeout", "Seconds to wait for boot (default 120; a cloud device's session, default 900)", nil, func(o *daemonOpts, v int) { o.StartTimeout = v }),
	}, cloudCreateFlags)
	deviceDeleteFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags[:1], targetFlags, []dflag{
		common.BoolFlag("force", "Delete even if maestro-d did not create it", nil, func(o *daemonOpts, v bool) { o.force = v }),
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
	Description: common.FlagHelp(`iOS comes from simctl; Android from sdkmanager and avdmanager (found under
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
	Description: common.FlagHelp(`Creates the device, records it in <home>/created.json (so device delete
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
	Description: common.FlagHelp(`Takes a UDID, a simulator name, avd:<name>, an AVD name or a running
emulator's serial. Only devices that device create made are deleted
without --force; anything else fails with DEVICE_EXTERNAL.`, deviceDeleteFlags),
	SkipFlagParsing: true,
	Action:          withArgs(deviceDeleteFlags, connected(runDeviceDelete)),
}

func runDeviceCatalog(ctx context.Context, c *daemon.Client, o *daemonOpts, args []string, w io.Writer) error {
	if len(args) != 0 {
		return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d device catalog [--platform ios|android] [--available] [--location cloud …]")
	}
	q := o.manage.query
	q.Platform, q.Available, q.Hub = o.Attach.Platform, o.manage.available, o.manage.hub
	res, err := c.CatalogQuery(ctx, q)
	if err != nil {
		return err
	}
	if o.JSON {
		common.PrintJSON(w, res)
		return nil
	}
	printCatalog(w, res)
	return nil
}

func printCatalog(w io.Writer, res *daemon.CatalogResult) {
	if len(res.Cloud) > 0 {
		printCloudCatalog(w, res.Cloud)
	}
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
	if o.manage.cloud.Provider != "" {
		return runCloudCreate(ctx, c, o, w)
	}
	spec := o.manage.spec
	boot := !o.manage.noBoot
	if !o.JSON {
		if boot {
			common.Progress("Creating and booting %s …", spec.Name)
		} else {
			common.Progress("Creating %s …", spec.Name)
		}
	}
	dev, err := c.CreateDevice(ctx, daemon.BootRequest{Platform: o.Attach.Platform, Name: spec.Name, Create: &spec, Hub: o.manage.hub,
		Boot: &boot, BootTimeout: o.StartTimeout})
	if err != nil {
		return err
	}
	if o.JSON {
		common.PrintJSON(w, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: *dev})
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
		common.PrintJSON(w, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: *dev})
		return nil
	}
	fmt.Fprintf(w, "%s deleted: %s (%s)%s\n", dev.ID, dev.Name, dev.Platform, minutesNote(dev.DeviceMinutes))
	return nil
}

// runCloudCreate launches a cloud device on the daemon (direct mode).
func runCloudCreate(ctx context.Context, c *daemon.Client, o *daemonOpts, w io.Writer) error {
	spec, err := cloudSpec(o)
	if err != nil {
		return err
	}
	if !o.JSON {
		common.Progress("Launching a cloud device (%s, %s) on %s …", spec.Platform, firstNonEmpty(spec.Name, "any model"), spec.Provider)
	}
	dev, err := c.CreateDevice(ctx, daemon.BootRequest{Platform: spec.Platform, Create: &daemon.CreateSpec{Cloud: spec}})
	if err != nil {
		return err
	}
	if o.JSON {
		common.PrintJSON(w, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: *dev})
		return nil
	}
	fmt.Fprintf(w, "%s launched: %s %s (%s, %s); released after %ds idle, by %s at the latest\n",
		dev.ID, dev.Name, dev.OSVersion, dev.Platform, dev.Provider, dev.IdleReleaseSec, dev.ExpiresAt)
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// printCloudCatalog prints the cloud providers' models, one row each.
func printCloudCatalog(w io.Writer, cats []daemon.CloudCatalog) {
	fmt.Fprintf(w, "%-11s %-14s %-40s %-8s %-28s %-8s %-7s %s\n", "PROVIDER", "HUB", "ID", "PLATFORM", "NAME", "OS", "FORM", "AVAILABILITY")
	n := 0
	for _, c := range cats {
		hub := c.Hub
		if hub == "" {
			hub = "-"
		}
		for _, m := range c.Models {
			n++
			fmt.Fprintf(w, "%-11s %-14s %-40s %-8s %-28s %-8s %-7s %s\n", c.Provider, hub, m.ID, m.Platform, m.Name, m.OSVersion, m.FormFactor, m.Availability)
		}
	}
	if n == 0 {
		fmt.Fprintln(w, "  (no models match)")
	}
}
