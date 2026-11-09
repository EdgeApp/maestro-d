package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
)

// Client talks the daemon protocol over the unix socket or TCP. Every
// failure it returns is an *Error: decoded from the response body when the
// daemon answered, DAEMON_UNAVAILABLE when it did not.
type Client struct {
	http  *http.Client
	base  string
	token string
	// retry makes the client send request ids and retry with the same id
	// (TCP clients, whose link can drop; the unix socket cannot).
	retry bool
	// sleep waits between retries (tests replace it).
	sleep func(context.Context, time.Duration) error
	// Name is the daemon name this client was dialled for (informational).
	Name string
	// Info is daemon.json when the client was created from it.
	Info *Info
}

// NewUnixClient connects over a unix socket.
func NewUnixClient(sockPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
		// The daemon serialises steps per device; keep a few idle
		// connections so SSE + steps don't fight over one.
		MaxIdleConnsPerHost: 8,
		DisableCompression:  true,
	}
	return &Client{http: &http.Client{Transport: tr}, base: "http://maestro-d"}
}

// NewHTTPClient connects over TCP to a daemon started with --http, or to a
// hub. An https:// base URL uses TLS, verified against tlsConfig's roots
// when given (nil means the system roots). The client sends request ids
// on step-running calls and retries them with the same id after a dropped
// connection, PEER_UNAVAILABLE, or a gateway's 502/504.
func NewHTTPClient(baseURL, token string, tlsConfig *tls.Config) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if tlsConfig != nil {
		tr.TLSClientConfig = tlsConfig
	}
	return &Client{http: &http.Client{Transport: tr}, base: baseURL, token: token, retry: true, sleep: sleepCtx}
}

// BaseURL is the daemon's address for a TCP client ("http://maestro-d" for
// the unix socket).
func (c *Client) BaseURL() string { return c.base }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryDelays are the waits before each retry: 0.5 s, then doubling, at
// most 4 s, three retries in all.
var retryDelays = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// carriesRequestID reports whether a request gets an X-Maestro-Request-Id:
// every POST that runs steps (commands, steps, flows), which the daemon's
// outcome cache answers once per id.
func carriesRequestID(method, path string) bool {
	if method != http.MethodPost || !strings.HasPrefix(path, "/v1/devices/") {
		return false
	}
	p, _, _ := strings.Cut(path, "?")
	return strings.Contains(p, "/commands/") || strings.HasSuffix(p, "/steps") || strings.HasSuffix(p, "/flows")
}

// retryable reports whether a request may be sent again: one with a
// request id, a read, or an attach (attaching an attached device returns
// it).
func retryable(req *http.Request) bool {
	switch {
	case req.Header.Get(HeaderRequestID) != "":
		return true
	case req.Method == http.MethodGet || req.Method == http.MethodHead:
		return true
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/attach"):
		return true
	}
	return false
}

// retryableErr reports whether a transport error is a dropped or refused
// connection, not a certificate or name problem that a retry cannot fix.
func retryableErr(err error) bool {
	if err == nil {
		return false
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var dnsErr *net.DNSError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) || errors.As(err, &dnsErr) {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"connection reset", "connection refused", "broken pipe", "EOF", "server closed"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// retryableResponse reports whether a response asks for a retry: the
// owning hub was unreachable (PEER_UNAVAILABLE), or a gateway in between
// answered 502/504 without a maestro-d envelope (a daemon's own
// DEVICE_ERROR is a 502 too, and is final).
func retryableResponse(status int, body []byte) bool {
	var env Envelope
	decoded := json.Unmarshal(body, &env) == nil && env.Error != nil && env.Error.Code != ""
	if decoded {
		return env.Error.Code == CodePeerUnavailable
	}
	return status == http.StatusBadGateway || status == http.StatusGatewayTimeout
}

// Dial returns a client for a running daemon, or (nil, nil) when no daemon
// of that name is alive. It never spawns one; see EnsureDaemon.
func Dial(name string) (*Client, error) {
	info, err := LiveInfo(name)
	if err != nil {
		return nil, WrapErr(CodeDaemonUnavailable, err)
	}
	if info == nil {
		return nil, nil
	}
	c := NewUnixClient(info.SocketPath)
	c.Name = name
	c.Info = info
	return c, nil
}

// ---------------------------------------------------------------------------
// Transport

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, WrapErr(CodeUsage, err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, WrapErr(CodeUsage, err)
	}
	req.Header.Set("X-Maestro-Api-Version", APIVersion)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.retry && carriesRequestID(method, path) {
		req.Header.Set(HeaderRequestID, uuid.New().String())
	}
	return req, nil
}

// do sends the request and returns the raw response. Transport failures
// become DAEMON_UNAVAILABLE; a cancelled ctx becomes INTERRUPTED.
func (c *Client) do(req *http.Request) (*http.Response, *Error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.http.Do(req)
		if err != nil && req.Context().Err() != nil {
			return nil, WrapErr(CodeInterrupted, req.Context().Err())
		}
		canRetry := c.retry && attempt < len(retryDelays) && retryable(req) && (req.Body == nil || req.GetBody != nil)
		if err == nil && canRetry && (resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout) {
			data, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil && !retryableResponse(resp.StatusCode, data) {
				resp.Body = io.NopCloser(bytes.NewReader(data))
				return resp, nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
			if rerr != nil {
				err = rerr
			}
		} else if err == nil {
			return resp, nil
		} else if !canRetry || !retryableErr(err) {
			return nil, Errorf(CodeDaemonUnavailable, "daemon %s: %v", c.describe(), unwrapURLError(err))
		}
		logRetry(req, attempt+1, unwrapURLError(err))
		if serr := c.sleep(req.Context(), retryDelays[attempt]); serr != nil {
			return nil, WrapErr(CodeInterrupted, serr)
		}
		next := req.Clone(req.Context())
		if req.GetBody != nil {
			body, berr := req.GetBody()
			if berr != nil {
				return nil, WrapErr(CodeInternal, berr)
			}
			next.Body = body
		}
		req = next
	}
}

// RetryLog, when set, is told about every retry (the CLI prints these with
// --verbose).
var RetryLog func(format string, args ...any)

func logRetry(req *http.Request, attempt int, err error) {
	if RetryLog == nil {
		return
	}
	id := req.Header.Get(HeaderRequestID)
	if id == "" {
		id = "-"
	}
	RetryLog("retry %d/%d of %s %s (request %s) after: %v", attempt, len(retryDelays), req.Method, req.URL.Path, id, err)
}

func (c *Client) describe() string {
	if c.Name != "" {
		return fmt.Sprintf("%q", c.Name)
	}
	return c.base
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// call performs a JSON request and decodes the body into out. A non-2xx
// response is decoded into out too (results carry their own envelope) and
// returned as an *Error built from the envelope.
func (c *Client) call(ctx context.Context, method, path string, body, out any) *Error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return WrapErr(CodeUsage, err)
	}
	resp, e := c.do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return WrapErr(CodeDaemonUnavailable, rerr)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil && resp.StatusCode < 300 {
			return Errorf(CodeInternal, "decode %s %s: %v", method, path, err)
		}
	}
	if resp.StatusCode >= 300 {
		return errorFromResponse(resp.StatusCode, data)
	}
	return nil
}

// errorFromResponse decodes a failure envelope, falling back to the status.
func errorFromResponse(status int, data []byte) *Error {
	var env Envelope
	if err := json.Unmarshal(data, &env); err == nil && env.Error != nil && env.Error.Code != "" {
		return &Error{ErrorBody: *env.Error}
	}
	msg := strings.TrimSpace(string(data))
	if msg == "" {
		msg = http.StatusText(status)
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return Errorf(CodeFromHTTPStatus(status), "HTTP %d: %s", status, msg)
}

func devPath(device, suffix string) string {
	return "/v1/devices/" + url.PathEscape(device) + suffix
}

func waitQuery(wait bool) string {
	if wait {
		return ""
	}
	return "?wait=false"
}

// ---------------------------------------------------------------------------
// Daemon

// Status returns the daemon's status.
func (c *Client) Status(ctx context.Context) (*StatusResult, error) {
	var out StatusResult
	if e := c.call(ctx, http.MethodGet, "/v1/status", nil, &out); e != nil {
		return nil, e
	}
	return &out, nil
}

// Ping checks the daemon answers within timeout.
func (c *Client) Ping(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := c.Status(ctx)
	return err
}

// Shutdown asks the daemon to stop. The daemon detaches its devices, shuts
// down the ones it booted and exits.
func (c *Client) Shutdown(ctx context.Context) error {
	var out Envelope
	if e := c.call(ctx, http.MethodPost, "/v1/shutdown", nil, &out); e != nil {
		return e
	}
	return nil
}

// ---------------------------------------------------------------------------
// Devices

// Devices lists devices; platform "" means all.
func (c *Client) Devices(ctx context.Context, platform string) ([]DeviceInfo, error) {
	path := "/v1/devices"
	if platform != "" {
		path += "?platform=" + url.QueryEscape(platform)
	}
	var out DevicesResult
	if e := c.call(ctx, http.MethodGet, path, nil, &out); e != nil {
		return nil, e
	}
	return out.Devices, nil
}

// Device returns one device.
func (c *Client) Device(ctx context.Context, id string) (*DeviceInfo, error) {
	var out DeviceResult
	if e := c.call(ctx, http.MethodGet, devPath(id, ""), nil, &out); e != nil {
		return nil, e
	}
	return &out.Device, nil
}

// StartDevice boots a simulator/emulator (or returns it if already up).
func (c *Client) StartDevice(ctx context.Context, req BootRequest) (*DeviceInfo, error) {
	var out DeviceResult
	if e := c.call(ctx, http.MethodPost, "/v1/devices", req, &out); e != nil {
		return nil, e
	}
	return &out.Device, nil
}

// StopDevice detaches (if attached) and shuts a device down.
func (c *Client) StopDevice(ctx context.Context, id string, force bool) error {
	var out Envelope
	if e := c.call(ctx, http.MethodDelete, devPath(id, ""), StopDeviceRequest{Force: force}, &out); e != nil {
		return e
	}
	return nil
}

// Catalog lists what `device create` can make; platform "" means both.
func (c *Client) Catalog(ctx context.Context, platform string, available bool) (*CatalogResult, error) {
	q := url.Values{}
	if platform != "" {
		q.Set("platform", platform)
	}
	if available {
		q.Set("available", "true")
	}
	path := "/v1/catalog"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out CatalogResult
	if e := c.call(ctx, http.MethodGet, path, nil, &out); e != nil {
		return nil, e
	}
	return &out, nil
}

// CreateDevice creates a simulator or AVD (req.Create) and, unless
// req.Boot is false, boots it.
func (c *Client) CreateDevice(ctx context.Context, req BootRequest) (*DeviceInfo, error) {
	if req.Create == nil {
		req.Create = &CreateSpec{}
	}
	return c.StartDevice(ctx, req)
}

// DeleteDevice deletes a simulator or AVD, stopping it first. Devices
// maestro-d did not create need force (DEVICE_EXTERNAL).
func (c *Client) DeleteDevice(ctx context.Context, id string, force bool) (*DeviceInfo, error) {
	path := devPath(id, "?delete=true")
	if force {
		path += "&force=true"
	}
	var out DeviceResult
	if e := c.call(ctx, http.MethodDelete, path, nil, &out); e != nil {
		return nil, e
	}
	return &out.Device, nil
}

// Attach opens a driver on the device (no-op when already attached).
func (c *Client) Attach(ctx context.Context, id string, cfg AttachConfig) (*DeviceInfo, error) {
	var out DeviceResult
	if e := c.call(ctx, http.MethodPost, devPath(id, "/attach"), cfg, &out); e != nil {
		return nil, e
	}
	return &out.Device, nil
}

// Detach closes the driver on the device.
func (c *Client) Detach(ctx context.Context, id string) error {
	var out Envelope
	if e := c.call(ctx, http.MethodPost, devPath(id, "/detach"), nil, &out); e != nil {
		return e
	}
	return nil
}

// ---------------------------------------------------------------------------
// Steps

// Command runs one YAML command. value is the command's YAML value (nil,
// scalar or map). On a step failure the result is returned together with
// the error so callers can inspect artifacts.
func (c *Client) Command(ctx context.Context, device, name string, value any, wait bool) (*StepResult, error) {
	var out StepResult
	path := devPath(device, "/commands/"+url.PathEscape(name)) + waitQuery(wait)
	if value == nil {
		// Send an explicit null so value-less commands have a body.
		value = json.RawMessage("null")
	}
	e := c.call(ctx, http.MethodPost, path, value, &out)
	if e != nil {
		if out.Step != "" || out.Type != "" {
			return &out, e
		}
		return nil, e
	}
	return &out, nil
}

// CommandCwd is Command with a working directory for relative paths.
func (c *Client) CommandCwd(ctx context.Context, device, name string, value any, cwd string, wait bool) (*StepResult, error) {
	var out StepResult
	q := url.Values{}
	if cwd != "" {
		q.Set("cwd", cwd)
	}
	if !wait {
		q.Set("wait", "false")
	}
	path := devPath(device, "/commands/"+url.PathEscape(name))
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if value == nil {
		value = json.RawMessage("null")
	}
	e := c.call(ctx, http.MethodPost, path, value, &out)
	if e != nil {
		if out.Step != "" || out.Type != "" {
			return &out, e
		}
		return nil, e
	}
	return &out, nil
}

// Steps runs a batch of steps.
func (c *Client) Steps(ctx context.Context, device string, req StepsRequest, wait bool) (*StepsResult, error) {
	var out StepsResult
	e := c.call(ctx, http.MethodPost, devPath(device, "/steps")+waitQuery(wait), req, &out)
	if e != nil {
		if len(out.Results) > 0 {
			return &out, e
		}
		return nil, e
	}
	return &out, nil
}

// Flow runs a flow file (or inline YAML) as one compound step.
func (c *Client) Flow(ctx context.Context, device string, req FlowRequest, wait bool) (*StepResult, error) {
	var out StepResult
	e := c.call(ctx, http.MethodPost, devPath(device, "/flows")+waitQuery(wait), req, &out)
	if e != nil {
		if out.Step != "" || out.Type != "" {
			return &out, e
		}
		return nil, e
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Inspection

// Screenshot returns a PNG.
func (c *Client) Screenshot(ctx context.Context, device string) ([]byte, error) {
	req, err := c.newRequest(ctx, http.MethodGet, devPath(device, "/screenshot"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/png")
	resp, e := c.do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, WrapErr(CodeDaemonUnavailable, rerr)
	}
	if resp.StatusCode >= 300 {
		return nil, errorFromResponse(resp.StatusCode, data)
	}
	return data, nil
}

// Hierarchy returns the view hierarchy: parsed JSON (iOS/mock) or the raw
// XML text (Android) as a string.
func (c *Client) Hierarchy(ctx context.Context, device string) (any, error) {
	var out DataResult
	if e := c.call(ctx, http.MethodGet, devPath(device, "/hierarchy"), nil, &out); e != nil {
		return nil, e
	}
	return out.Data, nil
}

// HierarchyRaw returns the hierarchy bytes as the driver produced them.
func (c *Client) HierarchyRaw(ctx context.Context, device string) ([]byte, error) {
	req, err := c.newRequest(ctx, http.MethodGet, devPath(device, "/hierarchy?format=raw"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	resp, e := c.do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, WrapErr(CodeDaemonUnavailable, rerr)
	}
	if resp.StatusCode >= 300 {
		return nil, errorFromResponse(resp.StatusCode, data)
	}
	return data, nil
}

// State returns the device state (foreground app, orientation, …).
func (c *Client) State(ctx context.Context, device string) (any, error) {
	var out DataResult
	if e := c.call(ctx, http.MethodGet, devPath(device, "/state"), nil, &out); e != nil {
		return nil, e
	}
	return out.Data, nil
}

// PlatformInfo returns the driver's platform info.
func (c *Client) PlatformInfo(ctx context.Context, device string) (*core.PlatformInfo, error) {
	var out struct {
		Envelope
		Data *core.PlatformInfo `json:"data"`
	}
	if e := c.call(ctx, http.MethodGet, devPath(device, "/info"), nil, &out); e != nil {
		return nil, e
	}
	return out.Data, nil
}

// Vars returns the session variables.
func (c *Client) Vars(ctx context.Context, device string) (map[string]string, error) {
	var out VarsResult
	if e := c.call(ctx, http.MethodGet, devPath(device, "/vars"), nil, &out); e != nil {
		return nil, e
	}
	return out.Vars, nil
}

// SetVars sets session variables and returns the full set.
func (c *Client) SetVars(ctx context.Context, device string, vars map[string]string) (map[string]string, error) {
	var out VarsResult
	if e := c.call(ctx, http.MethodPut, devPath(device, "/vars"), vars, &out); e != nil {
		return nil, e
	}
	return out.Vars, nil
}

// Eval evaluates JavaScript in the session and returns its value.
func (c *Client) Eval(ctx context.Context, device, script string) (any, error) {
	var out EvalResult
	if e := c.call(ctx, http.MethodPost, devPath(device, "/eval"), EvalRequest{Script: script}, &out); e != nil {
		return nil, e
	}
	return out.Value, nil
}

// ---------------------------------------------------------------------------
// Events

// Events streams events to fn until ctx ends, the stream closes, or fn
// returns false. device "" means all devices; afterSeq replays buffered
// events newer than it.
func (c *Client) Events(ctx context.Context, device string, afterSeq int64, fn func(Event) bool) error {
	q := url.Values{}
	if device != "" {
		q.Set("device", device)
	}
	if afterSeq > 0 {
		q.Set("after", strconv.FormatInt(afterSeq, 10))
	}
	path := "/v1/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, e := c.do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return errorFromResponse(resp.StatusCode, data)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				var ev Event
				if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &ev); err == nil {
					if !fn(ev) {
						return nil
					}
				}
				data = data[:0]
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := sc.Err(); err != nil {
		return WrapErr(CodeDaemonUnavailable, err)
	}
	return nil
}
