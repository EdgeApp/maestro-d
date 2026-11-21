package flow

// maestro-d: the traffic-shaping steps. The host that owns the device sends
// the HTTP call its network.json configures for it (pkg/netshape); the
// executor runs them, never a driver.

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// SetTrafficShapeStep sets the device's whole network shape through the call its host's network.json configures for it.
//
// Every value is optional and defaults to 0 (no cap, no loss, no latency,
// no jitter) or both, and every call sends all five, so a value left out is
// reset rather than kept.
type SetTrafficShapeStep struct {
	BaseStep  `yaml:",inline"`
	Mbit      float64 `yaml:"mbit"`      // Bandwidth cap in megabits per second, fractions allowed; 0 = no cap
	LossPct   float64 `yaml:"lossPct"`   // Random packet loss, 0–100
	LatencyMs float64 `yaml:"latencyMs"` // Added one-way latency in milliseconds
	JitterMs  float64 `yaml:"jitterMs"`  // Latency jitter in milliseconds
	Direction string  `yaml:"direction"` // download, upload or both (default both); ${VAR} allowed
}

// ResetTrafficShapeStep removes the device's network shape through the call its host's network.json configures for it.
type ResetTrafficShapeStep struct {
	BaseStep `yaml:",inline"`
}

// ValidateTrafficShape checks a setTrafficShape step's values with
// netshape's rules. A direction that still holds a ${VAR} is checked by the
// executor once it is expanded.
func ValidateTrafficShape(s *SetTrafficShapeStep) error {
	v := netshape.Values{Mbit: s.Mbit, LossPct: s.LossPct, LatencyMs: s.LatencyMs, JitterMs: s.JitterMs, Direction: s.Direction}
	if strings.Contains(v.Direction, "${") {
		v.Direction = ""
	}
	if err := v.Validate(); err != nil {
		return fmt.Errorf("setTrafficShape: %w", err)
	}
	return nil
}

// ValidateTrafficShapeDirection checks an expanded direction; empty means
// both.
func ValidateTrafficShapeDirection(d string) error {
	if err := netshape.ValidateDirection(d); err != nil {
		return fmt.Errorf("setTrafficShape: %w", err)
	}
	return nil
}

// decodeSetTrafficShape reads setTrafficShape's value: a map of its
// fields, or nothing at all (every value at its default).
func decodeSetTrafficShape(s *SetTrafficShapeStep, valueNode *yaml.Node, sourcePath string) (Step, error) {
	if !(valueNode.Kind == yaml.ScalarNode && (valueNode.Tag == "!!null" || valueNode.Value == "")) {
		// A misspelled field would silently send its default, 0: an
		// unshaped call where the flow meant to shape.
		if err := checkKnownKeys(valueNode, reflect.TypeOf(*s), "setTrafficShape"); err != nil {
			return nil, wrapParseError(sourcePath, valueNode.Line, err)
		}
		if err := valueNode.Decode(s); err != nil {
			return nil, wrapParseError(sourcePath, valueNode.Line, err)
		}
	}
	if err := ValidateTrafficShape(s); err != nil {
		return nil, wrapParseError(sourcePath, valueNode.Line, err)
	}
	s.StepType = StepSetTrafficShape
	return s, nil
}

// checkKnownKeys refuses a mapping key that no yaml tag of t (inline
// structs included) names.
func checkKnownKeys(node *yaml.Node, t reflect.Type, step string) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	known := map[string]bool{}
	var collect func(reflect.Type)
	collect = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if opts == "inline" && f.Type.Kind() == reflect.Struct {
				collect(f.Type)
			} else if name != "" && name != "-" {
				known[name] = true
			}
		}
	}
	collect(t)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if key := node.Content[i].Value; !known[key] {
			names := make([]string, 0, len(known))
			for k := range known {
				names = append(names, k)
			}
			sort.Strings(names)
			return fmt.Errorf("%s has no field %q; it takes %s", step, key, strings.Join(names, ", "))
		}
	}
	return nil
}

// decodeResetTrafficShape reads resetTrafficShape, which takes nothing but
// the options every step takes (optional, label, timeout, platform). A flow
// that wrote shape fields there meant a setTrafficShape, and would
// otherwise silently reset the shape.
func decodeResetTrafficShape(s *ResetTrafficShapeStep, valueNode *yaml.Node, sourcePath string) (Step, error) {
	switch {
	case valueNode == nil:
	case valueNode.Kind == yaml.ScalarNode && (valueNode.Tag == "!!null" || valueNode.Value == ""):
	case valueNode.Kind == yaml.MappingNode:
		if err := checkKnownKeys(valueNode, reflect.TypeOf(*s), "resetTrafficShape"); err != nil {
			return nil, wrapParseError(sourcePath, valueNode.Line, fmt.Errorf("%w; to set a shape use setTrafficShape", err))
		}
		if err := valueNode.Decode(s); err != nil {
			return nil, wrapParseError(sourcePath, valueNode.Line, err)
		}
	default:
		return nil, wrapParseError(sourcePath, valueNode.Line, errors.New("resetTrafficShape takes no value; to set a shape use setTrafficShape"))
	}
	s.StepType = StepResetTrafficShape
	return s, nil
}
