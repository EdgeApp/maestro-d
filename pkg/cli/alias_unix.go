//go:build !windows

package cli

import (
	"os"
	"syscall"
)

func runHubBinary(bin string, args []string) error {
	return syscall.Exec(bin, append([]string{bin}, args...), os.Environ())
}
