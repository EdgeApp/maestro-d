package hub

// rewrite.go: qualified ids at the API boundary. Inside a hub every
// device id is local (the registry, devices.json, the attached map, the
// lease table); a response leaving the hub's own handler names devices as
// <localId>@<hub> and leases as <uuid>@<hub>, and its device entries carry
// hub and localId. Requests have the suffix stripped before the handler
// sees them (router.go).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// splitID splits <localId>@<hub>; hub is "" for an unqualified id. Hub
// names are lowercase ([a-z0-9-]), so a client's suffix is lowercased
// here, where every route reads it: sim-1@Hub-A is sim-1@hub-a.
func splitID(id string) (local, hub string) {
	if i := strings.LastIndexByte(id, '@'); i > 0 && i < len(id)-1 {
		return id[:i], hubName(id[i+1:])
	}
	return id, ""
}

// hubName normalizes a hub name a client sent (an id's suffix, a
// selector's or a boot body's hub, a catalog query): hub names are
// compared lowercase everywhere.
func hubName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// qualify adds @hub to an id that has no hub yet.
func qualify(id, hub string) string {
	if id == "" || strings.Contains(id, "@") {
		return id
	}
	return id + "@" + hub
}

// qualifyJSON rewrites the ids in a JSON document; anything that is not
// JSON comes back unchanged.
func qualifyJSON(data []byte, hub string, indent bool) []byte {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return data
	}
	v = qualifyValue(v, hub)
	var out []byte
	var err error
	if indent {
		out, err = json.MarshalIndent(v, "", "  ")
		out = append(out, '\n')
	} else {
		out, err = json.Marshal(v)
	}
	if err != nil {
		return data
	}
	return out
}

func qualifyValue(v any, hub string) any {
	switch t := v.(type) {
	case []any:
		for i := range t {
			t[i] = qualifyValue(t[i], hub)
		}
	case map[string]any:
		qualifyObject(t, hub)
		for k, c := range t {
			t[k] = qualifyValue(c, hub)
		}
	}
	return v
}

func has(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

// qualifyObject rewrites one object's ids: a device entry (id, platform,
// ready), a lease (id, expiresAt, ttlSec), and the device and lease
// references of results, events and error details.
func qualifyObject(m map[string]any, hub string) {
	if id, ok := m["id"].(string); ok && has(m, "platform", "ready") && !strings.Contains(id, "@") {
		m["localId"] = id
		m["id"] = qualify(id, hub)
		m["hub"] = hub
	}
	if id, ok := m["id"].(string); ok && has(m, "expiresAt", "ttlSec") {
		m["id"] = qualify(id, hub)
	}
	for _, k := range []string{"device", "leaseDevice", "lease"} {
		if s, ok := m[k].(string); ok {
			m[k] = qualify(s, hub)
		}
	}
	for _, k := range []string{"candidates", "attached"} {
		if list, ok := m[k].([]any); ok {
			for i, x := range list {
				if s, ok := x.(string); ok {
					list[i] = qualify(s, hub)
				}
			}
		}
	}
}

// qualifyWriter qualifies the ids in the hub's own responses: a JSON body
// is rewritten whole, an event stream line by line, and anything else
// (screenshots, artifacts) passes through.
type qualifyWriter struct {
	w   http.ResponseWriter
	hub string
	// userData marks a route whose answer is the caller's own data (an
	// artifact, variables, an eval's value, the UI tree): it passes
	// through byte for byte, streamed, and only an error is qualified.
	userData bool
	mode     int
	status   int
	buf      bytes.Buffer
}

const (
	modeUndecided = iota
	modeJSON
	modeSSE
	modeRaw
)

func (q *qualifyWriter) Header() http.Header { return q.w.Header() }

func (q *qualifyWriter) WriteHeader(status int) {
	if q.mode != modeUndecided {
		return
	}
	ct := q.w.Header().Get("Content-Type")
	switch {
	case q.userData && status < http.StatusBadRequest:
		q.mode = modeRaw
	case strings.HasPrefix(ct, "application/json"):
		q.mode, q.status = modeJSON, status
		return
	case strings.HasPrefix(ct, "text/event-stream"):
		q.mode = modeSSE
	default:
		q.mode = modeRaw
	}
	q.w.WriteHeader(status)
}

func (q *qualifyWriter) Write(p []byte) (int, error) {
	q.WriteHeader(http.StatusOK)
	switch q.mode {
	case modeJSON:
		return q.buf.Write(p)
	case modeSSE:
		q.buf.Write(p)
		for {
			line, err := q.buf.ReadBytes('\n')
			if err != nil {
				// An unfinished line waits for the rest.
				rest := append([]byte(nil), line...)
				q.buf.Reset()
				q.buf.Write(rest)
				break
			}
			if bytes.HasPrefix(line, []byte("data: ")) {
				body := bytes.TrimSuffix(line[len("data: "):], []byte("\n"))
				line = append(append([]byte("data: "), qualifyJSON(body, q.hub, false)...), '\n')
			}
			if _, err := q.w.Write(line); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	}
	return q.w.Write(p)
}

// Unwrap lets http.ResponseController reach the request's writer: a
// leased request answered without its handler turns on full duplex there
// (serveLeased), so a stalled upload does not hold the answer.
func (q *qualifyWriter) Unwrap() http.ResponseWriter { return q.w }

func (q *qualifyWriter) Flush() {
	if f, ok := q.w.(http.Flusher); ok && q.mode != modeJSON {
		f.Flush()
	}
}

// finish writes a buffered JSON answer.
func (q *qualifyWriter) finish() {
	if q.mode != modeJSON {
		return
	}
	out := qualifyJSON(q.buf.Bytes(), q.hub, true)
	q.w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	q.w.WriteHeader(q.status)
	_, _ = q.w.Write(out)
}

// userDataRoute reports whether r asks for the caller's own data, which a
// qualifyWriter passes through: GET …/artifacts/<path>, …/vars, POST
// …/eval and GET …/hierarchy of a device.
func userDataRoute(r *http.Request) bool {
	seg := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(seg) < 4 || seg[0] != "v1" || seg[1] != "devices" {
		return false
	}
	switch seg[3] {
	case "artifacts":
		return len(seg) > 4 && r.Method == http.MethodGet
	case "vars", "eval":
		return len(seg) == 4
	case "hierarchy":
		return len(seg) == 4 && r.Method == http.MethodGet
	}
	return false
}

// recorder captures a whole answer (fan-out, selector tries, lookups).
type recorder struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func newRecorder() *recorder { return &recorder{header: http.Header{}} }

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *recorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.buf.Write(p)
}
func (r *recorder) Flush() {}

// sseBlocks splits an event stream into its events (blank-line ended),
// leaving out comment-only blocks.
func sseBlocks(r *bufio.Reader, out chan<- string, done <-chan struct{}) {
	var block strings.Builder
	data := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimRight(line, "\r\n") == "" {
			if data {
				select {
				case out <- block.String() + "\n":
				case <-done:
					return
				}
			}
			block.Reset()
			data = false
			continue
		}
		if !strings.HasPrefix(line, ":") {
			data = true
		}
		block.WriteString(line)
	}
}
