package cdp

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// enableNetworkEvents keeps the events used by idle waits and request matching
// without retaining downloaded response bodies in the renderer. Omitted limits
// let Chrome keep a large response cache for each debugging connection.
func enableNetworkEvents(client proto.Client) error {
	zero := 0
	return (proto.NetworkEnable{
		MaxTotalBufferSize:    &zero,
		MaxResourceBufferSize: &zero,
	}).Call(client)
}

type networkScope struct {
	page        *rod.Page
	consumers   int
	needsEnable bool
}

type networkEmulationHold struct {
	stop func() bool
}

type networkObservation struct {
	cancel  context.CancelFunc
	done    chan struct{}
	release func() error
	once    sync.Once
	err     error
}

// observeNetwork registers before the caller dispatches its action. Each caller
// owns its listener; overlapping callers share the page's enabled Network domain.
func (d *Driver) observeNetwork(page *rod.Page, ctx context.Context, callbacks ...interface{}) (*networkObservation, error) {
	ctx, cancel := context.WithCancel(ctx)
	// PageFromSession is deliberately used only for protocol calls and events.
	// Its context comes from Browser; it has no initialized DOM/helper state.
	events := page.Browser().Context(ctx).PageFromSession(page.SessionID)
	d.networkMu.Lock()
	scope := d.networkScopes[page.SessionID]
	if scope == nil {
		if d.networkScopes == nil {
			d.networkScopes = make(map[proto.TargetSessionID]*networkScope)
		}
		scope = &networkScope{page: page, needsEnable: true}
		d.networkScopes[page.SessionID] = scope
	}
	if scope.needsEnable {
		if err := enableNetworkEvents(events); err != nil {
			// Chrome may have enabled the domain before its reply failed. Keep
			// cleanup ownership until a disable is confirmed or the page closes.
			err = errors.Join(err, d.releaseIdleNetworkScopes())
			d.networkMu.Unlock()
			cancel()
			return nil, err
		}
		scope.needsEnable = false
	}
	scope.consumers++
	d.networkMu.Unlock()

	observation := &networkObservation{cancel: cancel, done: make(chan struct{})}
	observation.release = func() error {
		d.networkMu.Lock()
		defer d.networkMu.Unlock()
		scope.consumers--
		return d.releaseIdleNetworkScopes()
	}
	// Network is already explicitly enabled with zero buffers, so Rod's automatic
	// domain setup cannot replace those options or disable another listener.
	wait := events.EachEvent(callbacks...)
	go func() {
		defer close(observation.done)
		wait()
	}()
	return observation, nil
}

func (o *networkObservation) close() error {
	o.once.Do(func() {
		o.cancel()
		<-o.done
		o.err = o.release()
	})
	return o.err
}

// releaseIdleNetworkScopes requires networkMu. Network.disable also clears
// Chrome's process-wide connection override, so even a scope on another tab
// must stay enabled while explicit emulation is active. Restoring the override
// afterward would still emit false online/offline events.
func (d *Driver) releaseIdleNetworkScopes() error {
	if len(d.networkEmulation) != 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result error
	for id, scope := range d.networkScopes {
		if scope.consumers != 0 {
			continue
		}
		if scope.page.GetContext().Err() == nil {
			client := scope.page.Browser().Context(ctx).PageFromSession(id)
			if err := (proto.NetworkDisable{}).Call(client); err != nil {
				// A lost reply leaves Rod's enable cache uncertain. A future
				// consumer must explicitly establish events again before listening.
				scope.needsEnable = true
				result = errors.Join(result, err)
				continue // Keep the scope so a later release can retry cleanup.
			}
		}
		delete(d.networkScopes, id)
	}
	return result
}

// applyNetworkConditions requires networkMu, serializing settings changes with
// the last observer's cleanup. A failed reset must not release an active hold.
func (d *Driver) applyNetworkConditions(page *rod.Page, conditions proto.NetworkEmulateNetworkConditions) error {
	if err := conditions.Call(page); err != nil {
		return err
	}
	id := page.SessionID
	active := conditions.Offline || conditions.Latency > 0 ||
		conditions.DownloadThroughput >= 0 || conditions.UploadThroughput >= 0
	if active {
		if d.networkEmulation == nil {
			d.networkEmulation = make(map[proto.TargetSessionID]*networkEmulationHold)
		}
		if d.networkEmulation[id] == nil {
			hold := &networkEmulationHold{}
			d.networkEmulation[id] = hold
			hold.stop = context.AfterFunc(page.GetContext(), func() {
				d.networkMu.Lock()
				defer d.networkMu.Unlock()
				if d.networkEmulation[id] == hold {
					delete(d.networkEmulation, id)
					if err := d.releaseIdleNetworkScopes(); err != nil {
						log.Printf("[browser] failed to release Network after page closure: %v", err)
					}
				}
			})
		}
	} else if hold := d.networkEmulation[id]; hold != nil {
		hold.stop()
		delete(d.networkEmulation, id)
	}
	return d.releaseIdleNetworkScopes()
}
