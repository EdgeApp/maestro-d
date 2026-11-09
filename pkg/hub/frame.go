// Package hub joins maestro-d hubs into a mesh. Every hub owns the devices
// on its own machine and serves the full REST API to clients; the router
// (router.go) forwards a request for another hub's device to that hub over
// a peer link (link.go): a WebSocket carrying framed HTTP exchanges. Hubs
// find each other with mDNS once, at launch (discover.go), dial the
// addresses named with --peer, and keep the mesh full by passing their
// peer lists on (mesh.go). Large and streaming exchanges get their own
// short-lived link (bulk.go).
package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Frame types: JSON text messages on a peer link.
const (
	frameHello  = "hello"
	framePeers  = "peers"
	frameReq    = "req"
	frameRes    = "res"
	frameCancel = "cancel"
	// frameCredit acknowledges N bytes of a streamed body read (flow.go).
	frameCredit = "credit"
)

// maxInlineBody is the largest body a req or res frame carries inline
// (base64); a larger one streams as binary frames.
const maxInlineBody = 1 << 20

// chunkSize is the payload of one binary data frame.
const chunkSize = 256 << 10

// PeerRef names a hub and the address it is dialed at.
type PeerRef struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// Frame is one JSON text message on a peer link.
type Frame struct {
	T  string `json:"t"`
	ID string `json:"id,omitempty"`

	// req
	Method    string      `json:"method,omitempty"`
	Path      string      `json:"path,omitempty"`
	Query     string      `json:"query,omitempty"`
	Headers   http.Header `json:"headers,omitempty"`
	Principal string      `json:"principal,omitempty"`
	// Body is the whole body (base64 in JSON) unless Stream is set, when it
	// follows as binary data frames ended by an empty one.
	Body   []byte `json:"body,omitempty"`
	Stream bool   `json:"stream,omitempty"`

	// res
	Status int `json:"status,omitempty"`

	// credit
	N int `json:"n,omitempty"`

	// hello
	Name       string `json:"name,omitempty"`
	Version    string `json:"version,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
	// Instance is a random id per hub process: a hello from a known name
	// with a new instance is that hub restarted.
	Instance string `json:"instance,omitempty"`
	// Addr is the address the sender is dialed at (its --advertise, or the
	// link's remote host with Port when empty).
	Addr string `json:"addr,omitempty"`
	Port int    `json:"port,omitempty"`
	TLS  bool   `json:"tls,omitempty"`

	// hello and peers: the sender's linked peers.
	Peers []PeerRef `json:"peers,omitempty"`
}

func encodeFrame(f Frame) ([]byte, error) { return json.Marshal(f) }

func decodeFrame(data []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return Frame{}, fmt.Errorf("bad frame: %w", err)
	}
	if f.T == "" {
		return Frame{}, errors.New("bad frame: no type")
	}
	return f, nil
}

// encodeData builds a binary data frame: one byte of id length, the id,
// then the payload. An empty payload ends the body.
func encodeData(id string, payload []byte) []byte {
	out := make([]byte, 0, 1+len(id)+len(payload))
	out = append(out, byte(len(id)))
	out = append(out, id...)
	return append(out, payload...)
}

func decodeData(data []byte) (id string, payload []byte, err error) {
	if len(data) == 0 || len(data) < 1+int(data[0]) {
		return "", nil, errors.New("bad data frame")
	}
	n := int(data[0])
	return string(data[1 : 1+n]), data[1+n:], nil
}
