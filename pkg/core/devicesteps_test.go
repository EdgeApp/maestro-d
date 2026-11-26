package core

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAndroidStatusBarCommands(t *testing.T) {
	got, err := AndroidStatusBarCommands("demo", "9:41", 100)
	want := []string{
		"settings put global sysui_demo_allowed 1",
		"am broadcast -a com.android.systemui.demo -e command enter",
		"am broadcast -a com.android.systemui.demo -e command clock -e hhmm 0941",
		"am broadcast -a com.android.systemui.demo -e command battery -e level 100 -e plugged false",
		"am broadcast -a com.android.systemui.demo -e command network -e wifi show -e level 4 -e mobile show -e datatype none -e level 4",
		"am broadcast -a com.android.systemui.demo -e command notifications -e visible false",
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := AndroidStatusBarCommands("demo", "12:05", 7); got[2] != "am broadcast -a com.android.systemui.demo -e command clock -e hhmm 1205" || !strings.Contains(got[3], "level 7 ") {
		t.Fatalf("%q", got)
	}
	if got, _ := AndroidStatusBarCommands("live", "", 0); !reflect.DeepEqual(got, []string{"am broadcast -a com.android.systemui.demo -e command exit"}) {
		t.Fatalf("%q", got)
	}
	if _, err := AndroidStatusBarCommands("odd", "", 0); err == nil {
		t.Fatal("odd mode accepted")
	}
}

func TestRunAndroidStatusBar(t *testing.T) {
	var sent []string
	shell := func(c string) (string, error) { sent = append(sent, c); return "", nil }
	r := RunAndroidStatusBar(shell, "demo", "9:41", 100)
	if !r.Success || len(sent) != 6 || r.Data.(map[string]any)["time"] != "9:41" {
		t.Fatalf("%+v %q", r, sent)
	}
	if r := RunAndroidStatusBar(shell, "live", "9:41", 100); !r.Success || r.Message != "Status bar is live" {
		t.Fatalf("%+v", r)
	}
	fail := func(string) (string, error) { return "", errors.New("adb: device offline") }
	if r := RunAndroidStatusBar(fail, "demo", "9:41", 100); r.Success || !strings.Contains(r.Message, "device offline") {
		t.Fatalf("%+v", r)
	}
	if r := RunAndroidStatusBar(nil, "demo", "9:41", 100); r.Success || !strings.Contains(r.Message, "no adb shell") {
		t.Fatalf("%+v", r)
	}
	if r := RunAndroidStatusBar(shell, "odd", "", 0); r.Success {
		t.Fatalf("%+v", r)
	}
}

func stubXcrun(t *testing.T, out string, err error) *[][]string {
	t.Helper()
	var calls [][]string
	prev := RunXcrun
	RunXcrun = func(args ...string) ([]byte, error) { calls = append(calls, args); return []byte(out), err }
	t.Cleanup(func() { RunXcrun = prev })
	return &calls
}

func TestRunIOSSimulatorStatusBar(t *testing.T) {
	calls := stubXcrun(t, "", nil)
	if r := RunIOSSimulatorStatusBar("SIM-1", "demo", "9:41", 100); !r.Success {
		t.Fatalf("%+v", r)
	}
	if r := RunIOSSimulatorStatusBar("SIM-1", "live", "", 0); !r.Success {
		t.Fatalf("%+v", r)
	}
	want := [][]string{
		{"simctl", "status_bar", "SIM-1", "override", "--time", "9:41", "--batteryState", "charged", "--batteryLevel", "100",
			"--wifiBars", "3", "--cellularBars", "4", "--dataNetwork", "wifi"},
		{"simctl", "status_bar", "SIM-1", "clear"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("%q", *calls)
	}
	if r := RunIOSSimulatorStatusBar("", "demo", "9:41", 100); r.Success || !strings.Contains(r.Message, "UDID is unknown") {
		t.Fatalf("%+v", r)
	}
	if r := RunIOSSimulatorStatusBar("SIM-1", "odd", "", 0); r.Success {
		t.Fatalf("%+v", r)
	}
	stubXcrun(t, "Invalid device: SIM-1", errors.New("exit status 148"))
	if r := RunIOSSimulatorStatusBar("SIM-1", "demo", "9:41", 100); r.Success || !strings.Contains(r.Message, "Invalid device: SIM-1") {
		t.Fatalf("%+v", r)
	}
}

func TestKeyboardVisibleResultAndUnsupported(t *testing.T) {
	if r := KeyboardVisibleResult(true); !r.Success || r.Data.(map[string]any)["visible"] != true || r.Message != "Keyboard is visible" {
		t.Fatalf("%+v", r)
	}
	if r := KeyboardVisibleResult(false); r.Data.(map[string]any)["visible"] != false {
		t.Fatalf("%+v", r)
	}
	r := UnsupportedResult("nope")
	var u *UnsupportedError
	if r.Success || !errors.As(r.Error, &u) || u.Error() != "nope" {
		t.Fatalf("%+v", r)
	}
}

func TestParsers(t *testing.T) {
	if kb, ok := ParseMeminfoPSS("** MEMINFO in pid 123 [x] **\n App Summary\n TOTAL PSS:   45678   TOTAL RSS: 1"); !ok || kb != 45678 {
		t.Fatal(kb)
	}
	if kb, ok := ParseMeminfoPSS("        TOTAL    23456    1000\n"); !ok || kb != 23456 {
		t.Fatal(kb)
	}
	if _, ok := ParseMeminfoPSS("No process found for: x"); ok {
		t.Fatal("no process")
	}
	if _, ok := ParseMeminfoPSS("garbage"); ok {
		t.Fatal("garbage")
	}
	stat := "1234 (app name) S 1 2 3 4 5 6 7 8 9 10 250 50 0 0"
	if v, ok := ParseProcStatTicks(stat); !ok || v != 300 {
		t.Fatal(v)
	}
	for _, bad := range []string{"no paren", "1 (x) S 1 2", "1 (x) S 1 2 3 4 5 6 7 8 9 10 u s"} {
		if _, ok := ParseProcStatTicks(bad); ok {
			t.Fatal(bad)
		}
	}
	if n, pid, ok := ParseGfxinfoFrames("** Graphics info for pid 4321 [x] **\n(pid 4321)\nTotal frames rendered: 900\n"); !ok || n != 900 || pid != "4321" {
		t.Fatal(n, pid)
	}
	if n, pid, ok := ParseGfxinfoFrames("Total frames rendered: 5"); !ok || n != 5 || pid != "" {
		t.Fatal(n, pid)
	}
	if _, _, ok := ParseGfxinfoFrames("nothing"); ok {
		t.Fatal("nothing")
	}
	launchctl := "PID\tStatus\tLabel\n-\t0\tUIKitApplication:com.x[1][rb]\n812\t0\tUIKitApplication:com.example.app[a1b2][rb-legacy]\n9\t0\tcom.apple.x\n"
	if pid, ok := ParseLaunchctlPid(launchctl, "com.example.app"); !ok || pid != "812" {
		t.Fatal(pid)
	}
	if _, ok := ParseLaunchctlPid(launchctl, "com.x"); ok {
		t.Fatal("not running")
	}
	if cpu, rss, ok := ParsePsCPURSS("  12.5  20480\n"); !ok || cpu != 12.5 || rss != 20480 {
		t.Fatal(cpu, rss)
	}
	for _, bad := range []string{"", "1.0", "x 2", "1 y"} {
		if _, _, ok := ParsePsCPURSS(bad); ok {
			t.Fatal(bad)
		}
	}
}

// fakeShell answers the commands the Android probes send.
type fakeShell struct {
	pid, stat, meminfo, gfx, clk string
	err                          error
}

func (f *fakeShell) run(c string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	switch {
	case strings.HasPrefix(c, "getconf"):
		return f.clk, nil
	case strings.HasPrefix(c, "pidof"):
		return f.pid, nil
	case strings.HasPrefix(c, "cat /proc/"):
		if f.stat == "" {
			return "", errors.New("no such file")
		}
		return f.stat, nil
	case strings.HasPrefix(c, "dumpsys meminfo 'com.x'"):
		return f.meminfo, nil
	case strings.HasPrefix(c, "dumpsys gfxinfo 'com.x'"):
		return f.gfx, nil
	}
	return "", errors.New("unexpected " + c)
}

func stubClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Unix(1000, 0)
	prev := sampleNow
	sampleNow = func() time.Time { return now }
	t.Cleanup(func() { sampleNow = prev })
	return &now
}

func TestAndroidSampleProbe_CPU(t *testing.T) {
	now := stubClock(t)
	sh := &fakeShell{pid: "4321 99\n", clk: "100\n", stat: "4321 (com.x) S 1 1 1 0 -1 0 0 0 0 0 100 100 0 0"}
	probe, err := AndroidSampleProbe(sh.run, SampleCPU, "com.x")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Second)
	sh.stat = "4321 (com.x) S 1 1 1 0 -1 0 0 0 0 0 150 150 0 0"
	if v, err := probe(); err != nil || v != 50 { // 100 ticks / 100 Hz / 2 s
		t.Fatal(v, err)
	}
	// A relaunch restarts the baseline.
	*now = now.Add(time.Second)
	sh.pid, sh.stat = "5000", "5000 (com.x) S 1 1 1 0 -1 0 0 0 0 0 1 1 0 0"
	if _, err := probe(); !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	// The app gone.
	sh.pid = ""
	if _, err := probe(); !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	sh.pid, sh.stat = "5000", ""
	if _, err := probe(); !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	// getconf failing means 100 Hz.
	sh2 := &fakeShell{err: errors.New("offline")}
	if clockTicks(sh2.run) != 100 {
		t.Fatal("default hz")
	}
}

func TestAndroidSampleProbe_MemoryAndFrames(t *testing.T) {
	now := stubClock(t)
	sh := &fakeShell{meminfo: "TOTAL PSS: 1000 TOTAL RSS: 2", gfx: "(pid 7)\nTotal frames rendered: 100"}
	mem, _ := AndroidSampleProbe(sh.run, SampleMemory, "com.x")
	if v, err := mem(); err != nil || v != 1000*1024 {
		t.Fatal(v, err)
	}
	sh.meminfo = "No process found"
	if _, err := mem(); !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	frames, _ := AndroidSampleProbe(sh.run, SampleFrames, "com.x")
	*now = now.Add(500 * time.Millisecond)
	sh.gfx = "(pid 7)\nTotal frames rendered: 130"
	if v, err := frames(); err != nil || v != 60 {
		t.Fatal(v, err)
	}
	// No time passed: nothing to rate.
	if _, err := frames(); !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	sh.err = errors.New("adb: closed")
	if _, err := mem(); err == nil {
		t.Fatal("shell error")
	}
	if _, err := frames(); !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	if _, err := AndroidSampleProbe(sh.run, "heat", "com.x"); err == nil {
		t.Fatal("unknown kind")
	}
	var u *UnsupportedError
	if _, err := AndroidSampleProbe(nil, SampleCPU, "com.x"); !errors.As(err, &u) {
		t.Fatal(err)
	}
}

func TestIOSSampleProbe(t *testing.T) {
	var u *UnsupportedError
	if _, err := IOSSampleProbe("SIM", true, SampleFrames, "a"); !errors.As(err, &u) || !strings.Contains(u.Msg, "frames sampling is not available on iOS") {
		t.Fatal(err)
	}
	if _, err := IOSSampleProbe("DEV", false, SampleCPU, "a"); !errors.As(err, &u) || !strings.Contains(u.Msg, "physical iOS device") {
		t.Fatal(err)
	}
	if _, err := IOSSampleProbe("SIM", true, "heat", "a"); err == nil || errors.As(err, &u) {
		t.Fatal(err)
	}
	calls := stubXcrun(t, "PID\tStatus\tLabel\n812\t0\tUIKitApplication:com.example.app[a1][rb-legacy]\n", nil)
	var psArgs []string
	psOut, psErr := "  25.0  2048\n", error(nil)
	prev := RunHost
	RunHost = func(name string, args ...string) ([]byte, error) {
		psArgs = append([]string{name}, args...)
		return []byte(psOut), psErr
	}
	t.Cleanup(func() { RunHost = prev })
	cpu, _ := IOSSampleProbe("SIM", true, SampleCPU, "com.example.app")
	mem, _ := IOSSampleProbe("SIM", true, SampleMemory, "com.example.app")
	if v, err := cpu(); err != nil || v != 25 {
		t.Fatal(v, err)
	}
	if v, err := mem(); err != nil || v != 2048*1024 {
		t.Fatal(v, err)
	}
	if !reflect.DeepEqual((*calls)[0], []string{"simctl", "spawn", "SIM", "launchctl", "list"}) || !reflect.DeepEqual(psArgs, []string{"ps", "-o", "%cpu=,rss=", "-p", "812"}) {
		t.Fatalf("%q %q", *calls, psArgs)
	}
	psOut = "garbage"
	if _, err := cpu(); !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	psErr = errors.New("exit 1")
	if _, err := cpu(); !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	other, _ := IOSSampleProbe("SIM", true, SampleCPU, "com.not.running")
	if _, err := other(); !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	stubXcrun(t, "", errors.New("simctl gone"))
	if _, err := cpu(); err == nil || errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
}
