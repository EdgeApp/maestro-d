package daemontest

import (
	"context"
	"strings"
	"sync"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// Provider is an in-memory cloudprov.RemoteProvider.
type Provider struct {
	Mu        sync.Mutex
	Models    []cloudprov.CloudModel
	CatalogFn func(platform string) error
	Calls     []string
}

var _ cloudprov.RemoteProvider = (*Provider)(nil)

func NewProvider() *Provider {
	return &Provider{Models: []cloudprov.CloudModel{
		{ID: "df:android:google-pixel-8:14", ARN: "arn:p8", Platform: "android", Name: "Google Pixel 8", OSVersion: "14", Availability: "HIGHLY_AVAILABLE", Available: true},
		{ID: "df:ios:apple-iphone-15:17.4", ARN: "arn:i15", Platform: "ios", Name: "Apple iPhone 15", OSVersion: "17.4", Availability: "BUSY"},
		{ID: "df:ios:apple-iphone-14:27.0", ARN: "arn:i14", Platform: "ios", Name: "Apple iPhone 14", OSVersion: "27.0", Availability: "AVAILABLE", Available: true},
	}}
}

func (f *Provider) RecordCall(name string) {
	f.Mu.Lock()
	f.Calls = append(f.Calls, name)
	f.Mu.Unlock()
}

func (f *Provider) Name() string     { return "devicefarm" }
func (f *Provider) IDPrefix() string { return "df" }
func (f *Provider) Catalog(_ context.Context, platform string) ([]cloudprov.CloudModel, error) {
	f.RecordCall("Catalog")
	if f.CatalogFn != nil {
		if err := f.CatalogFn(platform); err != nil {
			return nil, err
		}
	}
	var out []cloudprov.CloudModel
	for _, m := range f.Models {
		if platform == "" || m.Platform == platform {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *Provider) Allocate(context.Context, cloudprov.CloudSpec, cloudprov.AllocateHooks) (*cloudprov.Allocation, error) {
	f.RecordCall("Allocate")
	return nil, cloudprov.Errorf(cloudprov.Other, "not in this test")
}
func (f *Provider) Install(context.Context, *cloudprov.Allocation, cloudprov.AppRef) (*cloudprov.InstallResult, error) {
	f.RecordCall("Install")
	return &cloudprov.InstallResult{}, nil
}
func (f *Provider) Refresh(context.Context, *cloudprov.Allocation) (string, string, error) {
	f.RecordCall("Refresh")
	return "RUNNING", "", nil
}
func (f *Provider) Release(context.Context, *cloudprov.Allocation) (*cloudprov.ReleaseResult, error) {
	f.RecordCall("Release")
	return &cloudprov.ReleaseResult{Minutes: &cloudprov.DeviceMinutes{Metered: 1, Total: 1}}, nil
}
func (f *Provider) ReleaseRef(context.Context, string) (*cloudprov.DeviceMinutes, error) {
	f.RecordCall("ReleaseRef")
	return nil, nil
}
func (f *Provider) Upload(context.Context, string) (*cloudprov.AppRef, error) {
	f.RecordCall("Upload")
	return &cloudprov.AppRef{Ref: "arn:upload"}, nil
}
func (f *Provider) Apps(context.Context) ([]cloudprov.AppRef, error) {
	f.RecordCall("Apps")
	return nil, nil
}
func (f *Provider) Account(context.Context) (map[string]any, error) {
	f.RecordCall("Account")
	return map[string]any{}, nil
}
func (f *Provider) Sessions(context.Context) ([]cloudprov.SessionSummary, error) {
	f.RecordCall("Sessions")
	return nil, nil
}
func (f *Provider) Redact(s string) string { return strings.ReplaceAll(s, "SECRET", "<redacted>") }
