package wda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/devicelab-dev/maestro-runner/pkg/config"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestTVSwipeTouchpadCoordinates(t *testing.T) {
	cases := []struct {
		s    flow.SwipeStep
		want [4]float64
	}{
		{flow.SwipeStep{Direction: "DOWN"}, [4]float64{.5, .2, .5, .8}},
		{flow.SwipeStep{Direction: "up"}, [4]float64{.5, .8, .5, .2}},
		{flow.SwipeStep{Direction: "LEFT"}, [4]float64{.8, .5, .2, .5}},
		{flow.SwipeStep{Direction: "RIGHT"}, [4]float64{.2, .5, .8, .5}},
		{flow.SwipeStep{Start: "0%,25%", End: "100%,75%"}, [4]float64{0, .25, 1, .75}},
	}
	for _, c := range cases {
		got, err := tvSwipeCoordinates(&c.s)
		if err != nil || got != c.want {
			t.Errorf("%+v: %v %v", c.s, got, err)
		}
	}
	for _, s := range []flow.SwipeStep{{Direction: "DOWN", Selector: &flow.Selector{}}, {Direction: "DOWN", StartX: 1}, {Direction: "DOWN", Speed: 10}, {Direction: "DOWN", Distance: .5}, {}, {Direction: "diagonal"}, {Start: "20%,20%"}, {Start: "bad", End: "50%,50%"}, {Start: "20%,20%", End: "bad"}, {Start: "NaN,20", End: "50,50"}, {Start: "-1,20", End: "50,50"}, {Start: "20,20", End: "101,50"}} {
		if _, err := tvSwipeCoordinates(&s); err == nil {
			t.Errorf("accepted %+v", s)
		}
	}
}
func TestTVSwipeDispatchAndFailure(t *testing.T) {
	for _, sim := range []bool{true, false} {
		d := NewDriver(nil, &core.PlatformInfo{IsSimulator: sim}, "fixture-tv")
		d.SetTVMode(true)
		calls := 0
		d.tvSwipeSend = func(ctx context.Context, id string, p [4]float64, ms int, isSim bool) error {
			calls++
			if id != "fixture-tv" || p != [4]float64{.5, .2, .5, .8} || ms != 100 || isSim != sim {
				t.Fatal("wrong transport arguments")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("unbounded gesture")
			}
			return nil
		}
		// Nil WDA client proves remote swipes do not hit touchscreen endpoints.
		if d.swipe(&flow.SwipeStep{}).Success {
			t.Fatal("invalid coordinates accepted")
		}
		if !d.swipe(&flow.SwipeStep{Direction: "DOWN"}).Success || calls != 1 {
			t.Fatal("not dispatched")
		}
		for _, duration := range []int{-1, 10001} {
			if d.swipe(&flow.SwipeStep{Direction: "DOWN", Duration: duration}).Success {
				t.Fatal("bad duration accepted")
			}
		}
		if calls != 1 {
			t.Fatal("invalid gesture sent")
		}
		d.tvSwipeSend = func(context.Context, string, [4]float64, int, bool) error { return errors.New("delivery failed") }
		if d.swipe(&flow.SwipeStep{Direction: "DOWN"}).Success {
			t.Fatal("delivery failure hidden")
		}
	}
}
func TestPhysicalTVRuntimeConfiguration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tv.json")
	t.Setenv("MAESTRO_TV_REMOTE_CONFIG", p)
	if err := os.WriteFile(p, []byte(`{"fixture-tv":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := NewRunner("fixture-tv", "TEAM", "")
	if err := r.configureRuntime(); err != nil || !r.IsTV() {
		t.Fatalf("TV not selected: %v", err)
	}
	if r.runnerScheme() != "WebDriverAgentRunner_tvOS" {
		t.Fatal("wrong scheme")
	}
	if err := os.WriteFile(p, []byte(`not json`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewRunner("fixture-tv", "TEAM", "").configureRuntime(); err == nil {
		t.Fatal("bad configuration ignored")
	}
	t.Setenv("MAESTRO_TV_REMOTE_CONFIG", "")
	if err := NewDriver(nil, nil, "fixture-tv").remoteCommand(context.Background(), []string{"500", "200", "500", "800", "300"}); err == nil {
		t.Fatal("missing pairing accepted")
	}
}

func TestTVRemoteProcessHelper(t *testing.T) {
	if os.Getenv("TV_REMOTE_PROCESS_TEST") != "1" {
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	for n := 0; ; n++ {
		var args []string
		if decoder.Decode(&args) != nil {
			os.Exit(0)
		}
		if args[0] == "malformed" {
			fmt.Println("invalid reply")
			os.Exit(0)
		}
		if args[0] == "wait" {
			time.Sleep(time.Minute)
		}
		if n == 2 {
			fmt.Println(`{"ok":false,"error":"TestFailure"}`)
		} else {
			fmt.Println(`{"ok":true}`)
		}
	}
}

func fakeTVRemote(t *testing.T) *tvRemoteProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTVRemoteProcessHelper$")
	cmd.Env = append(os.Environ(), "TV_REMOTE_PROCESS_TEST=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &tvRemoteProcess{cmd: cmd, input: input, encoder: json.NewEncoder(input), decoder: json.NewDecoder(output), done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p
}

func TestTVRemoteReusesSessionAndClosesOnFailure(t *testing.T) {
	d := NewDriver(nil, nil, "fixture-tv")
	d.tvRemote = fakeTVRemote(t)
	defer d.CloseTVRemote()
	for i := 0; i < 2; i++ {
		if err := d.remoteCommand(context.Background(), []string{"press", "select"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.remoteCommand(context.Background(), []string{"press", "select"}); err == nil {
		t.Fatal("helper error was swallowed")
	}
	if d.tvRemote != nil {
		t.Fatal("failed session retained")
	}
}

func TestTVRemoteCancellationClosesProcess(t *testing.T) {
	d := NewDriver(nil, nil, "fixture-tv")
	p := fakeTVRemote(t)
	d.tvRemote = p
	defer d.CloseTVRemote()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := d.remoteCommand(ctx, []string{"wait"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.tvRemote != nil {
		t.Fatal("cancelled session retained")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("helper leaked")
	}
}

func TestTVRemoteStartsConfiguredHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Apple transport requires macOS")
	}
	path := filepath.Join(t.TempDir(), "python-fixture")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec \"$TV_REMOTE_TEST_BINARY\" -test.run=^TestTVRemoteProcessHelper$\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TV_REMOTE_TEST_BINARY", os.Args[0])
	t.Setenv("TV_REMOTE_PROCESS_TEST", "1")
	t.Setenv("MAESTRO_TV_PYTHON", path)
	t.Setenv("MAESTRO_TV_REMOTE_CONFIG", filepath.Join(t.TempDir(), "fixture.json"))
	d := NewDriver(nil, &core.PlatformInfo{}, "fixture-tv")
	defer d.CloseTVRemote()
	if err := d.sendTVSwipe(context.Background(), "fixture-tv", [4]float64{.5, .2, .5, .8}, 100, false); err != nil {
		t.Fatal(err)
	}
	if err := d.pressTVButton("playPause"); err != nil {
		t.Fatal(err)
	}
	if err := d.pressTVButton("select"); err == nil {
		t.Fatal("helper rejection hidden")
	}
	// Reconnect after the failed session; cleanup also accepts repeated calls.
	if err := d.pressTVButton("menu"); err != nil {
		t.Fatal(err)
	}
	d.CloseTVRemote()
	d.CloseTVRemote()
	t.Setenv("MAESTRO_TV_PYTHON", filepath.Join(t.TempDir(), "missing"))
	if err := d.remoteCommand(context.Background(), []string{"press", "select"}); err == nil {
		t.Fatal("missing interpreter accepted")
	}
}

func TestSimulatorTVHelper(t *testing.T) {
	if os.Getenv("TV_TRACKPAD_PROCESS_TEST") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	data, _ := json.Marshal(args[1:])
	if os.WriteFile(os.Getenv("TV_TRACKPAD_ARGUMENTS"), data, 0600) != nil {
		os.Exit(3)
	}
	if os.Getenv("TV_TRACKPAD_FAIL") == "1" {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestSimulatorTVSwipeBuildCacheAndDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Apple transport requires macOS")
	}
	root := t.TempDir()
	t.Setenv("MAESTRO_RUNNER_HOME", root)
	config.ResetHome()
	t.Cleanup(config.ResetHome)
	source := filepath.Join(root, "drivers", "tvos", "trackpad.m")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("fixture source"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("xcode-select", "#!/bin/sh\nif [ \"$TV_XCODE_FAIL\" = 1 ]; then exit 1; fi\nprintf '%s\\n' /Applications/FixtureXcode.app/Contents/Developer\n")
	helper := filepath.Join(bin, "trackpad-fixture")
	write("trackpad-fixture", "#!/bin/sh\nexec \"$TV_REMOTE_TEST_BINARY\" -test.run=^TestSimulatorTVHelper$ -- \"$@\"\n")
	write("xcrun", "#!/bin/sh\nif [ \"$TV_COMPILE_FAIL\" = 1 ]; then exit 1; fi\nfor output do :; done\nprintf 'compile\\n' >> \"$TV_COMPILE_CALLS\"\nif [ \"$TV_COMPILE_NO_OUTPUT\" = 1 ]; then rm \"$output\"; exit 0; fi\ncp \"$TV_TRACKPAD_FIXTURE\" \"$output\"\nchmod +x \"$output\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TV_REMOTE_TEST_BINARY", os.Args[0])
	t.Setenv("TV_TRACKPAD_PROCESS_TEST", "1")
	t.Setenv("TV_TRACKPAD_FIXTURE", helper)
	arguments := filepath.Join(root, "arguments.json")
	t.Setenv("TV_TRACKPAD_ARGUMENTS", arguments)
	compiles := filepath.Join(root, "compiles")
	t.Setenv("TV_COMPILE_CALLS", compiles)
	d := NewDriver(nil, &core.PlatformInfo{IsSimulator: true}, "fixture-tv")
	d.SetTVMode(true)
	if result := d.swipe(&flow.SwipeStep{Direction: "DOWN"}); !result.Success {
		t.Fatalf("default sender: %+v", result)
	}
	for i := 0; i < 2; i++ {
		if err := d.sendTVSwipe(context.Background(), "fixture-tv", [4]float64{.5, .2, .5, .8}, 100, true); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(compiles)
	if err != nil || string(data) != "compile\n" {
		t.Fatalf("helper not cached: %s %v", data, err)
	}
	data, err = os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"/Applications/FixtureXcode.app/Contents/Developer", "fixture-tv", "0.500000", "0.200000", "0.500000", "0.800000", "100"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("helper args: %v", got)
	}
	t.Setenv("TV_TRACKPAD_FAIL", "1")
	if sendSimulatorTVSwipe(context.Background(), "fixture-tv", [4]float64{}, 100) == nil {
		t.Fatal("delivery failure hidden")
	}
	t.Setenv("TV_TRACKPAD_FAIL", "")
	t.Setenv("TV_COMPILE_FAIL", "1")
	if err = os.WriteFile(source, []byte("changed fixture source"), 0600); err != nil {
		t.Fatal(err)
	}
	if sendSimulatorTVSwipe(context.Background(), "fixture-tv", [4]float64{}, 100) == nil {
		t.Fatal("compile failure hidden")
	}
	t.Setenv("TV_COMPILE_FAIL", "")
	t.Setenv("TV_COMPILE_NO_OUTPUT", "1")
	if sendSimulatorTVSwipe(context.Background(), "fixture-tv", [4]float64{}, 100) == nil {
		t.Fatal("missing compiler output accepted")
	}
	t.Setenv("TV_COMPILE_NO_OUTPUT", "")
	cache := filepath.Join(root, "cache", "tv-trackpad")
	if os.Geteuid() != 0 {
		if err = os.Chmod(cache, 0500); err != nil {
			t.Fatal(err)
		}
		failure := sendSimulatorTVSwipe(context.Background(), "fixture-tv", [4]float64{}, 100)
		if err = os.Chmod(cache, 0700); err != nil {
			t.Fatal(err)
		}
		if failure == nil {
			t.Fatal("read-only build cache accepted")
		}
	}
	if err = os.RemoveAll(filepath.Join(root, "cache")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "cache"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if sendSimulatorTVSwipe(context.Background(), "fixture-tv", [4]float64{}, 100) == nil {
		t.Fatal("invalid cache layout accepted")
	}
	if err = os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if sendSimulatorTVSwipe(context.Background(), "fixture-tv", [4]float64{}, 100) == nil {
		t.Fatal("missing bundled helper ignored")
	}
	t.Setenv("TV_XCODE_FAIL", "1")
	if sendSimulatorTVSwipe(context.Background(), "fixture-tv", [4]float64{}, 100) == nil {
		t.Fatal("missing Xcode ignored")
	}
}

func TestTVRemoteRejectsBrokenProtocolAndClosedPipe(t *testing.T) {
	for _, broken := range []string{"malformed", "closed"} {
		t.Run(broken, func(t *testing.T) {
			d := NewDriver(nil, nil, "fixture-tv")
			d.tvRemote = fakeTVRemote(t)
			defer d.CloseTVRemote()
			if broken == "closed" {
				_ = d.tvRemote.input.Close()
			}
			if d.remoteCommand(context.Background(), []string{broken}) == nil {
				t.Fatal("broken transport was reported successful")
			}
			if d.tvRemote != nil {
				t.Fatal("broken transport retained")
			}
		})
	}

}
