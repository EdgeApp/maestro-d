package daemon

import (
	"net/http/httptest"
	"testing"
)

func TestPlainDaemonExplicitSharing(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	public := DeviceInfo{ID: "sim-1", Name: "Shared Phone", Platform: "ios", Kind: "simulator"}
	private := DeviceInfo{ID: "sim-2", Name: "Private Phone", Platform: "ios", Kind: "simulator"}
	listings := 0
	s := New(Config{ShareDevices: []string{"Shared Phone"}, ShareConfig: []string{"avd:Lab"}, ShareTVs: []string{"Living Room"}}, Deps{ListDevices: func(string) []DeviceInfo {
		listings++
		return []DeviceInfo{public, private}
	}})
	remote := httptest.NewRequest("GET", "/v1/devices", nil)
	remote = remote.WithContext(withCaller(remote.Context(), Caller{Remote: true}))
	if got := s.visible(remote, []DeviceInfo{public, private}); len(got) != 1 || got[0].ID != public.ID {
		t.Fatalf("visible devices: %+v", got)
	}
	if !s.shared(public.ID) || !s.shared(public.ID) || s.shared(private.ID) || s.shared(private.ID) || listings != 2 {
		t.Fatalf("cached discovery: %d listings", listings)
	}
	s.events.Publish(Event{Type: "device.booted", Device: public.ID})
	if !s.shared(public.ID) || s.shared(private.ID) || listings != 3 {
		t.Fatalf("discovery after boot: %d listings", listings)
	}
	for _, tc := range []struct {
		event   Event
		visible bool
	}{
		{Event{Type: "device.attached", Device: public.ID, Message: "private report path"}, true},
		{Event{Type: "device.stopped", Device: private.ID}, false},
		{Event{Type: "daemon.shutdown"}, true},
		{Event{Type: "other"}, false},
	} {
		ev, ok := s.remoteEvent(tc.event)
		if ok != tc.visible || (ok && ev.Type == "device.attached" && ev.Message != "") {
			t.Fatalf("event %+v became %+v, visible %t", tc.event, ev, ok)
		}
	}
	for _, tc := range []struct {
		req  BootRequest
		code Code
	}{
		{BootRequest{Name: public.ID}, ""}, {BootRequest{Name: private.ID}, CodeDeviceNotFound},
		{BootRequest{Platform: "android", Name: "Lab"}, ""},
		{BootRequest{Name: "Shared Phone", Create: &CreateSpec{}}, ""},
		{BootRequest{Create: &CreateSpec{Name: "private"}}, CodeForbidden},
	} {
		err := s.checkShareBoot(remote, tc.req)
		if err == nil && tc.code != "" || err != nil && tc.code != err.Code {
			t.Fatalf("boot %+v: %v, want %q", tc.req, err, tc.code)
		}
	}
	if got := s.shareList(); got != "Shared Phone; TVs Living Room; config.json avd:Lab" {
		t.Fatalf("share list: %q", got)
	}
	if got := s.stopTarget(public.Name); got.ID != public.ID {
		t.Fatalf("stop by name: %+v", got)
	}
	if got := s.stopTarget("missing"); got.ID != "missing" {
		t.Fatalf("missing stop target: %+v", got)
	}
}
