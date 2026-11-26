package hubd

import (
	"errors"
	"os"
	"reflect"
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
//
// A change resets derived state under mu and takes a ticket, then runs its
// callback with mu released once every earlier ticket's callback has run:
// device discovery never blocks a reader, and transitions publish their
// events in the order they took effect.
type configCache struct {
	mu          sync.Mutex
	publish     sync.Mutex // guards turn and its condition
	turnCond    *sync.Cond
	issued      uint64 // tickets handed out, under mu
	turn        uint64 // the ticket whose callback may run next, under publish
	path        string
	nextCheck   time.Time
	info        os.FileInfo
	config      *netshape.Config
	configError error
	warned      map[string]bool
	statFailed  bool
	load        func(string) (*netshape.Config, error)
	warn        func(string, ...any)
	reset       func()
	changed     func(old, current *netshape.Config)
}

// transition is a published change from old to current, in ticket order.
type transition struct {
	old, current *netshape.Config
	ticket       uint64
}

func newConfigCache(path string, reset func(), changed func(*netshape.Config, *netshape.Config)) *configCache {
	return &configCache{path: path, load: netshape.LoadConfig, warn: logger.Warn, reset: reset, changed: changed}
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
	cfg, t, err := c.snapshotLocked(now, force)
	c.unlock(t, c.changed)
	return cfg, err
}

// edit publishes the file and its snapshot together. A concurrent reader may
// never reload the file before the mutation publishes its verified identities.
func (c *configCache) edit(write func() error, changed func(old, current *netshape.Config)) (*netshape.Config, error) {
	c.mu.Lock()
	if err := write(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	cfg, t, err := c.snapshotLocked(time.Now(), true)
	c.unlock(t, changed)
	return cfg, err
}

// unlock releases mu, then runs changed for a transition once every earlier
// transition's callback has finished. No one waits for a turn holding mu.
func (c *configCache) unlock(t *transition, changed func(old, current *netshape.Config)) {
	c.mu.Unlock()
	if t == nil {
		return
	}
	c.publish.Lock()
	if c.turnCond == nil {
		c.turnCond = sync.NewCond(&c.publish)
	}
	for c.turn != t.ticket {
		c.turnCond.Wait()
	}
	c.publish.Unlock()
	defer func() {
		c.publish.Lock()
		c.turn++
		c.turnCond.Broadcast()
		c.publish.Unlock()
	}()
	if changed != nil {
		changed(t.old, t.current)
	}
}

func (c *configCache) snapshotLocked(now time.Time, force bool) (*netshape.Config, *transition, error) {
	if !force && now.Before(c.nextCheck) {
		return c.config, nil, c.configError
	}
	c.nextCheck = now.Add(time.Second)
	info, err := os.Stat(c.path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if !force && c.config != nil && !c.statFailed && err == nil && sameFile(c.info, info) {
		return c.config, nil, c.configError
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
		return c.config, nil, err
	}
	old := c.config
	c.configError, c.warned = nil, nil
	// An unchanged file, rewritten or repaired, keeps its snapshot and needs no
	// device discovery.
	if old != nil && reflect.DeepEqual(old, current) {
		return old, nil, nil
	}
	c.config = current
	if old == nil {
		return current, nil, nil
	}
	if c.reset != nil {
		c.reset()
	}
	c.issued++
	return current, &transition{old, current, c.issued - 1}, nil
}

func (s *Server) DeviceConfig() *netshape.Config {
	cfg, _ := s.config.snapshot(time.Now(), false)
	return cfg
}

func (s *Server) configChanged(old, current *netshape.Config) {
	s.publishConfigChange(old, current, s.Registry().List(""))
}

// forgetShares drops every cached share answer when the config changes.
func (s *Server) forgetShares() {
	s.shareMu.Lock()
	s.shareGen++
	clear(s.sharedIDs)
	clear(s.shareSeen)
	s.shareMu.Unlock()
}

func (s *Server) publishConfigChange(old, current *netshape.Config, devices []daemon.DeviceInfo) {
	// A running AVD can be configured by two spellings. Compare the effective
	// public state once per device and include configured ids absent from discovery.
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
