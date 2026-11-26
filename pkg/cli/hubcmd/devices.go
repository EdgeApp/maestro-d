package hubcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
	"github.com/devicelab-dev/maestro-runner/pkg/hubd"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

var deviceFlags = common.FlagSet(outputFlags, []dflag{
	common.StringFlag("config", "Use this config.json", []string{"MAESTRO_D_CONFIG"}, func(o *daemonOpts, v string) { o.DeviceConfig = v }),
	common.StringFlag("description", "Set the device description (an empty value clears it)", nil, func(o *daemonOpts, v string) { o.description = &v }),
	common.BoolFlag("end-leases", "End this device's leases and cancel its reservations before removal", nil, func(o *daemonOpts, v bool) { o.endLeases = v }),
})

func devicesCommand(withArgs actionFactory) *cli.Command {
	return &cli.Command{Name: "devices", Usage: "List or edit the devices shared by this home's hub",
		ArgsUsage:       "[list | add ID [--description TEXT] | set ID --description TEXT | remove ID [--end-leases]]",
		Description:     common.FlagHelp("Uses the hub's local socket. While the hub is stopped, edits its config file directly.", deviceFlags),
		SkipFlagParsing: true, Action: withArgs(deviceFlags, runHubDevices)}
}

func devicesArgs(args []string, o *daemonOpts) (op, id string, err error) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	op = args[0]
	switch op {
	case "list":
		if len(args) != 1 {
			return "", "", daemon.Errorf(daemon.CodeUsage, "devices list takes no id")
		}
	case "add", "set", "remove":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			return "", "", daemon.Errorf(daemon.CodeUsage, "devices %s needs one device id", op)
		}
		id = args[1]
	default:
		return "", "", daemon.Errorf(daemon.CodeUsage, "unknown devices command %q; give list, add, set or remove", op)
	}
	if op == "set" && o.description == nil {
		return "", "", daemon.Errorf(daemon.CodeUsage, "devices set needs --description TEXT")
	}
	if o.description != nil && op != "set" && op != "add" {
		return "", "", daemon.Errorf(daemon.CodeUsage, "--description needs devices add or set")
	}
	if o.explicit["end-leases"] && op != "remove" {
		return "", "", daemon.Errorf(daemon.CodeUsage, "--end-leases needs devices remove")
	}
	return op, id, nil
}

func runHubDevices(ctx context.Context, o *daemonOpts, args []string) error {
	op, id, err := devicesArgs(args, o)
	if err != nil {
		return err
	}
	path, err := filepath.Abs(daemon.DeviceConfigPath(o.DeviceConfig))
	if err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}

	lock, err := fsage.TryHubLock(daemon.Home())
	if err == nil {
		defer fsage.UnlockFile(lock)
		return offlineDevices(o, op, id, path)
	}
	if !errors.Is(err, fsage.ErrLocked) {
		return daemon.WrapErr(daemon.CodeDaemonUnavailable, err)
	}
	c, listing, err := waitConfigSocket(ctx)
	if err != nil {
		return err
	}
	expected := ""
	if o.DeviceConfig != "" {
		expected = path
		if e := daemon.CheckHubConfigPath(expected, listing.Path); e != nil {
			return e
		}
	}
	switch op {
	case "list":
		printConfigDevices(o, listing)
	case "remove":
		if err := c.RemoveHubDevice(ctx, id, expected, o.endLeases); err != nil {
			return err
		}
		printDeviceEdit(o, id, nil)
	default:
		res, err := c.PutHubDevice(ctx, id, netshape.DeviceFields{Description: o.description}, expected)
		if err != nil {
			return err
		}
		printDeviceEdit(o, id, res)
	}
	return nil
}

// A starting hub holds hub.lock before publishing its socket. Wait for its
// record, including a long home's fallback address. Once a mutation is sent,
// never retry it against a different process or fall back to an offline edit.
func waitConfigSocket(ctx context.Context) (*daemon.Client, *daemon.HubConfigListing, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		client, err := daemon.DialHub()
		if err == nil && client != nil {
			var listing *daemon.HubConfigListing
			listing, err = client.HubDevices(ctx)
			if err == nil {
				return client, listing, nil
			}
			var e *daemon.Error
			if !errors.As(err, &e) || e.Code != daemon.CodeDaemonUnavailable {
				return nil, nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil, daemon.Errorf(daemon.CodeDaemonUnavailable, "hub.lock is held but the hub socket is unavailable: %v", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func offlineDevices(o *daemonOpts, op, id, path string) error {
	if id != "" {
		name := os.Getenv("MAESTRO_D_HUB_NAME")
		if name == "" {
			name = daemon.DefaultHubName()
		}
		var e *daemon.Error
		id, e = daemon.LocalConfigID(id, name)
		if e != nil {
			return e
		}
	}
	var deps daemon.Deps
	if o.deps != nil {
		deps = o.deps()
	}
	f, err := hubd.OpenOfflineConfig(path, deps)
	if err != nil {
		return err
	}
	var res *daemon.HubConfigPutResult
	switch op {
	case "list":
		printConfigDevices(o, f.Listing())
		return nil
	case "remove":
		err = f.Remove(id, o.endLeases)
	default:
		res, err = f.Put(id, netshape.DeviceFields{Description: o.description})
	}
	if err != nil {
		return err
	}
	w := os.Stdout
	if o.JSON {
		printDeviceEdit(o, id, res)
		w = os.Stderr
	}
	fmt.Fprintf(w, "hub not running; edited %s\n", path)
	return nil
}

func printConfigDevices(o *daemonOpts, listing *daemon.HubConfigListing) {
	if o.JSON {
		common.PrintJSON(os.Stdout, listing)
		return
	}
	fmt.Println(listing.Path)
	if listing.ConfigError != "" {
		fmt.Fprintln(os.Stderr, listing.ConfigError)
	}
	for _, d := range listing.Devices {
		fmt.Printf("%s\t%s\n", d.ID, d.Description)
	}
}

func printDeviceEdit(o *daemonOpts, id string, result *daemon.HubConfigPutResult) {
	if result != nil {
		if o.JSON {
			common.PrintJSON(os.Stdout, result)
		} else {
			fmt.Printf("%s\t%s\n", result.Device.ID, result.Device.Description)
		}
	} else if o.JSON {
		common.PrintJSON(os.Stdout, daemon.Envelope{OK: true})
	} else {
		fmt.Printf("removed %s\n", id)
	}
}
