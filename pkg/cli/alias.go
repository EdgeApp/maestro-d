package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/urfave/cli/v2"
)

const hubAliasNotice = "maestro-d hub is now maestro-d-hub; this alias goes in 5.0"

func findHubBinary(executable string) (string, error) {
	name := "maestro-d-hub"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if override := os.Getenv(daemon.EnvHubBin); override != "" {
		return exec.LookPath(override)
	}
	for _, dir := range []string{filepath.Dir(executable), filepath.Join(daemon.Home(), "bin")} {
		if bin, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return bin, nil
		}
	}
	return exec.LookPath(name)
}

var hubAliasCommand = &cli.Command{Name: "hub", Usage: "Run maestro-d-hub", SkipFlagParsing: true, Action: func(c *cli.Context) error {
	args := c.Args().Slice()
	fmt.Fprintln(c.App.ErrWriter, hubAliasNotice)
	for _, arg := range args {
		key, _, _ := strings.Cut(arg, "=")
		if key == "--daemon" || key == "-n" {
			if args[0] == "start" && slices.Contains(args, "--foreground") {
				return common.FinishErr(daemon.Errorf(daemon.CodeUsage, "--daemon was removed from hubs. To update a 3.x LaunchAgent, run maestro-d-hub service install"), false)
			}
			return common.FinishErr(daemon.Errorf(daemon.CodeUsage, "--daemon was removed from hubs; use a separate MAESTRO_D_HOME"), false)
		}
	}
	exe, err := os.Executable()
	if err == nil {
		var bin string
		bin, err = findHubBinary(exe)
		if err == nil {
			return runHubBinary(bin, args)
		}
	}
	return common.FinishErr(daemon.Errorf(daemon.CodeUsage, "install maestro-d-hub beside maestro-d or set MAESTRO_D_HUB_BIN: %v", err), false)
}}
