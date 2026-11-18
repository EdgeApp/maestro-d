package capture

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Session errors.
var (
	// ErrLagged ends a subscriber whose channel filled up: it read too
	// slowly, and the source is never made to wait.
	ErrLagged = errors.New("capture: subscriber fell behind the stream")
	// ErrClosed ends a subscriber that closed itself, or every subscriber
	// of a closed session.
	ErrClosed = errors.New("capture: session closed")
	// ErrNotStarted answers a subscribe to an always-on session whose
	// source was never started.
	ErrNotStarted = errors.New("capture: always-on capture not started")
	// errSourceEnded is a source's Run returning nil on its own.
	errSourceEnded = errors.New("capture: source ended")
)

const (
	// lingerAfterLastConsumer keeps an on-demand source running after its
	// last subscriber leaves, so a quick reconnect does not restart it.
	lingerAfterLastConsumer = 10 * time.Second
	// gopCacheLimit caps the cached video since the last keyframe.
	gopCacheLimit = 32 << 20
	// subscriberBuffer and recorderBuffer are a subscriber's channel
	// capacity for live packets.
	subscriberBuffer = 512
	recorderBuffer   = 4096
)

// State is a session's capture state.
type State uint8

const (
	StateIdle        State = iota // an on-demand source not running
	StateCapturing                // the source is running
	StateUnavailable              // the source failed (an on-demand one retries on the next subscribe)
	StateLost                     // an always-on source failed after it had produced packets
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateCapturing:
		return "capturing"
	case StateUnavailable:
		return "unavailable"
	case StateLost:
		return "lost"
	}
	return "unknown"
}

// Session owns one device's Source and fans its packets out to
// subscribers. Delivery never blocks the source. The zero value is not
// usable; call NewSession.
type Session struct {
	src Source
	// afterFunc schedules the linger stop; tests replace it.
	afterFunc func(time.Duration, func()) (stop func() bool)
	// now is the host clock; tests replace it.
	now func() time.Time

	mu        sync.Mutex
	closed    bool
	subs      map[*Subscriber]struct{}
	running   bool
	gen       uint64
	cancel    context.CancelFunc
	done      chan struct{} // closed when the latest Run returned
	opts      Options       // the running source's options
	runAudio  bool          // the running source carries audio (AudioPlanner)
	emitted   bool          // the running source has emitted a packet
	lingerID  uint64
	lingerOff func() bool
	cache     gopCache
	// lastVideoPTS is the latest video packet's device time and
	// lastVideoAt when it arrived: a late joiner's live edge is
	// lastVideoPTS plus the time since. Only video counts: only the cached
	// video is retimed to the edge, and audio can run ahead of it (another
	// encoder, another latency), which put the edge after the next live
	// video frame.
	lastVideoPTS time.Duration
	lastVideoAt  time.Time
	lastErr      error
	failed       bool // an always-on source failed; it stays failed

	state     State
	reason    string
	listeners map[uint64]*listener
	nextID    uint64
	pending   []stateEvent

	notifyMu sync.Mutex // serialises listener calls, in state order
}

type listener struct {
	fn        func(State, string)
	cancelled atomic.Bool
}

type stateEvent struct {
	state  State
	reason string
}

// NewSession returns an idle session for src.
func NewSession(src Source) *Session {
	return &Session{
		src: src,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
		now:       time.Now,
		subs:      map[*Subscriber]struct{}{},
		listeners: map[uint64]*listener{},
		cache:     gopCache{limit: gopCacheLimit},
	}
}

// Mode is the source's mode.
func (s *Session) Mode() Mode { return s.src.Mode() }

// FPSControl reports whether the source honours the requested frame rate.
func (s *Session) FPSControl() bool { return s.src.FPSControl() }

// FPS is the running source's requested frame rate, or 0 when it is not
// running.
func (s *Session) FPS() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return 0
	}
	return s.opts.FPS
}

// State returns the current state and its reason.
func (s *Session) State() (State, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.reason
}

// LastError is the error the source last failed with, or nil.
func (s *Session) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// OnState calls fn with the current state at once, then on every change,
// until cancel is called. Calls are made one at a time, in order.
func (s *Session) OnState(fn func(state State, reason string)) (cancel func()) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.dispatchLocked()
	l := &listener{fn: fn}
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.listeners[id] = l
	st, reason := s.state, s.reason
	s.mu.Unlock()
	fn(st, reason)
	return func() {
		l.cancelled.Store(true)
		s.mu.Lock()
		delete(s.listeners, id)
		s.mu.Unlock()
	}
}

// Start runs an always-on source until Close. Starting a running session
// is a no-op; a failed one answers its error.
func (s *Session) Start(opts Options) error {
	opts, err := opts.Normalize()
	if err != nil {
		return err
	}
	defer s.flush()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return ErrClosed
	case s.src.Mode() != AlwaysOn:
		return errors.New("capture: Start is for always-on sources; on-demand ones start with a subscriber")
	case s.failed:
		return asUnavailable(s.lastErr)
	case s.running:
		return nil
	}
	s.startLocked(opts)
	return nil
}

// Subscribe adds a subscriber. An on-demand source starts with the first
// one, at its frame rate; later ones join at the running rate. The
// subscriber first receives the latest configs, then the cached video
// since the last keyframe, then live packets; its first video packet is a
// keyframe. The cached frames are retimed 1 ms apart ending at the live
// edge (of the video: the newest video packet's time plus the time since),
// so playback starts at the join instead of replaying the seconds
// since that keyframe; no audio is cached (audio starts with the first
// live packet).
func (s *Session) Subscribe(opts Options) (*Subscriber, error) {
	return s.subscribe(opts, subscriberBuffer)
}

func (s *Session) subscribe(opts Options, buffer int) (*Subscriber, error) {
	opts, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	defer s.flush()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if !s.running {
		if s.src.Mode() == AlwaysOn {
			if s.failed {
				return nil, asUnavailable(s.lastErr)
			}
			return nil, ErrNotStarted
		}
		s.startLocked(opts)
	}
	s.stopLingerLocked()
	audio := opts.Audio && s.runAudio
	cached := s.cache.packets(audio, s.liveEdgeLocked())
	sub := &Subscriber{
		s:    s,
		ch:   make(chan Packet, buffer+len(cached)),
		opts: Options{FPS: s.opts.FPS, Audio: audio},
	}
	for _, p := range cached {
		sub.deliver(p)
	}
	s.subs[sub] = struct{}{}
	return sub, nil
}

// Close stops the source, ends every subscriber with ErrClosed, and waits
// for the source's Run to return.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	done := s.done
	s.stopLingerLocked()
	if s.running {
		s.stopLocked()
	}
	for sub := range s.subs {
		s.dropLocked(sub, ErrClosed)
	}
	s.mu.Unlock()
	s.flush()
	if done != nil {
		<-done
	}
}

// startLocked starts a new Run. A previous Run that is still returning is
// waited for first, so two never overlap on one device.
func (s *Session) startLocked(opts Options) {
	s.gen++
	gen := s.gen
	ctx, cancel := context.WithCancel(context.Background())
	prev := s.done
	done := make(chan struct{})
	s.running, s.cancel, s.done, s.opts, s.emitted = true, cancel, done, opts, false
	s.runAudio = true
	if p, ok := s.src.(AudioPlanner); ok {
		s.runAudio = p.PlansAudio(opts)
	}
	s.setStateLocked(StateCapturing, "")
	go func() {
		defer close(done)
		if prev != nil {
			<-prev
		}
		err := s.src.Run(ctx, opts, func(p Packet) { s.emit(gen, p) })
		s.ended(gen, err)
	}()
}

// stopLocked cancels the running source without failing it.
func (s *Session) stopLocked() {
	s.running = false
	s.cancel()
	s.resetCacheLocked()
	s.setStateLocked(StateIdle, "")
}

// ended handles Run returning. A source stopped by the session is already
// accounted for; one that ended by itself fails every subscriber.
func (s *Session) ended(gen uint64, err error) {
	defer s.flush()
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen || !s.running {
		return
	}
	if err == nil {
		err = errSourceEnded
	}
	s.running = false
	s.cancel()
	s.resetCacheLocked()
	s.lastErr = err
	for sub := range s.subs {
		s.dropLocked(sub, err)
	}
	reason := ReasonOf(err, ReasonCaptureLost)
	switch {
	case s.src.Mode() == AlwaysOn && s.emitted:
		s.failed = true
		s.setStateLocked(StateLost, reason)
	case s.src.Mode() == AlwaysOn:
		s.failed = true
		s.setStateLocked(StateUnavailable, reason)
	default:
		s.setStateLocked(StateUnavailable, reason)
	}
}

func (s *Session) resetCacheLocked() {
	s.cache.reset()
	s.lastVideoPTS, s.lastVideoAt = 0, time.Time{}
}

// liveEdgeLocked estimates the device's video clock now: the latest video
// packet's PTS plus the host time since it arrived.
func (s *Session) liveEdgeLocked() time.Duration {
	if s.lastVideoAt.IsZero() {
		return 0
	}
	return s.lastVideoPTS + max(s.now().Sub(s.lastVideoAt), 0)
}

// emit delivers one packet from the source of generation gen.
func (s *Session) emit(gen uint64, p Packet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen || !s.running {
		return
	}
	s.emitted = true
	if p.Kind == Video {
		if p.PTS > s.lastVideoPTS || s.lastVideoAt.IsZero() {
			s.lastVideoPTS = p.PTS
		}
		s.lastVideoAt = s.now()
	}
	s.cache.add(p)
	for sub := range s.subs {
		if !sub.deliver(p) {
			s.dropLocked(sub, ErrLagged)
		}
	}
}

// dropLocked ends a subscriber with err. The last one leaving an
// on-demand source starts the linger.
func (s *Session) dropLocked(sub *Subscriber, err error) {
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	sub.err = err
	close(sub.ch)
	if len(s.subs) == 0 && s.running && !s.closed && s.src.Mode() == OnDemand {
		s.startLingerLocked()
	}
}

func (s *Session) startLingerLocked() {
	if s.lingerOff != nil {
		return
	}
	s.lingerID++
	id, gen := s.lingerID, s.gen
	s.lingerOff = s.afterFunc(lingerAfterLastConsumer, func() {
		defer s.flush()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.lingerID != id || s.lingerOff == nil {
			return
		}
		s.lingerOff = nil
		if s.gen == gen && s.running && len(s.subs) == 0 {
			s.stopLocked()
		}
	})
}

func (s *Session) stopLingerLocked() {
	if s.lingerOff == nil {
		return
	}
	s.lingerOff()
	s.lingerOff = nil
	s.lingerID++
}

func (s *Session) setStateLocked(st State, reason string) {
	if st == s.state && reason == s.reason {
		return
	}
	s.state, s.reason = st, reason
	s.pending = append(s.pending, stateEvent{st, reason})
}

// flush calls the listeners for every queued state change.
func (s *Session) flush() {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.dispatchLocked()
}

func (s *Session) dispatchLocked() {
	for {
		s.mu.Lock()
		if len(s.pending) == 0 {
			s.mu.Unlock()
			return
		}
		ev := s.pending[0]
		s.pending = s.pending[1:]
		ids := make([]uint64, 0, len(s.listeners))
		for id := range s.listeners {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		ls := make([]*listener, len(ids))
		for i, id := range ids {
			ls[i] = s.listeners[id]
		}
		s.mu.Unlock()
		for _, l := range ls {
			if !l.cancelled.Load() {
				l.fn(ev.state, ev.reason)
			}
		}
	}
}

// asUnavailable returns err as an *Unavailable, wrapping a plain error as
// capture-lost.
func asUnavailable(err error) error {
	var u *Unavailable
	if errors.As(err, &u) {
		return err
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return &Unavailable{Reason: ReasonCaptureLost, Message: msg}
}

// Subscriber receives a session's packets on a buffered channel.
type Subscriber struct {
	s     *Session
	ch    chan Packet
	opts  Options
	keyed bool  // a keyframe has been delivered
	err   error // set, under the session lock, when ch is closed
}

// Packets is the subscriber's channel. It is closed when the subscriber
// ends; Err then says why.
func (sub *Subscriber) Packets() <-chan Packet { return sub.ch }

// Options are the subscriber's options: FPS is the running source's rate,
// and Audio is false when the running source carries none.
func (sub *Subscriber) Options() Options { return sub.opts }

// Err is why the subscriber ended (ErrLagged, ErrClosed or the source's
// error), or nil while it runs.
func (sub *Subscriber) Err() error {
	sub.s.mu.Lock()
	defer sub.s.mu.Unlock()
	return sub.err
}

// Close unsubscribes. Its channel is closed and Err answers ErrClosed.
func (sub *Subscriber) Close() {
	sub.s.mu.Lock()
	sub.s.dropLocked(sub, ErrClosed)
	sub.s.mu.Unlock()
	sub.s.flush()
}

// deliver queues p unless the subscriber filters it out: audio when it
// did not ask for audio, and media before its first keyframe. It reports
// false when the channel is full.
func (sub *Subscriber) deliver(p Packet) bool {
	switch p.Kind {
	case AudioConfig:
		if !sub.opts.Audio {
			return true
		}
	case Audio:
		if !sub.opts.Audio || !sub.keyed {
			return true
		}
	case Video:
		if !sub.keyed {
			if !p.Key {
				return true
			}
			sub.keyed = true
		}
	}
	select {
	case sub.ch <- p:
		return true
	default:
		return false
	}
}

// gopCache holds the latest configs and every video packet since the last
// keyframe, up to limit bytes; over it, only the configs, until the next
// keyframe. Audio packets are not cached: a late joiner's audio starts
// with the first live packet, so they would only use up the limit (iOS's
// LPCM is about as large as its video).
type gopCache struct {
	limit int
	video *Packet
	audio *Packet
	media []Packet // video only, from a keyframe
	bytes int
}

func (c *gopCache) add(p Packet) {
	switch p.Kind {
	case VideoConfig:
		c.video = &p
	case AudioConfig:
		c.audio = &p
	case Video:
		if p.Key {
			c.media = append(c.media[:0], p)
			c.bytes = len(p.Data)
		} else if len(c.media) > 0 {
			c.media = append(c.media, p)
			c.bytes += len(p.Data)
		}
	}
	if c.bytes > c.limit {
		c.media, c.bytes = nil, 0
	}
}

// joinFrameStep is the spacing of a late joiner's cached video frames.
const joinFrameStep = time.Millisecond

// packets returns what a late joiner starts with: the configs (the audio
// one only when asked for), then the cached video frames retimed so the
// keyframe and every delta after it sit joinFrameStep apart, the last one
// at edge (the device clock now). A decoder still gets every frame it
// needs, but the span since the keyframe collapses to a few milliseconds
// and live packets go on at real time from edge. No audio is cached: it
// needs no keyframe, so audio starts with the first live packet.
func (c *gopCache) packets(audio bool, edge time.Duration) []Packet {
	out := make([]Packet, 0, len(c.media)+2)
	if c.video != nil {
		out = append(out, *c.video)
	}
	if audio && c.audio != nil {
		out = append(out, *c.audio)
	}
	video := c.media
	if n := len(video); n > 0 {
		// Never before the newest cached frame's own time, so live frames
		// keep their order after it.
		edge = max(edge, video[n-1].PTS)
		for i, p := range video {
			p.PTS = edge - time.Duration(n-1-i)*joinFrameStep
			out = append(out, p)
		}
	}
	return out
}

func (c *gopCache) reset() {
	c.video, c.audio, c.media, c.bytes = nil, nil, nil, 0
}
