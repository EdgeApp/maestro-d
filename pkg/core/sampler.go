package core

// maestro-d: the probes behind startSampler and stopSampler. A driver that
// can measure an app implements SampleProber; the executor runs the probe
// on a ticker and summarises what it read.

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sampler kinds (flow.SamplerCPU, …) and the unit each one's values are in.
const (
	SampleCPU    = "cpu"
	SampleMemory = "memory"
	SampleFrames = "frames"
)

// SampleUnits names each kind's unit.
var SampleUnits = map[string]string{SampleCPU: "percent", SampleMemory: "bytes", SampleFrames: "fps"}

// SampleFunc reads one sample. ErrNoSample means there is nothing to
// record this time (the app is not running, or a new process just started
// and has no baseline yet).
type SampleFunc func() (float64, error)

// ErrNoSample: the probe has no value this time.
var ErrNoSample = errors.New("no sample")

// SampleProber is a driver that can measure an app's cpu, memory or
// frames. SampleProbe returns the probe, or an *UnsupportedError when the
// device cannot be measured that way.
type SampleProber interface {
	SampleProbe(kind, appID string) (SampleFunc, error)
}

// UnsupportedError is something a driver's device cannot do (a
// measurement, a command). The daemon answers it with UNSUPPORTED.
type UnsupportedError struct{ Msg string }

func (e *UnsupportedError) Error() string { return e.Msg }

// UnsupportedResult is a failed step carrying an *UnsupportedError, which
// the daemon answers with UNSUPPORTED.
func UnsupportedResult(msg string) *CommandResult {
	return &CommandResult{Success: false, Error: &UnsupportedError{Msg: msg}, Message: msg}
}

// sampleNow is time.Now, a variable so tests control elapsed time.
var sampleNow = time.Now

// AndroidSampleProbe measures appID (a package) through the device shell:
// cpu from /proc/<pid>/stat, memory from dumpsys meminfo's TOTAL PSS,
// frames from dumpsys gfxinfo's rendered-frame count. cpu and frames are
// rates, so the probe takes its baseline now and each call answers the
// rate since the call before.
func AndroidSampleProbe(shell func(string) (string, error), kind, appID string) (SampleFunc, error) {
	if shell == nil {
		return nil, &UnsupportedError{Msg: "sampling needs an adb shell to the device, which this driver has not got"}
	}
	switch kind {
	case SampleMemory:
		return func() (float64, error) {
			out, err := shell("dumpsys meminfo " + ShellQuote(appID))
			if err != nil {
				return 0, err
			}
			kb, ok := ParseMeminfoPSS(out)
			if !ok {
				return 0, ErrNoSample
			}
			return float64(kb) * 1024, nil
		}, nil
	case SampleCPU:
		hz := clockTicks(shell)
		read := func() (string, float64, bool) {
			out, err := shell("pidof " + ShellQuote(appID))
			pids := strings.Fields(out)
			if err != nil || len(pids) == 0 {
				return "", 0, false
			}
			pid := pids[0]
			stat, err := shell("cat /proc/" + pid + "/stat")
			if err != nil {
				return "", 0, false
			}
			ticks, ok := ParseProcStatTicks(stat)
			return pid, ticks, ok
		}
		return rateProbe(read, func(delta, sec float64) float64 { return delta / hz / sec * 100 }), nil
	case SampleFrames:
		read := func() (string, float64, bool) {
			out, err := shell("dumpsys gfxinfo " + ShellQuote(appID))
			if err != nil {
				return "", 0, false
			}
			n, pid, ok := ParseGfxinfoFrames(out)
			return pid, n, ok
		}
		return rateProbe(read, func(delta, sec float64) float64 { return delta / sec }), nil
	}
	return nil, fmt.Errorf("unknown sampler kind %q", kind)
}

// rateProbe turns a counter read (the process it belongs to, the counter,
// whether it was read) into a per-second rate since the previous read. A
// new process restarts the baseline.
func rateProbe(read func() (string, float64, bool), rate func(delta, sec float64) float64) SampleFunc {
	lastPid, last, ok := read()
	lastAt := sampleNow()
	have := ok
	return func() (float64, error) {
		pid, v, ok := read()
		at := sampleNow()
		if !ok {
			have = false
			return 0, ErrNoSample
		}
		prevPid, prev, prevAt, prevHave := lastPid, last, lastAt, have
		lastPid, last, lastAt, have = pid, v, at, true
		sec := at.Sub(prevAt).Seconds()
		if !prevHave || pid != prevPid || v < prev || sec <= 0 {
			return 0, ErrNoSample
		}
		return rate(v-prev, sec), nil
	}
}

// clockTicks is the device's USER_HZ, 100 unless getconf says otherwise.
func clockTicks(shell func(string) (string, error)) float64 {
	if out, err := shell("getconf CLK_TCK"); err == nil {
		if n, err := strconv.ParseFloat(strings.TrimSpace(out), 64); err == nil && n > 0 {
			return n
		}
	}
	return 100
}

var (
	meminfoSummary = regexp.MustCompile(`TOTAL PSS:\s+(\d+)`)
	meminfoRow     = regexp.MustCompile(`(?m)^\s*TOTAL\s+(\d+)`)
	gfxFrames      = regexp.MustCompile(`Total frames rendered:\s*(\d+)`)
	gfxPid         = regexp.MustCompile(`\(pid (\d+)\)`)
)

// ParseMeminfoPSS reads dumpsys meminfo's TOTAL PSS in kilobytes, from the
// App Summary or, on an older Android, the TOTAL row.
func ParseMeminfoPSS(out string) (int64, bool) {
	if strings.Contains(out, "No process found") {
		return 0, false
	}
	m := meminfoSummary.FindStringSubmatch(out)
	if m == nil {
		m = meminfoRow.FindStringSubmatch(out)
	}
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	return n, err == nil
}

// ParseProcStatTicks reads utime+stime from a /proc/<pid>/stat line. The
// command name (field 2) may hold spaces, so fields count from its ")".
func ParseProcStatTicks(stat string) (float64, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(stat[i+1:])
	// After ")": state is field 3, so utime (14) and stime (15) are f[11], f[12].
	if len(f) < 13 {
		return 0, false
	}
	u, err1 := strconv.ParseFloat(f[11], 64)
	s, err2 := strconv.ParseFloat(f[12], 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return u + s, true
}

// ParseGfxinfoFrames reads dumpsys gfxinfo's rendered-frame count and the
// pid it reports.
func ParseGfxinfoFrames(out string) (float64, string, bool) {
	m := gfxFrames.FindStringSubmatch(out)
	if m == nil {
		return 0, "", false
	}
	n, _ := strconv.ParseFloat(m[1], 64) // digits only, so it parses
	pid := ""
	if p := gfxPid.FindStringSubmatch(out); p != nil {
		pid = p[1]
	}
	return n, pid, true
}

// RunHost runs a command on this host. A variable so tests answer it.
var RunHost = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// IOSSampleProbe measures appID (a bundle id). On a simulator the app is
// a process of this Mac, so cpu (ps's %cpu) and memory (its resident size)
// come from ps; the pid from the simulator's launchd. A physical device's
// processes are not readable without Instruments, and no frame counter is
// readable on either, so those answer *UnsupportedError.
func IOSSampleProbe(udid string, simulator bool, kind, appID string) (SampleFunc, error) {
	if kind == SampleFrames {
		return nil, &UnsupportedError{Msg: "frames sampling is not available on iOS: neither a simulator nor a device exposes a frame counter maestro-d can read"}
	}
	if kind != SampleCPU && kind != SampleMemory {
		return nil, fmt.Errorf("unknown sampler kind %q", kind)
	}
	if !simulator {
		return nil, &UnsupportedError{Msg: kind + " sampling on a physical iOS device needs Instruments (xctrace), which maestro-d does not run; use a simulator"}
	}
	return func() (float64, error) {
		out, err := RunXcrun("simctl", "spawn", udid, "launchctl", "list")
		if err != nil {
			return 0, fmt.Errorf("simctl spawn launchctl list: %w", err)
		}
		pid, ok := ParseLaunchctlPid(string(out), appID)
		if !ok {
			return 0, ErrNoSample
		}
		ps, err := RunHost("ps", "-o", "%cpu=,rss=", "-p", pid)
		if err != nil {
			return 0, ErrNoSample // it exited between the two reads
		}
		cpu, rssKB, ok := ParsePsCPURSS(string(ps))
		if !ok {
			return 0, ErrNoSample
		}
		if kind == SampleCPU {
			return cpu, nil
		}
		return rssKB * 1024, nil
	}, nil
}

// ParseLaunchctlPid finds a running app's pid in `launchctl list` output:
// "PID\tStatus\tLabel" rows, the app's label UIKitApplication:<bundle>[…].
func ParseLaunchctlPid(out, bundle string) (string, bool) {
	prefix := "UIKitApplication:" + bundle + "["
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.HasPrefix(f[2], prefix) {
			continue
		}
		if _, err := strconv.Atoi(f[0]); err == nil {
			return f[0], true
		}
	}
	return "", false
}

// ParsePsCPURSS reads `ps -o %cpu=,rss=`: the cpu percentage and the
// resident size in kilobytes.
func ParsePsCPURSS(out string) (float64, float64, bool) {
	f := strings.Fields(out)
	if len(f) < 2 {
		return 0, 0, false
	}
	cpu, err1 := strconv.ParseFloat(f[0], 64)
	rss, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return cpu, rss, true
}
