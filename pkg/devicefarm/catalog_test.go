package devicefarm

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Google Pixel 8":          "google-pixel-8",
		"Apple iPad 10.2 (2021)":  "apple-ipad-10.2-2021",
		"Samsung Galaxy S23+ 5G":  "samsung-galaxy-s23-5g",
		"  --Odd__Name!!  ":       "odd-name",
		"Samsung Galaxy A54 (5G)": "samsung-galaxy-a54-5g",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDevicesFiltersAndModels(t *testing.T) {
	c, f := newTestClient(t)
	f.SetPageSize(2)
	models, err := c.Devices(context.Background(), "Android")
	if err != nil {
		t.Fatal(err)
	}
	body := lastBody(t, f, "ListDevices")
	wantFilters := []any{
		map[string]any{"attribute": "REMOTE_ACCESS_ENABLED", "operator": "EQUALS", "values": []any{"TRUE"}},
		map[string]any{"attribute": "PLATFORM", "operator": "EQUALS", "values": []any{"ANDROID"}},
	}
	if !reflect.DeepEqual(body["filters"], wantFilters) {
		t.Fatalf("filters = %#v", body["filters"])
	}
	// 5 remote-access Android devices in pages of 2.
	if n := f.CountOp("ListDevices"); n != 3 {
		t.Fatalf("ListDevices pages = %d", n)
	}
	byID := map[string]cloudprov.CloudModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if len(models) != 5 {
		t.Fatalf("models = %+v", models)
	}
	want := cloudprov.CloudModel{ID: "df:android:google-pixel-8:14", ARN: dftest.ARNPixel8, Platform: "android",
		Name: "Google Pixel 8", OSVersion: "14", FormFactor: "phone", Manufacturer: "Google",
		Availability: "HIGHLY_AVAILABLE", Available: true}
	if byID[want.ID] != want {
		t.Fatalf("pixel 8 = %+v", byID[want.ID])
	}
	for id, avail := range map[string]bool{
		"df:android:google-pixel-7:13":               true,  // AVAILABLE
		"df:android:samsung-galaxy-tab-s9:14":        false, // TEMPORARY_NOT_AVAILABLE
		"df:android:samsung-galaxy-a54-5g:13":        true,  // "(5G)", first by name
		"df:android:samsung-galaxy-a54-5g-a54a00:13": false, // "5G", BUSY, suffixed by its ARN id
	} {
		m, ok := byID[id]
		if !ok || m.Available != avail {
			t.Errorf("%s: %+v (present %v)", id, m, ok)
		}
	}
	if byID["df:android:samsung-galaxy-tab-s9:14"].FormFactor != "tablet" {
		t.Errorf("form factor not lower-cased")
	}
	if _, ok := byID["df:android:google-pixel-6:12"]; ok {
		t.Errorf("a device without remote access is listed")
	}
}

func TestDevicesBothPlatformsAndCache(t *testing.T) {
	c, f := newTestClient(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	all, err := c.Devices(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 7 {
		t.Fatalf("all = %d", len(all))
	}
	if filters := lastBody(t, f, "ListDevices")["filters"].([]any); len(filters) != 1 {
		t.Fatalf("filters = %v", filters)
	}
	calls := f.CountOp("ListDevices")
	// A copy: callers cannot change the cache.
	all[0].Name = "changed"
	again, _ := c.Devices(context.Background(), "")
	if f.CountOp("ListDevices") != calls || again[0].Name == "changed" {
		t.Fatalf("cache: calls %d→%d", calls, f.CountOp("ListDevices"))
	}
	// Per platform.
	if _, err := c.Devices(context.Background(), "ios"); err != nil {
		t.Fatal(err)
	}
	if f.CountOp("ListDevices") == calls {
		t.Fatal("ios reused the all-platform cache")
	}
	calls = f.CountOp("ListDevices")
	now = now.Add(catalogTTL + time.Second)
	if _, err := c.Devices(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if f.CountOp("ListDevices") == calls {
		t.Fatal("an expired cache was reused")
	}
}

func TestDevicesErrors(t *testing.T) {
	c, f := newTestClient(t)
	if _, err := c.Devices(context.Background(), "windows"); cloudprov.KindOf(err) != cloudprov.Other {
		t.Fatalf("err = %v", err)
	}
	f.FailOp("ListDevices", "ServiceAccountException", "account problem")
	if _, err := c.Devices(context.Background(), "ios"); Classify(err).Kind != cloudprov.NoCapacity {
		t.Fatalf("err = %v", err)
	}
}
