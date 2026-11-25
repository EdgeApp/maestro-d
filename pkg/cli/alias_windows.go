//go:build windows

package cli

import (
	"github.com/urfave/cli/v2"
	"os"
	"os/exec"
)

func runHubBinary(bin string, args []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if e, ok := err.(*exec.ExitError); ok {
		return cli.Exit("", e.ExitCode())
	}
	return err
}
