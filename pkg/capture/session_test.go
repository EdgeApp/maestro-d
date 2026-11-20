package capture

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// stubRun is one Run of a stubSource.
type stubRun struct {
	opts   Options
	emit   func(Packet)
	ctx    context.Context
	fail   chan error
	exited chan struct{}
}

// stubSource hands each Run to the test, which emits packets through it
// and ends it.
type stubSource struct {
	mode Mode
	runs chan *stubRun
	// hold, when set, keeps a cancelled Run from returning until closed.
	hold chan struct{}
}

func newStub(mode Mode) *stubSource { return &stubSource{mode: mode, runs: make(chan *stubRun, 4)} }

func (s *stubSource) Mode() Mode       { return s.mode }
func (s *stubSource) FPSControl() bool { return s.mode == OnDemand }
func (s *stubSource) Run(ctx context.Context, opts Options, emit func(Packet)) error {
	r := &stubRun{opts: opts, emit: emit, ctx: ctx, fail: make(chan error, 1), exited: make(chan struct{})}
	defer close(r.exited)
	s.runs <- r
	select {
	case <-ctx.Done():
		if s.hold != nil {
			<-s.hold
		}
		return nil
	case err := <-r.fail:
		return err
	}
}

func (s *stubSource) next(t *testing.T) *stubRun {
	t.Helper()
	select {
	case r := <-s.runs:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("source not started")
		return nil
	}
}

func (s *stubSource) none(t *testing.T) {
	t.Helper()
	select {
	case r := <-s.runs:
		t.Fatalf("unexpected Run with %+v", r.opts)
	case <-time.After(50 * time.Millisecond):
	}
}

func waitDone(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

// runDone returns the session's latest Run's done channel, closed after
// the session has handled its return (listeners included).
func runDone(s *Session) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// fakeTimer replaces the session's linger timer.
type fakeTimer struct {
	mu      sync.Mutex
	d       time.Duration
	fn      func()
	stopped bool
}

func useFakeTimers(s *Session) *[]*fakeTimer {
	var timers []*fakeTimer
	var mu sync.Mutex
	s.afterFunc = func(d time.Duration, fn func()) func() bool {
		ft := &fakeTimer{d: d, fn: fn}
		mu.Lock()
		timers = append(timers, ft)
		mu.Unlock()
		return func() bool {
			ft.mu.Lock()
			defer ft.mu.Unlock()
			ft.stopped = true
			return true
		}
	}
	return &timers
}

var (
	vcfg = Packet{Kind: VideoConfig, Video: &VideoFormat{Width: 640, Height: 360, SPS: [][]byte{{0x67, 0x64, 0, 0x1e}}, PPS: [][]byte{{0x68}}}}
	acfg = Packet{Kind: AudioConfig, Audio: &AudioFormat{Codec: CodecAAC, SampleRate: 48000, Channels: 2, ASC: []byte{0x11, 0x90}}}
	key  = func(pts time.Duration) Packet {
		return Packet{Kind: Video, PTS: pts, Key: true, Data: []byte{0, 0, 0, 1, 0x65}}
	}
	delta = func(pts time.Duration) Packet { return Packet{Kind: Video, PTS: pts, Data: []byte{0, 0, 0, 1, 0x41}} }
	audio = func(pts time.Duration) Packet { return Packet{Kind: Audio, PTS: pts, Data: []byte{1}} }
)

// drain reads what is queued on a subscriber without blocking.
func drain(sub *Subscriber) []Packet {
	var out []Packet
	for {
		select {
		case p, ok := <-sub.Packets():
			if !ok {
				return out
			}
			out = append(out, p)
		default:
			return out
		}
	}
}

func kinds(ps []Packet) []Kind {
	out := make([]Kind, len(ps))
	for i, p := range ps {
		out[i] = p.Kind
	}
	return out
}

func equalKinds(a []Kind, b ...Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// U7: the first subscriber starts an on-demand source with its fps; a
// second joins at the running fps; the source is cancelled 10 s after the
// last leave and restarted on the next subscribe.
func TestSession_OnDemandLifecycle(t *testing.T) {
	src := newStub(OnDemand)
	s := NewSession(src)
	timers := useFakeTimers(s)
	if s.Mode() != OnDemand || !s.FPSControl() || s.FPS() != 0 {
		t.Fatal("idle session")
	}

	a, err := s.Subscribe(Options{FPS: 30, Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	r1 := src.next(t)
	if r1.opts.FPS != 30 || !r1.opts.Audio {
		t.Fatalf("Run opts = %+v", r1.opts)
	}
	b, err := s.Subscribe(Options{FPS: 60})
	if err != nil {
		t.Fatal(err)
	}
	src.none(t)
	if b.Options().FPS != 30 || b.Options().Audio || s.FPS() != 30 || a.Options().FPS != 30 {
		t.Errorf("joined at %+v, session fps %d", b.Options(), s.FPS())
	}
	if st, _ := s.State(); st != StateCapturing {
		t.Errorf("state %v", st)
	}

	a.Close()
	if len(*timers) != 0 {
		t.Fatal("linger started with a subscriber left")
	}
	b.Close()
	b.Close() // twice is harmless
	if len(*timers) != 1 || (*timers)[0].d != 10*time.Second {
		t.Fatalf("linger timers = %d", len(*timers))
	}
	if !errors.Is(a.Err(), ErrClosed) {
		t.Errorf("closed subscriber Err = %v", a.Err())
	}
	if _, ok := <-a.Packets(); ok {
		t.Error("closed subscriber's channel open")
	}
	select {
	case <-r1.ctx.Done():
		t.Fatal("source cancelled before the linger elapsed")
	default:
	}

	// A subscriber during the linger keeps the source and stops the timer;
	// the stale timer firing changes nothing.
	c, err := s.Subscribe(Options{FPS: 15})
	if err != nil {
		t.Fatal(err)
	}
	src.none(t)
	if !(*timers)[0].stopped || c.Options().FPS != 30 {
		t.Fatalf("linger not stopped (%v) or fps %d", (*timers)[0].stopped, c.Options().FPS)
	}
	(*timers)[0].fn()
	if r1.ctx.Err() != nil {
		t.Fatal("stale linger timer cancelled the source")
	}

	c.Close()
	(*timers)[1].fn()
	waitDone(t, r1.exited, "first Run return")
	if st, _ := s.State(); st != StateIdle || s.FPS() != 0 {
		t.Errorf("after linger: state %v fps %d", st, s.FPS())
	}

	d, err := s.Subscribe(Options{FPS: 5})
	if err != nil {
		t.Fatal(err)
	}
	r2 := src.next(t)
	if r2.opts.FPS != 5 || d.Options().FPS != 5 {
		t.Errorf("restart opts = %+v", r2.opts)
	}
	// Packets of the cancelled run are ignored.
	r1.emit(vcfg)
	if got := drain(d); len(got) != 0 {
		t.Errorf("stale run delivered %v", kinds(got))
	}
	s.Close()
	waitDone(t, r2.exited, "second Run return")
}

// A restart waits for the previous Run to return, so two never overlap.
func TestSession_RestartWaitsForPreviousRun(t *testing.T) {
	src := newStub(OnDemand)
	src.hold = make(chan struct{})
	s := NewSession(src)
	timers := useFakeTimers(s)
	a, _ := s.Subscribe(Options{})
	r1 := src.next(t)
	a.Close()
	(*timers)[0].fn()
	if _, err := s.Subscribe(Options{}); err != nil {
		t.Fatal(err)
	}
	src.none(t)
	close(src.hold)
	waitDone(t, r1.exited, "first Run return")
	r2 := src.next(t)
	s.Close()
	waitDone(t, r2.exited, "second Run return")
}

// fixedClock makes the session's host clock stand still at the returned
// pointer's value.
func fixedClock(s *Session) *time.Time {
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	return &now
}

// U8: a late joiner gets the configs, then the cached GOP's video from its
// keyframe retimed 1 ms apart up to the live edge, then live packets; the
// cached audio is dropped; a subscriber without audio gets no audio; over
// 32 MiB the cache keeps only the configs until the next keyframe.
func TestSession_GOPCache(t *testing.T) {
	src := newStub(OnDemand)
	s := NewSession(src)
	fixedClock(s)
	first, _ := s.Subscribe(Options{Audio: true})
	r := src.next(t)
	r.emit(delta(0)) // before any keyframe: not cached, not delivered
	r.emit(audio(0))
	r.emit(vcfg)
	r.emit(acfg)
	r.emit(key(1))
	r.emit(audio(1))
	r.emit(delta(2))
	if got := kinds(drain(first)); !equalKinds(got, VideoConfig, AudioConfig, Video, Audio, Video) {
		t.Fatalf("first subscriber got %v", got)
	}

	late, _ := s.Subscribe(Options{Audio: true})
	quiet, _ := s.Subscribe(Options{})
	if got := drain(late); !equalKinds(kinds(got), VideoConfig, AudioConfig, Video, Video) || !got[2].Key ||
		got[2].PTS != 2-time.Millisecond || got[3].PTS != 2 {
		t.Fatalf("late joiner got %v %v", kinds(got), got)
	}
	if got := kinds(drain(quiet)); !equalKinds(got, VideoConfig, Video, Video) {
		t.Fatalf("video-only joiner got %v", got)
	}
	r.emit(audio(3))
	r.emit(delta(3))
	if got := kinds(drain(late)); !equalKinds(got, Audio, Video) {
		t.Errorf("late joiner live = %v", got)
	}
	if got := kinds(drain(quiet)); !equalKinds(got, Video) {
		t.Errorf("video-only joiner live = %v", got)
	}

	// Over the cap: only the configs stay, until the next keyframe.
	big := make([]byte, 1<<20)
	r.emit(Packet{Kind: Video, PTS: 4, Key: true, Data: big})
	for i := 0; i < 32; i++ {
		r.emit(Packet{Kind: Video, PTS: time.Duration(5 + i), Data: big})
	}
	r.emit(delta(40))
	for _, sub := range []*Subscriber{first, late, quiet} {
		drain(sub)
	}
	over, _ := s.Subscribe(Options{Audio: true})
	if got := kinds(drain(over)); !equalKinds(got, VideoConfig, AudioConfig) {
		t.Fatalf("joiner after overflow got %v", got)
	}
	r.emit(delta(41)) // still waiting for a keyframe
	r.emit(audio(41))
	if got := drain(over); len(got) != 0 {
		t.Fatalf("joiner got %v before a keyframe", kinds(got))
	}
	r.emit(key(42))
	r.emit(delta(43))
	if got := drain(over); !equalKinds(kinds(got), Video, Video) || !got[0].Key {
		t.Fatalf("joiner after the next keyframe got %v", kinds(got))
	}
	refilled, _ := s.Subscribe(Options{})
	if got := drain(refilled); !equalKinds(kinds(got), VideoConfig, Video, Video) || !got[1].Key || got[1].PTS != 43-time.Millisecond {
		t.Fatalf("cache after refill = %v", kinds(got))
	}
	s.Close()
}

// U8 (live edge): a late joiner's cached frames end at the device clock
// now, the latest video packet's PTS plus the host time since it arrived, and
// never before the newest cached frame; live packets keep their PTS.
func TestSession_LateJoinerLiveEdge(t *testing.T) {
	src := newStub(AlwaysOn)
	s := NewSession(src)
	now := fixedClock(s)
	if err := s.Start(Options{}); err != nil {
		t.Fatal(err)
	}
	r := src.next(t)
	r.emit(vcfg)
	r.emit(acfg)
	r.emit(key(0))
	for i := 1; i <= 20; i++ {
		r.emit(audio(time.Duration(i)*time.Second - 500*time.Millisecond))
		r.emit(delta(time.Duration(i) * time.Second))
	}
	*now = now.Add(300 * time.Millisecond) // no packet for 300 ms
	late, err := s.Subscribe(Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	got := drain(late)
	if len(got) != 23 || got[0].Kind != VideoConfig || got[1].Kind != AudioConfig {
		t.Fatalf("late joiner got %v", kinds(got))
	}
	edge := 20*time.Second + 300*time.Millisecond
	for i, p := range got[2:] {
		if p.Kind != Video || p.PTS != edge-time.Duration(20-i)*time.Millisecond || p.Key != (i == 0) {
			t.Fatalf("cached frame %d = %+v", i, p)
		}
	}
	r.emit(audio(20*time.Second + 400*time.Millisecond))
	r.emit(delta(20*time.Second + 450*time.Millisecond))
	if got := drain(late); !equalKinds(kinds(got), Audio, Video) || got[1].PTS != 20*time.Second+450*time.Millisecond {
		t.Fatalf("live = %v", got)
	}

	// A clock that went back still ends the cached frames at the newest
	// frame's own PTS.
	*now = now.Add(-time.Hour)
	back, _ := s.Subscribe(Options{})
	got = drain(back)
	if n := len(got); n != 23 || got[n-1].PTS != 20*time.Second+450*time.Millisecond || got[1].PTS != got[n-1].PTS-21*time.Millisecond {
		t.Fatalf("after the clock went back = %v", got)
	}
	s.Close()
}

// Review 2.harness-review.5: the live edge follows the video only. Audio
// whose PTS runs ahead of the video at arrival (another encoder, another
// latency) no longer pushes the late joiner's cached frames past the next
// live video frame, which the muxer would clamp into a stutter.
func TestSession_LiveEdgeFollowsVideo(t *testing.T) {
	src := newStub(AlwaysOn)
	s := NewSession(src)
	defer s.Close()
	now := fixedClock(s)
	if err := s.Start(Options{}); err != nil {
		t.Fatal(err)
	}
	r := src.next(t)
	r.emit(vcfg)
	r.emit(acfg)
	r.emit(key(10 * time.Second))
	r.emit(delta(10*time.Second + 100*time.Millisecond))
	*now = now.Add(10 * time.Millisecond)
	r.emit(audio(10*time.Second + 600*time.Millisecond)) // 500 ms ahead of the video
	*now = now.Add(40 * time.Millisecond)
	late, err := s.Subscribe(Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	got := drain(late)
	if len(got) != 4 || got[2].Kind != Video || got[3].Kind != Video {
		t.Fatalf("late joiner got %v", kinds(got))
	}
	// The newest video arrived 50 ms ago at 10.1 s: the edge is 10.15 s.
	if edge := 10*time.Second + 150*time.Millisecond; got[3].PTS != edge || got[2].PTS != edge-time.Millisecond {
		t.Fatalf("cached frames at %v, %v; want ending at %v", got[2].PTS, got[3].PTS, edge)
	}
	next := 10*time.Second + 166*time.Millisecond
	r.emit(delta(next))
	if live := drain(late); len(live) != 1 || live[0].PTS != next || live[0].PTS <= got[3].PTS {
		t.Fatalf("the next live frame %v does not follow the cached ones", live)
	}
}

// Review 2.harness-review.6: the GOP cache holds no audio, so audio never
// counts toward its limit (iOS's LPCM is about as large as its video and
// overflowed the cache twice as soon, for packets a joiner never gets).
func TestGOPCache_AudioIsNotCached(t *testing.T) {
	c := gopCache{limit: 100}
	c.add(vcfg)
	c.add(acfg)
	c.add(Packet{Kind: Audio, PTS: 1, Data: make([]byte, 60)}) // before any keyframe
	c.add(Packet{Kind: Video, PTS: 2, Key: true, Data: make([]byte, 40)})
	for i := 0; i < 10; i++ {
		c.add(Packet{Kind: Audio, PTS: time.Duration(3 + i), Data: make([]byte, 60)})
	}
	if len(c.media) != 1 || c.bytes != 40 {
		t.Fatalf("after 600 bytes of audio: %d packets, %d bytes; want the keyframe's 40", len(c.media), c.bytes)
	}
	c.add(Packet{Kind: Video, PTS: 20, Data: make([]byte, 50)})
	if len(c.media) != 2 || c.bytes != 90 {
		t.Fatalf("after a delta: %d packets, %d bytes", len(c.media), c.bytes)
	}
	if got := c.packets(true, 0); !equalKinds(kinds(got), VideoConfig, AudioConfig, Video, Video) {
		t.Fatalf("packets = %v", kinds(got))
	}
	c.add(Packet{Kind: Video, PTS: 21, Data: make([]byte, 20)}) // 110 > 100: over the limit
	if len(c.media) != 0 || c.bytes != 0 || c.video == nil || c.audio == nil {
		t.Fatalf("over the limit: %d packets, %d bytes, configs %v %v", len(c.media), c.bytes, c.video != nil, c.audio != nil)
	}
	c.add(Packet{Kind: Audio, PTS: 22, Data: make([]byte, 60)})
	if len(c.media) != 0 || c.bytes != 0 {
		t.Fatal("audio refilled the cache before a keyframe")
	}
}

// U9: a subscriber that stops reading is closed with ErrLagged while
// another keeps receiving; emit never waits.
func TestSession_LaggedSubscriber(t *testing.T) {
	src := newStub(OnDemand)
	s := NewSession(src)
	stalled, _ := s.Subscribe(Options{})
	r := src.next(t)
	reader, _ := s.Subscribe(Options{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.emit(key(0))
		<-reader.Packets()
		for i := 1; i < subscriberBuffer+50; i++ {
			r.emit(delta(time.Duration(i)))
			if _, ok := <-reader.Packets(); !ok {
				t.Errorf("reader closed at packet %d: %v", i, reader.Err())
				return
			}
		}
	}()
	waitDone(t, done, "emitting past a stalled subscriber")
	n := 0
	for range stalled.Packets() {
		n++
	}
	if n != subscriberBuffer || !errors.Is(stalled.Err(), ErrLagged) {
		t.Errorf("stalled subscriber: %d packets, Err %v; want %d and ErrLagged", n, stalled.Err(), subscriberBuffer)
	}
	if reader.Err() != nil {
		t.Errorf("reader Err = %v", reader.Err())
	}
	s.Close()
	if !errors.Is(reader.Err(), ErrClosed) {
		t.Errorf("after Close reader Err = %v", reader.Err())
	}
}

// U10: an always-on source runs with no subscribers; its failure ends every
// subscriber, sets LastError and reports lost; OnState replays the current
// state, reports changes and stops after cancel.
func TestSession_AlwaysOn(t *testing.T) {
	src := newStub(AlwaysOn)
	s := NewSession(src)
	if _, err := s.Subscribe(Options{}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("subscribe before Start: %v", err)
	}

	type ev struct {
		st     State
		reason string
	}
	var mu sync.Mutex
	var events []ev
	cancel := s.OnState(func(st State, reason string) {
		mu.Lock()
		events = append(events, ev{st, reason})
		mu.Unlock()
	})
	if err := s.Start(Options{FPS: 2}); err == nil {
		t.Fatal("Start with fps 2 accepted")
	}
	if err := s.Start(Options{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(Options{}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	r := src.next(t)
	src.none(t)
	if r.opts.FPS != DefaultFPS {
		t.Errorf("Start opts = %+v", r.opts)
	}
	r.emit(vcfg)
	r.emit(key(0))
	r.emit(delta(1)) // nobody is subscribed: cached and dropped
	sub, err := s.Subscribe(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(drain(sub)); !equalKinds(got, VideoConfig, Video, Video) {
		t.Fatalf("subscriber got %v", got)
	}
	late := make(chan ev, 4)
	cancel2 := s.OnState(func(st State, reason string) { late <- ev{st, reason} })
	if e := <-late; e.st != StateCapturing {
		t.Errorf("replay = %+v, want capturing", e)
	}

	boom := &Unavailable{Reason: ReasonHelperFailed, Message: "boom"}
	r.fail <- boom
	waitDone(t, runDone(s), "Run return")
	for range sub.Packets() {
	}
	if !errors.Is(sub.Err(), boom) || !errors.Is(s.LastError(), boom) {
		t.Fatalf("subscriber Err %v, LastError %v", sub.Err(), s.LastError())
	}
	if e := <-late; e.st != StateLost || e.reason != ReasonHelperFailed {
		t.Errorf("late listener got %+v, want lost/helper-failed", e)
	}
	cancel()
	cancel2()
	mu.Lock()
	got := append([]ev(nil), events...)
	mu.Unlock()
	if len(got) != 3 || got[0].st != StateIdle || got[1].st != StateCapturing || got[2] != (ev{StateLost, ReasonHelperFailed}) {
		t.Errorf("events = %+v", got)
	}
	if st, reason := s.State(); st != StateLost || reason != ReasonHelperFailed {
		t.Errorf("State = %v %q", st, reason)
	}

	// It stays failed: Subscribe and Start answer the failure.
	var u *Unavailable
	if _, err := s.Subscribe(Options{}); !errors.As(err, &u) || u.Reason != ReasonHelperFailed {
		t.Errorf("subscribe after loss: %v", err)
	}
	if err := s.Start(Options{}); !errors.As(err, &u) {
		t.Errorf("Start after loss: %v", err)
	}
	s.Close()
	mu.Lock()
	if len(events) != 3 {
		t.Errorf("listener called after cancel: %+v", events)
	}
	mu.Unlock()
}

// An always-on source that fails before any packet is unavailable, not
// lost; a plain error is reported as capture-lost.
func TestSession_AlwaysOnFailsAtStart(t *testing.T) {
	src := newStub(AlwaysOn)
	s := NewSession(src)
	if err := s.Start(Options{}); err != nil {
		t.Fatal(err)
	}
	r := src.next(t)
	r.fail <- errors.New("socket closed")
	waitDone(t, runDone(s), "Run return")
	if st, reason := s.State(); st != StateUnavailable || reason != ReasonCaptureLost {
		t.Errorf("State = %v %q", st, reason)
	}
	var u *Unavailable
	if _, err := s.Subscribe(Options{}); !errors.As(err, &u) || u.Reason != ReasonCaptureLost || u.Message != "socket closed" {
		t.Errorf("subscribe: %v", err)
	}
	s.Close()
}

// An on-demand source's failure ends its subscribers and the session
// retries on the next subscribe; a Run that returns nil on its own counts
// as a failure.
func TestSession_OnDemandFailureRetries(t *testing.T) {
	src := newStub(OnDemand)
	s := NewSession(src)
	if err := s.Start(Options{}); err == nil {
		t.Error("Start on an on-demand session succeeded")
	}
	sub, _ := s.Subscribe(Options{})
	r := src.next(t)
	r.fail <- &Unavailable{Reason: ReasonScrcpyFailed, Message: "server died"}
	waitDone(t, runDone(s), "Run return")
	for range sub.Packets() {
	}
	if st, reason := s.State(); st != StateUnavailable || reason != ReasonScrcpyFailed {
		t.Errorf("State = %v %q", st, reason)
	}
	sub2, err := s.Subscribe(Options{})
	if err != nil {
		t.Fatal(err)
	}
	r2 := src.next(t)
	if st, _ := s.State(); st != StateCapturing {
		t.Errorf("retry state %v", st)
	}
	r2.fail <- nil
	waitDone(t, runDone(s), "Run return")
	for range sub2.Packets() {
	}
	if !errors.Is(sub2.Err(), errSourceEnded) {
		t.Errorf("Err = %v", sub2.Err())
	}
	s.Close()
	s.Close()
	if _, err := s.Subscribe(Options{}); !errors.Is(err, ErrClosed) {
		t.Errorf("subscribe after Close: %v", err)
	}
	if err := s.Start(Options{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close: %v", err)
	}
	if _, err := s.Subscribe(Options{FPS: 99}); err == nil {
		t.Error("fps 99 accepted")
	}
}

// The real linger timer stops the source after the delay.
func TestSession_RealTimer(t *testing.T) {
	src := newStub(OnDemand)
	s := NewSession(src)
	stop := s.afterFunc(time.Hour, func() { t.Error("stopped timer fired") })
	if !stop() {
		t.Error("Stop of a pending timer reported false")
	}
	fired := make(chan struct{})
	s.afterFunc(time.Millisecond, func() { close(fired) })
	waitDone(t, fired, "timer")
	s.Close()
}

func TestStateAndKindStrings(t *testing.T) {
	for want, v := range map[string]interface{ String() string }{
		"idle": StateIdle, "capturing": StateCapturing, "unavailable": StateUnavailable, "lost": StateLost, "unknown": State(9),
		"video-config": VideoConfig, "video": Video, "audio-config": AudioConfig, "audio": Audio,
	} {
		if v.String() != want {
			t.Errorf("%v.String() = %q, want %q", v, v.String(), want)
		}
	}
	if Kind(0).String() != "unknown" {
		t.Error("Kind(0)")
	}
}

// plannedStub is a stubSource that plans audio only when the first
// subscriber asks for it (Android 11–12's scrcpy).
type plannedStub struct{ *stubSource }

func (p plannedStub) PlansAudio(first Options) bool { return first.Audio }

// A later subscriber asking for audio from a run that carries none gets
// Options.Audio false and no audio packets; a run started with audio gives
// it to later subscribers.
func TestSession_AudioPlanner(t *testing.T) {
	src := plannedStub{newStub(OnDemand)}
	s := NewSession(src)
	defer s.Close()
	first, err := s.Subscribe(Options{})
	if err != nil {
		t.Fatal(err)
	}
	run := src.next(t)
	if run.opts.Audio {
		t.Fatal("run started with audio for a subscriber that did not ask")
	}
	run.emit(vcfg)
	run.emit(acfg)
	run.emit(key(0))
	run.emit(audio(0))
	late, err := s.Subscribe(Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	if late.Options().Audio {
		t.Error("late subscriber on a run without audio got Audio true")
	}
	if got := kinds(drain(late)); !equalKinds(got, VideoConfig, Video) {
		t.Errorf("late subscriber got %v", got)
	}
	first.Close()
	late.Close()
	s.Close()

	src2 := plannedStub{newStub(OnDemand)}
	s2 := NewSession(src2)
	defer s2.Close()
	if _, err := s2.Subscribe(Options{Audio: true}); err != nil {
		t.Fatal(err)
	}
	src2.next(t)
	sub, err := s2.Subscribe(Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sub.Options().Audio {
		t.Error("subscriber on a run with audio got Audio false")
	}
}

// RunDone is closed before any Run, open while one runs, and closed once a
// failed Run's state is recorded.
func TestSession_RunDone(t *testing.T) {
	src := newStub(AlwaysOn)
	s := NewSession(src)
	waitDone(t, s.RunDone(), "RunDone before any Run")
	s.Start(Options{})
	r := src.next(t)
	select {
	case <-s.RunDone():
		t.Fatal("RunDone closed while the source runs")
	default:
	}
	r.fail <- &Unavailable{Reason: ReasonNoCaptureDevice}
	waitDone(t, s.RunDone(), "RunDone after the Run failed")
	if st, reason := s.State(); st != StateUnavailable || reason != ReasonNoCaptureDevice {
		t.Fatalf("state %v %s once RunDone closed", st, reason)
	}
	s.Close()
}
