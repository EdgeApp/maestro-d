package hubd

import (
	"errors"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// configCache publishes immutable snapshots. A broken edit leaves every consumer
// (admission, listings and attached shapers) on the same last good snapshot.
type configCache struct {
	mu          sync.Mutex
	path        string
	nextCheck   time.Time
	info        os.FileInfo
	config      *netshape.Config
	configError error
	warned      map[string]bool
	statFailed  bool
	load        func(string) (*netshape.Config, error)
	warn        func(string, ...any)
	changed     func(old, current *netshape.Config)
}

func newConfigCache(path string, changed func(*netshape.Config, *netshape.Config)) *configCache {
	return &configCache{path: path, load: netshape.LoadConfig, warn: logger.Warn, changed: changed}
}

func sameFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode() == b.Mode() &&
		(runtime.GOOS == "windows" || os.SameFile(a, b))
}

func (c *configCache) snapshot(now time.Time, force bool) (*netshape.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && now.Before(c.nextCheck) {
		return c.config, c.configError
	}
	c.nextCheck = now.Add(time.Second)
	info, err := os.Stat(c.path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if !force && c.config != nil && !c.statFailed && err == nil && sameFile(c.info, info) {
		return c.config, c.configError
	}
	c.info, c.statFailed = info, err != nil
	var current *netshape.Config
	if err == nil {
		current, err = c.load(c.path)
	}
	if err != nil {
		if c.config == nil {
			c.config = &netshape.Config{}
		}
		if c.warned == nil {
			c.warned = make(map[string]bool)
		}
		if !c.warned[err.Error()] {
			c.warned[err.Error()] = true
			c.warn("config: %s is invalid (%v); still sharing %v", c.path, err, c.config.Shared)
		}
		c.configError = err
		return c.config, err
	}
	old := c.config
	c.config, c.configError, c.warned = current, nil, nil
	if old != nil {
		c.changed(old, current)
	}
	return current, nil
}

func (s *Server) DeviceConfig() *netshape.Config {
	cfg, _ := s.config.snapshot(time.Now(), false)
	return cfg
}

func (s *Server) configChanged(old, current *netshape.Config) {
	s.shareMu.Lock()
	s.shareGen++
	clear(s.sharedIDs)
	clear(s.shareSeen)
	s.shareMu.Unlock()

	// A running AVD can be configured by two spellings. Compare the effective
	// public state once per device and include configured ids absent from discovery.
	devices := s.Registry().List("")
	seen := make(map[string]bool)
	for _, d := range devices {
		seen[d.ID] = true
		if d.Kind == "emulator" && d.Name != "" {
			seen[daemon.AVDPrefix+d.Name] = true
		}
		s.publishConfigTransition(d, old, current)
	}
	for _, ids := range [][]string{old.Shared, current.Shared} {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				s.publishConfigTransition(daemon.DeviceInfo{ID: id}, old, current)
			}
		}
	}
}

func (s *Server) publishConfigTransition(d daemon.DeviceInfo, old, current *netshape.Config) {
	was, now := publiclyShared(old, d), publiclyShared(current, d)
	ev := daemon.Event{Device: d.ID}
	switch {
	case was && !now:
		ev.Type, ev.PreviouslyShared = "device.unshared", true
	case !was && now:
		ev.Type = "device.shared"
	case now && daemon.ConfigDescription(old, d) != daemon.ConfigDescription(current, d):
		ev.Type = "device.updated"
	default:
		return
	}
	s.Events().Publish(ev)
}

func publiclyShared(cfg *netshape.Config, d daemon.DeviceInfo) bool {
	return !daemon.IsComputer(d) && (d.Provider != "" || slices.ContainsFunc(cfg.Shared, func(id string) bool { return daemon.ConfigNames(id, d) }))
}
