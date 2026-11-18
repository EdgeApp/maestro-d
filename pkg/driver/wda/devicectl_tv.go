package wda

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
)

// DevicectlTV is a physical Apple TV that CoreDevice knows on this host.
// Apple TVs pair over the network, so usbmuxd never lists them.
type DevicectlTV struct {
	UDID      string
	Name      string
	OSVersion string
	// Paired is true when the TV is paired with this host and in developer
	// mode, so WDA and devicectl may drive it.
	Paired bool
	// Reachable is false while CoreDevice cannot reach the TV (it is off or
	// on another network); devicectl keeps listing a paired TV regardless.
	Reachable bool
	// Tunnel is true while CoreDevice holds a connected tunnel to the TV;
	// devicectl opens one on demand otherwise.
	Tunnel bool
}

// Ready reports whether a session can start on the TV now.
func (tv DevicectlTV) Ready() bool { return tv.Paired && tv.Reachable }

// ListDevicectlTVs returns the physical Apple TVs devicectl knows, paired or
// not, reading `devicectl list devices`, which needs no tunnel to any
// device. It fails when devicectl is missing or does not answer within
// timeout.
func ListDevicectlTVs(timeout time.Duration) ([]DevicectlTV, error) {
	data, err := runDevicectlJSON(timeout, "list", "devices")
	if err != nil {
		return nil, err
	}
	tvs, err := parseDevicectlTVs(data)
	if err == nil {
		RememberTVs(tvs)
	}
	return tvs, err
}

// knownTVs remembers every TV a devicectl listing has returned in this
// process, by UDID, with its name: a device listing's reading and an
// attach's alike, so a TV is known before its attach builds WebDriverAgent.
// A UDID never changes devices, so a TV once seen stays a TV, including
// while devicectl fails.
var knownTVs struct {
	mu    sync.Mutex
	names map[string]string
}

// RememberTVs records tvs in the memory KnownTV answers from.
func RememberTVs(tvs []DevicectlTV) {
	knownTVs.mu.Lock()
	defer knownTVs.mu.Unlock()
	if knownTVs.names == nil {
		knownTVs.names = map[string]string{}
	}
	for _, tv := range tvs {
		knownTVs.names[tv.UDID] = tv.Name
	}
}

// KnownTV reports whether udid is an Apple TV a devicectl listing has
// returned in this process, and its name. It never runs devicectl.
func KnownTV(udid string) (name string, ok bool) {
	knownTVs.mu.Lock()
	defer knownTVs.mu.Unlock()
	name, ok = knownTVs.names[udid]
	return name, ok
}

// forgetTVs clears knownTVs, for tests.
func forgetTVs() {
	knownTVs.mu.Lock()
	knownTVs.names = nil
	knownTVs.mu.Unlock()
}

// devicectlRecord is the part of one devicectl device record this package
// reads, in `list devices` and `device info details` alike.
type devicectlRecord struct {
	ConnectionProperties struct {
		PairingState    string `json:"pairingState"`
		TunnelIPAddress string `json:"tunnelIPAddress"`
		TunnelState     string `json:"tunnelState"`
	} `json:"connectionProperties"`
	DeviceProperties struct {
		Name                string `json:"name"`
		OSVersionNumber     string `json:"osVersionNumber"`
		DeveloperModeStatus string `json:"developerModeStatus"`
	} `json:"deviceProperties"`
	HardwareProperties struct {
		Platform string `json:"platform"`
		Reality  string `json:"reality"`
		UDID     string `json:"udid"`
	} `json:"hardwareProperties"`
}

// parseDevicectlTVs reads `devicectl list devices --json-output` and keeps
// the physical tvOS devices.
func parseDevicectlTVs(data []byte) ([]DevicectlTV, error) {
	var doc struct {
		Result struct {
			Devices []devicectlRecord `json:"devices"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("devicectl output: %w", err)
	}
	var tvs []DevicectlTV
	for _, d := range doc.Result.Devices {
		hw := d.HardwareProperties
		if hw.Platform != "tvOS" || hw.Reality != "physical" || hw.UDID == "" {
			continue
		}
		tunnel := d.ConnectionProperties.TunnelState
		tvs = append(tvs, DevicectlTV{
			UDID:      hw.UDID,
			Name:      d.DeviceProperties.Name,
			OSVersion: d.DeviceProperties.OSVersionNumber,
			Paired: d.ConnectionProperties.PairingState == "paired" &&
				d.DeviceProperties.DeveloperModeStatus == "enabled",
			Reachable: tunnel != "unavailable" && tunnel != "",
			Tunnel:    tunnel == "connected",
		})
	}
	return tvs, nil
}

// tvLookupTimeout bounds the devicectl listing an attach reads; an attach
// can wait longer than a device listing.
const tvLookupTimeout = 10 * time.Second

// physicalTVLookup reports whether udid is a physical Apple TV devicectl
// knows. A device usbmuxd lists is a phone or tablet on USB, so devicectl is
// not asked about it; for any other device a devicectl failure is returned,
// since without it the runner cannot tell a TV from a phone.
func physicalTVLookup(udid string) (DevicectlTV, bool, error) {
	if _, err := goios.GetDevice(udid); err == nil {
		return DevicectlTV{}, false, nil
	}
	tvs, err := ListDevicectlTVs(tvLookupTimeout)
	if err != nil {
		return DevicectlTV{}, false, err
	}
	for _, tv := range tvs {
		if tv.UDID == udid {
			return tv, true, nil
		}
	}
	return DevicectlTV{}, false, nil
}
