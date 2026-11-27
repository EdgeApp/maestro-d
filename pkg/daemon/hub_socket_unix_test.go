//go:build !windows

package daemon

import (
	"os"
	"syscall"
	"testing"
)

type socketDirInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i socketDirInfo) Sys() any { return &i.stat }

func TestPrivateHubSocketDirRequiresCurrentOwner(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !privateHubSocketDir(info) {
		t.Fatal("private directory owned by the current user was refused")
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid ^= 1
	if privateHubSocketDir(socketDirInfo{info, stat}) {
		t.Fatal("accepted directory owned by a different user")
	}
	if err := os.Chmod(dir, 0750); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if privateHubSocketDir(info) {
		t.Fatal("accepted a shared directory")
	}
}
