package daemon

// cloudroute.go: which hub in a mesh launches a cloud device. The catalog,
// cloud leases and the provider utilities all use this rule.

import (
	"slices"
	"sort"
)

// PeerCloud is a linked peer hub and the cloud providers it announced in its
// hello.
type PeerCloud struct {
	Hub       string
	Providers []string
}

// RouteCloud picks the hub that launches provider's devices ("" means any
// provider):
//  1. the hub the client named;
//  2. otherwise this hub (self), when it has the provider;
//  3. otherwise the first linked peer, by name, whose hello lists it.
//
// No hub with the provider is USAGE.
func RouteCloud(named, self string, selfProviders []string, peers []PeerCloud, provider string) (string, *Error) {
	if named != "" {
		return named, nil
	}
	has := func(list []string) bool {
		if provider == "" {
			return len(list) > 0
		}
		return slices.Contains(list, provider)
	}
	if has(selfProviders) {
		return self, nil
	}
	sorted := slices.Clone(peers)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Hub < sorted[j].Hub })
	for _, p := range sorted {
		if has(p.Providers) {
			return p.Hub, nil
		}
	}
	return "", Errorf(CodeUsage, "no hub in this mesh has %s credentials", providerTitle(provider))
}

// providerTitle names a provider for people.
func providerTitle(provider string) string {
	switch provider {
	case "", "devicefarm":
		return "Device Farm"
	default:
		return provider
	}
}
