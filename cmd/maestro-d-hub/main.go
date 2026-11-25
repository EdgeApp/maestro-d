package main

import (
	"fmt"
	"os"

	client "github.com/devicelab-dev/maestro-runner/pkg/cli"
	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/cli/hubcmd"
	"github.com/devicelab-dev/maestro-runner/pkg/version"
	"github.com/urfave/cli/v2"
)

func main() {
	app := &cli.App{Name: "maestro-d-hub", Usage: "Share devices and manage the local hub", Version: version.Version,
		Commands: append(hubcmd.Commands(client.HubDriverDeps), common.VersionCommand())}
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
