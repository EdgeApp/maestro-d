package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SpawnOptions control how EnsureDaemon starts a daemon that isn't running.
type SpawnOptions struct {
	// NoSpawn fails with DAEMON_UNAVAILABLE instead of starting one.
	NoSpawn bool
	// IdleTimeout in seconds; 0 keeps the daemon's default, -1 disables.
	IdleTimeout int
	// HTTPAddr / Token are passed through to `serve`.
	HTTPAddr string
	Token    string
	// StartTimeout bounds the wait for READY (default 20s).
	StartTimeout time.Duration
	// ExtraArgs are appended to the serve command line.
	ExtraArgs []string
	// Args, when set, replace the whole command line (`hub start
	// --foreground …`); the spawn options above are then not added.
	Args []string
	// Env is added to the child's environment (a hub's token travels here
	// rather than on its command line).
	Env []string
	// Log receives progress lines ("Starting daemon …"); nil = silent.
	Log func(format string, args ...any)
}

// Binary returns the executable used to spawn daemons: MAESTRO_D_BIN
// or the current binary.
func Binary() (string, error) {
	if v := os.Getenv(EnvBin); v != "" {
		return v, nil
	}
	return os.Executable()
}

// EnsureDaemon returns a client for the named daemon, spawning it in the
// background when it isn't running. started reports whether this call
// spawned it.
func EnsureDaemon(ctx context.Context, name string, opts SpawnOptions) (c *Client, started bool, err error) {
	if err := ValidateName(name); err != nil {
		return nil, false, err
	}
	if c, err := Dial(name); err != nil {
		return nil, false, err
	} else if c != nil {
		if perr := c.Ping(3 * time.Second); perr == nil {
			return c, false, nil
		} else if opts.Log != nil {
			opts.Log("daemon %q (pid %d) is not answering: %v", name, c.Info.PID, perr)
		}
		// Recorded pid alive but socket dead: treat as stale.
		if !PIDAlive(c.Info.PID) {
			RemoveRunFiles(name)
		} else {
			return nil, false, Errorf(CodeDaemonUnavailable,
				"daemon %q (pid %d) exists but does not answer on %s; stop it with `maestro-d stop --daemon %s`",
				name, c.Info.PID, c.Info.SocketPath, name)
		}
	}
	if opts.NoSpawn {
		return nil, false, Errorf(CodeDaemonUnavailable, "daemon %q is not running", name)
	}
	c, err = spawn(ctx, name, opts)
	if err != nil {
		return nil, false, err
	}
	return c, true, nil
}

// Spawn starts a daemon in the background and waits until it answers, like
// EnsureDaemon does for one that is not running.
func Spawn(ctx context.Context, name string, opts SpawnOptions) (*Client, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	return spawn(ctx, name, opts)
}

func spawn(ctx context.Context, name string, opts SpawnOptions) (*Client, error) {
	bin, err := Binary()
	if err != nil {
		return nil, WrapErr(CodeDaemonUnavailable, err)
	}
	if err := os.MkdirAll(RunDir(name), 0o700); err != nil {
		return nil, WrapErr(CodeDaemonUnavailable, err)
	}
	logPath := StartupLogPath(name)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, WrapErr(CodeDaemonUnavailable, err)
	}
	defer logFile.Close()

	args := []string{"serve", "--daemon", name}
	switch {
	case opts.IdleTimeout < 0:
		args = append(args, "--idle-timeout", "0")
	case opts.IdleTimeout > 0:
		args = append(args, "--idle-timeout", strconv.Itoa(opts.IdleTimeout))
	}
	if opts.HTTPAddr != "" {
		args = append(args, "--http", opts.HTTPAddr)
	}
	if opts.Token != "" {
		args = append(args, "--token", opts.Token)
	}
	args = append(args, opts.ExtraArgs...)
	if len(opts.Args) > 0 {
		args = opts.Args
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), opts.Env...)
	// Own session so a Ctrl-C in the caller's terminal doesn't kill it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if opts.Log != nil {
		opts.Log("Starting daemon %q: %s %s", name, bin, strings.Join(args, " "))
	}
	if err := cmd.Start(); err != nil {
		return nil, Errorf(CodeDaemonUnavailable, "spawn %s: %v", bin, err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timeout := opts.StartTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case werr := <-exited:
			return nil, Errorf(CodeDaemonUnavailable, "daemon %q (pid %d) exited during startup: %v\n%s",
				name, pid, werr, tailFile(logPath, 20))
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, WrapErr(CodeInterrupted, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
		if info, _ := ReadInfo(name); info != nil && info.PID == pid {
			c := NewUnixClient(info.SocketPath)
			c.Name = name
			c.Info = info
			if err := c.Ping(2 * time.Second); err == nil {
				if opts.Log != nil {
					opts.Log("Daemon %q ready (pid %d, socket %s)", name, pid, info.SocketPath)
				}
				return c, nil
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, Errorf(CodeDaemonUnavailable, "daemon %q (pid %d) did not become ready within %v\n%s",
				name, pid, timeout, tailFile(logPath, 20))
		}
	}
}

// WaitForExit blocks until the daemon's pid is gone or timeout passes.
func WaitForExit(info *Info, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for PIDAlive(info.PID) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// tailFile returns the last n lines of a file for error messages.
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return fmt.Sprintf("--- %s ---\n%s", path, strings.Join(lines, "\n"))
}
