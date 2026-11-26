package daemon

import (
	"context"
	"net/http"
	"net/url"
	"strings"

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

func CheckHubConfigPath(requested, serving string) *Error {
	if requested != "" && requested != serving {
		return Errorf(CodeUsage, "requested config %s differs from the hub's config %s", requested, serving)
	}
	return nil
}

func LocalConfigID(raw, hub string) (string, *Error) {
	id, suffix, qualified := strings.Cut(raw, "@")
	id = strings.TrimSpace(id)
	if id == "" || (qualified && !strings.EqualFold(suffix, hub)) {
		return "", Errorf(CodeUsage, "device %q must name a local device on hub %s", raw, hub)
	}
	return id, nil
}
