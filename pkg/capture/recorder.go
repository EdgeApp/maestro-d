package capture

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/fmp4"
)

// ErrEndedEarly is Stop's answer when the recording ended before Stop (it
// fell behind, ErrLagged, or the source failed) but what it recorded was
// saved at dest. Callers treat it as saved with a warning; the error
// wraps the reason too.
var ErrEndedEarly = errors.New("capture: the recording ended early")

// Seams tests replace: rename moves a finished recording into place, and
// startLoop runs a recorder's packet loop.
var (
	rename    = os.Rename
	startLoop = func(loop func()) { go loop() }
)

// Recorder writes a session's stream to an MP4 file: the init segment and
// fragments go to a temporary file as they arrive (the bytes a stream
// sends, but each track's newest fragment is held until that track's next
// one, so it is written with its real duration), and Stop finalizes it so
// a player knows its length and can seek.
type Recorder struct {
	sub  *Subscriber
	f    *os.File
	tmp  string
	mux  *Mux
	done chan struct{}

	// Written by the loop goroutine, read after done.
	offset   int64
	index    map[uint32][]fmp4.IndexEntry
	held     map[uint32]Chunk // each track's newest fragment, not yet written
	writeErr error

	stopMu  sync.Mutex
	stopped bool
}

// NewRecorder subscribes to s and records into a temporary file in tmpDir
// until Stop. The recorder's channel holds 4096 packets; falling further
// behind ends the recording with ErrLagged.
func NewRecorder(s *Session, tmpDir string, opts Options) (*Recorder, error) {
	opts, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(tmpDir, ".maestro-d-recording-*.mp4")
	if err != nil {
		return nil, err
	}
	sub, err := s.subscribe(opts, recorderBuffer)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	r := &Recorder{
		sub:   sub,
		f:     f,
		tmp:   f.Name(),
		mux:   NewMux(sub.Options()),
		done:  make(chan struct{}),
		index: map[uint32][]fmp4.IndexEntry{},
		held:  map[uint32]Chunk{},
	}
	startLoop(r.loop)
	return r, nil
}

// Err is why the recording ended early (the subscriber's error, such as
// ErrLagged or the source's failure), or nil while it records.
func (r *Recorder) Err() error {
	select {
	case <-r.done:
	default:
		return nil
	}
	if err := r.sub.Err(); err != nil && !errors.Is(err, ErrClosed) {
		return err
	}
	return r.writeErr
}

func (r *Recorder) loop() {
	defer close(r.done)
	for p := range r.sub.Packets() {
		if r.writeErr != nil {
			continue
		}
		chunks, _ := r.mux.Push(p) // a sample that cannot be muxed is skipped
		r.write(chunks)
	}
	// The recording ended before the mux started (it was still waiting for
	// the audio config): what it holds is the recording, video-only.
	if r.writeErr == nil {
		chunks, _ := r.mux.Flush()
		r.write(chunks)
	}
	// The last fragment of each track keeps its estimated duration.
	for _, id := range []uint32{fmp4.VideoTrackID, fmp4.AudioTrackID} {
		if c, ok := r.held[id]; ok {
			delete(r.held, id)
			r.put(c)
		}
	}
}

// write retimes chunks and appends them to the file. The muxer can only
// estimate a sample's duration from the gap before it (clamped to a
// second), so a sample after a still screen overlapped the next one or
// left a hole; holding each track's newest fragment until the next one
// arrives writes the real gap instead.
func (r *Recorder) write(chunks []Chunk) {
	for _, c := range chunks {
		if c.Init {
			r.put(c)
			continue
		}
		prev, ok := r.held[c.Track]
		r.held[c.Track] = c
		if !ok {
			continue
		}
		if gap := c.Time - prev.Time; c.Time > prev.Time && gap <= math.MaxUint32 {
			fmp4.SetSampleDuration(prev.Data, uint32(gap))
		}
		r.put(prev)
	}
}

// put appends one chunk to the file and indexes its keyframes and audio.
func (r *Recorder) put(c Chunk) {
	if r.writeErr != nil {
		return
	}
	if !c.Init && (c.Track == fmp4.AudioTrackID || c.Key) {
		r.index[c.Track] = append(r.index[c.Track], fmp4.IndexEntry{Time: c.Time, MoofOffset: uint64(r.offset)})
	}
	n, err := r.f.Write(c.Data)
	r.offset += int64(n)
	if err != nil {
		r.writeErr = err
	}
}

// Stop ends the recording and moves it to dest: it patches mehd with the
// duration, appends mfra (every video keyframe and audio fragment with its
// moof offset), fsyncs, and renames, copying instead across filesystems.
// When the recording ended early the file is still saved, and the error
// wraps ErrEndedEarly and the reason. Stopping again is a no-op.
func (r *Recorder) Stop(dest string) error {
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	if r.stopped {
		return nil
	}
	r.stopped = true
	return r.stop(dest)
}

func (r *Recorder) stop(dest string) error {
	r.sub.Close()
	<-r.done
	early := r.Err()
	if !r.mux.Started() {
		r.f.Close()
		os.Remove(r.tmp)
		return errors.Join(errors.New("capture: the recording has no video"), early)
	}
	if err := r.finalize(); err != nil {
		r.f.Close()
		os.Remove(r.tmp)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		os.Remove(r.tmp)
		return err
	}
	if err := moveFile(r.tmp, dest); err != nil {
		os.Remove(r.tmp)
		return err
	}
	if early != nil {
		return fmt.Errorf("%w: %w", ErrEndedEarly, early)
	}
	return nil
}

func (r *Recorder) finalize() error {
	if r.writeErr != nil {
		return r.writeErr
	}
	var d [8]byte
	binary.BigEndian.PutUint64(d[:], r.mux.Duration())
	if _, err := r.f.WriteAt(d[:], int64(r.mux.MehdOffset())); err != nil {
		return err
	}
	var tracks []fmp4.TrackIndex
	for _, id := range []uint32{fmp4.VideoTrackID, fmp4.AudioTrackID} {
		if id == fmp4.AudioTrackID && !r.mux.HasAudio() {
			continue
		}
		tracks = append(tracks, fmp4.TrackIndex{Track: id, Entries: r.index[id]})
	}
	if _, err := r.f.WriteAt(fmp4.MFRA(tracks), r.offset); err != nil {
		return err
	}
	if err := r.f.Sync(); err != nil {
		return err
	}
	return r.f.Close()
}

// moveFile renames src to dst, or copies and removes it when they are on
// different filesystems.
func moveFile(src, dst string) error {
	err := rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	in.Close()
	return os.Remove(src)
}
