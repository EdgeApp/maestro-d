package wda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"os"
	"os/exec"
	"path/filepath"
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
	for _, s := range []flow.SwipeStep{{}, {Direction: "diagonal"}, {Start: "20%,20%"}, {Start: "bad", End: "50%,50%"}, {Start: "20%,20%", End: "bad"}, {Start: "NaN,20", End: "50,50"}, {Start: "-1,20", End: "50,50"}, {Start: "20,20", End: "101,50"}} {
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
