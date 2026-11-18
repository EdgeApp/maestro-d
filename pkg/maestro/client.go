package maestro

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/devicelab-dev/maestro-runner/pkg/tracelog"
)

const (
	defaultCallTimeout = 30 * time.Second
	defaultDialTimeout = 10 * time.Second
)

// Client communicates with the DeviceLab on-device driver over WebSocket.
type Client struct {
	conn *websocket.Conn

	// Connection parameters — exactly one will be set
	socketPath string
	tcpPort    int

	// Request ID counter
	nextID atomic.Int64

	// Pending requests: id → channel
	pending sync.Map // map[int64]chan *Response

	// Event handlers
	events sync.Map // map[string]EventHandler

	// Lifecycle
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	logger    *log.Logger
	traceFile io.Closer // the trace's file, closed by Close
}

// NewClient creates a client that connects via Unix socket.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		done:       make(chan struct{}),
		logger:     newLogger(),
	}
}

// NewClientTCP creates a client that connects via TCP port.
func NewClientTCP(port int) *Client {
	return &Client{
		tcpPort: port,
		done:    make(chan struct{}),
		logger:  newLogger(),
	}
}

// newLogger is the default trace: none. A session that asks for one
// (MAESTRO_D_ANDROID_TRACE=1) gets it through SetLogPath.
func newLogger() *log.Logger {
	return tracelog.Discard()
}

// SetLogPath traces method names and ids to path, capped at
// tracelog.MaxBytes.
func (c *Client) SetLogPath(path string) {
	if l, f := tracelog.Open(path, tracelog.MaxBytes); l != nil {
		c.closeTrace()
		c.logger, c.traceFile = l, f
	}
}

// closeTrace closes the trace file, if any; later calls trace nowhere.
func (c *Client) closeTrace() {
	if c.traceFile != nil {
		_ = c.traceFile.Close()
		c.traceFile = nil
		c.logger = tracelog.Discard()
	}
}

// Connect dials the WebSocket server and starts the read loop.
func (c *Client) Connect() error {
	return c.ConnectWithTimeout(defaultDialTimeout)
}

// ConnectWithTimeout dials with a custom timeout.
func (c *Client) ConnectWithTimeout(timeout time.Duration) error {
	c.ctx, c.cancel = context.WithCancel(context.Background())

	dialCtx, dialCancel := context.WithTimeout(c.ctx, timeout)
	defer dialCancel()

	var conn *websocket.Conn
	var err error

	if c.socketPath != "" {
		httpClient := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", c.socketPath)
				},
			},
		}
		conn, _, err = websocket.Dial(dialCtx, "ws://localhost/ws", &websocket.DialOptions{
			HTTPClient: httpClient,
		})
	} else {
		url := fmt.Sprintf("ws://127.0.0.1:%d/ws", c.tcpPort)
		conn, _, err = websocket.Dial(dialCtx, url, nil)
	}
	if err != nil {
		c.cancel()
		return fmt.Errorf("websocket dial: %w", err)
	}

	// Allow large messages (screenshots can be several MB)
	conn.SetReadLimit(32 * 1024 * 1024) // 32 MB

	c.conn = conn
	go c.readLoop()
	return nil
}

// Call sends a request and waits for the matching response with the default timeout.
func (c *Client) Call(method string, params interface{}) (*Response, error) {
	return c.CallWithTimeout(method, params, defaultCallTimeout)
}

// CallWithTimeout sends a request and waits for the matching response.
func (c *Client) CallWithTimeout(method string, params interface{}, timeout time.Duration) (*Response, error) {
	id := c.nextID.Add(1)

	req := Request{
		ID:     id,
		Method: method,
		Params: params,
	}

	// Register pending channel before sending
	ch := make(chan *Response, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	start := time.Now()
	c.logger.Printf("→ %s id=%d", method, id)

	writeCtx, writeCancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer writeCancel()

	if err := c.conn.Write(writeCtx, websocket.MessageText, data); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	// Wait for response
	select {
	case resp := <-ch:
		elapsed := time.Since(start)
		if resp.Error != nil {
			// The driver's error text names the element or app, never typed
			// input; it is what a trace is read for.
			c.logger.Printf("← %s id=%d [%v] ERR: %s", method, id, elapsed, resp.Error.Message)
			return nil, resp.Error
		}
		c.logger.Printf("← %s id=%d [%v] OK", method, id, elapsed)
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for response to %s (id=%d)", method, id)
	case <-c.ctx.Done():
		return nil, fmt.Errorf("client closed")
	}
}

// readLoop reads frames from the WebSocket and dispatches them.
func (c *Client) readLoop() {
	defer close(c.done)

	for {
		msgType, data, err := c.conn.Read(c.ctx)
		if err != nil {
			if c.ctx.Err() != nil {
				return // normal shutdown
			}
			c.logger.Printf("read error: %v", err)
			return
		}

		if msgType == websocket.MessageBinary {
			c.dispatchBinary(data)
		} else {
			c.dispatch(data)
		}
	}
}

// dispatchBinary handles binary frames (e.g., screenshots).
// Format: [8-byte big-endian request ID][raw payload bytes]
func (c *Client) dispatchBinary(data []byte) {
	if len(data) < 8 {
		c.logger.Printf("binary frame too short: %d bytes", len(data))
		return
	}

	id := int64(binary.BigEndian.Uint64(data[:8]))
	resp := &Response{
		ID:         id,
		BinaryData: data[8:],
	}

	if ch, ok := c.pending.Load(id); ok {
		ch.(chan *Response) <- resp
	}
}

// dispatch routes an incoming frame to the correct handler.
func (c *Client) dispatch(data []byte) {
	// Peek at the JSON to determine type
	var peek rawMessage
	if err := json.Unmarshal(data, &peek); err != nil {
		c.logger.Printf("unmarshal frame error: %v", err)
		return
	}

	if peek.ID != nil {
		// Response — route to pending channel
		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
			c.logger.Printf("unmarshal response error: %v", err)
			return
		}
		if ch, ok := c.pending.Load(resp.ID); ok {
			ch.(chan *Response) <- &resp
		}
		return
	}

	if peek.Event != "" {
		// Event — call registered handler
		var evt Event
		if err := json.Unmarshal(data, &evt); err != nil {
			c.logger.Printf("unmarshal event error: %v", err)
			return
		}
		if handler, ok := c.events.Load(evt.Event); ok {
			// Fire handler in a goroutine to avoid blocking readLoop
			go handler.(EventHandler)(evt.Params)
		}
		return
	}

	// An unknown frame may carry anything, typed text included: its size
	// only.
	c.logger.Printf("unknown frame (%d bytes)", len(data))
}

// Close cleanly shuts down the connection.
func (c *Client) Close() error {
	defer c.closeTrace()
	if c.cancel != nil {
		c.cancel()
	}
	if c.conn != nil {
		err := c.conn.Close(websocket.StatusNormalClosure, "client closing")
		// Wait for readLoop to finish
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
		}
		return err
	}
	return nil
}
