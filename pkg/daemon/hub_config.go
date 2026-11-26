package daemon

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// HubConfigDevice describes a config entry without exposing its shaping calls.
type HubConfigDevice struct {
	ID             string        `json:"id"`
	Description    string        `json:"description,omitempty"`
	TrafficShaping bool          `json:"trafficShaping"`
	Listed         bool          `json:"listed"`
	Lease          *DeviceLease  `json:"lease,omitempty"`
	Reservations   []DeviceLease `json:"reservations,omitempty"`
}

type HubConfigListing struct {
	Envelope
	Path        string            `json:"path"`
	ConfigError string            `json:"configError,omitempty"`
	Devices     []HubConfigDevice `json:"devices"`
}

type HubConfigPutResult struct {
	OK     bool            `json:"ok"`
	Device HubConfigDevice `json:"device"`
}

func (c *Client) HubDevices(ctx context.Context) (*HubConfigListing, error) {
	var out HubConfigListing
	if err := c.call(ctx, http.MethodGet, "/v1/config/devices", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func configDevicePath(id, expectedPath string, endLeases bool) string {
	path := "/v1/config/devices/" + url.PathEscape(id)
	q := url.Values{}
	if expectedPath != "" {
		q.Set("expectedPath", expectedPath)
	}
	if endLeases {
		q.Set("endLeases", "true")
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return path
}

func (c *Client) PutHubDevice(ctx context.Context, id string, fields netshape.DeviceFields, expectedPath string) (*HubConfigPutResult, error) {
	var out HubConfigPutResult
	if err := c.call(ctx, http.MethodPut, configDevicePath(id, expectedPath, false), fields, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RemoveHubDevice(ctx context.Context, id, expectedPath string, endLeases bool) error {
	if err := c.call(ctx, http.MethodDelete, configDevicePath(id, expectedPath, endLeases), nil, nil); err != nil {
		return err
	}
	return nil
}

// ExpectedHubConfigPath binds a local edit to the resolved file that was
// serving when the client checked the socket.
func ExpectedHubConfigPath(requested, serving string) (string, *Error) {
	if requested == "" {
		return "", nil
	}
	target, err := netshape.ResolveConfigPath(requested)
	if err != nil {
		return "", WrapErr(CodeUsage, err)
	}
	active, err := netshape.ResolveConfigPath(serving)
	if err != nil {
		return "", WrapErr(CodeUsage, err)
	}
	same, err := configPathMatches(target, active)
	if err != nil {
		return "", WrapErr(CodeUsage, err)
	}
	if !same {
		return "", Errorf(CodeUsage, "requested config %s differs from the hub's config %s", requested, serving)
	}
	return active, nil
}

func configPathMatches(requested, serving string) (bool, error) {
	if sameConfigEntry(requested, serving) {
		return true, nil
	}
	// Before the first upsert there is no config inode. Its ordinary writer
	// lock provides a directory entry whose spelling the filesystem can check.
	// Only consider names that could denote the same future entry.
	if !equalConfigSpelling(filepath.Base(requested), filepath.Base(serving)) {
		return false, nil
	}
	dir := filepath.Dir(serving)
	if !equalConfigSpelling(filepath.Dir(requested), dir) && !sameConfigDirectory(filepath.Dir(requested), dir) {
		return false, nil
	}
	_, requestedErr := os.Stat(requested)
	_, servingErr := os.Stat(serving)
	if !os.IsNotExist(requestedErr) || !os.IsNotExist(servingErr) {
		return false, nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	lock, err := fsage.LockFile(serving + ".lock")
	if err != nil {
		return false, err
	}
	defer fsage.UnlockFile(lock)
	return sameConfigEntry(requested+".lock", serving+".lock"), nil
}

func equalConfigSpelling(a, b string) bool {
	return strings.EqualFold(norm.NFC.String(a), norm.NFC.String(b))
}

// A rename replaces one directory entry. Two hard links to the same inode
// are separate entries, while case variants on an insensitive volume are one.
func sameConfigEntry(a, b string) bool {
	if a == b {
		return true
	}
	ad, bd := filepath.Dir(a), filepath.Dir(b)
	if !sameConfigDirectory(ad, bd) {
		return false
	}
	an, bn := filepath.Base(a), filepath.Base(b)
	if an == bn {
		return true
	}
	if !sameExistingFile(a, b) {
		return false
	}
	if !equalConfigSpelling(an, bn) {
		return false
	}
	entries, err := os.ReadDir(ad)
	if err != nil {
		return false
	}
	matches := 0
	for _, e := range entries {
		if equalConfigSpelling(an, e.Name()) {
			matches++
		}
	}
	return matches == 1
}

// The config and some of its parent directories may still need to be created.
func sameConfigDirectory(a, b string) bool {
	for a != b {
		if sameExistingFile(a, b) {
			return true
		}
		parent := filepath.Dir(a)
		if parent == a || filepath.Base(a) != filepath.Base(b) {
			return false
		}
		a, b = parent, filepath.Dir(b)
	}
	return true
}

func sameExistingFile(a, b string) bool {
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && os.SameFile(left, right)
}

func LocalConfigID(raw, hub string) (string, *Error) {
	id, suffix, qualified := strings.Cut(raw, "@")
	id = strings.TrimSpace(id)
	if id == "" || (qualified && !strings.EqualFold(suffix, hub)) {
		return "", Errorf(CodeUsage, "device %q must name a local device on hub %s", raw, hub)
	}
	return id, nil
}
