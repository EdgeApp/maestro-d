package hub

import (
	"context"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// A hub lists a peer's devices with their name, model and version, through
// the fan-out list and a single-device lookup.
func TestMesh_PeerDevicesCarryModel(t *testing.T) {
	setupHome(t)
	phone := daemon.DeviceInfo{ID: "FAKEPIXEL0001", Platform: "android", Kind: "device", Name: "Lab Pixel", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true}
	a := startHub(t, "hub-a", []daemon.DeviceInfo{phone})
	b := startHub(t, "hub-b", testDevices("b"), withPeers(a.addr))
	waitLinked(t, a, "hub-b")
	waitLinked(t, b, "hub-a")
	ctx := context.Background()
	// The first fan-out can miss a peer that is slow to answer on a busy
	// machine; ask again (past the cache) until it lists the phone.
	var res *daemon.DevicesResult
	for deadline := time.Now().Add(10 * time.Second); ; {
		var err error
		res, err = b.client().DeviceList(ctx, "android")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Devices) == 1 || time.Now().After(deadline) {
			break
		}
		b.mesh.cache.drop("hub-a")
		time.Sleep(100 * time.Millisecond)
	}
	if len(res.Devices) != 1 {
		t.Fatalf("devices %+v, unreachable %+v", res.Devices, res.Unreachable)
	}
	got := res.Devices[0]
	if got.ID != "FAKEPIXEL0001@hub-a" || got.Name != "Lab Pixel" || got.Model != "Pixel 8" || got.OSVersion != "16" {
		t.Errorf("fan-out entry %+v", got)
	}
	one, err := b.client().Device(ctx, "FAKEPIXEL0001@hub-a")
	if err != nil || one.Model != "Pixel 8" {
		t.Errorf("GET device: %+v %v", one, err)
	}
}
