//go:build darwin

package quicktime

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

const (
	// relaunchWindow: launchd may not start an app again within 10 s of
	// its previous launch, so a helper that does not connect after such a
	// launch is retried once, after the window.
	relaunchWindow = 10 * time.Second
	// maxSocketPath is the longest unix socket path macOS accepts
	// (sun_path is 104 bytes with its NUL).
	maxSocketPath = 103
	// lastLaunchFile records the latest helper launch of any maestro-d
	// process of this user, in the socket directory.
	lastLaunchFile = "last-launch"
)

// runCommand runs a command and returns its combined output (a seam the
// tests replace for open, xcrun, codesign and security).
var runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// socketPath is the Host's socket: <tmp>/maestro-d-cap-<uid>/<pid>.sock,
// or the same under /tmp when that is too long for a unix socket.
func socketPath(tmp string, uid, pid int) (dir, path string) {
	name := fmt.Sprintf("maestro-d-cap-%d", uid)
	sock := fmt.Sprintf("%d.sock", pid)
	dir = filepath.Join(tmp, name)
	path = filepath.Join(dir, sock)
	if len(path) > maxSocketPath {
		dir = filepath.Join("/tmp", name)
		path = filepath.Join(dir, sock)
	}
	return dir, path
}

// listenLocked opens the Host's socket once per process: its directory is
// 0700, the socket 0600.
func (h *Host) listenLocked() error {
	if h.ln != nil {
		return nil
	}
	dir, path := socketPath(h.cfg.tmpDir(), os.Getuid(), os.Getpid())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	h.ln, h.dir, h.sockPath = ln, dir, path
	go h.serve(ln)
	return nil
}

// launchArgs is the open(1) command line that starts the helper app as
// a new instance, in the background and hidden.
func launchArgs(app, socket string) []string {
	return []string{"-n", "-g", "-j", "-a", app, "--args", "--socket", socket, "--protocol", strconv.Itoa(ProtocolVersion)}
}

// launchHelper starts the helper through LaunchServices, which makes macOS
// attribute the Camera prompt to the helper app.
func launchHelper(app, socket string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := runCommand(ctx, "open", launchArgs(app, socket)...); err != nil {
		return fmt.Errorf("open %s: %w: %s", app, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func readLastLaunch(dir string) time.Time {
	b, err := os.ReadFile(filepath.Join(dir, lastLaunchFile))
	if err != nil {
		return time.Time{}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func writeLastLaunch(dir string, t time.Time) {
	f, err := os.CreateTemp(dir, ".last-launch-*")
	if err != nil {
		return
	}
	_, err = f.WriteString(strconv.FormatInt(t.UnixNano(), 10))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, lastLaunchFile))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
}

// launch builds (on first use) and launches the helper, and waits for its
// control connection. A helper launched within relaunchWindow of the
// previous launch that does not connect is launched once more.
func (h *Host) launch(hp *helper) {
	app, err := h.cfg.app()
	if err != nil {
		h.helperFailed(hp, &capture.Unavailable{Reason: capture.ReasonHelperFailed, Message: "build the capture helper: " + err.Error()})
		return
	}
	logger.Warn("screen capture switches every USB iOS device on this Mac into capture mode; other processes using those devices lose their connections now")
	for attempt := 0; ; attempt++ {
		prev := readLastLaunch(h.dir)
		at := h.cfg.now()
		writeLastLaunch(h.dir, at)
		if err := h.cfg.launch(app, h.sockPath); err != nil {
			h.helperFailed(hp, &capture.Unavailable{Reason: capture.ReasonHelperFailed, Message: err.Error()})
			return
		}
		t := time.NewTimer(h.cfg.connectTimeout)
		select {
		case <-hp.connected:
			t.Stop()
			return
		case <-t.C:
		}
		if attempt == 0 && !prev.IsZero() && at.Sub(prev) < relaunchWindow {
			if rest := relaunchWindow - h.cfg.now().Sub(prev); rest > 0 {
				h.cfg.sleep(rest)
			}
			logger.Warn("capture helper relaunched within 10 s of the previous launch; retrying once")
			continue
		}
		h.helperFailed(hp, &capture.Unavailable{Reason: capture.ReasonHelperFailed,
			Message: fmt.Sprintf("the capture helper did not connect within %s", h.cfg.connectTimeout)})
		return
	}
}
