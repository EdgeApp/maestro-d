package devicefarm

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm/types"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// catalogTTL is how long a platform's device list is reused.
var catalogTTL = 2 * time.Minute

type catalogEntry struct {
	models []cloudprov.CloudModel
	at     time.Time
}

var reSlugRun = regexp.MustCompile(`[^a-z0-9.]+`)

// slug lower-cases name and collapses runs of characters outside [a-z0-9.]
// to "-".
func slug(name string) string {
	return strings.Trim(reSlugRun.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// arnID is the last component of an ARN, lower-cased.
func arnID(arn string) string {
	if i := strings.LastIndexAny(arn, ":/"); i >= 0 {
		arn = arn[i+1:]
	}
	return strings.ToLower(arn)
}

// platformName maps "android"/"ios" (any case) to Device Farm's platform.
func platformName(p string) (types.DevicePlatform, error) {
	switch strings.ToLower(p) {
	case "android":
		return types.DevicePlatformAndroid, nil
	case "ios":
		return types.DevicePlatformIos, nil
	}
	return "", cloudprov.Errorf(cloudprov.Other, "unknown platform %q: use android or ios", p)
}

// Devices lists the remote-access models, for one platform ("android",
// "ios") or both (""). Results are cached for two minutes per platform.
func (c *Client) Devices(ctx context.Context, platform string) ([]cloudprov.CloudModel, error) {
	key := strings.ToLower(platform)
	filters := []types.DeviceFilter{{
		Attribute: types.DeviceFilterAttributeRemoteAccessEnabled,
		Operator:  types.RuleOperatorEquals,
		Values:    []string{"TRUE"},
	}}
	if key != "" {
		p, err := platformName(key)
		if err != nil {
			return nil, err
		}
		filters = append(filters, types.DeviceFilter{
			Attribute: types.DeviceFilterAttributePlatform,
			Operator:  types.RuleOperatorEquals,
			Values:    []string{string(p)},
		})
	}
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	if e, ok := c.catalog[key]; ok && c.now().Sub(e.at) < catalogTTL {
		return append([]cloudprov.CloudModel(nil), e.models...), nil
	}
	var devices []types.Device
	pages := devicefarm.NewListDevicesPaginator(c.api, &devicefarm.ListDevicesInput{Filters: filters})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		devices = append(devices, page.Devices...)
	}
	models := toModels(devices)
	c.catalog[key] = catalogEntry{models: models, at: c.now()}
	return append([]cloudprov.CloudModel(nil), models...), nil
}

// toModels maps devices to catalog models with unique ids. Devices are
// ordered by platform, name, OS and ARN first, so the same pool always gets
// the same ids: the first of a colliding id keeps it bare, later ones get
// "-<first 6 hex of the ARN id>".
func toModels(devices []types.Device) []cloudprov.CloudModel {
	sort.SliceStable(devices, func(i, j int) bool {
		a, b := devices[i], devices[j]
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if str(a.Name) != str(b.Name) {
			return str(a.Name) < str(b.Name)
		}
		if str(a.Os) != str(b.Os) {
			return str(a.Os) < str(b.Os)
		}
		return str(a.Arn) < str(b.Arn)
	})
	seen := map[string]bool{}
	models := make([]cloudprov.CloudModel, 0, len(devices))
	for _, d := range devices {
		platform := strings.ToLower(string(d.Platform))
		s := slug(str(d.Name))
		id := fmt.Sprintf("df:%s:%s:%s", platform, s, str(d.Os))
		if seen[id] {
			suffix := arnID(str(d.Arn))
			if len(suffix) > 6 {
				suffix = suffix[:6]
			}
			id = fmt.Sprintf("df:%s:%s-%s:%s", platform, s, suffix, str(d.Os))
		}
		seen[id] = true
		models = append(models, cloudprov.CloudModel{
			ID:           id,
			ARN:          str(d.Arn),
			Platform:     platform,
			Name:         str(d.Name),
			OSVersion:    str(d.Os),
			FormFactor:   strings.ToLower(string(d.FormFactor)),
			Manufacturer: str(d.Manufacturer),
			Availability: string(d.Availability),
			Available:    d.Availability == types.DeviceAvailabilityHighlyAvailable || d.Availability == types.DeviceAvailabilityAvailable,
		})
	}
	return models
}
