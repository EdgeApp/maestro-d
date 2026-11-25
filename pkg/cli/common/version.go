package common

import (
	"fmt"
	"github.com/devicelab-dev/maestro-runner/pkg/version"
	"github.com/urfave/cli/v2"
)

func VersionCommand() *cli.Command {
	return &cli.Command{Name: "version", Usage: "Print the build version", Action: func(c *cli.Context) error { _, err := fmt.Fprintln(c.App.Writer, version.Version); return err }}
}
