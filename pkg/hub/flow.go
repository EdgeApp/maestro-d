package hub

// flow.go: per-exchange flow control for streamed bodies. A link's read
// loop never blocks on a body's reader: each data frame is queued on its
// exchange's inbound body. The sender of a body may have at most
// flowWindow bytes unacknowledged; the reader acknowledges what it has
// consumed with credit frames. So a slow reader stalls only its own
// exchange's sender, never the link (other exchanges, pongs). A reader
// that closes its body before the end tells the sender to stop.

import (
	"errors"
	"io"
	"sync"
)

// flowWindow is how many bytes of one body may be in flight unread.
const flowWindow = 8 << 20

// creditStop, as a credit frame's N, tells the sender the reader is gone.
const creditStop = -1

// errStreamStopped ends a body whose reader closed it.
var errStreamStopped = errors.New("the reader closed the body")

// errFlowOverrun ends a body whose sender ignored its window.
var errFlowOverrun = errors.New("peer sent past its flow-control window")

// inbound is a streamed body this side receives: the read loop pushes
// data frames, one reader reads them.
type inbound struct {
	l  *Link
	id string

	mu      sync.Mutex
	chunks  [][]byte
	queued  int
	unacked int
	// end is the error a drained body returns (io.EOF at its end frame).
	end error
	// closed is set when the reader closed the body; it then returns
	// closedErr and drops what still arrives.
	closed    bool
	closedErr error
	wake      chan struct{}
}

func newInbound(l *Link, id string) *inbound {
	return &inbound{l: l, id: id, wake: make(chan struct{}, 1)}
}

func (in *inbound) signal() {
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// push queues one data frame's payload; it never blocks.
func (in *inbound) push(p []byte) {
	in.mu.Lock()
	if in.closed || in.end != nil {
		in.mu.Unlock()
		return
	}
	in.chunks = append(in.chunks, p)
	in.queued += len(p)
	over := in.queued > 2*flowWindow
	in.mu.Unlock()
	if over {
		_ = in.CloseWithError(errFlowOverrun)
		return
	}
	in.signal()
}

// finish ends the body: io.EOF for its end frame, else the error the
// reader gets once it has drained what came before.
func (in *inbound) finish(err error) {
	if err == nil {
		err = io.EOF
	}
	in.mu.Lock()
	if in.end == nil {
		in.end = err
	}
	in.mu.Unlock()
	in.signal()
}

func (in *inbound) Read(p []byte) (int, error) {
	for {
		in.mu.Lock()
		if in.closed {
			err := in.closedErr
			in.mu.Unlock()
			return 0, err
		}
		if len(in.chunks) > 0 {
			n := copy(p, in.chunks[0])
			if n == len(in.chunks[0]) {
				in.chunks[0] = nil
				in.chunks = in.chunks[1:]
			} else {
				in.chunks[0] = in.chunks[0][n:]
			}
			in.queued -= n
			in.unacked += n
			credit := 0
			if in.unacked >= flowWindow/4 {
				credit, in.unacked = in.unacked, 0
			}
			in.mu.Unlock()
			if credit > 0 {
				_ = in.l.writeFrame(Frame{T: frameCredit, ID: in.id, N: credit})
			}
			return n, nil
		}
		if in.end != nil {
			err := in.end
			in.mu.Unlock()
			return 0, err
		}
		in.mu.Unlock()
		<-in.wake
	}
}

// CloseWithError closes the body from the reader's side: Read returns
// err, what still arrives is dropped, and a sender that has not finished
// is told to stop.
func (in *inbound) CloseWithError(err error) error {
	if err == nil {
		err = io.ErrClosedPipe
	}
	in.mu.Lock()
	if in.closed {
		in.mu.Unlock()
		return nil
	}
	in.closed, in.closedErr = true, err
	in.chunks, in.queued = nil, 0
	stop := in.end == nil
	in.mu.Unlock()
	in.signal()
	if stop {
		_ = in.l.writeFrame(Frame{T: frameCredit, ID: in.id, N: creditStop})
	}
	return nil
}

func (in *inbound) Close() error { return in.CloseWithError(nil) }

// credits is a streamed body this side sends: its unacknowledged bytes.
type credits struct {
	mu      sync.Mutex
	out     int
	stopped bool
	wake    chan struct{}
}

func newCredits() *credits { return &credits{wake: make(chan struct{})} }

// acquire waits until n more bytes may be sent, the reader stops the body
// (errStreamStopped), or done closes (errLinkDown).
func (o *credits) acquire(n int, done <-chan struct{}) error {
	for {
		o.mu.Lock()
		if o.stopped {
			o.mu.Unlock()
			return errStreamStopped
		}
		if o.out < flowWindow {
			o.out += n
			o.mu.Unlock()
			return nil
		}
		wake := o.wake
		o.mu.Unlock()
		select {
		case <-wake:
		case <-done:
			return errLinkDown
		}
	}
}

// credit applies a credit frame: n bytes read, or creditStop.
func (o *credits) credit(n int) {
	o.mu.Lock()
	if n == creditStop {
		o.stopped = true
	} else {
		o.out = max(0, o.out-n)
	}
	close(o.wake)
	o.wake = make(chan struct{})
	o.mu.Unlock()
}
