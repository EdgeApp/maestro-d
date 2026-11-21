package netshape

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Values is one shape. Each number defaults to 0 (no cap, no loss, no
// latency, no jitter) and Direction to both, so a call always carries the
// whole shape.
type Values struct {
	Mbit      float64
	LossPct   float64
	LatencyMs float64
	JitterMs  float64
	Direction string
}

// ValueError is a shape value out of range: the step's fault, found
// before any call is made.
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Validate checks the values a step gives. It is the one place the rules
// live: the step parser and the shaper both call it.
func (v Values) Validate() error {
	for _, f := range []struct {
		name string
		n    float64
	}{{"mbit", v.Mbit}, {"lossPct", v.LossPct}, {"latencyMs", v.LatencyMs}, {"jitterMs", v.JitterMs}} {
		if math.IsNaN(f.n) || math.IsInf(f.n, 0) || f.n < 0 {
			return &ValueError{Msg: fmt.Sprintf("%s must be a number >= 0, got %v", f.name, f.n)}
		}
	}
	if v.LossPct > 100 {
		return &ValueError{Msg: fmt.Sprintf("lossPct must be at most 100, got %v", v.LossPct)}
	}
	return ValidateDirection(v.Direction)
}

// ValidateDirection checks a direction; empty means both.
func ValidateDirection(d string) error {
	if d != "" && !isDirection(d) {
		return &ValueError{Msg: fmt.Sprintf("direction must be one of %s, got %q", strings.Join(Directions, ", "), d)}
	}
	return nil
}

// WithDefaults fills the one value whose default is not zero: an empty
// direction is both.
func (v Values) WithDefaults() Values {
	if v.Direction == "" {
		v.Direction = DirectionBoth
	}
	return v
}

var placeholder = regexp.MustCompile(`\$\{([^}]*)\}`)

var placeholders = map[string]bool{
	"mbit": true, "lossPct": true, "latencyMs": true, "jitterMs": true, "direction": true, "deviceId": true,
}

func checkPlaceholders(s string) error {
	for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
		if !placeholders[m[1]] {
			return fmt.Errorf("unknown placeholder ${%s}; maestro-d fills ${mbit}, ${lossPct}, ${latencyMs}, ${jitterMs}, ${direction} and ${deviceId}", m[1])
		}
	}
	return nil
}

// Request is a call with its placeholders filled.
type Request struct {
	Method             string
	URL                string
	Headers            map[string]string
	Body               []byte // nil when the call has none
	InsecureSkipVerify bool
}

// Fill substitutes values into a call. A reset passes zero Values, so its
// placeholders, if it uses any, take their defaults.
func Fill(c *Call, v Values, deviceID string) (*Request, error) {
	typed := typedValues(c, v, deviceID)
	fill := func(s string, escape func(string) string) string {
		return placeholder.ReplaceAllStringFunc(s, func(m string) string {
			return escape(asText(typed[m[2:len(m)-1]]))
		})
	}
	text := func(s string) string { return fill(s, func(v string) string { return v }) }
	// A value in the path is path-escaped and one in the query string
	// query-escaped, so a space reaches the shaper as a space either way.
	path, query, hasQuery := strings.Cut(c.URL, "?")
	filled := fill(path, url.PathEscape)
	if hasQuery {
		filled += "?" + fill(query, url.QueryEscape)
	}
	req := &Request{Method: c.Method, URL: filled, InsecureSkipVerify: c.InsecureSkipVerify}
	if len(c.Headers) > 0 {
		req.Headers = make(map[string]string, len(c.Headers))
		for k, h := range c.Headers {
			req.Headers[k] = text(h)
		}
	}
	if c.Body != nil {
		body, err := json.Marshal(fillBody(c.Body, typed, text))
		if err != nil {
			return nil, err
		}
		req.Body = body
	}
	return req, nil
}

func typedValues(c *Call, v Values, deviceID string) map[string]any {
	dir := v.WithDefaults().Direction
	if word, ok := c.DirectionValues[dir]; ok {
		dir = word
	}
	return map[string]any{
		"mbit": v.Mbit, "lossPct": v.LossPct, "latencyMs": v.LatencyMs, "jitterMs": v.JitterMs,
		"direction": dir, "deviceId": deviceID,
	}
}

// fillBody copies a JSON value with its placeholders filled. A string that
// is exactly one placeholder becomes the value with its own JSON type, so a
// shaper receives "mbit": 1.5 rather than "1.5".
func fillBody(v any, typed map[string]any, text func(string) string) any {
	switch t := v.(type) {
	case string:
		if m := placeholder.FindStringSubmatch(t); m != nil && m[0] == t {
			return typed[m[1]]
		}
		return text(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = fillBody(e, typed, text)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = fillBody(e, typed, text)
		}
		return out
	}
	return v
}

func asText(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
