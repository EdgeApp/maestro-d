// Package netshape reads a host's device config and shapes a device's
// network through the HTTP calls it holds. <home>/config.json lists the
// hub's devices: each one's id, optional description, and optional pair of
// traffic-shaping calls, one that sets the shape and one that resets it.
// <home>/network.json, the older format, holds only the calls. maestro-d
// fills in the shape's values and makes the call; it knows nothing about
// the shaper behind it.
package netshape

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// ConfigFileName is the config's name in the daemon's home.
const ConfigFileName = "config.json"

// FileName is the older traffic-shaping-only config's name in the daemon's
// home, read when there is no config.json.
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

// ConfigError is a config that cannot be used: unreadable, too open,
// malformed, or with a device whose entry fails validation.
type ConfigError struct {
	Path   string
	Device string // empty when the problem is the whole file
	Err    error
}

func (e *ConfigError) Error() string {
	if e.Device != "" {
		return fmt.Sprintf("config %s: device %s: %v", e.Path, e.Device, e.Err)
	}
	return fmt.Sprintf("config %s: %v", e.Path, e.Err)
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

// Device holds one device's entry: its description, which device listings
// report, and its two traffic-shaping calls, all optional. The JSON tags
// are network.json's; config.json is read through hubDevice.
type Device struct {
	Description string `json:"-"`
	// Capabilities is config.json's capabilities for the device: any JSON
	// value, kept as written. maestro-d gives it no meaning.
	Capabilities json.RawMessage `json:"-"`
	Set          *Call           `json:"setTrafficShape,omitempty"`
	Reset        *Call           `json:"resetTrafficShape,omitempty"`
}

// Shapeable reports whether the entry configures both of the device's
// calls.
func (d Device) Shapeable() bool { return d.Set != nil && d.Reset != nil }

// Config is a host's device config, from either file.
type Config struct {
	Devices map[string]Device `json:"devices"`
	// Shared lists config.json's hub devices, in file order: the devices a
	// hub shares. network.json has none.
	Shared []string `json:"-"`
	// AI is config.json's ai block, nil without one (network.json has
	// none).
	AI *AI `json:"-"`
}

// Source is where a daemon reads its device config. Config (--config) is
// read as config.json and Legacy (--network-config) as network.json, each
// with no fallback; otherwise Home/config.json is read when it exists, and
// Home/network.json when it does not. Never both.
type Source struct {
	Config, Legacy, Home string
}

// Resolve names the file Load reads, and whether it is network.json.
func (s Source) Resolve() (path string, legacy bool) {
	switch {
	case s.Config != "":
		return s.Config, false
	case s.Legacy != "":
		return s.Legacy, true
	}
	path = filepath.Join(s.Home, ConfigFileName)
	// Any answer but "does not exist" means config.json is there: a file
	// that cannot be read is the error Load reports, not a fallback.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return path, false
	}
	return filepath.Join(s.Home, FileName), true
}

// Unread lists the <home> config files that exist but that Resolve
// passes over. One may hold what its operator meant to be read: a
// config.json beside a --network-config, or a network.json beside the
// config.json that replaced it.
func (s Source) Unread() []string {
	path, _ := s.Resolve()
	var out []string
	for _, p := range []string{filepath.Join(s.Home, ConfigFileName), filepath.Join(s.Home, FileName)} {
		if filepath.Clean(p) == filepath.Clean(path) {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// Load reads the file Resolve names, and returns its path.
func (s Source) Load() (*Config, string, error) {
	path, legacy := s.Resolve()
	if legacy {
		cfg, err := Load(path)
		return cfg, path, err
	}
	cfg, err := LoadConfig(path)
	return cfg, path, err
}

// Load reads and validates a network.json at path. A missing file is an
// empty config. Any other problem is a *ConfigError.
func Load(path string) (*Config, error) {
	return load(path, Parse)
}

// LoadConfig reads and validates a config.json at path, like Load.
func LoadConfig(path string) (*Config, error) {
	return load(path, ParseConfig)
}

func load(path string, parse func([]byte) (*Config, error)) (*Config, error) {
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
	cfg, err := parse(data)
	if err != nil {
		var ce *ConfigError
		if errors.As(err, &ce) {
			ce.Path = path
			return nil, ce
		}
		return nil, &ConfigError{Path: path, Err: err}
	}
	if cfg.AI != nil {
		cfg.AI.resolve(filepath.Dir(path))
	}
	return cfg, nil
}

// decode reads one JSON object into v. Unknown keys are errors, so a
// misspelled one is never silently ignored.
func decode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// A number in a body is sent as written: a 64-bit id must not round
	// through float64.
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

// hubDevice is one entry of config.json's hubDevices.
type hubDevice struct {
	ID             string          `json:"id"`
	Description    string          `json:"description,omitempty"`
	Capabilities   json.RawMessage `json:"capabilities,omitempty"`
	TrafficShaping *struct {
		Set   *Call `json:"setTrafficShape"`
		Reset *Call `json:"resetTrafficShape"`
	} `json:"trafficShaping,omitempty"`
}

// ParseConfig decodes and validates a config.json: {"hubDevices": [{"id",
// "description", "capabilities", "trafficShaping": {"setTrafficShape",
// "resetTrafficShape"}}]}. Each id must be given once; description,
// capabilities (any JSON value, a null one as if absent) and trafficShaping are optional,
// and trafficShaping needs both calls.
func ParseConfig(data []byte) (*Config, error) {
	var file struct {
		HubDevices []hubDevice `json:"hubDevices"`
		AI         *AI         `json:"ai"`
	}
	if err := decode(data, &file); err != nil {
		return nil, err
	}
	if file.AI != nil {
		if err := file.AI.validate(); err != nil {
			return nil, fmt.Errorf("ai: %w", err)
		}
	}
	cfg := &Config{Devices: map[string]Device{}, AI: file.AI}
	for i, e := range file.HubDevices {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			return nil, fmt.Errorf("hubDevices[%d] has no id", i)
		}
		if _, dup := cfg.Devices[id]; dup {
			return nil, &ConfigError{Device: id, Err: errors.New("is listed twice in hubDevices")}
		}
		d := Device{Description: strings.TrimSpace(e.Description)}
		if c := bytes.TrimSpace(e.Capabilities); len(c) > 0 && !bytes.Equal(c, []byte("null")) {
			d.Capabilities = c
		}
		if t := e.TrafficShaping; t != nil {
			d.Set, d.Reset = t.Set, t.Reset
			if err := validateCalls(d); err != nil {
				return nil, &ConfigError{Device: id, Err: fmt.Errorf("trafficShaping: %w", err)}
			}
		}
		cfg.Devices[id] = d
		cfg.Shared = append(cfg.Shared, id)
	}
	return cfg, nil
}

// Parse decodes and validates a network.json: {"devices": {id: {calls}}}.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := decode(data, &cfg); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cfg.Devices))
	for id := range cfg.Devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := cfg.Devices[id]
		if err := validateCalls(d); err != nil {
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

// validateCalls checks a device's two calls, which go together.
func validateCalls(d Device) error {
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
