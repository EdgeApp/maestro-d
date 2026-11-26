package hubweb

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

type feed struct {
	source      source
	add, remove chan chan []byte
	slots       chan struct{}
	done        chan struct{}
	ping        time.Duration
}

func newFeed(source source) *feed {
	return &feed{source: source, done: make(chan struct{}), add: make(chan chan []byte), remove: make(chan chan []byte), slots: make(chan struct{}, 64), ping: 25 * time.Second}
}

// One scan serves every browser. Local events reapply the sharing policy and
// leases immediately, even when discovery or a peer request is in flight.
func (f *feed) run(ctx context.Context, events <-chan daemon.Event, polls <-chan time.Time, now func() time.Time) {
	defer close(f.done)
	var latest *inventory
	var digest [sha256.Size]byte
	var scanning bool
	clients := map[chan []byte]bool{} // true once this socket has its first frame
	results := make(chan inventory, 1)
	scan := func() {
		if scanning || len(clients) == 0 {
			return
		}
		scanning = true
		go func() { results <- f.source.scan(ctx) }()
	}
	emit := func() {
		if latest == nil || len(clients) == 0 {
			return
		}
		s := f.source.project(*latest)
		body, _ := json.Marshal(s)
		hash := sha256.Sum256(body)
		changed := hash != digest
		digest = hash
		var message []byte
		for client, sent := range clients {
			if sent && !changed {
				continue
			}
			if message == nil {
				s.GeneratedAt = now().UTC().Format(time.RFC3339Nano)
				message, _ = json.Marshal(s)
			}
			// A slow browser only needs the newest complete snapshot.
			select {
			case <-client:
			default:
			}
			client <- message
			clients[client] = true
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ch := <-f.add:
			clients[ch] = false
			if latest == nil {
				scan()
			} else {
				emit()
			}
		case ch := <-f.remove:
			delete(clients, ch)
			if len(clients) == 0 {
				latest = nil
			}
		case <-polls:
			scan()
		case next := <-results:
			scanning = false
			if len(clients) > 0 {
				latest = &next
				emit()
			}
		case ev := <-events:
			if strings.HasPrefix(ev.Type, "device.") || strings.HasPrefix(ev.Type, "lease.") {
				emit()
				if strings.HasPrefix(ev.Type, "device.") {
					scan()
				}
			}
		}
	}
}

func (f *feed) socket(w http.ResponseWriter, r *http.Request) {
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	default:
		http.Error(w, "all dashboard sockets are in use", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn.SetReadLimit(4096)
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	updates := make(chan []byte, 1)
	select {
	case <-ctx.Done():
		return
	case f.add <- updates:
	}
	defer func() {
		select {
		case f.remove <- updates:
		case <-f.done:
		}
	}()
	ping := time.NewTicker(f.ping)
	defer ping.Stop()
	for {
		var payload []byte
		select {
		case <-ctx.Done():
			return
		case payload = <-updates:
		case <-ping.C:
		}
		send, stop := context.WithTimeout(ctx, 5*time.Second)
		if payload == nil {
			err = conn.Ping(send)
		} else {
			err = conn.Write(send, websocket.MessageText, payload)
		}
		stop()
		if err != nil {
			return
		}
	}
}
