package simulator

// catalog.go (maestro-d): what `simctl create` can make on this Mac — the
// installed iOS runtimes and the iPhone and iPad device types — and the
// rules `device create` uses to pick a runtime and a type.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Catalog lists the iOS runtimes and device types simctl knows.
type Catalog struct {
	Runtimes    []CatalogRuntime    `json:"runtimes"`
	DeviceTypes []CatalogDeviceType `json:"deviceTypes"`
}

// CatalogRuntime is one installed iOS simulator runtime.
type CatalogRuntime struct {
	// ID is the runtime identifier, e.g. com.apple.CoreSimulator.SimRuntime.iOS-26-5.
	ID string `json:"id"`
	// Name is simctl's display name, e.g. "iOS 26.5".
	Name      string `json:"name"`
	Version   string `json:"version"`
	Build     string `json:"build,omitempty"`
	Available bool   `json:"available"`
	// DeviceTypes are the identifiers of the iPhone and iPad types this
	// runtime supports, in simctl's order.
	DeviceTypes []string `json:"deviceTypes,omitempty"`
}

// CatalogDeviceType is one iPhone or iPad simulator type.
type CatalogDeviceType struct {
	// ID is the type identifier, e.g. com.apple.CoreSimulator.SimDeviceType.iPhone-16.
	ID            string `json:"id"`
	Name          string `json:"name"`
	ProductFamily string `json:"productFamily"`
	// MinRuntimeVersion is the oldest iOS that runs this type; the newest
	// models have the highest.
	MinRuntimeVersion string `json:"minRuntimeVersion,omitempty"`
}

type simctlCatalogRuntimes struct {
	Runtimes []struct {
		Identifier           string                  `json:"identifier"`
		Name                 string                  `json:"name"`
		Version              string                  `json:"version"`
		BuildVersion         string                  `json:"buildversion"`
		Platform             string                  `json:"platform"`
		IsAvailable          bool                    `json:"isAvailable"`
		SupportedDeviceTypes []simctlDeviceTypeEntry `json:"supportedDeviceTypes"`
	} `json:"runtimes"`
}

type simctlCatalogDeviceTypes struct {
	DeviceTypes []struct {
		Identifier              string `json:"identifier"`
		Name                    string `json:"name"`
		ProductFamily           string `json:"productFamily"`
		MinRuntimeVersionString string `json:"minRuntimeVersionString"`
	} `json:"devicetypes"`
}

// ListCatalog runs `simctl list runtimes -j` and `simctl list devicetypes -j`.
func ListCatalog() (*Catalog, error) {
	if _, err := FindSimctlBinary(); err != nil {
		return nil, err
	}
	rt, err := execCommand("xcrun", "simctl", "list", "runtimes", "-j").Output()
	if err != nil {
		return nil, fmt.Errorf("xcrun simctl list runtimes: %w", err)
	}
	dt, err := execCommand("xcrun", "simctl", "list", "devicetypes", "-j").Output()
	if err != nil {
		return nil, fmt.Errorf("xcrun simctl list devicetypes: %w", err)
	}
	return parseCatalog(rt, dt)
}

// parseCatalog reads the two simctl documents. Empty output is an empty
// catalog: a fresh Xcode without simulator runtimes prints nothing useful.
func parseCatalog(runtimesJSON, typesJSON []byte) (*Catalog, error) {
	cat := &Catalog{Runtimes: []CatalogRuntime{}, DeviceTypes: []CatalogDeviceType{}}
	if len(strings.TrimSpace(string(typesJSON))) > 0 {
		var dts simctlCatalogDeviceTypes
		if err := json.Unmarshal(typesJSON, &dts); err != nil {
			return nil, fmt.Errorf("parse simctl devicetypes: %w", err)
		}
		for _, d := range dts.DeviceTypes {
			if d.ProductFamily != "iPhone" && d.ProductFamily != "iPad" {
				continue
			}
			cat.DeviceTypes = append(cat.DeviceTypes, CatalogDeviceType{
				ID: d.Identifier, Name: d.Name, ProductFamily: d.ProductFamily, MinRuntimeVersion: d.MinRuntimeVersionString,
			})
		}
	}
	if len(strings.TrimSpace(string(runtimesJSON))) > 0 {
		var rts simctlCatalogRuntimes
		if err := json.Unmarshal(runtimesJSON, &rts); err != nil {
			return nil, fmt.Errorf("parse simctl runtimes: %w", err)
		}
		for _, r := range rts.Runtimes {
			if r.Platform != "iOS" && extractPlatform(r.Identifier) != "iOS" {
				continue
			}
			rt := CatalogRuntime{ID: r.Identifier, Name: r.Name, Version: r.Version, Build: r.BuildVersion, Available: r.IsAvailable}
			for _, t := range r.SupportedDeviceTypes {
				if t.ProductFamily == "iPhone" || t.ProductFamily == "iPad" {
					rt.DeviceTypes = append(rt.DeviceTypes, t.Identifier)
				}
			}
			cat.Runtimes = append(cat.Runtimes, rt)
		}
	}
	return cat, nil
}

// DefaultRuntime is the newest available iOS runtime.
func (c *Catalog) DefaultRuntime() (*CatalogRuntime, error) {
	var best *CatalogRuntime
	for i := range c.Runtimes {
		r := &c.Runtimes[i]
		if r.Available && (best == nil || compareVersions(r.Version, best.Version) > 0) {
			best = r
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no iOS simulator runtime is installed; add one in Xcode > Settings > Components")
	}
	return best, nil
}

// ResolveRuntime finds an available runtime by identifier, by its short form
// ("iOS-26-5"), by name ("iOS 26.5") or by version ("26.5").
func (c *Catalog) ResolveRuntime(spec string) (*CatalogRuntime, error) {
	s := strings.TrimSpace(spec)
	for i := range c.Runtimes {
		r := &c.Runtimes[i]
		short := r.ID[strings.LastIndex(r.ID, ".")+1:]
		if r.ID == s || strings.EqualFold(short, s) || strings.EqualFold(r.Name, s) || r.Version == s {
			if !r.Available {
				return nil, fmt.Errorf("runtime %s is installed but not available (simctl reports it unusable)", r.ID)
			}
			return r, nil
		}
	}
	var names []string
	for _, r := range c.Runtimes {
		if r.Available {
			names = append(names, r.ID[strings.LastIndex(r.ID, ".")+1:])
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no iOS runtime %q: no iOS simulator runtime is installed", spec)
	}
	return nil, fmt.Errorf("no iOS runtime %q; installed: %s", spec, strings.Join(names, ", "))
}

// ResolveDeviceType finds a device type by identifier, by its short form
// ("iPhone-16") or by name ("iPhone 16"). A runtime with a list of
// supported types must include it.
func (c *Catalog) ResolveDeviceType(spec string, rt *CatalogRuntime) (*CatalogDeviceType, error) {
	s := strings.TrimSpace(spec)
	for i := range c.DeviceTypes {
		d := &c.DeviceTypes[i]
		short := d.ID[strings.LastIndex(d.ID, ".")+1:]
		if d.ID == s || strings.EqualFold(short, s) || strings.EqualFold(d.Name, s) {
			if rt != nil && len(rt.DeviceTypes) > 0 && !contains(rt.DeviceTypes, d.ID) {
				return nil, fmt.Errorf("device type %q is not supported by %s", d.Name, rt.Name)
			}
			return d, nil
		}
	}
	return nil, fmt.Errorf("no simulator device type %q; see `maestro-d device catalog --platform ios`", spec)
}

// DefaultDeviceType is the newest iPhone the runtime supports: the highest
// minimum runtime version, then simctl's own order, which lists the newest
// models first.
func (c *Catalog) DefaultDeviceType(rt *CatalogRuntime) (*CatalogDeviceType, error) {
	var best *CatalogDeviceType
	for i := range c.DeviceTypes {
		d := &c.DeviceTypes[i]
		if d.ProductFamily != "iPhone" {
			continue
		}
		if rt != nil && len(rt.DeviceTypes) > 0 && !contains(rt.DeviceTypes, d.ID) {
			continue
		}
		if best == nil || compareVersions(d.MinRuntimeVersion, best.MinRuntimeVersion) > 0 {
			best = d
		}
	}
	if best == nil {
		name := "the runtime"
		if rt != nil {
			name = rt.Name
		}
		return nil, fmt.Errorf("no iPhone simulator type is available for %s", name)
	}
	return best, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// compareVersions compares dotted numeric versions ("26.5" < "26.10").
// A missing or non-numeric part counts as 0.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
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
