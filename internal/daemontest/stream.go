package daemontest

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
)

// StepSource is a capture source a test drives: Run emits the clip's
// config, then one packet per value sent on next (a keyframe when true),
// and fails with the error sent on fail.
type StepSource struct {
	CanSetFPS bool
	Next      chan bool
	Fail      chan error
	Started   atomic.Int32
	Silent    bool // emit nothing at all
	Tiny      bool // emit 4-byte samples
}

func NewStepSource() *StepSource {
	return &StepSource{CanSetFPS: true, Next: make(chan bool, 64), Fail: make(chan error, 1)}
}

func (s *StepSource) Mode() capture.Mode { return capture.OnDemand }
func (s *StepSource) FPSControl() bool   { return s.CanSetFPS }

func (s *StepSource) Run(ctx context.Context, opts capture.Options, emit func(capture.Packet)) error {
	s.Started.Add(1)
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		return err
	}
	if !s.Silent {
		emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	}
	var pts time.Duration
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return nil
		case err := <-s.Fail:
			return err
		case key := <-s.Next:
			f := frames[i%len(frames)]
			if key {
				f = frames[0]
			}
			data := f.Data
			if s.Tiny {
				data = []byte{0, 0, 0, 1, 0x65}[:5]
			}
			emit(capture.Packet{Kind: capture.Video, PTS: pts, Key: key || f.Key, Data: data})
			pts += time.Second / 15
		}
	}
}

// BurstSource emits the config and a keyframe, then large packets as fast
// as it can until ctx ends: a subscriber that does not read falls behind.
type BurstSource struct{}

func (BurstSource) Mode() capture.Mode { return capture.OnDemand }
func (BurstSource) FPSControl() bool   { return true }
func (BurstSource) Run(ctx context.Context, _ capture.Options, emit func(capture.Packet)) error {
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		return err
	}
	emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	big := append(append([]byte(nil), frames[0].Data...), make([]byte, 16<<10)...)
	for i := 0; ctx.Err() == nil; i++ {
		emit(capture.Packet{Kind: capture.Video, PTS: time.Duration(i) * time.Millisecond, Key: true, Data: big})
		if i%256 == 255 {
			time.Sleep(time.Millisecond)
		}
	}
	return nil
}

// CaptureDriver is the mock driver with the capture a test gives it.
type CaptureDriver struct {
	*mock.Driver
	Session *capture.Session
	Err     error
}

func (d *CaptureDriver) Capture() (*capture.Session, error) { return d.Session, d.Err }
