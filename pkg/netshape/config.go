// Package netshape shapes a device's network through the HTTP calls a host
// is configured with. Each device has two calls in <home>/network.json, one
// that sets its shape and one that resets it; maestro-d fills in the shape's
// values and makes the call. It knows nothing about the shaper behind it.
package netshape

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
)

// FileName is the config's name in the daemon's home.
const FileName = "network.json"

// Command names, as the config keys them and the steps are called.
const (
	CommandSet   = "setTrafficShape"
	CommandReset = "resetTrafficShape"
)

// Directions a shape applies to, as the step names them.
const (
	DirectionDownload = "download"
	DirectionUpload   = "upload"
	DirectionBoth     = "both"
)

// Directions lists the accepted direction names.
var Directions = []string{DirectionDownload, DirectionUpload, DirectionBoth}

var methods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// goos is runtime.GOOS, a variable so tests can take the Windows path.
var goos = runtime.GOOS

// ErrNotConfigured: the host has no shaper for this device. A missing file
// means no device is configured.
var ErrNotConfigured = errors.New("no traffic shaper is configured")

// ConfigError is a network.json that cannot be used: unreadable, too open,
// malformed, or with a device whose calls fail validation.
type ConfigError struct {
	Path   string
	Device string // empty when the problem is the whole file
	Err    error
}

func (e *ConfigError) Error() string {
	if e.Device != "" {
		return fmt.Sprintf("network config %s: device %s: %v", e.Path, e.Device, e.Err)
	}
	return fmt.Sprintf("network config %s: %v", e.Path, e.Err)
}

func (e *ConfigError) Unwrap() error { return e.Err }

// Call is one HTTP request, like a curl command line.
type Call struct {
	Method             string            `json:"method"`
	URL                string            `json:"url"`
	Headers            map[string]string `json:"headers,omitempty"`
	Body               any               `json:"body,omitempty"`
	DirectionValues    map[string]string `json:"directionValues,omitempty"`
	InsecureSkipVerify bool              `json:"insecureSkipVerify,omitempty"`
}

// Device holds one device's two calls.
type Device struct {
	Set   *Call `json:"setTrafficShape,omitempty"`
	Reset *Call `json:"resetTrafficShape,omitempty"`
}

// Config is the whole file.
type Config struct {
	Devices map[string]Device `json:"devices"`
}

// Load reads and validates the file at path. A missing file is an empty
// config. Any other problem is a *ConfigError.
func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, &ConfigError{Path: path, Err: err}
	}
	// It may hold credentials, so nobody but its owner may read it. Windows
	// has no such mode bits (every file reads 0666); its ACL governs.
	if perm := info.Mode().Perm(); goos != "windows" && perm&0o077 != 0 {
		return nil, &ConfigError{Path: path, Err: fmt.Errorf("mode %04o is readable by others; chmod 600 it", perm)}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ConfigError{Path: path, Err: err}
	}
	cfg, err := Parse(data)
	if err != nil {
		var ce *ConfigError
		if errors.As(err, &ce) {
			ce.Path = path
			return nil, ce
		}
		return nil, &ConfigError{Path: path, Err: err}
	}
	return cfg, nil
}

// Parse decodes and validates a config. Unknown keys are errors, so a
// misspelled one is never silently ignored.
func Parse(data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// A number in a body is sent as written: a 64-bit id must not round
	// through float64.
	dec.UseNumber()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON object")
	}
	ids := make([]string, 0, len(cfg.Devices))
	for id := range cfg.Devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := cfg.Devices[id]
		if err := validateDevice(d); err != nil {
			return nil, &ConfigError{Device: id, Err: err}
		}
	}
	return &cfg, nil
}

// Lookup returns a device's calls. ok is false when it has no entry.
func (c *Config) Lookup(id string) (Device, bool) {
	d, ok := c.Devices[id]
	return d, ok
}

func validateDevice(d Device) error {
	if d.Set == nil && d.Reset == nil {
		return errors.New("has neither setTrafficShape nor resetTrafficShape")
	}
	// A shape the session could never undo is refused before it is sent.
	if d.Set == nil {
		return errors.New("has resetTrafficShape but no setTrafficShape")
	}
	if d.Reset == nil {
		return errors.New("has setTrafficShape but no resetTrafficShape")
	}
	if err := validateCall(d.Set, true); err != nil {
		return fmt.Errorf("setTrafficShape: %w", err)
	}
	if err := validateCall(d.Reset, false); err != nil {
		return fmt.Errorf("resetTrafficShape: %w", err)
	}
	return nil
}

func validateCall(c *Call, isSet bool) error {
	if !methods[c.Method] {
		return fmt.Errorf("method %q is not one of GET, POST, PUT, PATCH, DELETE", c.Method)
	}
	if c.Body != nil && (c.Method == "GET" || c.Method == "DELETE") {
		return fmt.Errorf("a %s call has no body", c.Method)
	}
	if err := checkPlaceholders(c.URL); err != nil {
		return fmt.Errorf("url: %w", err)
	}
	// A filled value is escaped for the path or the query string; in the
	// scheme or host it would make another URL than the one checked here.
	if _, rest, ok := strings.Cut(c.URL, "://"); ok {
		host := rest
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			host = rest[:i]
		}
		if strings.Contains(host, "${") {
			return fmt.Errorf("url %s: placeholders belong in the path or the query string, not the host", redactURL(c.URL))
		}
	}
	// Placeholders are filled before the request, so check the URL's shape
	// with them standing in as plain values.
	u, err := url.Parse(placeholder.ReplaceAllString(c.URL, "0"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// Quoted redacted: the message reaches callers and the log.
		return fmt.Errorf("url %s is not an absolute http or https URL", redactURL(c.URL))
	}
	for name, v := range c.Headers {
		if err := checkPlaceholders(v); err != nil {
			return fmt.Errorf("header %s: %w", name, err)
		}
	}
	if err := checkBody(c.Body); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	if c.DirectionValues != nil {
		if !isSet {
			return errors.New("directionValues belongs on setTrafficShape only")
		}
		for k := range c.DirectionValues {
			if !isDirection(k) {
				return fmt.Errorf("directionValues key %q is not one of %s", k, strings.Join(Directions, ", "))
			}
		}
	}
	return nil
}

func checkBody(v any) error {
	switch t := v.(type) {
	case string:
		return checkPlaceholders(t)
	case map[string]any:
		for _, e := range t {
			if err := checkBody(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range t {
			if err := checkBody(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func isDirection(s string) bool {
	for _, d := range Directions {
		if s == d {
			return true
		}
	}
	return false
}
