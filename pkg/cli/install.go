package cli

// install.go: `maestro-d install FILE` and `maestro-d uninstall APP_ID`, and
// the daemon's install, uninstall and app-id hooks (daemon.Deps) behind
// `POST /v1/devices/{id}/apps` and `DELETE /v1/devices/{id}/apps/{appId}`.
// They use the installers `test` and `attach --app-file` use:
// AndroidDevice.Install (adb install -r -g), installViaSimctl and
// installIOSApp (devicectl, then go-ios zipconduit).

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/urfave/cli/v2"
	"howett.net/plist"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/device"
)

// appExecCommand is the seam over exec.Command for the app tools (aapt2,
// simctl, devicectl).
var appExecCommand = exec.CommandContext

// appIDOf reads an app file's id: CFBundleIdentifier from an .app's or an
// .ipa's Info.plist, or an .apk's package name through aapt2.
func appIDOf(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(strings.TrimRight(path, "/"))) {
	case ".app":
		data, err := os.ReadFile(filepath.Join(path, "Info.plist"))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
		}
		return bundleIDFromPlist(data)
	case ".ipa":
		return ipaBundleID(path)
	case ".apk":
		return apkPackage(path)
	}
	return "", fmt.Errorf("%s: not an .app, .ipa or .apk", filepath.Base(path))
}

func bundleIDFromPlist(data []byte) (string, error) {
	var info struct {
		BundleID string `plist:"CFBundleIdentifier"`
	}
	if _, err := plist.Unmarshal(data, &info); err != nil {
		return "", fmt.Errorf("Info.plist: %w", err)
	}
	if info.BundleID == "" {
		return "", fmt.Errorf("Info.plist has no CFBundleIdentifier")
	}
	return info.BundleID, nil
}

// ipaBundleID reads Payload/<name>.app/Info.plist from an .ipa.
func ipaBundleID(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		parts := strings.Split(f.Name, "/")
		if len(parts) == 3 && parts[0] == "Payload" && strings.HasSuffix(parts[1], ".app") && parts[2] == "Info.plist" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			data, err := io.ReadAll(io.LimitReader(rc, 4<<20))
			rc.Close()
			if err != nil {
				return "", err
			}
			return bundleIDFromPlist(data)
		}
	}
	return "", fmt.Errorf("%s has no Payload/*.app/Info.plist", filepath.Base(path))
}

// findAAPT2 returns aapt2 from the newest build-tools under the Android SDK,
// or "" when none is installed.
func findAAPT2() string {
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		sdk := os.Getenv(env)
		if sdk == "" {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(sdk, "build-tools"))
		if err != nil {
			continue
		}
		var versions []string
		for _, e := range entries {
			if e.IsDir() {
				versions = append(versions, e.Name())
			}
		}
		sort.Slice(versions, func(i, j int) bool { return versionLess(versions[j], versions[i]) })
		for _, v := range versions {
			p := filepath.Join(sdk, "build-tools", v, "aapt2")
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return ""
}

// versionLess compares dotted versions numerically (34.0.0 < 35.0.1).
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			fmt.Sscanf(as[i], "%d", &x)
		}
		if i < len(bs) {
			fmt.Sscanf(bs[i], "%d", &y)
		}
		if x != y {
			return x < y
		}
	}
	return a < b
}

// apkPackage runs `aapt2 dump packagename`.
func apkPackage(path string) (string, error) {
	aapt2 := findAAPT2()
	if aapt2 == "" {
		return "", fmt.Errorf("no aapt2 under $ANDROID_HOME/build-tools: the app id is not reported")
	}
	out, err := appExecCommand(context.Background(), aapt2, "dump", "packagename", path).Output()
	if err != nil {
		return "", fmt.Errorf("aapt2 dump packagename: %w", err)
	}
	pkg := strings.TrimSpace(string(bytes.SplitN(out, []byte("\n"), 2)[0]))
	if pkg == "" {
		return "", fmt.Errorf("aapt2 printed no package name")
	}
	return pkg, nil
}

// Hooks the daemon calls; variables so tests replace the device tools.
var (
	iosInstall     = installIOSApp
	isIOSSim       = isIOSSimulator
	androidInstall = func(serial, path string) error {
		d, err := device.New(serial)
		if err != nil {
			return err
		}
		return d.Install(path)
	}
	androidUninstall = func(serial, pkg string) error {
		d, err := device.New(serial)
		if err != nil {
			return err
		}
		return d.Uninstall(pkg)
	}
)

// daemonInstallApp is daemon.Deps.InstallApp.
func daemonInstallApp(ctx context.Context, id, platform, path string) error {
	if platform == "" {
		platform = guessPlatform(id)
	}
	switch platform {
	case "android":
		return androidInstall(id, path)
	case "ios":
		return iosInstall(id, path, isIOSSim(id))
	case "mock":
		return nil // the mock driver's devices have nothing to install to
	}
	return fmt.Errorf("cannot install on %s: unknown platform %q", id, platform)
}

// daemonUninstallApp is daemon.Deps.UninstallApp.
func daemonUninstallApp(ctx context.Context, id, platform, appID string) error {
	if platform == "" {
		platform = guessPlatform(id)
	}
	switch platform {
	case "android":
		return androidUninstall(id, appID)
	case "ios":
		var cmd *exec.Cmd
		if isIOSSim(id) {
			cmd = appExecCommand(ctx, "xcrun", "simctl", "uninstall", id, appID)
		} else {
			cmd = appExecCommand(ctx, "xcrun", "devicectl", "device", "uninstall", "app", "--device", id, appID)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("uninstall %s: %w: %s", appID, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "mock":
		return nil
	}
	return fmt.Errorf("cannot uninstall from %s: unknown platform %q", id, platform)
}

// ---------------------------------------------------------------------------
// Commands

var installFlags = flagSet(daemonSelectFlags, daemonSpawnFlags, one(deviceFlag), targetFlags, []dflag{
	boolFlag("reinstall", "Uninstall the app first (when its id can be read)", nil, func(o *daemonOpts, v bool) { o.force = v }),
	strFlag("platform", "Platform of the device, when neither the file nor the device tells", nil, func(o *daemonOpts, v string) { o.Attach.Platform = v }, "p"),
})

var installCommand = &cli.Command{
	Name:      "install",
	Usage:     "Install an app (.apk, .app, .ipa) on a device",
	ArgsUsage: "FILE",
	Description: flagsDoc(`Installs with the same installers as attach --app-file, without attaching.
With --target-hub the file is uploaded first (skipped when the daemon
already has it) and installed on the device's machine.

  maestro-d install app.apk --device emulator-5554
  maestro-d install build/My.app --device <udid> --target-hub=hub:7433`, installFlags),
	SkipFlagParsing: true,
	Action: withArgs(installFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) != 1 {
			return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d install FILE --device ID")
		}
		client, err := connectDaemon(ctx, o, true)
		if err != nil {
			return err
		}
		id, err := resolveDevice(ctx, client, o)
		if err != nil {
			return err
		}
		app, err := filepath.Abs(args[0])
		if err != nil {
			return daemon.WrapErr(daemon.CodeUsage, err)
		}
		if _, err := os.Stat(app); err != nil {
			return daemon.Errorf(daemon.CodeUsage, "install: %v", err)
		}
		if o.Target != "" {
			u, err := uploadPath(ctx, client, id, app, o.JSON)
			if err != nil {
				return err
			}
			app = u.Ref
		}
		res, err := client.InstallApp(ctx, id, daemon.InstallRequest{App: app, Reinstall: o.force, Platform: o.Attach.Platform})
		if err != nil {
			return err
		}
		if o.JSON {
			printJSON(os.Stdout, res)
			return nil
		}
		if res.AppID != "" {
			fmt.Printf("Installed %s on %s\n", res.AppID, id)
		} else {
			fmt.Printf("Installed %s on %s\n", filepath.Base(args[0]), id)
		}
		return nil
	}),
}

var uninstallFlags = flagSet(daemonSelectFlags, daemonSpawnFlags, one(deviceFlag), targetFlags)

var uninstallCommand = &cli.Command{
	Name:            "uninstall",
	Usage:           "Uninstall an app from a device by bundle id or package name",
	ArgsUsage:       "APP_ID",
	Description:     flagsDoc("  maestro-d uninstall io.appium.settings --device emulator-5554", uninstallFlags),
	SkipFlagParsing: true,
	Action: withArgs(uninstallFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) != 1 {
			return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d uninstall APP_ID --device ID")
		}
		client, err := connectDaemon(ctx, o, true)
		if err != nil {
			return err
		}
		id, err := resolveDevice(ctx, client, o)
		if err != nil {
			return err
		}
		if err := client.UninstallApp(ctx, id, args[0]); err != nil {
			return err
		}
		if o.JSON {
			printJSON(os.Stdout, daemon.InstallResult{Envelope: daemon.Envelope{OK: true}, Device: id, AppID: args[0]})
			return nil
		}
		fmt.Printf("Uninstalled %s from %s\n", args[0], id)
		return nil
	}),
}
