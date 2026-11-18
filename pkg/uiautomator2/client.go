package uiautomator2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/tracelog"
)

// Client communicates with UIAutomator2 server.
type Client struct {
	http       *http.Client
	baseURL    string
	sessionID  string
	socketPath string
	logger     *log.Logger
	traced     bool      // SetLogPath was given a trace; bodies are redacted only then
	traceFile  io.Closer // the trace's file, closed by Close
}

// NewClient creates a client using Unix socket (Linux/Mac).
func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}

	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second, // Balanced timeout for UIA2 operations
		},
		baseURL:    "http://localhost",
		socketPath: socketPath,
		logger:     createLogger(),
	}
}

// NewClientTCP creates a client using TCP port (Windows).
func NewClientTCP(port int) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 10 * time.Second, // Balanced timeout for UIA2 operations
		},
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		logger:  createLogger(),
	}
}

// createLogger is the default request trace: none. A session that asks for
// one (MAESTRO_D_ANDROID_TRACE=1) gets it through SetLogPath.
func createLogger() *log.Logger {
	return tracelog.Discard()
}

// SetLogPath traces each request's timing and redacted body to path,
// capped at tracelog.MaxBytes.
func (c *Client) SetLogPath(path string) {
	if l, f := tracelog.Open(path, tracelog.MaxBytes); l != nil {
		c.closeTrace()
		c.logger, c.traceFile, c.traced = l, f, true
	}
}

// SessionID returns the current session ID.
func (c *Client) SessionID() string {
	return c.sessionID
}

// HasSession returns true if a session is active.
func (c *Client) HasSession() bool {
	return c.sessionID != ""
}

// request makes an HTTP request to UIAutomator2.
func (c *Client) request(method, path string, body interface{}) ([]byte, error) {
	start := time.Now()

	var reqBody io.Reader
	var bodyStr string
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
		if c.traced {
			bodyStr = tracelog.Redact(data)
		}
		if len(bodyStr) > 100 {
			bodyStr = bodyStr[:100] + "..."
		}
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		c.logger.Printf("%s %s [%v] ERROR: %v", method, path, elapsed, err)
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp == nil {
		c.logger.Printf("%s %s [%v] ERROR: nil response", method, path, elapsed)
		return nil, fmt.Errorf("nil response from server")
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// Log request timing
	status := "OK"
	if resp.StatusCode >= 400 {
		status = fmt.Sprintf("ERR:%d", resp.StatusCode)
	}
	c.logger.Printf("%s %s [%v] %s body=%s", method, path, elapsed, status, bodyStr)

	if resp.StatusCode >= 400 {
		var errResp Response
		if json.Unmarshal(respBody, &errResp) == nil {
			if errVal, ok := errResp.Value.(map[string]interface{}); ok {
				errMsg, _ := errVal["message"].(string)
				errType, _ := errVal["error"].(string)
				return nil, fmt.Errorf("%s: %s", errType, errMsg)
			}
		}
		return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// sessionPath returns path with session ID prefix.
func (c *Client) sessionPath(path string) string {
	return fmt.Sprintf("/session/%s%s", c.sessionID, path)
}

// Status checks if the server is ready.
func (c *Client) Status() (bool, error) {
	data, err := c.request("GET", "/status", nil)
	if err != nil {
		return false, err
	}

	var resp struct {
		Value struct {
			Ready   bool   `json:"ready"`
			Message string `json:"message"`
		} `json:"value"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}

	return resp.Value.Ready, nil
}

// CreateSession starts a new automation session.
func (c *Client) CreateSession(caps Capabilities) error {
	req := SessionRequest{Capabilities: caps}
	data, err := c.request("POST", "/session", req)
	if err != nil {
		return err
	}

	var resp struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("parse session response: %w", err)
	}

	if resp.SessionID == "" {
		// Try alternate response format
		var altResp struct {
			Value struct {
				SessionID string `json:"sessionId"`
			} `json:"value"`
		}
		if json.Unmarshal(data, &altResp) == nil && altResp.Value.SessionID != "" {
			resp.SessionID = altResp.Value.SessionID
		}
	}

	if resp.SessionID == "" {
		return fmt.Errorf("no session ID in response")
	}

	c.sessionID = resp.SessionID
	return nil
}

// GetSession returns the current session info.
func (c *Client) GetSession() (map[string]interface{}, error) {
	if c.sessionID == "" {
		return nil, fmt.Errorf("no active session")
	}

	data, err := c.request("GET", c.sessionPath(""), nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Value map[string]interface{} `json:"value"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	return resp.Value, nil
}

// DeleteSession ends the current session.
func (c *Client) DeleteSession() error {
	if c.sessionID == "" {
		return nil
	}

	_, err := c.request("DELETE", c.sessionPath(""), nil)
	c.sessionID = ""
	return err
}

// Close ends the session and cleans up.
func (c *Client) Close() error {
	defer c.closeTrace()
	return c.DeleteSession()
}

// closeTrace closes the trace file, if any; later requests trace nowhere.
func (c *Client) closeTrace() {
	if c.traceFile != nil {
		_ = c.traceFile.Close()
		c.traceFile, c.traced = nil, false
		c.logger = tracelog.Discard()
	}
}

// SetImplicitWait sets the implicit wait timeout for element finding.
// When set, the server automatically polls for elements until found or timeout.
func (c *Client) SetImplicitWait(timeout time.Duration) error {
	if c.sessionID == "" {
		return fmt.Errorf("no active session")
	}

	return c.SetAppiumSettings(map[string]interface{}{
		"implicitWait": timeout.Milliseconds(),
	})
}

// SetAppiumSettings configures Appium-specific settings.
// Key settings:
//   - waitForIdleTimeout: ms to wait for device idle (0 = disabled, default is 10000)
//   - waitForSelectorTimeout: ms to wait for selector (default 0)
func (c *Client) SetAppiumSettings(settings map[string]interface{}) error {
	if c.sessionID == "" {
		return fmt.Errorf("no active session")
	}

	_, err := c.request("POST", c.sessionPath("/appium/settings"), map[string]interface{}{
		"settings": settings,
	})
	return err
}

// DetectWebView is a no-op for native UIAutomator2 client.
// WebView detection is only meaningful for the DeviceLab Android Driver.
func (c *Client) DetectWebView() (*core.WebViewInfo, error) {
	return nil, nil
}
