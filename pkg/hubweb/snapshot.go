// Package hubweb serves a read-only, public view of the hub mesh.
package hubweb

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/device"
	"github.com/devicelab-dev/maestro-runner/pkg/hub"
	"github.com/devicelab-dev/maestro-runner/pkg/hubd"
)

// Snapshot is the entire public protocol. Never marshal the API's DeviceInfo:
// it also carries host addresses, local paths and cloud session details.
type Snapshot struct {
	Hub         string     `json:"hub"`
	GeneratedAt string     `json:"generatedAt,omitempty"`
	Hubs        []computer `json:"hubs"`
	Devices     []row      `json:"devices"`
}

type computer struct {
	Name    string `json:"name"`
	Machine string `json:"machine"`
	State   string `json:"state"`
}
type row struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Platform string   `json:"platform"`
	Kind     string   `json:"kind"`
	Computer string   `json:"computer"`
	Hub      string   `json:"hub"`
	State    string   `json:"state"`
	Lease    *window  `json:"lease,omitempty"`
	Upcoming []window `json:"upcoming"`
}
type window struct {
	Holder      string `json:"holder"`
	Description string `json:"description"`
	StartsAt    string `json:"startsAt,omitempty"`
	ExpiresAt   string `json:"expiresAt"`
}

func publicWindow(lease daemon.DeviceLease) window {
	holder := strings.TrimSpace(lease.Holder)
	if callerHolder(holder) {
		holder = ""
	}
	return window{daemon.HolderName(holder), lease.Description, lease.StartsAt, lease.ExpiresAt}
}

// callerHolder reports a holder that daemon.DescribeCaller generated from the
// transport address. Explicit names that merely start with "local " or
// "remote " stay public.
func callerHolder(holder string) bool {
	if holder == "unix socket" {
		return true
	}
	for _, prefix := range []string{"remote ", "local "} {
		if addr, ok := strings.CutPrefix(holder, prefix); ok {
			if _, err := netip.ParseAddrPort(addr); err == nil {
				return true
			}
		}
	}
	return false
}

func deviceType(d daemon.DeviceInfo) string {
	model := d.Model
	osName := d.Platform
	switch strings.ToLower(d.Platform) {
	case "android":
		osName = "Android"
		if d.Kind == "emulator" {
			model = cmp.Or(d.DeviceType, model, d.Name)
		}
	case "ios", "ipados", "tvos":
		if friendly, ok := device.AppleModel(model); ok {
			model = friendly
		}
		osName = "iOS"
		switch {
		case d.Kind == "tv" || strings.HasPrefix(model, "Apple TV") || strings.HasPrefix(d.Model, "AppleTV") || d.Platform == "tvos":
			osName = "tvOS"
		case strings.HasPrefix(model, "iPad") || strings.HasPrefix(d.Name, "iPad") || d.Platform == "ipados":
			osName = "iPadOS"
		}
	case "web":
		osName = cmp.Or(d.Name, model)
		if model == osName {
			model = ""
		}
	}
	os := strings.TrimSpace(osName + " " + d.OSVersion)
	if model == "" || os == "" {
		return model + os
	}
	return model + " · " + os
}

func publicRow(d daemon.DeviceInfo) row {
	r := row{Key: fmt.Sprintf("%x", sha256.Sum256([]byte(d.ID))), Name: cmp.Or(d.Name, d.Model, d.Kind),
		Type: deviceType(d), Platform: d.Platform, Kind: d.Kind, Computer: cmp.Or(d.Machine, d.Hub), Hub: d.Hub,
		State: "ready", Upcoming: make([]window, 0, len(d.Reservations))}
	if !d.Ready {
		r.State = "offline"
	} else if d.Busy || d.Attached || d.Owner != "" || d.Lease != nil {
		r.State = "busy"
	}
	if d.Lease != nil {
		w := publicWindow(*d.Lease)
		r.Lease = &w
	}
	for _, res := range d.Reservations {
		r.Upcoming = append(r.Upcoming, publicWindow(res))
	}
	slices.SortFunc(r.Upcoming, func(a, b window) int {
		ta, _ := time.Parse(time.RFC3339Nano, a.StartsAt)
		tb, _ := time.Parse(time.RFC3339Nano, b.StartsAt)
		return cmp.Or(ta.Compare(tb), strings.Compare(a.StartsAt, b.StartsAt), strings.Compare(a.ExpiresAt, b.ExpiresAt), strings.Compare(a.Holder, b.Holder), strings.Compare(a.Description, b.Description))
	})
	return r
}

// inventory holds raw discovery only inside the hub. The serving hub reapplies
// its share policy and current lease table for every emission.
type inventory struct {
	local    []daemon.DeviceInfo
	peers    daemon.DevicesResult
	machines map[string]string
}

type source struct {
	scan    func(context.Context) inventory
	project func(inventory) Snapshot
}

func meshSource(policy *hubd.Server, mesh *hub.Mesh) source {
	return source{
		scan: func(ctx context.Context) inventory {
			p := inventory{}
			local := make(chan []daemon.DeviceInfo, 1)
			go func() { local <- policy.Registry().Live("") }()
			p.peers, p.machines = mesh.PollPublicDevices(ctx)
			p.local = <-local
			return p
		},
		project: func(p inventory) Snapshot {
			self := policy.HubName()
			local := policy.PublicDevices(p.local)
			for i := range local {
				local[i].LocalID, local[i].Hub = local[i].ID, self
				local[i].ID += "@" + self
			}
			return project(self, daemon.MachineName(), local, p)
		},
	}
}

func project(self, machine string, local []daemon.DeviceInfo, p inventory) Snapshot {
	s := Snapshot{Hub: self, Hubs: []computer{}, Devices: []row{}}
	hubs := map[string]computer{self: {self, machine, "self"}}
	for h, host := range p.machines {
		hubs[h] = computer{h, cmp.Or(host, h), "linked"}
	}
	for _, h := range p.peers.Unreachable {
		hubs[h.Hub] = computer{h.Hub, cmp.Or(p.machines[h.Hub], h.Hub), "down"}
	}
	for _, devices := range [][]daemon.DeviceInfo{local, p.peers.Devices} {
		for _, d := range devices {
			if daemon.IsComputer(d) || d.LocalID == daemon.ComputerID {
				continue
			}
			s.Devices = append(s.Devices, publicRow(d))
			if d.Hub != self && d.Machine != "" {
				hubs[d.Hub] = computer{d.Hub, d.Machine, "linked"}
			}
		}
	}
	for _, h := range hubs {
		s.Hubs = append(s.Hubs, h)
	}
	slices.SortFunc(s.Hubs, func(a, b computer) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(s.Devices, func(a, b row) int {
		return cmp.Or(strings.Compare(a.Computer, b.Computer), strings.Compare(a.Name, b.Name), strings.Compare(a.Key, b.Key))
	})
	return s
}
