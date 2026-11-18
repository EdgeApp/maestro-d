package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"syscall"
	"time"
)

// Runtime file layout, mirroring ~/.edge-cli/run/<profile>/:
//
//	~/.maestro-d/run/<name>/daemon.sock   unix socket (0600)
//	~/.maestro-d/run/<name>/daemon.json   Info: pid, apiVersion, socketPath, httpAddr, …
//	~/.maestro-d/run/<name>/devices.json  device registry (what we booted / attached)
//	~/.maestro-d/run/<name>/startup.log   child stdout/stderr
//	~/.maestro-d/run/<name>/reports/<device>/<ts>/
//
// MAESTRO_D_HOME overrides ~/.maestro-d.

const (
	DefaultName     = "default"
	EnvName         = "MAESTRO_D"
	EnvHome         = "MAESTRO_D_HOME"
	EnvDevice       = "MAESTRO_DEVICE"
	EnvBin          = "MAESTRO_D_BIN"
	sockFile        = "daemon.sock"
	infoFile        = "daemon.json"
	devicesFile     = "devices.json"
	startupLogFile  = "startup.log"
	reportsDirName  = "reports"
	DefaultIdleSecs = 1800
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ResolveName applies the `--daemon` → MAESTRO_D → "default" rule.
func ResolveName(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv(EnvName); v != "" {
		return v
	}
	return DefaultName
}

// ValidateName rejects names that would escape the run directory.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return Errorf(CodeUsage, "invalid daemon name %q: use letters, digits, '.', '_' or '-'", name)
	}
	return nil
}

// Home is the maestro-d state directory.
func Home() string {
	if v := os.Getenv(EnvHome); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "maestro-d")
	}
	return filepath.Join(home, ".maestro-d")
}

// RunRoot is the directory holding one run dir per daemon name.
func RunRoot() string { return filepath.Join(Home(), "run") }

// RunDir is the run directory for a daemon name.
func RunDir(name string) string { return filepath.Join(RunRoot(), name) }

// SocketPath is the unix socket path for a daemon name. macOS limits unix
// socket paths to 104 bytes, so a long home directory falls back to a
// short path under the temp dir; daemon.json records whichever was used.
func SocketPath(name string) string {
	p := filepath.Join(RunDir(name), sockFile)
	if len(p) < 100 {
		return p
	}
	return filepath.Join(os.TempDir(), "maestro-d-"+name+".sock")
}

// InfoPath is the daemon.json path.
func InfoPath(name string) string { return filepath.Join(RunDir(name), infoFile) }

// LockPath is the file a running daemon holds locked (daemon.lock).
func LockPath(name string) string { return filepath.Join(RunDir(name), "daemon.lock") }

// DevicesPath is the devices.json path.
func DevicesPath(name string) string { return filepath.Join(RunDir(name), devicesFile) }

// StartupLogPath is the startup.log path.
func StartupLogPath(name string) string { return filepath.Join(RunDir(name), startupLogFile) }

// ReportsDir is the default report root for a daemon name.
func ReportsDir(name string) string { return filepath.Join(RunDir(name), reportsDirName) }

// ReadInfo reads daemon.json. os.ErrNotExist when the daemon never
// started or cleaned up.
func ReadInfo(name string) (*Info, error) {
	data, err := os.ReadFile(InfoPath(name))
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parse %s: %w", InfoPath(name), err)
	}
	return &info, nil
}

// WriteInfo writes daemon.json atomically.
func WriteInfo(info *Info) error {
	if err := os.MkdirAll(RunDir(info.Name), 0o700); err != nil {
		return err
	}
	return atomicWriteJSON(InfoPath(info.Name), info)
}

// RemoveRunFiles deletes the socket, daemon.json and devices.json. The
// startup log and reports are kept.
func RemoveRunFiles(name string) {
	_ = os.Remove(SocketPath(name))
	_ = os.Remove(InfoPath(name))
	_ = os.Remove(DevicesPath(name))
}

// PIDAlive reports whether a process with pid exists and has not exited.
// On Linux a process that exited but was never reaped (a zombie, or one
// being reaped) still answers signal 0; its /proc stat state says it is
// gone. That is the case in a container whose PID 1 reaps nothing.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return !procExited(pid)
}

// procOS and procStat are seams over runtime.GOOS and reading
// /proc/<pid>/stat, for tests.
var (
	procOS   = runtime.GOOS
	procStat = func(pid int) ([]byte, error) {
		return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	}
)

// procExited reports whether Linux's /proc says pid has exited: state Z
// (zombie) or X (dead). Elsewhere, or when the file cannot be read, it
// reports false and signal 0's answer stands.
func procExited(pid int) bool {
	if procOS != "linux" {
		return false
	}
	data, err := procStat(pid)
	if err != nil {
		return false
	}
	// "pid (comm) S …": comm may hold spaces and parentheses, so the
	// state is the first field after the last ')'.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return false
	}
	fields := bytes.Fields(data[i+1:])
	if len(fields) == 0 {
		return false
	}
	switch string(fields[0]) {
	case "Z", "X":
		return true
	}
	return false
}

// LiveInfo reads daemon.json and checks the recorded pid is alive. It
// returns (nil, nil) when the daemon is not running; stale files are
// removed so the next spawn starts clean.
func LiveInfo(name string) (*Info, error) {
	info, err := ReadInfo(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !PIDAlive(info.PID) {
		RemoveRunFiles(name)
		return nil, nil
	}
	return info, nil
}

// DaemonEntry is one row of `maestro-d ps`.
type DaemonEntry struct {
	Name    string       `json:"name"`
	Info    *Info        `json:"info,omitempty"`
	Alive   bool         `json:"alive"`
	Devices []DeviceInfo `json:"devices,omitempty"`
}

// ListDaemons scans the run root. Entries whose pid is dead but whose
// devices.json still lists daemon-booted devices are reported with
// Alive=false so `device stop --orphans` can clean them up.
func ListDaemons() ([]DaemonEntry, error) {
	entries, err := os.ReadDir(RunRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []DaemonEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		info, err := ReadInfo(name)
		if err != nil {
			// No daemon.json: only interesting if devices.json remains.
			if devs, _ := ReadDevicesFile(name); len(devs) > 0 {
				out = append(out, DaemonEntry{Name: name, Devices: devs})
			}
			continue
		}
		alive := PIDAlive(info.PID)
		devs, _ := ReadDevicesFile(name)
		if !alive && len(devs) == 0 {
			RemoveRunFiles(name)
			continue
		}
		out = append(out, DaemonEntry{Name: name, Info: info, Alive: alive, Devices: devs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReadDevicesFile reads a daemon's devices.json.
func ReadDevicesFile(name string) ([]DeviceInfo, error) {
	data, err := os.ReadFile(DevicesPath(name))
	if err != nil {
		return nil, err
	}
	var devs []DeviceInfo
	if err := json.Unmarshal(data, &devs); err != nil {
		return nil, err
	}
	return devs, nil
}

// FindDeviceOwner scans every other live daemon's registry for a device
// that is attached there. It returns the owning daemon's entry or nil.
func FindDeviceOwner(deviceID, exceptName string) *DaemonEntry {
	daemons, _ := ListDaemons()
	for i := range daemons {
		d := &daemons[i]
		if d.Name == exceptName || !d.Alive {
			continue
		}
		for _, dev := range d.Devices {
			if dev.ID == deviceID && dev.Attached {
				return d
			}
		}
	}
	return nil
}

func atomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func nowRFC3339() string { return time.Now().Format(time.RFC3339) }
