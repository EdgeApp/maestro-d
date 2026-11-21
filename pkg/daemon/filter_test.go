package daemon

import (
	"net/url"
	"reflect"
	"testing"
)

func TestParseDeviceQuery(t *testing.T) {
	v := url.Values{
		"location": {"Local, cloud,local"}, "provider": {"DeviceFarm"}, "platform": {"IOS"}, "kind": {"Simulator"},
		"name": {"iphone"}, "osVersionMin": {"16"}, "osVersionMax": {"17.2"}, "available": {"true"}, "hub": {"b"},
	}
	q, e := ParseDeviceQuery(v)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(q.Location, []string{"local", "cloud"}) || q.Provider != "devicefarm" || q.Platform != "ios" ||
		q.Kind != "simulator" || q.Name != "iphone" || q.OSVersionMin != "16" || q.OSVersionMax != "17.2" || !q.Available || q.Hub != "b" {
		t.Fatalf("parsed %+v", q)
	}
	// Values round-trips the filters.
	back, e := ParseDeviceQuery(q.Values())
	if e != nil || !reflect.DeepEqual(back.Values(), q.Values()) {
		t.Fatalf("round trip: %v %v", back.Values(), e)
	}
	if got := (DeviceQuery{}).Values(); len(got) != 0 {
		t.Fatalf("zero query encodes %v", got)
	}

	for _, bad := range []url.Values{
		{"location": {"moon"}},
		{"name": {"(["}},
		{"osVersionMin": {"x"}},
		{"osVersionMax": {"1..2"}},
		{"available": {"maybe"}},
	} {
		if _, e := ParseDeviceQuery(bad); e == nil || e.Code != CodeUsage {
			t.Errorf("%v: want USAGE, got %v", bad, e)
		}
	}
	if q, e := ParseDeviceQuery(url.Values{"available": {"0"}}); e != nil || q.Available {
		t.Fatalf("available=0: %+v %v", q, e)
	}
}

func TestDeviceQueryMatch(t *testing.T) {
	devs := []DeviceInfo{
		{ID: "u1", Platform: "ios", Kind: "simulator", Name: "iPhone 15", OSVersion: "17.4", Location: "local", Available: true},
		{ID: "e1", Platform: "android", Kind: "emulator", Name: "Pixel_7", OSVersion: "13", Location: "mesh"},
		{ID: "df-1", Platform: "android", Kind: "cloud", Name: "Google Pixel 8", OSVersion: "14", Provider: "devicefarm", Location: "cloud", Available: true},
		{ID: "mock-1", Platform: "mock", Kind: "device", Name: "mock-1", Location: "local"},
	}
	ids := func(q DeviceQuery) []string {
		var out []string
		for _, d := range q.Filter(devs) {
			out = append(out, d.ID)
		}
		return out
	}
	mustQ := func(v url.Values) DeviceQuery {
		q, e := ParseDeviceQuery(v)
		if e != nil {
			t.Fatal(e)
		}
		return q
	}
	for _, c := range []struct {
		v    url.Values
		want []string
	}{
		{url.Values{}, []string{"u1", "e1", "df-1", "mock-1"}},
		{url.Values{"location": {"local"}}, []string{"u1", "mock-1"}},
		{url.Values{"location": {"mesh,cloud"}}, []string{"e1", "df-1"}},
		{url.Values{"provider": {"devicefarm"}}, []string{"df-1"}},
		{url.Values{"platform": {"android"}}, []string{"e1", "df-1"}},
		{url.Values{"kind": {"cloud"}}, []string{"df-1"}},
		{url.Values{"name": {"PIXEL"}}, []string{"e1", "df-1"}},
		{url.Values{"name": {"^mock"}}, []string{"mock-1"}},
		{url.Values{"name": {"^df-"}}, []string{"df-1"}}, // matched against the id
		{url.Values{"available": {"true"}}, []string{"u1", "df-1"}},
		{url.Values{"osVersionMin": {"14"}}, []string{"u1", "df-1"}}, // no osVersion fails any bound
		{url.Values{"osVersionMax": {"13"}}, []string{"e1"}},
		{url.Values{"osVersionMin": {"1"}}, []string{"u1", "e1", "df-1"}},
	} {
		if got := ids(mustQ(c.v)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: got %v, want %v", c.v, got, c.want)
		}
	}
	// A query built in code (no compiled regexp) still matches names.
	if got := ids(DeviceQuery{Name: "iphone"}); !reflect.DeepEqual(got, []string{"u1"}) {
		t.Errorf("unparsed name filter: %v", got)
	}
	if got := ids(DeviceQuery{Name: "(["}); got != nil {
		t.Errorf("an invalid unparsed regexp matches nothing, got %v", got)
	}
}

func TestDeviceQueryMatchModel(t *testing.T) {
	q, _ := ParseDeviceQuery(url.Values{"platform": {"ios"}, "name": {"iphone 1[45]"}, "osVersionMin": {"16"}, "osVersionMax": {"18"}, "available": {"true"}})
	for _, c := range []struct {
		platform, name, id, os string
		avail, want            bool
	}{
		{"ios", "Apple iPhone 15", "df:ios:apple-iphone-15:17.4", "17.4", true, true},
		{"ios", "Apple iPhone 15", "x", "17.4", false, false},
		{"ios", "Apple iPhone 14", "x", "27.0", true, false},
		{"android", "Apple iPhone 15", "x", "17", true, false},
		{"ios", "Apple iPhone 13", "x", "17", true, false},
	} {
		if got := q.MatchModel(c.platform, c.name, c.id, c.os, c.avail); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

func TestVersionInRange(t *testing.T) {
	for _, c := range []struct {
		v, min, max string
		want        bool
	}{
		{"17.5", "", "17", true},
		{"17.4", "", "17.2", false},
		{"17.2.1", "", "17.2", true},
		{"17", "17.0", "", true},
		{"16.9", "17", "", false},
		{"18", "17", "17", false},
		{"", "", "", true},
		{"", "1", "", false},
		{"abc", "", "20", false},
		{"17.0b2", "17", "17", true},
		{"17", "x", "", false},
		{"17", "", "x", false},
		{"9", "10", "", false},
	} {
		if got := VersionInRange(c.v, c.min, c.max); got != c.want {
			t.Errorf("VersionInRange(%q, %q, %q) = %v, want %v", c.v, c.min, c.max, got, c.want)
		}
	}
}
