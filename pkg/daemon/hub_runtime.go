package daemon

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const EnvHubBin = "MAESTRO_D_HUB_BIN"

// HubExecutable selects the hub executable for a process already running as a hub.
func HubExecutable() (string, error) {
	if bin := os.Getenv(EnvHubBin); bin != "" {
		return bin, nil
	}
	return os.Executable()
}

func hubSocketPath() (string, error) {
	dir := HubRunDir()
	if p := filepath.Join(dir, sockFile); len(p) < 100 {
		return p, nil
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	tmp := os.TempDir()
	if runtime.GOOS != "windows" {
		tmp = "/tmp"
	}
	dir = filepath.Join(tmp, fmt.Sprintf("maestro-hub-%x", sum[:16]))
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || (runtime.GOOS != "windows" && st.Mode().Perm() != 0700) {
		return "", fmt.Errorf("hub socket directory %s must be a private directory", dir)
	}
	return filepath.Join(dir, sockFile), nil
}

// DialHub only consults the dedicated hub record. Its recorded socket may
// be outside the run directory when the platform limits unix socket length.
func DialHub() (*Client, error) {
	info, err := readInfoAt(HubRunDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, WrapErr(CodeDaemonUnavailable, err)
	}
	if !PIDAlive(info.PID) {
		return nil, nil
	}
	c := NewUnixClient(info.SocketPath)
	c.Info, c.Name = info, info.Name
	return c, nil
}
