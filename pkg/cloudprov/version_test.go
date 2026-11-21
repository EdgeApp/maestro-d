package cloudprov

import (
	"reflect"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []int
		ok   bool
	}{
		{"17.4.1", []int{17, 4, 1}, true},
		{" 16 ", []int{16}, true},
		{"17.0b2", []int{17, 0}, true},
		{"14 beta", []int{14}, true},
		{"", nil, false},
		{"x", nil, false},
		{"17.x", nil, false},
		{"17.x.1", nil, false},
		{"99999999999999999999", nil, false},
	} {
		got, ok := ParseVersion(c.in)
		if ok != c.ok || (ok && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("ParseVersion(%q) = %v %v, want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	if CompareVersions([]int{17}, []int{17, 0}) != 0 || CompareVersions([]int{17, 1}, []int{17}) != 1 || CompareVersions([]int{16}, []int{17}) != -1 {
		t.Fatal("CompareVersions")
	}
}

func TestVersionInRange(t *testing.T) {
	for _, c := range []struct {
		v, min, max string
		want        bool
	}{
		{"17.5", "", "17", true},
		{"17.4", "", "17.2", false},
		{"16.4", "17", "", false},
		{"17.0", "17", "", true},
		{"", "17", "", false},
		{"17.x", "17", "", false},
		{"17", "", "", true},
		{"17", "17.x", "", false},
	} {
		if got := VersionInRange(c.v, c.min, c.max); got != c.want {
			t.Errorf("VersionInRange(%q, %q, %q) = %v, want %v", c.v, c.min, c.max, got, c.want)
		}
	}
}

func TestOSVersionMatches(t *testing.T) {
	for _, c := range []struct {
		v, want string
		ok      bool
	}{
		{"17.4", "17", true},
		{"17.4", "17.4", true},
		{"17.4.1", "17.4", true},
		{"170", "17", false},
		{"17.4", "17.2", false},
		{"17.4", "", true},
	} {
		if got := OSVersionMatches(c.v, c.want); got != c.ok {
			t.Errorf("OSVersionMatches(%q, %q) = %v", c.v, c.want, got)
		}
	}
}
