package daemon

// filter.go: the filters on GET /v1/devices and GET /v1/catalog
// ?location=cloud. They apply to every device: local, mesh, mock, web and
// cloud.

import (
	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// DeviceQuery filters a device listing. The zero value matches everything.
type DeviceQuery struct {
	// Location lists the locations to keep (local, mesh, cloud); empty
	// keeps all.
	Location []string
	Provider string
	Platform string
	// Kind is device, emulator, simulator, tv or cloud.
	Kind string
	// Name is a case-insensitive regexp matched against name and id.
	Name string
	// OSVersionMin and OSVersionMax bound the OS version, inclusively.
	OSVersionMin string
	OSVersionMax string
	// Available keeps only devices a lease could take now.
	Available bool
	// Hub names the hub that answers (catalog only).
	Hub string

	name *regexp.Regexp
}

// Values encodes q as query parameters.
func (q DeviceQuery) Values() url.Values {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	set("location", strings.Join(q.Location, ","))
	set("provider", q.Provider)
	set("platform", q.Platform)
	set("kind", q.Kind)
	set("name", q.Name)
	set("osVersionMin", q.OSVersionMin)
	set("osVersionMax", q.OSVersionMax)
	set("hub", q.Hub)
	if q.Available {
		v.Set("available", "true")
	}
	return v
}

// ParseDeviceQuery reads the filters from query parameters. An invalid
// location word, regexp or version is USAGE.
func ParseDeviceQuery(v url.Values) (DeviceQuery, *Error) {
	q := DeviceQuery{
		Provider:     strings.ToLower(strings.TrimSpace(v.Get("provider"))),
		Platform:     strings.ToLower(strings.TrimSpace(v.Get("platform"))),
		Kind:         strings.ToLower(strings.TrimSpace(v.Get("kind"))),
		Name:         v.Get("name"),
		OSVersionMin: strings.TrimSpace(v.Get("osVersionMin")),
		OSVersionMax: strings.TrimSpace(v.Get("osVersionMax")),
		Hub:          strings.TrimSpace(v.Get("hub")),
	}
	locs, e := parseLocations(v.Get("location"))
	if e != nil {
		return q, e
	}
	q.Location = locs
	switch a := strings.ToLower(v.Get("available")); a {
	case "", "false", "0":
	case "true", "1":
		q.Available = true
	default:
		return q, Errorf(CodeUsage, "available must be true or false, not %q", a).WithDetails("field", "available")
	}
	if q.Name != "" {
		re, err := regexp.Compile("(?i)" + q.Name)
		if err != nil {
			return q, Errorf(CodeUsage, "name %q is not a valid regular expression: %v", q.Name, err).WithDetails("field", "name")
		}
		q.name = re
	}
	for _, f := range []struct{ field, val string }{{"osVersionMin", q.OSVersionMin}, {"osVersionMax", q.OSVersionMax}} {
		if f.val != "" {
			if _, ok := cloudprov.ParseVersion(f.val); !ok {
				return q, Errorf(CodeUsage, "%s %q is not a version (digits and dots, like 17 or 16.4)", f.field, f.val).WithDetails("field", f.field)
			}
		}
	}
	return q, nil
}

// parseLocations reads a comma list of local, mesh and cloud.
func parseLocations(s string) ([]string, *Error) {
	var out []string
	for _, w := range strings.Split(s, ",") {
		w = strings.ToLower(strings.TrimSpace(w))
		switch w {
		case "":
		case LocationLocal, LocationMesh, LocationCloud:
			if !slices.Contains(out, w) {
				out = append(out, w)
			}
		default:
			return nil, Errorf(CodeUsage, "location %q is not one of local, mesh, cloud", w).WithDetails("field", "location")
		}
	}
	return out, nil
}

// Match reports whether d passes every filter but location (which needs the
// caller's machine; see MatchLocation).
func (q DeviceQuery) Match(d DeviceInfo) bool {
	switch {
	case q.Provider != "" && !strings.EqualFold(d.Provider, q.Provider):
		return false
	case q.Platform != "" && !strings.EqualFold(d.Platform, q.Platform):
		return false
	case q.Kind != "" && !strings.EqualFold(d.Kind, q.Kind):
		return false
	case q.Available && !d.Available:
		return false
	case !q.matchName(d.Name, d.ID):
		return false
	}
	return VersionInRange(d.OSVersion, q.OSVersionMin, q.OSVersionMax)
}

func (q DeviceQuery) matchName(name, id string) bool {
	if q.Name == "" {
		return true
	}
	re := q.name
	if re == nil {
		var err error
		if re, err = regexp.Compile("(?i)" + q.Name); err != nil {
			return false
		}
	}
	return re.MatchString(name) || re.MatchString(id)
}

// MatchLocation reports whether d's location is one q keeps.
func (q DeviceQuery) MatchLocation(d DeviceInfo) bool {
	return len(q.Location) == 0 || slices.Contains(q.Location, d.Location)
}

// Filter keeps the devices that pass every filter, location included.
func (q DeviceQuery) Filter(devs []DeviceInfo) []DeviceInfo {
	out := make([]DeviceInfo, 0, len(devs))
	for _, d := range devs {
		if q.Match(d) && q.MatchLocation(d) {
			out = append(out, d)
		}
	}
	return out
}

// MatchModel reports whether a cloud model passes the catalog filters.
func (q DeviceQuery) MatchModel(platform, name, id, osVersion string, available bool) bool {
	switch {
	case q.Platform != "" && !strings.EqualFold(platform, q.Platform):
		return false
	case q.Available && !available:
		return false
	case !q.matchName(name, id):
		return false
	}
	return VersionInRange(osVersion, q.OSVersionMin, q.OSVersionMax)
}

// VersionInRange reports whether version v lies within [min, max]
// (cloudprov.VersionInRange).
func VersionInRange(v, min, max string) bool { return cloudprov.VersionInRange(v, min, max) }
