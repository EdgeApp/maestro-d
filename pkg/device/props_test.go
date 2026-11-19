package device

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeADB answers `adb devices -l` with list and `adb -s <serial> shell …`
// from shell: a serial's output, or "!fail" (exit 1) or "!sleep" (sleeps
// 5 s). It counts each serial's shell calls.
type fakeADB struct {
	mu    sync.Mutex
	list  string
	shell map[string]string
	reads map[string]int
	delay time.Duration // added to every successful shell answer
}

func (f *fakeADB) setList(list string) {
	f.mu.Lock()
	f.list = list
	f.mu.Unlock()
}

func (f *fakeADB) count(serial string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[serial]
}

func (f *fakeADB) cmd(name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) >= 1 && args[0] == "devices" {
		return exec.Command("printf", "%s", f.list)
	}
	if len(args) >= 3 && args[0] == "-s" && args[2] == "shell" {
		serial := args[1]
		f.reads[serial]++
		switch out := f.shell[serial]; out {
		case "!fail":
			return exec.Command("false")
		case "!sleep":
			return exec.Command("sleep", "5")
		default:
			if f.delay > 0 {
				return exec.Command("sh", "-c", fmt.Sprintf(`sleep %.3f; printf '%%s' "$0"`, f.delay.Seconds()), out)
			}
			return exec.Command("printf", "%s", out)
		}
	}
	return exec.Command("false")
}

func withFakeADB(t *testing.T, f *fakeADB) {
	t.Helper()
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	withFakeExec(t, f.cmd)
	withFakeLookPath(t, func(string) (string, error) { return "/usr/local/bin/adb", nil })
	resetPropsCache(t)
}

func resetPropsCache(t *testing.T) {
	t.Helper()
	clear := func() {
		propsCache.mu.Lock()
		propsCache.m = map[string]propsEntry{}
		propsCache.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func withClock(t *testing.T, at *time.Time) {
	t.Helper()
	prev := now
	now = func() time.Time { return *at }
	t.Cleanup(func() { now = prev })
}

func withPropsTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := propsReadTimeout
	propsReadTimeout = d
	t.Cleanup(func() { propsReadTimeout = prev })
}

func bySerial(devs []DeviceWithProps) map[string]DeviceWithProps {
	m := map[string]DeviceWithProps{}
	for _, d := range devs {
		m[d.Serial] = d
	}
	return m
}

const pixelList = "List of devices attached\n" +
	"FAKEPIXEL0001         device usb:1048576X product:shiba model:Pixel_8 device:shiba transport_id:113\n"

func TestParseTransportIDs(t *testing.T) {
	out := "List of devices attached\n" +
		"FAKEPIXEL0001         device usb:1048576X product:shiba model:Pixel_8 device:shiba transport_id:113\n" +
		"emulator-5554          device product:sdk_gphone64_arm64 model:sdk_gphone64_arm64 device:emu64a transport_id:7\n" +
		"RF8M33XXXXX            unauthorized usb:1-1 transport_id:9\n" +
		"ABC                    no permissions (user in plugdev group); see [http://developer.android.com/tools/device.html] usb:1-2 transport_id:10\n" +
		"PLAIN\tdevice\n"
	ids := parseTransportIDs(out)
	want := map[string]string{"FAKEPIXEL0001": "113", "emulator-5554": "7", "RF8M33XXXXX": "9", "ABC": "10"}
	for serial, id := range want {
		if ids[serial] != id {
			t.Errorf("%s: transport id %q, want %q", serial, ids[serial], id)
		}
	}
	if _, ok := ids["PLAIN"]; ok {
		t.Errorf("plain line has a transport id: %v", ids)
	}

	// Upstream's parser still reads serial, state and type from -l lines.
	devs := parseDeviceList(out)
	if len(devs) != 5 {
		t.Fatalf("parseDeviceList: %d devices, want 5: %+v", len(devs), devs)
	}
	got := map[string][2]string{}
	for _, d := range devs {
		got[d.Serial] = [2]string{d.State, d.Type}
	}
	for serial, st := range map[string][2]string{
		"FAKEPIXEL0001": {"device", "device"},
		"emulator-5554":  {"device", "emulator"},
		"RF8M33XXXXX":    {"unauthorized", "device"},
		"ABC":            {"no", "device"},
		"PLAIN":          {"device", "device"},
	} {
		if got[serial] != st {
			t.Errorf("%s: state/type %v, want %v", serial, got[serial], st)
		}
	}
}

func TestParseProps(t *testing.T) {
	cases := []struct {
		out                  string
		model, version, name string
		complete             bool
	}{
		{"Pixel 8\n16\n1\nAlex's Pixel\n", "Pixel 8", "16", "Alex's Pixel", true},
		{"Pixel 8\r\n16\r\n1\r\nnull\r\n", "Pixel 8", "16", "Pixel 8", true},
		{"Pixel 8\n16\n1\n\n", "Pixel 8", "16", "Pixel 8", true},
		// settings did not answer, or boot has not completed: partial.
		{"Pixel 8\n16\n1\n" + nameUnknown + "\n", "Pixel 8", "16", "Pixel 8", false},
		{"Pixel 8\n16\n0\nAlex's Pixel\n", "Pixel 8", "16", "Alex's Pixel", false},
		// Cut off before the name line ended.
		{"Pixel 8\n16\n1\n", "Pixel 8", "16", "Pixel 8", false},
		{"Pixel 8\n16\n1\nPaul", "Pixel 8", "16", "Paul", false},
		{"  Pixel 8  \n", "Pixel 8", "", "Pixel 8", false},
		{"", "", "", "", false},
	}
	for _, c := range cases {
		p, complete := parseProps(c.out)
		if p.model != c.model || p.osVersion != c.version || p.name != c.name || complete != c.complete {
			t.Errorf("parseProps(%q) = %+v complete %v, want %q/%q/%q complete %v", c.out, p, complete, c.model, c.version, c.name, c.complete)
		}
	}
}

func TestReadPropsRunsOneShellCall(t *testing.T) {
	var gotArgs []string
	withFakeExec(t, func(name string, args ...string) *exec.Cmd {
		gotArgs = args
		return exec.Command("printf", "%s", "Pixel 8\n16\n1\nnull\n")
	})
	p, complete, err := readProps("/usr/local/bin/adb", "SER")
	if err != nil || !complete {
		t.Fatalf("complete %v, err %v", complete, err)
	}
	if p != (deviceProps{name: "Pixel 8", model: "Pixel 8", osVersion: "16"}) {
		t.Errorf("props %+v", p)
	}
	want := []string{"-s", "SER", "shell", propsScript}
	if strings.Join(gotArgs, "|") != strings.Join(want, "|") {
		t.Errorf("args %q, want %q", gotArgs, want)
	}
}

func TestReadPropsFailure(t *testing.T) {
	withFakeExec(t, fakeExecError())
	if _, _, err := readProps("/usr/local/bin/adb", "SER"); err == nil || !strings.Contains(err.Error(), "adb props SER") {
		t.Errorf("err = %v", err)
	}
	withFakeExec(t, func(string, ...string) *exec.Cmd { return exec.Command("/nonexistent/adb") })
	if _, _, err := readProps("/nonexistent/adb", "SER"); err == nil {
		t.Error("a binary that cannot start: no error")
	}
}

func TestReadPropsKilledAtTimeout(t *testing.T) {
	withPropsTimeout(t, 50*time.Millisecond)
	withFakeExec(t, func(string, ...string) *exec.Cmd { return exec.Command("sleep", "5") })
	start := time.Now()
	_, _, err := readProps("/usr/local/bin/adb", "SER")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("read took %v; the bound did not kill it", d)
	}
}

func TestListDevicesWithPropsFillsReadyDevices(t *testing.T) {
	f := &fakeADB{
		list: "List of devices attached\n" +
			"PHONE  device usb:1 transport_id:1\n" +
			"emulator-5554  device transport_id:2\n" +
			"OFF  offline transport_id:3\n" +
			"UNAUTH  unauthorized usb:2 transport_id:4\n",
		shell: map[string]string{
			"PHONE":         "Pixel 8\n16\n1\nAlex's Pixel\n",
			"emulator-5554": "sdk_gphone64_arm64\n15\n1\nnull\n",
		},
	}
	withFakeADB(t, f)
	devs, err := ListDevicesWithProps()
	if err != nil {
		t.Fatal(err)
	}
	m := bySerial(devs)
	if p := m["PHONE"]; p.Name != "Alex's Pixel" || p.Model != "Pixel 8" || p.OSVersion != "16" || p.TransportID != "1" || p.Type != "device" {
		t.Errorf("phone %+v", p)
	}
	if e := m["emulator-5554"]; e.Model != "sdk_gphone64_arm64" || e.OSVersion != "15" || e.Type != "emulator" {
		t.Errorf("emulator %+v", e)
	}
	for _, s := range []string{"OFF", "UNAUTH"} {
		if d := m[s]; d.Name != "" || d.Model != "" || d.OSVersion != "" {
			t.Errorf("%s not blank: %+v", s, d)
		}
		if f.count(s) != 0 {
			t.Errorf("%s was read %d times", s, f.count(s))
		}
	}
}

func TestListDevicesWithPropsCachesPerConnection(t *testing.T) {
	f := &fakeADB{list: pixelList, shell: map[string]string{"FAKEPIXEL0001": "Pixel 8\n16\n1\nnull\n"}}
	withFakeADB(t, f)
	const s = "FAKEPIXEL0001"
	for i := 0; i < 3; i++ {
		devs, err := ListDevicesWithProps()
		if err != nil {
			t.Fatal(err)
		}
		if devs[0].Model != "Pixel 8" {
			t.Fatalf("listing %d: %+v", i, devs[0])
		}
	}
	if n := f.count(s); n != 1 {
		t.Errorf("same connection read %d times, want 1", n)
	}

	// A reconnect gets a new transport id: read again.
	f.setList(strings.Replace(pixelList, "transport_id:113", "transport_id:114", 1))
	if _, err := ListDevicesWithProps(); err != nil {
		t.Fatal(err)
	}
	if n := f.count(s); n != 2 {
		t.Errorf("after a new transport id read %d times, want 2", n)
	}

	// Gone, then back on the same transport id: evicted, so read again.
	f.setList("List of devices attached\n")
	if _, err := ListDevicesWithProps(); err != nil {
		t.Fatal(err)
	}
	f.setList(strings.Replace(pixelList, "transport_id:113", "transport_id:114", 1))
	if _, err := ListDevicesWithProps(); err != nil {
		t.Fatal(err)
	}
	if n := f.count(s); n != 3 {
		t.Errorf("after a disconnect read %d times, want 3", n)
	}

	// Offline, then back: evicted as well.
	f.setList("List of devices attached\nFAKEPIXEL0001 offline transport_id:114\n")
	devs, _ := ListDevicesWithProps()
	if devs[0].Model != "" {
		t.Errorf("offline device has props: %+v", devs[0])
	}
	f.setList(strings.Replace(pixelList, "transport_id:113", "transport_id:114", 1))
	if _, err := ListDevicesWithProps(); err != nil {
		t.Fatal(err)
	}
	if n := f.count(s); n != 4 {
		t.Errorf("after going offline read %d times, want 4", n)
	}
}

func TestListDevicesWithPropsWithoutTransportIDs(t *testing.T) {
	f := &fakeADB{list: "List of devices attached\nOLD\tdevice\n", shell: map[string]string{"OLD": "Nexus 5\n6.0\n1\nnull\n"}}
	withFakeADB(t, f)
	for i := 0; i < 2; i++ {
		devs, err := ListDevicesWithProps()
		if err != nil {
			t.Fatal(err)
		}
		if devs[0].Model != "Nexus 5" || devs[0].TransportID != "" {
			t.Fatalf("%+v", devs[0])
		}
	}
	if n := f.count("OLD"); n != 1 {
		t.Errorf("read %d times, want 1 (the serial alone is the key)", n)
	}
}

func TestListDevicesWithPropsFailureEntry(t *testing.T) {
	clock := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	withClock(t, &clock)
	f := &fakeADB{list: pixelList, shell: map[string]string{"FAKEPIXEL0001": "!fail"}}
	withFakeADB(t, f)
	const s = "FAKEPIXEL0001"
	devs, err := ListDevicesWithProps()
	if err != nil {
		t.Fatal(err)
	}
	if devs[0].Model != "" || devs[0].State != "device" {
		t.Errorf("failed read: %+v", devs[0])
	}
	clock = clock.Add(propsFailFresh - time.Second)
	if _, err := ListDevicesWithProps(); err != nil {
		t.Fatal(err)
	}
	if n := f.count(s); n != 1 {
		t.Errorf("inside the failure window read %d times, want 1", n)
	}

	f.mu.Lock()
	f.shell[s] = "Pixel 8\n16\n1\nnull\n"
	f.mu.Unlock()
	clock = clock.Add(2 * time.Second)
	devs, _ = ListDevicesWithProps()
	if n := f.count(s); n != 2 || devs[0].Model != "Pixel 8" {
		t.Errorf("after the window: %d reads, %+v", n, devs[0])
	}
}

func TestListDevicesWithPropsReadsInParallel(t *testing.T) {
	f := &fakeADB{
		list: "List of devices attached\nA device transport_id:1\nB device transport_id:2\nC device transport_id:3\n",
		shell: map[string]string{
			"A": "Pixel 6\n14\n1\nnull\n",
			"B": "Pixel 7\n15\n1\nnull\n",
			"C": "Pixel 8\n16\n1\nnull\n",
		},
		delay: 200 * time.Millisecond,
	}
	withFakeADB(t, f)
	start := time.Now()
	devs, err := ListDevicesWithProps()
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 450*time.Millisecond {
		t.Errorf("three 200 ms reads took %v; not parallel", d)
	}
	for _, d := range devs {
		if d.Model == "" {
			t.Errorf("%s not filled", d.Serial)
		}
	}
}

func TestListDevicesWithPropsHangingDeviceIsBounded(t *testing.T) {
	withPropsTimeout(t, 50*time.Millisecond)
	f := &fakeADB{
		list:  "List of devices attached\nHANG device transport_id:1\nOK device transport_id:2\n",
		shell: map[string]string{"HANG": "!sleep", "OK": "Pixel 8\n16\n1\nnull\n"},
	}
	withFakeADB(t, f)
	start := time.Now()
	devs, err := ListDevicesWithProps()
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("listing took %v with one hanging device", d)
	}
	m := bySerial(devs)
	if m["OK"].Model != "Pixel 8" || m["HANG"].Model != "" {
		t.Errorf("OK %+v, HANG %+v", m["OK"], m["HANG"])
	}
}

func TestListDevicesWithPropsConcurrentListings(t *testing.T) {
	f := &fakeADB{list: pixelList, shell: map[string]string{"FAKEPIXEL0001": "Pixel 8\n16\n1\nnull\n"}}
	withFakeADB(t, f)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			devs, err := ListDevicesWithProps()
			if err != nil || len(devs) != 1 || devs[0].Model != "Pixel 8" {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d concurrent listings were wrong", bad.Load())
	}
}

func TestListDevicesWithPropsErrors(t *testing.T) {
	resetPropsCache(t)
	withFakeLookPath(t, func(string) (string, error) { return "", fmt.Errorf("not found") })
	if _, err := ListDevicesWithProps(); err == nil {
		t.Error("no adb: no error")
	}
	withFakeLookPath(t, func(string) (string, error) { return "/usr/local/bin/adb", nil })
	withFakeExec(t, fakeExecError())
	if _, err := ListDevicesWithProps(); err == nil {
		t.Error("adb devices failed: no error")
	}
}

// A wrapper that does not exec adb leaves a child holding stdout when it is
// killed; the read still ends near the bound.
func TestReadPropsKilledWrapperDoesNotWaitForItsChild(t *testing.T) {
	withPropsTimeout(t, 50*time.Millisecond)
	withFakeExec(t, func(string, ...string) *exec.Cmd { return exec.Command("sh", "-c", "sleep 5; true") })
	start := time.Now()
	if _, _, err := readProps("/usr/local/bin/adb", "SER"); err == nil {
		t.Error("no error from a killed read")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("read took %v; it waited for the wrapper's child", d)
	}
}

// On a device whose settings service is not up yet, `settings` fails; the
// call still succeeds and keeps the model and version. The script runs in
// a local shell with getprop and settings stubbed as the device answers.
func TestReadPropsKeepsModelWhenSettingsFails(t *testing.T) {
	stubs := `getprop() { case "$1" in ro.product.model) echo "Pixel 8";; ro.build.version.release) echo 16;; sys.boot_completed) echo 1;; esac; }; ` +
		`settings() { echo "cmd: Can't find service: settings" >&2; return 255; }; `
	withFakeExec(t, func(name string, args ...string) *exec.Cmd {
		return exec.Command("sh", "-c", stubs+args[len(args)-1])
	})
	p, complete, err := readProps("/usr/local/bin/adb", "SER")
	if err != nil {
		t.Fatalf("settings failure failed the read: %v", err)
	}
	if p != (deviceProps{name: "Pixel 8", model: "Pixel 8", osVersion: "16"}) || complete {
		t.Errorf("props %+v, complete %v; want the model and version, partial", p, complete)
	}
}

// A read cut off after the model and version keeps them, as a partial read.
func TestReadPropsKeepsWhatATimedOutReadPrinted(t *testing.T) {
	withPropsTimeout(t, 200*time.Millisecond)
	withFakeExec(t, func(string, ...string) *exec.Cmd {
		return exec.Command("sh", "-c", `printf 'Pixel 8\n16\n1\n'; exec sleep 5`)
	})
	p, complete, err := readProps("/usr/local/bin/adb", "SER")
	if err != nil || complete || p.model != "Pixel 8" || p.osVersion != "16" || p.name != "Pixel 8" {
		t.Errorf("props %+v, complete %v, err %v", p, complete, err)
	}
}

// A phone read while it boots lists its model and version at once and
// its user-set name after propsFailFresh, on the same connection.
func TestListDevicesWithPropsReadsAPartialReadAgain(t *testing.T) {
	clock := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	withClock(t, &clock)
	const s = "FAKEPIXEL0001"
	f := &fakeADB{list: pixelList, shell: map[string]string{s: "Pixel 8\n16\n0\n" + nameUnknown + "\n"}}
	withFakeADB(t, f)
	devs, _ := ListDevicesWithProps()
	if d := devs[0]; d.Model != "Pixel 8" || d.OSVersion != "16" || d.Name != "Pixel 8" {
		t.Fatalf("while booting: %+v", d)
	}
	f.mu.Lock()
	f.shell[s] = "Pixel 8\n16\n1\nLab Pixel\n"
	f.mu.Unlock()
	clock = clock.Add(propsFailFresh - time.Second)
	devs, _ = ListDevicesWithProps()
	if devs[0].Name != "Pixel 8" || f.count(s) != 1 {
		t.Errorf("inside the window: %+v after %d reads", devs[0], f.count(s))
	}
	clock = clock.Add(2 * time.Second)
	devs, _ = ListDevicesWithProps()
	if devs[0].Name != "Lab Pixel" || f.count(s) != 2 {
		t.Errorf("after the window: %+v after %d reads", devs[0], f.count(s))
	}
	// Complete now: no more reads on this connection.
	clock = clock.Add(time.Hour)
	ListDevicesWithProps()
	if f.count(s) != 2 {
		t.Errorf("a complete read was read again: %d reads", f.count(s))
	}
}

// A failed read keeps what an earlier read on the connection found.
func TestListDevicesWithPropsFailedReadKeepsEarlierProps(t *testing.T) {
	clock := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	withClock(t, &clock)
	const s = "FAKEPIXEL0001"
	f := &fakeADB{list: pixelList, shell: map[string]string{s: "Pixel 8\n16\n0\n" + nameUnknown + "\n"}}
	withFakeADB(t, f)
	ListDevicesWithProps()
	f.mu.Lock()
	f.shell[s] = "!fail"
	f.mu.Unlock()
	clock = clock.Add(propsFailFresh + time.Second)
	devs, _ := ListDevicesWithProps()
	if d := devs[0]; d.Model != "Pixel 8" || d.OSVersion != "16" || f.count(s) != 2 {
		t.Errorf("after a failed re-read: %+v, %d reads", d, f.count(s))
	}
}

// An adb wrapper that exits 0 and leaves a child holding stdout gives a
// complete read, which is not read again.
func TestListDevicesWithPropsWrapperChildStillCompletes(t *testing.T) {
	f := &fakeADB{list: pixelList}
	withFakeADB(t, f)
	var reads atomic.Int32
	withFakeExec(t, func(name string, args ...string) *exec.Cmd {
		if len(args) >= 1 && args[0] == "devices" {
			return f.cmd(name, args...)
		}
		reads.Add(1)
		return exec.Command("sh", "-c", `printf 'Pixel 8\n16\n1\nLab Pixel\n'; sleep 2 &`)
	})
	for i := 0; i < 2; i++ {
		devs, err := ListDevicesWithProps()
		if err != nil || devs[0].Name != "Lab Pixel" {
			t.Fatalf("listing %d: %+v %v", i, devs, err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("read %d times, want 1 (the read is complete)", n)
	}
}
