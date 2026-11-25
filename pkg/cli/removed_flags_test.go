package cli

import (
	"flag"
	"strings"
	"testing"

	ucli "github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/hubcmd"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestRemovedDeviceFlags(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	t.Setenv("MAESTRO_D_MOCK_DEVICES", "mock-1")
	deps := func() daemon.Deps {
		return daemon.Deps{ListDevices: func(string) []daemon.DeviceInfo {
			return []daemon.DeviceInfo{{ID: "emulator-5554", Kind: "emulator", Name: "Lab Pixel"}, {ID: "tv-1", Kind: "tv", Name: "Living Room"}}
		}}
	}
	hub := &ucli.App{Commands: hubcmd.Commands(deps)}
	service := &ucli.App{Commands: hub.Command("service").Subcommands}
	for _, cmd := range []*ucli.Command{daemonServeCommand, hub.Command("start"), service.Command("install")} {
		for _, tc := range []struct{ key, value, hint string }{
			{"--network-config", "x.json", "<id>"}, {"--share-device", "mock-1", "mock-1"}, {"--share-tv", "unlisted", "unlisted"}, {"MAESTRO_D_NETWORK_CONFIG", "x.json", "<id>"},
		} {
			t.Run(cmd.Name+"/"+tc.key, func(t *testing.T) {
				t.Setenv("MAESTRO_D_NETWORK_CONFIG", "")
				args := []string{tc.key, tc.value}
				if tc.key == "MAESTRO_D_NETWORK_CONFIG" {
					t.Setenv(tc.key, tc.value)
					args = nil
				}
				checkRemovedFlag(t, cmd, args, tc.key, tc.hint)
			})
		}
	}
	checkRemovedFlag(t, hub.Command("start"), []string{"--share-device=Lab Pixel"}, "--share-device", "avd:Lab Pixel")
	checkRemovedFlag(t, service.Command("install"), []string{"--share-tv", "Living Room"}, "--share-tv", "tv-1")
}

func checkRemovedFlag(t *testing.T, cmd *ucli.Command, args []string, key, hint string) {
	t.Helper()
	fs := flag.NewFlagSet(cmd.Name, flag.ContinueOnError)
	if err := fs.Parse(append([]string{"--"}, args...)); err != nil {
		t.Fatal(err)
	}
	ctx := ucli.NewContext(&ucli.App{}, fs, nil)
	ctx.Command = cmd
	var err error
	stderr := captureStderr(t, func() { err = cmd.Action(ctx) })
	if exit, ok := err.(ucli.ExitCoder); !ok || exit.ExitCode() != 2 || !strings.Contains(stderr, `"code": "USAGE"`) || !strings.Contains(stderr, key+" was removed in 4.0") || !strings.Contains(stderr, "maestro-d-hub devices add "+hint) {
		t.Fatalf("%s %v: %v\n%s", cmd.Name, args, err, stderr)
	}
}
