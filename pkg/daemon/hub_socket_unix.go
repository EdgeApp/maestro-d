//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

func privateHubSocketDir(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.IsDir() && info.Mode().Perm() == 0700 && ok && uint64(stat.Uid) == uint64(os.Geteuid())
}
