package cloudprov

// Versions of devices and OS bounds, shared by the daemon's filters and
// every provider's model selection.

import (
	"strconv"
	"strings"
)

// VersionInRange reports whether version v lies within [min, max]. Versions
// compare numerically, component by component, missing components counting
// as 0; max compares at its own precision, so max 17 admits 17.5 and max
// 17.2 does not admit 17.4. An empty bound is open. A version that is
// missing or unparsable fails any bound.
func VersionInRange(v, min, max string) bool {
	if min == "" && max == "" {
		return true
	}
	ver, ok := ParseVersion(v)
	if !ok {
		return false
	}
	if min != "" {
		lo, ok := ParseVersion(min)
		if !ok || CompareVersions(ver, lo) < 0 {
			return false
		}
	}
	if max != "" {
		hi, ok := ParseVersion(max)
		if !ok {
			return false
		}
		// Compare only as many components as max names.
		if len(ver) > len(hi) {
			ver = ver[:len(hi)]
		}
		if CompareVersions(ver, hi) > 0 {
			return false
		}
	}
	return true
}

// ParseVersion reads digits and dots ("17", "16.4.1"); a trailing
// non-numeric suffix on a component ("17.0b2") is ignored.
func ParseVersion(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for i, p := range parts {
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 || (end < len(p) && i < len(parts)-1) {
			return nil, false
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			return nil, false
		}
		out = append(out, n)
		if end < len(p) {
			break
		}
	}
	return out, true
}

// CompareVersions compares two parsed versions, missing components as 0.
func CompareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// OSVersionMatches reports whether v is the version want names: want
// itself, or a version it is a prefix of ("17" matches 17.4, not 170).
// An empty want matches everything.
func OSVersionMatches(v, want string) bool {
	return want == "" || v == want || strings.HasPrefix(v, want+".")
}
