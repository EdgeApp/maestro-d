package netshape

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout bounds one call: no answer within it fails the step.
const DefaultTimeout = 10 * time.Second

// excerptMax is how much of a failed answer an error quotes.
const excerptMax = 2048

// Shaper sends one device's calls, reading the config on every call so an
// edit needs no restart.
type Shaper struct {
	Config     func() *Config // hub snapshot, otherwise read ConfigPath
	ConfigPath string         // path to the host device config
	DeviceID   string
	// Alias, when set, names another entry whose calls the device uses
	// when its own entry has none (a running emulator's avd:<name>).
	Alias   string
	Timeout time.Duration                 // DefaultTimeout when zero
	Log     func(format string, a ...any) // one line per call; nil logs nothing
}

// Result is a call's answer.
type Result struct {
	Status   int
	Duration time.Duration
}

// FetchError is a call that did not answer 2xx in time.
type FetchError struct {
	Command string
	Method  string
	URL     string // redacted
	Status  int    // 0 when there was no answer
	Excerpt string // the first 2 KB of the answer
	Err     error  // the transport error when there was no answer
}

func (e *FetchError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s: %s %s: %v", e.Command, e.Method, e.URL, e.Err)
	}
	msg := fmt.Sprintf("%s: %s %s answered %d", e.Command, e.Method, e.URL, e.Status)
	if e.Excerpt != "" {
		msg += ": " + e.Excerpt
	}
	return msg
}

func (e *FetchError) Unwrap() error { return e.Err }

// Device returns this device's calls, from its own entry or its Alias's:
// ErrNotConfigured when neither has them, a *ConfigError when the file cannot be
// used.
func (s *Shaper) Device() (Device, error) {
	var cfg *Config
	if s.Config != nil {
		cfg = s.Config()
	} else {
		var err error
		cfg, err = LoadConfig(s.ConfigPath)
		if err != nil {
			return Device{}, err
		}
	}
	d, ok := cfg.Lookup(s.DeviceID)
	if (!ok || !d.Shapeable()) && s.Alias != "" {
		if a, found := cfg.Lookup(s.Alias); found && a.Shapeable() {
			d, ok = a, true
		}
	}
	if !ok || !d.Shapeable() {
		return Device{}, fmt.Errorf("%w for %s on this host", ErrNotConfigured, s.DeviceID)
	}
	return d, nil
}

// Set sends the device's setTrafficShape call with every value filled.
func (s *Shaper) Set(ctx context.Context, v Values) (Result, error) {
	if err := v.Validate(); err != nil {
		return Result{}, err
	}
	d, err := s.Device()
	if err != nil {
		return Result{}, err
	}
	return s.send(ctx, CommandSet, d.Set, v)
}

// Reset sends the device's resetTrafficShape call.
func (s *Shaper) Reset(ctx context.Context) (Result, error) {
	d, err := s.Device()
	if err != nil {
		return Result{}, err
	}
	return s.send(ctx, CommandReset, d.Reset, Values{})
}

func (s *Shaper) send(ctx context.Context, command string, c *Call, v Values) (Result, error) {
	req, err := Fill(c, v, s.DeviceID)
	if err != nil {
		return Result{}, err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	shown := redactURL(req.URL)
	hreq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		// The parse error repeats the filled URL; keep only its cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return Result{}, &FetchError{Command: command, Method: req.Method, URL: shown, Err: err}
	}
	if req.Body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	for k, h := range req.Headers {
		hreq.Header.Set(k, h)
	}

	started := time.Now()
	resp, err := client(req.InsecureSkipVerify).Do(hreq)
	res := Result{Duration: time.Since(started)}
	if err != nil {
		// The transport's error repeats the whole URL; keep only its cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		s.logf("traffic shape %s %s: %s %s%s failed after %s: %v", s.DeviceID, command, req.Method, shown, redactedHeaders(req.Headers), res.Duration.Round(time.Millisecond), err)
		return res, &FetchError{Command: command, Method: req.Method, URL: shown, Err: err}
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	s.logf("traffic shape %s %s: %s %s%s -> %d in %s", s.DeviceID, command, req.Method, shown, redactedHeaders(req.Headers), res.Status, res.Duration.Round(time.Millisecond))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, excerptMax))
		return res, &FetchError{Command: command, Method: req.Method, URL: shown, Status: resp.StatusCode, Excerpt: strings.TrimSpace(string(excerpt))}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return res, nil
}

func (s *Shaper) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
	}
}

// noRedirects makes a 3xx the answer: following one would re-send a POST
// as a body-less GET (a login page's 200 would then read as success) and
// carry custom credential headers to another host.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

var (
	plainClient    = &http.Client{CheckRedirect: noRedirects}
	insecureClient = func() *http.Client {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// Per call, for a shaper with a self-signed certificate.
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		return &http.Client{Transport: t, CheckRedirect: noRedirects}
	}()
)

func client(insecure bool) *http.Client {
	if insecure {
		return insecureClient
	}
	return plainClient
}

// redactURL hides user-info and the query string, either of which may
// carry a credential: what a caller or the log sees is where the call went.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparsable URL]"
	}
	out := u.Scheme + "://"
	if u.User != nil {
		out += "[redacted]@"
	}
	out += u.Host + u.EscapedPath()
	if u.RawQuery != "" {
		out += "?[redacted]"
	}
	return out
}

// redactedHeaders names the headers a call sent, never their values.
func redactedHeaders(h map[string]string) string {
	if len(h) == 0 {
		return ""
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k+"=[redacted]")
	}
	sort.Strings(names)
	return " (headers " + strings.Join(names, ", ") + ")"
}
