package wda

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/config"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// tvSwipeCoordinates are remote touchpad ratios, never TV screen pixels.
func tvSwipeCoordinates(s *flow.SwipeStep) ([4]float64, error) {
	var p [4]float64
	if s.Selector != nil || s.StartX != 0 || s.StartY != 0 || s.EndX != 0 || s.EndY != 0 || s.Speed != 0 || s.Distance != 0 {
		return p, fmt.Errorf("TV swipes use direction or touchpad percentages and duration; screen anchors, pixels, speed and distance are unsupported")
	}
	if s.Start != "" || s.End != "" {
		if s.Start == "" || s.End == "" {
			return p, fmt.Errorf("TV swipe requires both start and end")
		}
		x, y, err := parsePercentageCoords(s.Start)
		if err != nil {
			return p, err
		}
		ex, ey, err := parsePercentageCoords(s.End)
		if err != nil {
			return p, err
		}
		p = [4]float64{x, y, ex, ey}
	} else {
		switch strings.ToUpper(s.Direction) {
		case "UP":
			p = [4]float64{.5, .8, .5, .2}
		case "DOWN":
			p = [4]float64{.5, .2, .5, .8}
		case "LEFT":
			p = [4]float64{.8, .5, .2, .5}
		case "RIGHT":
			p = [4]float64{.2, .5, .8, .5}
		default:
			return p, fmt.Errorf("TV swipe needs UP, DOWN, LEFT, RIGHT or start/end percentages")
		}
	}
	for _, v := range p {
		if !(v >= 0 && v <= 1) {
			return p, fmt.Errorf("TV swipe coordinates must be within 0–100%%")
		}
	}
	return p, nil
}

func (d *Driver) swipeTV(s *flow.SwipeStep) *core.CommandResult {
	p, err := tvSwipeCoordinates(s)
	if err != nil {
		return errorResult(err, "Invalid TV swipe")
	}
	duration := s.Duration
	if duration == 0 {
		duration = 100
	}
	if duration < 1 || duration > 10000 {
		return errorResult(fmt.Errorf("duration must be 1–10000 ms"), "Invalid TV swipe")
	}
	ctx, cancel := context.WithTimeout(d.parentContext(), 30*time.Second)
	defer cancel()
	sender := d.tvSwipeSend
	if sender == nil {
		sender = d.sendTVSwipe
	}
	err = sender(ctx, d.udid, p, duration, d.info != nil && d.info.IsSimulator)
	if err != nil {
		return errorResult(err, "TV touchpad swipe failed")
	}
	return successResult("TV touchpad swiped", nil)
}

func sendSimulatorTVSwipe(ctx context.Context, udid string, p [4]float64, duration int) error {
	developer, err := exec.CommandContext(ctx, "xcode-select", "-p").Output()
	if err != nil {
		return fmt.Errorf("locate Xcode: %w", err)
	}
	source := filepath.Join(config.GetHome(), "drivers", "tvos", "trackpad.m")
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	key := sha256.Sum256(append(data, developer...))
	dir := filepath.Join(config.GetCacheDir(), "tv-trackpad")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	binary := filepath.Join(dir, fmt.Sprintf("trackpad-%x", key[:8]))
	if _, err = os.Stat(binary); os.IsNotExist(err) {
		f, e := os.CreateTemp(dir, "build-")
		if e != nil {
			return e
		}
		tmp := f.Name()
		_ = f.Close()
		defer os.Remove(tmp)
		output, e := exec.CommandContext(ctx, "xcrun", "clang", "-fobjc-arc", "-fblocks", "-framework", "Foundation", "-framework", "CoreGraphics", source, "-o", tmp).CombinedOutput()
		if e != nil {
			return fmt.Errorf("compile TV trackpad helper: %w: %s", e, output)
		}
		if e = os.Rename(tmp, binary); e != nil {
			return e
		}
	}
	args := []string{strings.TrimSpace(string(developer)), udid}
	for _, v := range p {
		args = append(args, strconv.FormatFloat(v, 'f', 6, 64))
	}
	args = append(args, strconv.Itoa(duration))
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("simulator touchpad: %w: %s", err, output)
	}
	return nil
}

type tvRemoteProcess struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	encoder *json.Encoder
	decoder *json.Decoder
	done    chan struct{}
}

func (p *tvRemoteProcess) close() {
	_ = p.input.Close()
	// Closing stdin lets the helper release its paired session normally.
	select {
	case <-p.done:
	case <-time.After(time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (d *Driver) CloseTVRemote() {
	d.tvRemoteMu.Lock()
	defer d.tvRemoteMu.Unlock()
	if d.tvRemote != nil {
		d.tvRemote.close()
		d.tvRemote = nil
	}
}

func (d *Driver) remoteCommand(ctx context.Context, args []string) error {
	d.tvRemoteMu.Lock()
	defer d.tvRemoteMu.Unlock()
	if d.tvRemote == nil {
		python := os.Getenv("MAESTRO_TV_PYTHON")
		if python == "" {
			python = "python3"
		}
		configPath := os.Getenv("MAESTRO_TV_REMOTE_CONFIG")
		if configPath == "" {
			return fmt.Errorf("physical TV input requires MAESTRO_TV_REMOTE_CONFIG; see docs/tvos.md")
		}
		// The driver context is per command; the paired session lives until cleanup.
		cmd := exec.Command(python, filepath.Join(config.GetHome(), "drivers", "tvos", "companion.py"), configPath, d.udid, "serve")
		input, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			_ = input.Close()
			return err
		}
		if err = cmd.Start(); err != nil {
			_ = input.Close()
			_ = output.Close()
			return err
		}
		p := &tvRemoteProcess{cmd: cmd, input: input, encoder: json.NewEncoder(input), decoder: json.NewDecoder(output), done: make(chan struct{})}
		go func() { _ = cmd.Wait(); close(p.done) }()
		d.tvRemote = p
	}
	p := d.tvRemote
	result := make(chan error, 1)
	go func() {
		if err := p.encoder.Encode(args); err != nil {
			result <- err
			return
		}
		var reply struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := p.decoder.Decode(&reply); err != nil {
			result <- fmt.Errorf("Companion connection failed; check pairing and Python dependencies: %w", err)
			return
		}
		if !reply.OK {
			result <- fmt.Errorf("Companion input failed (%s)", reply.Error)
			return
		}
		result <- nil
	}()
	var err error
	select {
	case err = <-result:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		p.close()
		d.tvRemote = nil
	}
	return err
}

func (d *Driver) sendTVSwipe(ctx context.Context, udid string, p [4]float64, duration int, simulator bool) error {
	if simulator {
		return sendSimulatorTVSwipe(ctx, udid, p, duration)
	}
	args := make([]string, 0, 5)
	for _, v := range p {
		args = append(args, strconv.Itoa(int(v*1000)))
	}
	args = append(args, strconv.Itoa(duration))
	return d.remoteCommand(ctx, args)
}

func (d *Driver) pressTVButton(button string) error {
	if d.info == nil || d.info.IsSimulator || os.Getenv("MAESTRO_TV_REMOTE_CONFIG") == "" {
		return d.client.PressButton(button)
	}
	if button == "playPause" {
		button = "play_pause"
	}
	ctx, cancel := context.WithTimeout(d.parentContext(), 30*time.Second)
	defer cancel()
	return d.remoteCommand(ctx, []string{"press", button})
}
