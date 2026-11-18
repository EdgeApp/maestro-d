package hub

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// twoHubs starts hub-a (devices a1, a2) and hub-b (b1, b2), b dialing a.
func twoHubs(t *testing.T, opts ...hubOpt) (*testHub, *testHub) {
	t.Helper()
	setupHome(t)
	a := startHub(t, "hub-a", testDevices("a"))
	b := startHub(t, "hub-b", testDevices("b"), append([]hubOpt{withPeers(a.addr)}, opts...)...)
	waitLinked(t, a, "hub-b")
	waitLinked(t, b, "hub-a")
	return a, b
}

func TestMesh_LinkStatusAndHello(t *testing.T) {
	a, b := twoHubs(t)
	st, err := b.client().Hub(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Hub.Name != "hub-b" || st.Hub.MDNS || len(st.Hub.Peers) != 1 {
		t.Fatalf("status: %+v", st.Hub)
	}
	p := st.Hub.Peers[0]
	if p.Name != "hub-a" || p.State != "linked" || p.Direction != "outbound" || p.Devices == nil || *p.Devices != 2 || p.Addr != a.addr {
		t.Fatalf("peer: %+v", p)
	}
	sa := a.mesh.Status(context.Background())
	if sa.Peers[0].Direction != "inbound" || sa.Peers[0].Addr != b.addr {
		t.Fatalf("a sees b: %+v", sa.Peers)
	}
	// The ping measures an RTT soon after the link opens.
	deadline := time.Now().Add(3 * time.Second)
	for waitLinked(t, b, "hub-a").RTT() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if waitLinked(t, b, "hub-a").RTT() == 0 {
		t.Fatal("no RTT")
	}
	// A wrong client token is refused by the router, as by the daemon.
	req, _ := http.NewRequest(http.MethodGet, "http://"+a.addr+"/v1/hub", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %v %v", resp, err)
	}
	resp.Body.Close()
}

func TestMesh_FanoutListAndCache(t *testing.T) {
	a, b := twoHubs(t)
	res, err := b.client().DeviceList(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range res.Devices {
		ids = append(ids, d.ID)
		if d.Hub == "" || d.LocalID == "" || !strings.HasSuffix(d.ID, "@"+d.Hub) {
			t.Fatalf("entry not qualified: %+v", d)
		}
	}
	if strings.Join(ids, ",") != "b1@hub-b,b2@hub-b,a1@hub-a,a2@hub-a" || len(res.Unreachable) != 0 {
		t.Fatalf("merged: %v %+v", ids, res.Unreachable)
	}
	// Peers' answers are cached for 2 s: a second list does not ask a.
	before := a.count("GET /v1/devices")
	if _, err := b.client().DeviceList(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if a.count("GET /v1/devices") != before {
		t.Fatal("the second list within 2 s asked the peer again")
	}
	// A peer that does not answer in time is listed under unreachable.
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/devices" {
			time.Sleep(1500 * time.Millisecond)
		}
		return false
	})
	b.mesh.cache.drop("hub-a")
	res, err = b.client().DeviceList(context.Background(), "mock")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Devices) != 2 || len(res.Unreachable) != 1 || res.Unreachable[0].Hub != "hub-a" || !strings.Contains(res.Unreachable[0].Error, "no answer") {
		t.Fatalf("slow peer: %+v", res)
	}
	a.setHook(func(http.ResponseWriter, *http.Request) bool { return false })
	// scope=local is just this hub.
	code, body := b.do(t, http.MethodGet, "/v1/devices?scope=local", nil)
	if code != 200 || strings.Contains(string(body), "@hub-a") {
		t.Fatalf("scope=local: %d %s", code, body)
	}
}

func TestMesh_RoutingLeasesAndCommands(t *testing.T) {
	a, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	// An unqualified id of a peer's device resolves to that peer.
	info, err := c.Device(ctx, "a1")
	if err != nil || info.ID != "a1@hub-a" || info.Hub != "hub-a" {
		t.Fatalf("resolve a1: %+v %v", info, err)
	}
	// POST /v1/leases routes by the body's device suffix.
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a", Holder: "tester", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	if lr.Lease.ID == "" || !strings.HasSuffix(lr.Lease.ID, "@hub-a") || lr.Lease.Device != "a1@hub-a" || lr.Device == nil || !lr.Device.Attached {
		t.Fatalf("lease: %+v %+v", lr.Lease, lr.Device)
	}
	// Without the lease the owner refuses; with it the command runs there.
	code, body := b.do(t, http.MethodPost, "/v1/devices/a1@hub-a/commands/tapOn", `{"text":"Login"}`)
	if code != http.StatusConflict || codeIn(body) != daemon.CodeLeaseRequired {
		t.Fatalf("no lease: %d %s", code, body)
	}
	var principal atomic.Value
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/commands/") {
			principal.Store(r.RemoteAddr + " " + r.URL.Path + " " + r.Header.Get(daemon.HeaderLease))
		}
		return false
	})
	c.SetLease("a1@hub-a", lr.Lease.ID)
	res, err := c.Command(ctx, "a1@hub-a", "tapOn", map[string]any{"text": "Login"}, true)
	if err != nil || !res.OK || res.Device != "a1@hub-a" {
		t.Fatalf("command: %+v %v", res, err)
	}
	got, _ := principal.Load().(string)
	if !strings.Contains(got, "via hub hub-b") || !strings.Contains(got, "/v1/devices/a1/commands/tapOn") || strings.Contains(got, "@hub-a") {
		t.Fatalf("owner saw %q (want the principal, local ids)", got)
	}
	// The registry and devices.json keep local ids.
	data, _ := os.ReadFile(daemon.DevicesPath("hub-a"))
	if !strings.Contains(string(data), `"id": "a1"`) || strings.Contains(string(data), "@hub-a") {
		t.Fatalf("devices.json: %s", data)
	}
	// A plain daemon on the same machine sees the hub's attachment.
	if owner := daemon.FindDeviceOwner("a1", "other"); owner == nil || owner.Name != "hub-a" {
		t.Fatalf("FindDeviceOwner: %+v", owner)
	}
	// Mesh-wide lease list, and scope=local.
	ls, err := c.Leases(ctx)
	if err != nil || len(ls) != 1 || ls[0].ID != lr.Lease.ID {
		t.Fatalf("leases: %+v %v", ls, err)
	}
	code, body = b.do(t, http.MethodGet, "/v1/leases?scope=local", nil)
	if code != 200 || strings.Contains(string(body), lr.Lease.ID) {
		t.Fatalf("scope=local leases: %s", body)
	}
	// Lease routes by the id's suffix.
	if l, err := c.Lease(ctx, lr.Lease.ID); err != nil || l.Lease.Device != "a1@hub-a" {
		t.Fatalf("get lease: %+v %v", l, err)
	}
	if _, err := c.RenewLease(ctx, lr.Lease.ID, 0); err != nil {
		t.Fatal(err)
	}
	// Another client: DEVICE_IN_USE, from the owner.
	if _, err := a.client().AcquireLease(ctx, daemon.LeaseRequest{Device: "a1"}); !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("second lease: %v", err)
	}
	if _, err := c.ReleaseLease(ctx, lr.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lease(ctx, lr.Lease.ID); !isCode(err, daemon.CodeLeaseExpired) {
		t.Fatalf("released: %v", err)
	}
	// A hub serves a peer only its own devices and leases.
	code, body = a.do(t, http.MethodGet, "/v1/leases/x@hub-c", nil)
	if code != http.StatusNotFound && codeIn(body) != daemon.CodeDeviceNotFound {
		t.Fatalf("unknown hub lease: %d %s", code, body)
	}
	code, body = a.do(t, http.MethodGet, "/v1/devices/zz@hub-c", nil)
	if codeIn(body) != daemon.CodeDeviceNotFound {
		t.Fatalf("unknown hub device: %d %s", code, body)
	}
}

// settle waits until h has n live links for a while (join propagation
// can briefly open, then drop, a duplicate link).
func settle(t *testing.T, h *testHub, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	stable := 0
	for time.Now().Before(deadline) && stable < 10 {
		if linkCount(h.mesh) == n {
			stable++
		} else {
			stable = 0
		}
		time.Sleep(30 * time.Millisecond)
	}
	if stable < 10 {
		t.Fatalf("%s has %d links, want %d", h.name, linkCount(h.mesh), n)
	}
}

func isCode(err error, code daemon.Code) bool {
	var e *daemon.Error
	return errors.As(err, &e) && e.Code == code
}

func TestMesh_SelectorAcrossTheMesh(t *testing.T) {
	_, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	// Local devices come first.
	var got []string
	for i := 0; i < 4; i++ {
		lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock"}})
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		got = append(got, lr.Lease.Device)
	}
	if strings.Join(got, ",") != "b1@hub-b,b2@hub-b,a1@hub-a,a2@hub-a" {
		t.Fatalf("order: %v", got)
	}
	if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock"}}); !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("all taken: %v", err)
	}
	if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "ios", Boot: true}}); !isCode(err, daemon.CodeDeviceNotFound) {
		t.Fatalf("no match: %v", err)
	}
	if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Hub: "nowhere"}}); !isCode(err, daemon.CodeDeviceNotFound) {
		t.Fatalf("unknown hub: %v", err)
	}
	// A usage error from an owner is passed on as is.
	if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Hub: "hub-a"}, TTLSec: -1}); !isCode(err, daemon.CodeUsage) {
		t.Fatalf("bad ttl: %v", err)
	}
}

// A selector skips a hub whose devices' maximum window is shorter than
// the one asked, and tries the next; with no hub left, the answer is the
// owner's USAGE naming the maximum. Listings carry each owner's maxSec,
// machine and addresses through the mesh.
func TestMesh_SelectorOverTheMaximumAndMachine(t *testing.T) {
	_, b := twoHubs(t, func(_ *Config, d *daemon.Config) {
		d.LeaseMax = time.Minute
		d.Advertise = "hub-b.example:7433"
	})
	c := b.client()
	ctx := context.Background()
	devs, err := c.Devices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	for _, d := range devs {
		want := 28800
		if d.Hub == "hub-b" {
			want = 60
		}
		if d.MaxSec != want || d.Machine != host || len(d.Addresses) == 0 {
			t.Fatalf("%s: maxSec %d machine %q addresses %v", d.ID, d.MaxSec, d.Machine, d.Addresses)
		}
		if (d.Hub == "hub-b") != (d.Addresses[0] == "hub-b.example:7433") {
			t.Fatalf("%s: addresses %v", d.ID, d.Addresses)
		}
	}
	var got []string
	for i := 0; i < 2; i++ {
		lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, TTLSec: 600})
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		got = append(got, lr.Lease.Device)
	}
	if strings.Join(got, ",") != "a1@hub-a,a2@hub-a" {
		t.Fatalf("order: %v", got)
	}
	_, err = c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, TTLSec: 600})
	var de *daemon.Error
	if !errors.As(err, &de) || de.Code != daemon.CodeUsage || de.Details["maxSec"] != float64(60) {
		t.Fatalf("over every free device's maximum: %v", err)
	}
	// Within hub-b's maximum the local devices come first again.
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, TTLSec: 60})
	if err != nil || lr.Lease.Device != "b1@hub-b" || lr.Lease.MaxSec != 60 {
		t.Fatalf("within the maximum: %+v %v", lr, err)
	}
	if overMaxBody([]byte("not json")) {
		t.Fatal("overMaxBody")
	}
}

func TestMesh_SelectorHubAndAmbiguousIDs(t *testing.T) {
	setupHome(t)
	shared := daemon.DeviceInfo{ID: "same", Platform: "mock", Kind: "device", Name: "Same", State: "device", Ready: true}
	a := startHub(t, "hub-a", append(testDevices("a"), shared))
	b := startHub(t, "hub-b", append(testDevices("b"), shared), withPeers(a.addr))
	waitLinked(t, b, "hub-a")
	c := b.client()
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Hub: "hub-a", Platform: "mock"}, Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil || !strings.HasSuffix(lr.Lease.Device, "@hub-a") {
		t.Fatalf("select hub-a: %+v %v", lr, err)
	}
	// "same" is listed on b itself, so it is b's; a local id wins.
	if info, err := c.Device(ctx, "same"); err != nil || info.ID != "same@hub-b" {
		t.Fatalf("local wins: %+v %v", info, err)
	}
	// From a third hub that lists neither, it is ambiguous.
	cc := startHub(t, "hub-c", nil, withPeers(a.addr, b.addr))
	waitLinked(t, cc, "hub-a")
	waitLinked(t, cc, "hub-b")
	settle(t, cc, 2)
	code, body := cc.do(t, http.MethodGet, "/v1/devices/same", nil)
	if code != http.StatusBadRequest || codeIn(body) != daemon.CodeUsage || !strings.Contains(string(body), "same@hub-a") {
		t.Fatalf("ambiguous: %d %s", code, body)
	}
	code, body = cc.do(t, http.MethodPost, "/v1/leases", map[string]any{"device": "same"})
	if codeIn(body) != daemon.CodeUsage {
		t.Fatalf("ambiguous lease: %d %s", code, body)
	}
	// An id nobody lists is not found, and no hub grants a lease on it:
	// not hub-c, which lists no devices at all.
	code, body = cc.do(t, http.MethodPost, "/v1/leases", map[string]any{"device": "ghost"})
	if code != http.StatusNotFound || codeIn(body) != daemon.CodeDeviceNotFound {
		t.Fatalf("unlisted: %d %s", code, body)
	}
	for _, h := range []*testHub{a, b, cc} {
		if _, body = h.do(t, http.MethodGet, "/v1/leases?scope=local", nil); strings.Contains(string(body), "ghost") {
			t.Fatalf("%s granted the unlisted id: %s", h.name, body)
		}
	}
	// A peer's device, by bare id, still leases through hub-c.
	code, body = cc.do(t, http.MethodPost, "/v1/leases", map[string]any{"device": "b1"})
	if code != 200 || !strings.Contains(string(body), `"b1@hub-b"`) {
		t.Fatalf("peer device: %d %s", code, body)
	}
}

func TestMesh_JoinPropagationAndDedupe(t *testing.T) {
	setupHome(t)
	a := startHub(t, "hub-a", testDevices("a"))
	b := startHub(t, "hub-b", testDevices("b"), withPeers(a.addr))
	waitLinked(t, a, "hub-b")
	// C knows only B, and links A through B's peer list.
	c := startHub(t, "hub-c", testDevices("c"), withPeers(b.addr))
	waitLinked(t, c, "hub-b")
	waitLinked(t, c, "hub-a")
	waitLinked(t, a, "hub-c")
	if c.logged("from a peer's list") == 0 {
		t.Fatal("no join line")
	}
	res, err := a.client().DeviceList(context.Background(), "")
	if err != nil || len(res.Devices) != 6 {
		t.Fatalf("full mesh list: %+v %v", res, err)
	}
	// Two hubs that dial each other at once keep one link, the one the
	// lower name dialed.
	d := startHub(t, "hub-d", nil, withPeers(a.addr))
	e := startHub(t, "hub-e", nil, withPeers(d.addr))
	waitLinked(t, d, "hub-e")
	time.Sleep(300 * time.Millisecond)
	ld, le := waitLinked(t, d, "hub-e"), waitLinked(t, e, "hub-d")
	if !ld.outbound || le.outbound {
		// hub-d < hub-e: whichever survives, both agree it is d's dial,
		// unless only e dialed (then e's link is the one).
		if !(le.outbound && !ld.outbound) {
			t.Fatalf("directions: d outbound %v, e outbound %v", ld.outbound, le.outbound)
		}
	}
}

func TestMesh_Dedupe(t *testing.T) {
	setupHome(t)
	m := New(fastConfig("hub-a"))
	defer m.Close()
	mk := func(outbound bool, inst string) *Link {
		l := &Link{m: m, peer: "hub-b", instance: inst, outbound: outbound, closed: make(chan struct{}),
			pending: map[string]*exchange{}, serving: map[string]*served{}}
		l.ctx, l.cancel = context.WithCancel(context.Background())
		return l
	}
	fakeClose := func(l *Link) { l.closeOnce.Do(func() { close(l.closed) }) }
	in := mk(false, "i1")
	in.conn = nil
	m.peers["hub-b"] = &peerState{name: "hub-b", link: in, instance: "i1"}
	out := mk(true, "i1")
	// hub-a < hub-b: the link hub-a dialed (out) survives over b's (in).
	if dialerName("hub-a", out) != "hub-a" || dialerName("hub-a", in) != "hub-b" {
		t.Fatal("dialerName")
	}
	m.mu.Lock()
	keepNew := !(dialerName(m.cfg.Name, in) < dialerName(m.cfg.Name, out))
	m.mu.Unlock()
	if !keepNew {
		t.Fatal("the lower name's dial should win")
	}
	fakeClose(in)
	fakeClose(out)
}

func TestMesh_RefusalsAndExitCodes(t *testing.T) {
	setupHome(t)
	a := startHub(t, "hub-a", testDevices("a"))
	// A hub with the receiver's own name: HUB_NAME_TAKEN, and since it was
	// the only --peer and no browse found anything, the hub fails.
	h, line := launchHub(t, "hub-a2", nil, withPeers(a.addr), func(c *Config, d *daemon.Config) { c.Name, d.HubName = "hub-a", "hub-a" })
	if !strings.HasPrefix(line, "FAILED code=HUB_NAME_TAKEN") {
		t.Fatalf("taken: %s", line)
	}
	if err := <-h.done; !isCode(err, daemon.CodeHubNameTaken) || daemon.CodeHubNameTaken.ExitCode() != 5 || daemon.CodeHubNameTaken.HTTPStatus() != 409 {
		t.Fatalf("run: %v", err)
	}
	// A wrong peer token: refused 401 (USAGE).
	h, line = launchHub(t, "hub-x", nil, withPeers(a.addr), func(c *Config, _ *daemon.Config) { c.PeerToken = "nope" })
	if !strings.HasPrefix(line, "FAILED code=USAGE") {
		t.Fatalf("bad peer token: %s", line)
	}
	<-h.done
	// Another API version: HUB_VERSION, 409.
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+testPeerToken)
	hdr.Set(headerHub, "hub-v")
	hdr.Set("X-Maestro-Api-Version", "0")
	_, resp, err := websocket.Dial(context.Background(), "ws://"+a.addr+"/v1/hub/link", &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("version: %v %v", resp, err)
	}
	if e := refusalFrom(resp); !isCode(e, daemon.CodeHubVersion) || daemon.CodeHubVersion.ExitCode() != 5 {
		t.Fatalf("version refusal: %v", e)
	}
	// A bad name header.
	hdr.Set("X-Maestro-Api-Version", daemon.APIVersion)
	hdr.Set(headerHub, "Bad Name")
	_, resp, _ = websocket.Dial(context.Background(), "ws://"+a.addr+"/v1/hub/link", &websocket.DialOptions{HTTPHeader: hdr})
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name: %v", resp)
	}
	// A name linked from another address by another process.
	b := startHub(t, "hub-b", nil, withPeers(a.addr))
	waitLinked(t, a, "hub-b")
	if e := a.mesh.checkName("hub-b", "other-instance", "10.9.9.9:7433"); e == nil || e.Code != daemon.CodeHubNameTaken {
		t.Fatalf("linked elsewhere: %v", e)
	}
	if e := a.mesh.checkName("hub-b", "other-instance", b.addr); e != nil {
		t.Fatalf("a restart at the same address is accepted: %v", e)
	}
	// Peer links are made on TCP only.
	code, body := a.do(t, http.MethodGet, "/v1/hub/link", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no peer token: %d %s", code, body)
	}
	if st := b.mesh.Status(context.Background()); st.Peers[0].State != "linked" {
		t.Fatalf("status: %+v", st)
	}
	// The status lists a refused address.
	x := New(fastConfig("hub-x"))
	x.cfg.PeerToken = "wrong"
	if err := x.Start(context.Background(), daemon.Info{HTTPAddr: "127.0.0.1:1"}); err == nil {
		x.cfg.Peers = nil
	}
	x.cfg.Peers = []string{a.addr}
	if err := x.Start(context.Background(), daemon.Info{HTTPAddr: "127.0.0.1:1"}); !isCode(err, daemon.CodeUsage) {
		t.Fatalf("refused start: %v", err)
	}
	if st := x.Status(context.Background()); len(st.Peers) != 1 || st.Peers[0].State != "refused" {
		t.Fatalf("refused status: %+v", st.Peers)
	}
	x.Close()
}

func TestMesh_ReconnectAndPeerUnavailable(t *testing.T) {
	a, b := twoHubs(t)
	l := waitLinked(t, b, "hub-a")
	// Drop the link from a's side: b, the dialer, redials.
	waitLinked(t, a, "hub-b").close(websocket.StatusNormalClosure, "test drop")
	<-l.closed
	l2 := waitLinked(t, b, "hub-a")
	if l2 == l {
		t.Fatal("same link")
	}
	if b.logged("dialing") < 2 {
		t.Fatal("no redial")
	}
	// An in-flight request whose link drops fails PEER_UNAVAILABLE.
	started := make(chan struct{})
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/slow") {
			close(started)
			<-r.Context().Done()
			return true
		}
		return false
	})
	done := make(chan []byte, 1)
	go func() {
		_, body := b.do(t, http.MethodGet, "/v1/devices/a1@hub-a/slow", nil)
		done <- body
	}()
	<-started
	waitLinked(t, b, "hub-a").close(websocket.StatusNormalClosure, "drop mid-request")
	select {
	case body := <-done:
		if codeIn(body) != daemon.CodePeerUnavailable || !strings.Contains(string(body), `"hub": "hub-a"`) {
			t.Fatalf("mid-request drop: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never failed")
	}
	// A stopped peer: its devices leave the list, and requests fail.
	waitLinked(t, b, "hub-a")
	a.stop()
	waitDown(t, b, "hub-a")
	code, body := b.do(t, http.MethodPost, "/v1/devices/a1@hub-a/commands/tapOn", `{"text":"x"}`)
	if code != http.StatusServiceUnavailable || codeIn(body) != daemon.CodePeerUnavailable {
		t.Fatalf("stopped peer: %d %s", code, body)
	}
	res, err := b.client().DeviceList(context.Background(), "")
	if err != nil || len(res.Devices) != 2 || len(res.Unreachable) != 0 {
		t.Fatalf("after a clean stop: %+v %v", res, err)
	}
	st := b.mesh.Status(context.Background())
	if st.Peers[0].State != "stopped" {
		t.Fatalf("status: %+v", st.Peers)
	}
}

func TestMesh_PingTimeoutMarksDown(t *testing.T) {
	a, b := twoHubs(t)
	// Make a stop answering pings, as a stopped process would: its read
	// loop blocks on the link's lock, held here, at the next data frame.
	la := waitLinked(t, a, "hub-b")
	la.mu.Lock()
	lb := waitLinked(t, b, "hub-a")
	if err := lb.writeData("stuck", []byte("x")); err != nil {
		la.mu.Unlock()
		t.Fatal(err)
	}
	select {
	case <-lb.closed:
	case <-time.After(5 * time.Second):
		la.mu.Unlock()
		t.Fatal("missed pongs did not close the link")
	}
	la.mu.Unlock()
	if !strings.Contains(lb.Reason(), "no pong") {
		t.Fatalf("reason: %s", lb.Reason())
	}
	waitLinked(t, b, "hub-a")
	res, err := b.client().DeviceList(context.Background(), "")
	if err != nil || len(res.Devices) != 4 {
		t.Fatalf("relinked list: %+v %v", res, err)
	}
	// While a peer is known but down, listings name it unreachable.
	b.mesh.mu.Lock()
	b.mesh.peers["hub-z"] = &peerState{name: "hub-z", reason: "test"}
	b.mesh.mu.Unlock()
	res, _ = b.client().DeviceList(context.Background(), "")
	if len(res.Unreachable) != 1 || res.Unreachable[0].Hub != "hub-z" {
		t.Fatalf("unreachable: %+v", res.Unreachable)
	}
	code, body := b.do(t, http.MethodGet, "/v1/devices/x@hub-z", nil)
	if code != http.StatusServiceUnavailable || codeIn(body) != daemon.CodePeerUnavailable {
		t.Fatalf("down peer: %d %s", code, body)
	}
	code, body = b.do(t, http.MethodPost, "/v1/leases", map[string]any{"select": map[string]any{"hub": "hub-z"}})
	if codeIn(body) != daemon.CodePeerUnavailable {
		t.Fatalf("select a down hub: %d %s", code, body)
	}
	// An unqualified id no live hub lists may be the down peer's device:
	// PEER_UNAVAILABLE naming it, not DEVICE_NOT_FOUND.
	code, body = b.do(t, http.MethodGet, "/v1/devices/x", nil)
	if code != http.StatusServiceUnavailable || codeIn(body) != daemon.CodePeerUnavailable || !strings.Contains(string(body), "hub-z") {
		t.Fatalf("unqualified id with a down peer: %d %s", code, body)
	}
	// A peer that shut down cleanly has left: the id is simply not found.
	b.mesh.mu.Lock()
	b.mesh.peers["hub-z"].stopped = true
	b.mesh.mu.Unlock()
	code, body = b.do(t, http.MethodGet, "/v1/devices/x", nil)
	if code != http.StatusNotFound || codeIn(body) != daemon.CodeDeviceNotFound {
		t.Fatalf("unqualified id with a stopped peer: %d %s", code, body)
	}
}

func TestMesh_CancelOnClientDisconnect(t *testing.T) {
	a, b := twoHubs(t)
	cancelled := make(chan struct{})
	started := make(chan struct{})
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/slow") {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			return true
		}
		return false
	})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+b.addr+"/v1/devices/a1@hub-a/slow", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the owner's request was not cancelled")
	}
}

// A blob upload's first request is a HEAD, whose refusal has no body: the
// owner's X-Maestro-Error-Code header survives the relay, so a peer's
// client sees LEASE_REQUIRED and LEASE_EXPIRED (not BUSY) and forgets the
// expired lease; the relaying hub's own refusals carry it too.
func TestMesh_BlobHEADRefusalsKeepTheirCode(t *testing.T) {
	_, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	f := t.TempDir() + "/b.bin"
	if err := os.WriteFile(f, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	hx := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if _, err := c.UploadBlob(ctx, "a1@hub-a", hx, f, 5); !isCode(err, daemon.CodeLeaseRequired) {
		t.Fatalf("without a lease: %v", err)
	}
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("a1@hub-a", lr.Lease.ID)
	var told []string
	c.OnLeaseExpired = func(device, lease string) { told = append(told, device+"="+lease) }
	if _, err := c.ReleaseLease(ctx, lr.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadBlob(ctx, "a1@hub-a", hx, f, 5); !isCode(err, daemon.CodeLeaseExpired) {
		t.Fatalf("expired lease: %v", err)
	}
	if c.LeaseOf("a1@hub-a") != "" || len(told) != 1 || told[0] != "a1@hub-a="+lr.Lease.ID {
		t.Fatalf("told %v, lease %q", told, c.LeaseOf("a1@hub-a"))
	}
	if _, _, err := c.HasBlob(ctx, "zz@hub-c", hx); !isCode(err, daemon.CodeDeviceNotFound) {
		t.Fatalf("unknown hub: %v", err)
	}
}

func TestMesh_BulkUploadEventsAndScreenshot(t *testing.T) {
	a, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("a1@hub-a", lr.Lease.ID)
	// An upload over 1 MiB goes on a bulk link.
	payload := make([]byte, 3<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	f := t.TempDir() + "/big.bin"
	if err := os.WriteFile(f, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	hx := hex.EncodeToString(sum[:])
	uploaded, err := c.UploadBlob(ctx, "a1@hub-a", hx, f, int64(len(payload)))
	if err != nil || !uploaded {
		t.Fatalf("upload: %v %v", uploaded, err)
	}
	if b.logged("bulk link to hub-a for PUT") != 1 {
		t.Fatalf("no bulk link: %v", b.logs)
	}
	if ok, _, err := c.HasBlob(ctx, "a1@hub-a", hx); err != nil || !ok {
		t.Fatalf("HEAD: %v %v", ok, err)
	}
	// The screenshot streams back on a bulk link too.
	png, err := c.Screenshot(ctx, "a1@hub-a")
	if err != nil || len(png) == 0 {
		t.Fatalf("screenshot: %d %v", len(png), err)
	}
	// Events for a peer's device are routed to it, with qualified ids.
	evCtx, stop := context.WithCancel(ctx)
	got := make(chan daemon.Event, 16)
	go func() {
		_ = c.Events(evCtx, "a1@hub-a", 0, func(ev daemon.Event) bool {
			got <- ev
			return true
		})
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := c.Command(ctx, "a1@hub-a", "tapOn", map[string]any{"text": "Ev"}, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for found := false; !found; {
		select {
		case ev := <-got:
			if ev.Device != "a1@hub-a" {
				t.Fatalf("event device %q", ev.Device)
			}
			found = ev.Type == "step"
		case <-deadline:
			t.Fatal("no routed step event")
		}
	}
	stop()
	// The merged stream carries both hubs' events.
	mctx, mstop := context.WithTimeout(ctx, 5*time.Second)
	defer mstop()
	req, _ := http.NewRequestWithContext(mctx, http.MethodGet, "http://"+b.addr+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.Sleep(300 * time.Millisecond)
	if _, err := c.Command(ctx, "a1@hub-a", "tapOn", map[string]any{"text": "Merged"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "b1"}); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	seenA, seenB := false, false
	for sc.Scan() && !(seenA && seenB) {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev daemon.Event
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev)
		seenA = seenA || ev.Device == "a1@hub-a"
		seenB = seenB || ev.Device == "b1@hub-b"
	}
	if !seenA || !seenB {
		t.Fatalf("merged stream: a %v b %v", seenA, seenB)
	}
	// scope=local and a local device filter stay on this hub.
	evs := make(chan daemon.Event, 4)
	lctx, lstop := context.WithTimeout(ctx, 2*time.Second)
	defer lstop()
	go func() {
		_ = c.Events(lctx, "b1@hub-b", 0, func(ev daemon.Event) bool { evs <- ev; return false })
	}()
	select {
	case ev := <-evs:
		if ev.Device != "b1@hub-b" {
			t.Fatalf("local filter: %+v", ev)
		}
	case <-lctx.Done():
		t.Fatal("no local event")
	}
	_ = a
}

func TestMesh_BootAndCatalogRouteByHub(t *testing.T) {
	a, b := twoHubs(t)
	var bootBody atomic.Value
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices":
			var req daemon.BootRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			bootBody.Store(req)
			writeJSON(w, 200, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: daemon.DeviceInfo{ID: "NEW-UDID", Platform: "ios", Ready: true}})
			return true
		case r.URL.Path == "/v1/catalog":
			writeJSON(w, 200, map[string]any{"ok": true, "exitCode": 0, "answeredBy": "hub-a"})
			return true
		}
		return false
	})
	dev, err := b.client().CreateDevice(context.Background(), daemon.BootRequest{Name: "sim", Hub: "hub-a", Create: &daemon.CreateSpec{Type: "iPhone 16"}})
	if err != nil || dev.ID != "NEW-UDID@hub-a" {
		t.Fatalf("create on hub-a: %+v %v", dev, err)
	}
	if req, _ := bootBody.Load().(daemon.BootRequest); req.Name != "sim" || req.Hub != "" || req.Create == nil {
		t.Fatalf("owner got %+v", req)
	}
	// A qualified name routes by its suffix, which is stripped.
	if _, err := b.client().StartDevice(context.Background(), daemon.BootRequest{Name: "OLD@hub-a"}); err != nil {
		t.Fatal(err)
	}
	if req, _ := bootBody.Load().(daemon.BootRequest); req.Name != "OLD" {
		t.Fatalf("suffix not stripped: %+v", req)
	}
	code, body := b.do(t, http.MethodGet, "/v1/catalog?hub=hub-a", nil)
	if code != 200 || !strings.Contains(string(body), "answeredBy") {
		t.Fatalf("catalog: %d %s", code, body)
	}
	// Without hub, b answers itself (here: no catalog tools, INTERNAL).
	code, _ = b.do(t, http.MethodGet, "/v1/catalog", nil)
	if code == 200 {
		t.Fatal("b has no catalog hook")
	}
	code, body = b.do(t, http.MethodPost, "/v1/devices", "not json")
	if codeIn(body) != daemon.CodeUsage {
		t.Fatalf("bad body: %d %s", code, body)
	}
	code, body = b.do(t, http.MethodPost, "/v1/leases", "not json")
	if codeIn(body) != daemon.CodeUsage {
		t.Fatalf("bad lease body: %d %s", code, body)
	}
}

func TestMesh_ShareDeviceAcrossTheMesh(t *testing.T) {
	setupHome(t)
	a := startHub(t, "hub-a", testDevices("a"), withShare("a2"))
	b := startHub(t, "hub-b", nil, withPeers(a.addr))
	waitLinked(t, b, "hub-a")
	res, err := b.client().DeviceList(context.Background(), "")
	if err != nil || len(res.Devices) != 1 || res.Devices[0].ID != "a2@hub-a" {
		t.Fatalf("shared only: %+v %v", res, err)
	}
	code, body := b.do(t, http.MethodPost, "/v1/leases", map[string]any{"device": "a1@hub-a"})
	if codeIn(body) != daemon.CodeDeviceNotFound {
		t.Fatalf("unshared: %d %s", code, body)
	}
}

func TestMesh_DiscoveryOnceAtLaunch(t *testing.T) {
	setupHome(t)
	a := startHub(t, "hub-a", testDevices("a"))
	disco := &fakeDisco{found: []Found{{Name: "hub-a", Addr: a.addr}, {Name: "hub-b", Addr: "127.0.0.1:1"}}}
	b := startHub(t, "hub-b", nil, withDisco(disco))
	waitLinked(t, b, "hub-a")
	waitLinked(t, a, "hub-b")
	if disco.browses != 1 || b.mesh.Browses() != 1 || len(disco.advertise) != 1 || b.logged("mdns browse") != 1 {
		t.Fatalf("browses %d, advertised %v", disco.browses, disco.advertise)
	}
	// A restart of the link is a redial, not another browse.
	waitLinked(t, a, "hub-b").close(websocket.StatusNormalClosure, "drop")
	waitLinked(t, b, "hub-a")
	time.Sleep(100 * time.Millisecond)
	if disco.browses != 1 {
		t.Fatalf("browsed again: %d", disco.browses)
	}
	// A failing discoverer is logged, and the hub still starts.
	bad := &fakeDisco{err: errors.New("no multicast")}
	c := startHub(t, "hub-c", nil, withDisco(bad), withPeers(a.addr))
	waitLinked(t, c, "hub-a")
	if c.logged("mdns advertise: no multicast") != 1 || c.logged("mdns browse: no multicast") != 1 {
		t.Fatalf("logs: %v", c.logs)
	}
}

func TestMesh_ReadyWithoutPeers(t *testing.T) {
	setupHome(t)
	// A --peer that never answers: READY after ReadyWait, and the dialer
	// keeps trying.
	h := startHub(t, "hub-a", nil, withPeers("127.0.0.1:1"), func(c *Config, _ *daemon.Config) {
		c.ReadyWait = 300 * time.Millisecond
		c.DialTimeout = 100 * time.Millisecond
	})
	if h.logged("dial 127.0.0.1:1") == 0 {
		t.Fatalf("no failed dial: %v", h.logs)
	}
	st := h.mesh.Status(context.Background())
	if len(st.Peers) != 1 || st.Peers[0].State != "connecting" {
		t.Fatalf("status: %+v", st.Peers)
	}
	// A bad --peer is a usage error.
	m := New(fastConfig("x"))
	m.cfg.Peers = []string{"http://h/x"}
	if err := m.Start(context.Background(), daemon.Info{}); !isCode(err, daemon.CodeUsage) {
		t.Fatalf("bad peer: %v", err)
	}
	m.Close()
}

func TestMesh_UnixSocketRouting(t *testing.T) {
	_, b := twoHubs(t)
	c, err := daemon.Dial("hub-b")
	if err != nil || c == nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := c.DeviceList(ctx, "")
	if err != nil || len(res.Devices) != 4 {
		t.Fatalf("unix list: %+v %v", res, err)
	}
	// From the unix socket a peer's device is forwarded with the hub's own
	// token, as a remote caller there.
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a2@hub-a"})
	if err != nil || lr.Lease.Holder != "unix socket on hub hub-b via hub hub-b" && !strings.Contains(lr.Lease.Holder, "unix socket on hub hub-b") {
		t.Fatalf("unix lease: %+v %v", lr, err)
	}
	if _, err := c.Hub(ctx); err != nil {
		t.Fatal(err)
	}
	_ = b
	// No peer links on the unix socket.
	req, _ := http.NewRequest(http.MethodGet, "http://maestro-d/v1/hub/link", nil)
	rec := newRecorder()
	b.mesh.unixRouter.Load().ServeHTTP(rec, req)
	if envelopeCode(rec.buf.Bytes()) != daemon.CodeUsage {
		t.Fatalf("unix link: %s", rec.buf.String())
	}
}

// TestMesh_UnqualifiedIDWithSilentPeer: when a linked peer does not answer
// the device-list fan-out in time, an unqualified id this hub does not list
// may be that peer's device, so it fails PEER_UNAVAILABLE (retryable) naming
// the peer, not DEVICE_NOT_FOUND.
func TestMesh_UnqualifiedIDWithSilentPeer(t *testing.T) {
	_, b := twoHubs(t, func(c *Config, _ *daemon.Config) {
		c.FanoutTimeout = time.Nanosecond
		c.CacheFor = time.Nanosecond
	})
	code, body := b.do(t, http.MethodGet, "/v1/devices/a1", nil)
	if code != http.StatusServiceUnavailable || codeIn(body) != daemon.CodePeerUnavailable || !strings.Contains(string(body), "hub-a") {
		t.Fatalf("silent peer: %d %s", code, body)
	}
	// This hub's own device still resolves locally.
	if code, body := b.do(t, http.MethodGet, "/v1/devices/b1", nil); code != http.StatusOK {
		t.Fatalf("local b1: %d %s", code, body)
	}
}

// A streamed request body the owner refuses before reading must not stall
// the link: the data frames still arriving are dropped, and the next
// exchange and the pings get through.
func TestMesh_RefusedStreamedBodyDoesNotStallTheLink(t *testing.T) {
	a, b := twoHubs(t)
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/blobs/") {
			return false
		}
		// Answer while the body is still arriving, never reading it.
		time.Sleep(100 * time.Millisecond)
		writeError(w, 0, daemon.Errorf(daemon.CodeLeaseRequired, "refused unread"))
		return true
	})
	l := waitLinked(t, b, "hub-a")
	auth := http.Header{"Authorization": {"Bearer " + testToken}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := make([]byte, 4<<20)
	res, err := l.RoundTrip(ctx, Request{Method: http.MethodPut, Path: "/v1/devices/a1/blobs/" + strings.Repeat("0", 64),
		Header: auth.Clone(), Principal: "test", Stream: bytes.NewReader(payload)})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if codeIn(body) != daemon.CodeLeaseRequired {
		t.Fatalf("refusal: %d %s", res.Status, body)
	}
	// The same link still serves an exchange, more than once.
	for i := 0; i < 3; i++ {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := l.RoundTrip(rctx, Request{Method: http.MethodGet, Path: "/v1/hub", Header: auth.Clone(), Principal: "test"})
		rcancel()
		if err != nil {
			t.Fatalf("exchange %d after the refused upload: %v", i, err)
		}
		_ = res.Body.Close()
	}
	if !l.alive() {
		t.Fatalf("link closed: %s", l.Reason())
	}
}

// Event streams to a peer's devices do not hold bulk slots: with more open
// than bulkPerPeer, a screenshot from the same peer still flows.
func TestMesh_EventStreamsLeaveBulkSlots(t *testing.T) {
	_, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("a1@hub-a", lr.Lease.ID)
	evCtx, stop := context.WithCancel(ctx)
	defer stop()
	for i := 0; i < bulkPerPeer+1; i++ {
		go func() { _ = c.Events(evCtx, "a1@hub-a", 0, func(daemon.Event) bool { return true }) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for b.logged("bulk link to hub-a for GET /v1/events") < bulkPerPeer+1 {
		if time.Now().After(deadline) {
			t.Fatalf("event streams: %d open", b.logged("bulk link to hub-a for GET /v1/events"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	defer scancel()
	if png, err := c.Screenshot(sctx, "a1@hub-a"); err != nil || len(png) == 0 {
		t.Fatalf("screenshot with %d event streams open: %d %v", bulkPerPeer+1, len(png), err)
	}
}

// Two hubs with one name: the dialing hub links the first and refuses the
// second, as an inbound link would be refused, instead of flipping its
// link between them; and a hub answering where another was expected is
// not linked.
func TestMesh_DialRefusesASecondHubOfTheSameName(t *testing.T) {
	setupHome(t)
	same := func(c *Config, d *daemon.Config) { c.Name, d.HubName = "hub-b", "hub-b" }
	b1 := startHub(t, "hub-b1", testDevices("b"), same)
	b2 := startHub(t, "hub-b2", testDevices("c"), same)
	a := startHub(t, "hub-a", testDevices("a"), withPeers(b1.addr, b2.addr))
	l := waitLinked(t, a, "hub-b")
	deadline := time.Now().Add(10 * time.Second)
	for a.logged("not linking hub-b") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no refusal: %v", a.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Several redial periods later the first link still stands.
	time.Sleep(10 * fastConfig("x").BackoffMin)
	if l2, e := a.mesh.link("hub-b"); e != nil || l2 != l || a.logged("not linking hub-b") != 1 || a.logged("replaced") != 0 {
		t.Fatalf("link flipped: %v %v %v", l2 == l, e, a.logs)
	}

	disco := &fakeDisco{found: []Found{{Name: "hub-c", Addr: b1.addr}}}
	d := startHub(t, "hub-d", nil, withDisco(disco))
	deadline = time.Now().Add(10 * time.Second)
	for d.logged("where hub hub-c was expected") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no mismatch: %v", d.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// hub-b may still link to d on its own, inbound, once join propagation
	// (through hub-a) tells it about d; d's dialer must not link it.
	if l, e := d.mesh.link("hub-b"); e == nil && l.outbound {
		t.Fatal("linked the hub that was not expected")
	}
}

// --advertise without a port, on a hub: peers record it with the hub's
// listener port, as its own status shows it.
func TestMesh_AdvertiseNormalized(t *testing.T) {
	setupHome(t)
	a := startHub(t, "hub-a", testDevices("a"))
	b := startHub(t, "hub-b", nil, withPeers(a.addr), func(c *Config, _ *daemon.Config) { c.Advertise = "127.0.0.1" })
	waitLinked(t, a, "hub-b")
	_, port, _ := net.SplitHostPort(b.addr)
	want := "127.0.0.1:" + port
	if st := b.mesh.Status(context.Background()); st.Addr != want {
		t.Fatalf("own status: %q", st.Addr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got string
		for _, p := range a.mesh.Status(context.Background()).Peers {
			if p.Name == "hub-b" {
				got = p.Addr
			}
		}
		if got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer's record: %q, want %q", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	bad := New(fastConfig("hub-z"))
	bad.cfg.Advertise = "h/x"
	if err := bad.Start(context.Background(), daemon.Info{HTTPAddr: "127.0.0.1:1"}); !isCode(err, daemon.CodeUsage) {
		t.Fatalf("bad --advertise: %v", err)
	}
	bad.Close()
}

// The caller's own data comes back through a hub as it was: a JSON
// artifact byte for byte, and variables that look like ids unqualified.
func TestMesh_UserDataNotRewritten(t *testing.T) {
	a, b := twoHubs(t)
	artifact := []byte(`{"device":"pixel","lease":"l1",  "id":"x","platform":"p","ready":true}`)
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/artifacts/flow.json") {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(artifact)
		return true
	})
	c := b.client()
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("a1@hub-a", lr.Lease.ID)
	var got bytes.Buffer
	if err := c.Artifact(ctx, "a1@hub-a", "flow.json", &got); err != nil || !bytes.Equal(got.Bytes(), artifact) {
		t.Fatalf("artifact: %q %v", got.Bytes(), err)
	}
	if _, err := c.SetVars(ctx, "a1@hub-a", map[string]string{"device": "pixel"}); err != nil {
		t.Fatal(err)
	}
	if vars, err := c.Vars(ctx, "a1@hub-a"); err != nil || vars["device"] != "pixel" {
		t.Fatalf("vars: %v %v", vars, err)
	}
	if v, err := c.Eval(ctx, "a1@hub-a", "({device: 'pixel'})"); err != nil || !strings.Contains(fmt.Sprint(v), "device:pixel]") {
		t.Fatalf("eval: %v %v", v, err)
	}
	// An error on the same route is still an error envelope.
	c.SetLease("a1@hub-a", "")
	if _, err = c.Vars(ctx, "a1@hub-a"); !isCode(err, daemon.CodeLeaseRequired) {
		t.Fatalf("error: %v", err)
	}
}

// A client naming a device without its hub does not make the hub run live
// discovery for every step: an attached device is found in the daemon's
// records, and a listed one is remembered for LocalCacheFor.
func TestMesh_UnqualifiedIDSkipsDiscovery(t *testing.T) {
	a, _ := twoHubs(t)
	c := a.client()
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("a1", lr.Lease.ID)
	before, statuses := a.lists.Load(), a.count("GET /v1/status")
	for i := 0; i < 5; i++ {
		if _, err := c.Command(ctx, "a1", "tapOn", map[string]any{"text": "X"}, true); err != nil {
			t.Fatal(err)
		}
	}
	if n := a.lists.Load() - before; n != 0 {
		t.Fatalf("five steps on an attached device ran %d discoveries", n)
	}
	// Nor build a status answer for each: the id is remembered.
	if n := a.count("GET /v1/status") - statuses; n > 1 {
		t.Fatalf("five steps on an attached device built %d status answers", n)
	}
	// An unattached device: the first lookup discovers, the next is
	// remembered (only the route's own handler lists it).
	if _, err := c.Device(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	first := a.lists.Load()
	if _, err := c.Device(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if n := a.lists.Load() - first; n != 1 {
		t.Fatalf("a remembered id ran %d discoveries, want the handler's 1", n)
	}
}

// A bare id for a peer's device is resolved once: a run of steps on it
// through another hub runs that hub's live discovery at most once, and
// fans out at most once. Booting a device here forgets the resolution.
func TestMesh_UnqualifiedPeerIDResolvedOnce(t *testing.T) {
	a, b := twoHubs(t)
	c := a.client()
	ctx := context.Background()
	before, fans := a.lists.Load(), b.count("GET /v1/devices")
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "b1", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	if lr.Lease.Device != "b1@hub-b" {
		t.Fatalf("lease: %+v", lr.Lease)
	}
	c.SetLease("b1", lr.Lease.ID)
	for i := 0; i < 5; i++ {
		if _, err := c.Command(ctx, "b1", "tapOn", map[string]any{"text": "X"}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Screenshot(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if n := a.lists.Load() - before; n > 1 {
		t.Fatalf("seven requests on a peer's device by bare id ran %d local discoveries", n)
	}
	if n := b.count("GET /v1/devices") - fans; n > 1 {
		t.Fatalf("and fanned out %d times", n)
	}
	if hub, ok := a.mesh.peerIDFor("b1"); !ok || hub != "hub-b" {
		t.Fatalf("remembered: %q %v", hub, ok)
	}
	// A boot here may make b1 local: the resolution is forgotten.
	_, _ = a.do(t, http.MethodPost, "/v1/devices", map[string]any{"platform": "mock", "name": "zz"})
	if _, ok := a.mesh.peerIDFor("b1"); ok {
		t.Fatal("a local boot kept the resolution")
	}
	// So is one whose peer's link is down.
	a.mesh.rememberPeerID("b1", "hub-b")
	b.stop()
	waitDown(t, a, "hub-b")
	if _, ok := a.mesh.peerIDFor("b1"); ok {
		t.Fatal("a resolution to a down peer was used")
	}
}

// A hub that joined by dialing out, at an address nobody can reach (a
// listener that never answers, as a NAT drops the SYN): its peer sends it
// screenshots and event streams on the control link at once, never
// waiting DialTimeout for a bulk link, and does not pass the address on.
func TestMesh_UndialablePeerUsesTheControlLink(t *testing.T) {
	setupHome(t)
	hole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hole.Close()
	slowDial := func(c *Config, _ *daemon.Config) { c.DialTimeout, c.BulkRetry = 3*time.Second, time.Hour }
	a := startHub(t, "hub-a", testDevices("a"), slowDial)
	b := startHub(t, "hub-b", testDevices("b"), withPeers(a.addr), slowDial,
		func(c *Config, _ *daemon.Config) { c.Advertise = hole.Addr().String() })
	l := waitLinked(t, a, "hub-b")
	c := a.client()
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "b1@hub-b", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("b1@hub-b", lr.Lease.ID)
	for i := 0; i < 2; i++ {
		start := time.Now()
		if png, err := c.Screenshot(ctx, "b1@hub-b"); err != nil || len(png) == 0 {
			t.Fatalf("screenshot %d: %d %v", i, len(png), err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("screenshot %d took %v: it waited for a bulk dial", i, took)
		}
	}
	if a.logged("bulk link to hub-b for") != 0 || l.dialable.Load() {
		t.Fatalf("a bulk link to an undialable peer: %v", a.logs)
	}
	for _, r := range a.mesh.peerRefs() {
		if r.Name == "hub-b" {
			t.Fatalf("passed on an address it could not dial: %+v", r)
		}
	}
	// The probe gives up after DialTimeout and logs it.
	deadline := time.Now().Add(10 * time.Second)
	for a.logged("bulk links to hub-b at "+hole.Addr().String()+" fail") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no failed probe: %v", a.logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The address of a link this hub dialed is dialable.
	if !waitLinked(t, b, "hub-a").dialable.Load() {
		t.Fatal("an address this hub dialed is not dialable")
	}
	// A bulk dial that fails on an address that was dialable marks it
	// undialable (the next request does not wait again) and probes it;
	// the exchange still runs, on the control link.
	l.dialable.Store(true)
	if png, err := c.Screenshot(ctx, "b1@hub-b"); err != nil || len(png) == 0 {
		t.Fatalf("screenshot after a failed bulk dial: %d %v", len(png), err)
	}
	if l.dialable.Load() || a.logged("bulk link to hub-b: ") != 1 {
		t.Fatalf("a failed bulk dial: dialable %v, %v", l.dialable.Load(), a.logs)
	}
}

// A peer that dialed in at a reachable address is probed, then gets bulk
// links and is passed on to other peers.
func TestMesh_DialedInPeerProbedForBulk(t *testing.T) {
	a, b := twoHubs(t)
	l := waitLinked(t, a, "hub-b")
	deadline := time.Now().Add(10 * time.Second)
	for !l.dialable.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("never probed: %v", a.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if refs := a.mesh.peerRefs(); len(refs) != 1 || refs[0].Name != "hub-b" {
		t.Fatalf("peer refs: %+v", refs)
	}
	c := a.client()
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "b1@hub-b", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("b1@hub-b", lr.Lease.ID)
	if png, err := c.Screenshot(ctx, "b1@hub-b"); err != nil || len(png) == 0 {
		t.Fatalf("screenshot: %d %v", len(png), err)
	}
	if a.logged("bulk link to hub-b for GET") != 1 {
		t.Fatalf("no bulk link: %v", a.logs)
	}
	_ = b
}

// A merged event stream whose client goes away ends its subscriptions on
// the peers at once, even with no event on the way: the peer's handler
// is cancelled and its bulk link closed, so idle subscriptions do not pile
// up there.
func TestMesh_MergedEventsEndPeerSubscriptions(t *testing.T) {
	a, b := twoHubs(t)
	ended := make(chan struct{}, 4)
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/events" {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ": connected\n\ndata: {\"seq\":1,\"type\":\"hello\",\"device\":\"a1\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		ended <- struct{}{}
		return true
	})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+b.addr+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Once the peer's event has come through, its subscription is idle.
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && !strings.Contains(sc.Text(), "a1@hub-a") {
	}
	cancel()
	resp.Body.Close()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer's subscription outlived its client")
	}
}

// A selector's tries each reach their owner under a request id of their
// own, derived from the client's: the owner's outcome cache must not
// answer the boot pass with the ready pass's DEVICE_IN_USE (it would never
// boot anything), and a retry of the whole selector maps onto the same ids.
func TestMesh_SelectorTriesHaveTheirOwnRequestIDs(t *testing.T) {
	a, b := twoHubs(t)
	var mu sync.Mutex
	var ids []string
	record := func(h *testHub) {
		h.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPost && r.URL.Path == "/v1/leases" {
				mu.Lock()
				ids = append(ids, h.name+" "+r.Header.Get(daemon.HeaderRequestID))
				mu.Unlock()
			}
			return false
		})
	}
	record(a)
	record(b)
	sel := map[string]any{"select": map[string]any{"platform": "ios", "boot": true}}
	for i := 0; i < 2; i++ {
		code, body := b.do(t, http.MethodPost, "/v1/leases", sel, daemon.HeaderRequestID, "rid-1")
		if codeIn(body) != daemon.CodeDeviceNotFound {
			t.Fatalf("try %d: %d %s", i, code, body)
		}
	}
	want := []string{"hub-b rid-1/hub-b/ready", "hub-a rid-1/hub-a/ready", "hub-b rid-1/hub-b/boot", "hub-a rid-1/hub-a/boot"}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(ids, ",") != strings.Join(append(want, want...), ",") {
		t.Fatalf("request ids:\n%s", strings.Join(ids, "\n"))
	}
}

// Hub names are lowercase, and a client's hub name in any case means the
// same hub on every route: a selector's hub, an id's suffix, a lease id's
// suffix, a boot body's hub and a catalog query.
func TestMesh_HubNamesAnyCase(t *testing.T) {
	a, b := twoHubs(t)
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices":
			writeJSON(w, 200, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: daemon.DeviceInfo{ID: "NEW", Platform: "ios", Ready: true}})
			return true
		case r.URL.Path == "/v1/catalog":
			writeJSON(w, 200, map[string]any{"ok": true, "exitCode": 0, "answeredBy": "hub-a"})
			return true
		}
		return false
	})
	code, body := b.do(t, http.MethodPost, "/v1/leases", map[string]any{"select": map[string]any{"hub": "Hub-A", "platform": "mock"}, "attach": map[string]any{"platform": "mock"}})
	var lr daemon.LeaseResult
	if err := json.Unmarshal(body, &lr); err != nil || code != 200 || lr.Lease.Device != "a1@hub-a" {
		t.Fatalf("select hub Hub-A: %d %s", code, body)
	}
	lid := strings.TrimSuffix(lr.Lease.ID, "@hub-a") + "@HUB-A"
	code, body = b.do(t, http.MethodPost, "/v1/devices/a1@Hub-A/commands/tapOn", map[string]any{"text": "x"}, daemon.HeaderLease, lid)
	if code != 200 {
		t.Fatalf("command on a1@Hub-A: %d %s", code, body)
	}
	if code, body = b.do(t, http.MethodGet, "/v1/leases/"+lid, nil); code != 200 {
		t.Fatalf("lease %s: %d %s", lid, code, body)
	}
	if code, body = b.do(t, http.MethodGet, "/v1/devices/a2@HUB-A", nil); code != 200 || !strings.Contains(string(body), `"a2@hub-a"`) {
		t.Fatalf("device a2@HUB-A: %d %s", code, body)
	}
	if code, body = b.do(t, http.MethodPost, "/v1/devices", map[string]any{"name": "x", "platform": "ios", "hub": "Hub-A"}); code != 200 || !strings.Contains(string(body), "NEW@hub-a") {
		t.Fatalf("boot on Hub-A: %d %s", code, body)
	}
	if code, body = b.do(t, http.MethodGet, "/v1/catalog?hub=HUB-A", nil); code != 200 || !strings.Contains(string(body), "answeredBy") {
		t.Fatalf("catalog on HUB-A: %d %s", code, body)
	}
}

// Only the hub's unix socket stops it: a TCP client and a request a peer
// forwarded are FORBIDDEN, and the hub keeps running and linked.
func TestMesh_ShutdownOnlyFromTheUnixSocket(t *testing.T) {
	a, b := twoHubs(t)
	status, body := a.do(t, http.MethodPost, "/v1/shutdown", nil)
	if status != http.StatusForbidden || codeIn(body) != daemon.CodeForbidden {
		t.Fatalf("TCP shutdown: %d %s", status, body)
	}
	req, _ := http.NewRequest(http.MethodPost, "/v1/shutdown", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	a.mesh.serveForwarded(rec, req, b.name)
	if rec.Code != http.StatusForbidden || codeIn(rec.Body.Bytes()) != daemon.CodeForbidden {
		t.Fatalf("forwarded shutdown: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := a.client().Status(context.Background()); err != nil {
		t.Fatalf("hub stopped: %v", err)
	}
	waitLinked(t, b, a.name)
	uc, err := daemon.Dial(a.name)
	if err != nil || uc == nil {
		t.Fatalf("dial: %v", err)
	}
	if err := uc.Shutdown(context.Background()); err != nil {
		t.Fatalf("unix shutdown: %v", err)
	}
	select {
	case <-a.done:
		a.stopMu.Do(func() {})
	case <-time.After(10 * time.Second):
		t.Fatal("hub did not stop from its unix socket")
	}
}

// The merged event stream follows the mesh: a peer that restarts is
// subscribed again, and one that links later joins. It cannot be resumed,
// so it carries no id: lines and refuses after/Last-Event-ID.
func TestMesh_MergedEventsFollowTheMesh(t *testing.T) {
	a, b := twoHubs(t)
	for _, hdr := range [][]string{nil, {"Last-Event-ID", "3"}} {
		path := "/v1/events"
		if hdr == nil {
			path += "?after=3"
		}
		code, body := a.do(t, http.MethodGet, path, nil, hdr...)
		if code != http.StatusBadRequest || codeIn(body) != daemon.CodeUsage {
			t.Fatalf("resume a merged stream (%v): %d %s", hdr, code, body)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.addr+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	// expect leases dev on h (and releases it) until its lease.granted
	// event comes through the merged stream.
	expect := func(h *testHub, dev string) {
		t.Helper()
		c := h.client()
		want := `"device":"` + dev + `@` + h.name + `"`
		deadline := time.After(10 * time.Second)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if res, err := c.AcquireLease(context.Background(), daemon.LeaseRequest{Device: dev}); err == nil {
					_, _ = c.ReleaseLease(context.Background(), res.Lease.ID)
				}
			case ln, ok := <-lines:
				if !ok {
					t.Fatal("the merged stream ended")
				}
				if strings.HasPrefix(ln, "id:") {
					t.Fatalf("the merged stream offers an id: %q", ln)
				}
				if strings.Contains(strings.ReplaceAll(ln, " ", ""), want) {
					return
				}
			case <-deadline:
				t.Fatalf("no event for %s@%s on the merged stream", dev, h.name)
			}
		}
	}
	expect(b, "b1")
	expect(a, "a1")
	// hub-b restarts: a new process of it links again.
	b.stop()
	waitDown(t, a, "hub-b")
	b2 := startHub(t, "hub-b", testDevices("b"), withPeers(a.addr))
	waitLinked(t, a, "hub-b")
	expect(b2, "b2")
	// hub-c links after the stream opened.
	c := startHub(t, "hub-c", testDevices("c"), withPeers(a.addr))
	waitLinked(t, a, "hub-c")
	expect(c, "c1")
}

// A device's event stream runs on a bulk link, which is never pinged: when
// the peer goes silent without closing (power loss), the control link's
// missed pongs close it, and its bulk links with it, so the client's
// stream ends instead of staying open and silent.
func TestMesh_BulkLinksCloseWithTheControlLink(t *testing.T) {
	a, b := twoHubs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+b.addr+"/v1/events?device=a1@hub-a", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ended := make(chan struct{})
	first := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(resp.Body)
		once := sync.Once{}
		for sc.Scan() {
			once.Do(func() { close(first) })
		}
		close(ended)
	}()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("the device stream never started")
	}
	if b.logged("bulk link to hub-a for GET /v1/events") == 0 {
		t.Fatal("the stream is not on a bulk link")
	}
	// hub-a stops answering on its control link, as if its host lost
	// power; its bulk link stays open but silent.
	la := waitLinked(t, a, "hub-b")
	la.mu.Lock()
	lb := waitLinked(t, b, "hub-a")
	if err := lb.writeData("stuck", []byte("x")); err != nil {
		la.mu.Unlock()
		t.Fatal(err)
	}
	select {
	case <-lb.closed:
	case <-time.After(5 * time.Second):
		la.mu.Unlock()
		t.Fatal("missed pongs did not close the control link")
	}
	la.mu.Unlock()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the device stream outlived its peer's control link")
	}
}

// A hub that restarts at a new address while its old link still looks
// alive (no FIN; the pongs have not been missed yet) is refused
// HUB_NAME_TAKEN for that while only: it keeps dialing, and links once the
// old link closes. A proxy's answer without an envelope is retried too.
func TestMesh_RestartAtANewAddressRelinks(t *testing.T) {
	setupHome(t)
	a := startHub(t, "hub-a", testDevices("a"))
	b1 := startHub(t, "hub-b", testDevices("b"), withPeers(a.addr))
	old := waitLinked(t, a, "hub-b")
	// hub-b's old process goes silent: its read loop blocks at the next
	// data frame, so it answers no more pings and never closes.
	l1 := waitLinked(t, b1, "hub-a")
	l1.mu.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			l1.mu.Unlock()
		}
	}
	defer unlock()
	if err := old.writeData("stuck", []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Its new process, at another address, dials hub-a meanwhile.
	b2 := startHub(t, "hub-b2", testDevices("b"), withPeers(a.addr), func(c *Config, d *daemon.Config) {
		c.Name, d.HubName = "hub-b", "hub-b"
	})
	deadline := time.Now().Add(10 * time.Second)
	for b2.logged("HUB_NAME_TAKEN") == 0 && a.logged("refused a link from hub-b") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no refusal while the old link stood: %v", b2.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-old.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the old link never missed its pongs")
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		if l, e := a.mesh.link("hub-b"); e == nil && l != old && l.instance == b2.mesh.instance {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted hub never linked: %v", b2.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b2.logged("not dialing it again") != 0 {
		t.Fatalf("a taken name was treated as permanent: %v", b2.logs)
	}
	unlock()
	b1.stop()

	// Answers that carry no hub envelope are not refusals.
	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusBadGateway} {
		resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("<html>nope</html>"))}
		var de *daemon.Error
		if err := refusalFrom(resp); err == nil || errors.As(err, &de) {
			t.Fatalf("HTTP %d: %v", status, err)
		}
	}
	if !isRefusal(daemon.Errorf(daemon.CodeHubNameTaken, "own name")) ||
		isRefusal(daemon.Errorf(daemon.CodeHubNameTaken, "held").WithDetails("addr", "x:1")) ||
		!isRefusal(daemon.Errorf(daemon.CodeHubVersion, "v")) || !isRefusal(daemon.Errorf(daemon.CodeUsage, "token")) {
		t.Fatal("isRefusal")
	}
}

// GET /v1/events?device=<bare id> follows the one mesh device that has the
// id, as the device routes do: a peer's device's events reach a client of
// this hub.
func TestMesh_EventsByBareIDRouteToTheOwner(t *testing.T) {
	a, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("a1", lr.Lease.ID)
	evCtx, stop := context.WithCancel(ctx)
	defer stop()
	got := make(chan daemon.Event, 16)
	go func() {
		_ = c.Events(evCtx, "a1", 0, func(ev daemon.Event) bool {
			got <- ev
			return true
		})
	}()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case ev := <-got:
			if ev.Device != "a1@hub-a" {
				t.Fatalf("event device %q", ev.Device)
			}
			if ev.Type == "step" {
				if n := a.count("GET /v1/events"); n != 1 {
					t.Fatalf("hub-a served %d event streams", n)
				}
				return
			}
		case <-tick.C:
			// Steps until the subscription is up and one is seen.
			if _, err := c.Command(ctx, "a1", "tapOn", map[string]any{"text": "Ev"}, true); err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("no step event for a peer's device followed by bare id")
		}
	}
}

// One shared config, `--peer seed`, on every machine: on the seed itself
// that --peer is its own address. The seed drops it and starts READY
// rather than failing HUB_NAME_TAKEN, and still links the hubs that dial
// it.
func TestMesh_PeerThatIsThisHubItself(t *testing.T) {
	setupHome(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	seed := startHub(t, "hub-seed", testDevices("s"), withPeers(addr), func(_ *Config, d *daemon.Config) { d.HTTPAddr = addr })
	deadline := time.Now().Add(5 * time.Second)
	for seed.logged("is this hub itself") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if seed.logged("is this hub itself") != 1 || seed.logged("refused") != 0 {
		t.Fatalf("logs: %v", seed.logs)
	}
	if st := seed.mesh.Status(context.Background()); len(st.Peers) != 0 {
		t.Fatalf("its own address is listed: %+v", st.Peers)
	}
	b := startHub(t, "hub-b", testDevices("b"), withPeers(addr))
	waitLinked(t, seed, "hub-b")
	waitLinked(t, b, "hub-seed")
	// Another process with the seed's name is still refused.
	x := New(fastConfig("hub-seed"))
	x.cfg.Peers = []string{addr}
	if err := x.Start(context.Background(), daemon.Info{HTTPAddr: "127.0.0.1:1"}); !isCode(err, daemon.CodeHubNameTaken) {
		t.Fatalf("a second hub-seed: %v", err)
	}
	x.Close()
}

// A peer that links while a merged event stream's loop is between reading
// the links and waiting for a change still wakes that loop: its events
// reach the stream at once, not after the hour-long retry timer.
func TestMesh_MergedEventsPeerLinkedWhileReadingLinks(t *testing.T) {
	a := startHub(t, "hub-a", testDevices("a"))
	inWindow, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook := func() {
		once.Do(func() {
			close(inWindow)
			<-proceed
		})
	}
	a.mesh.afterLinked.Store(&hook)
	t.Cleanup(func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.addr+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	select {
	case <-inWindow:
	case <-time.After(10 * time.Second):
		t.Fatal("the merged stream never read its links")
	}
	// hub-c links in the window, then the loop goes on to wait.
	c := startHub(t, "hub-c", testDevices("c"), withPeers(a.addr))
	waitLinked(t, a, "hub-c")
	close(proceed)

	cl := c.client()
	want := `"device":"c1@hub-c"`
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if res, err := cl.AcquireLease(context.Background(), daemon.LeaseRequest{Device: "c1"}); err == nil {
				_, _ = cl.ReleaseLease(context.Background(), res.Lease.ID)
			}
		case ln, ok := <-lines:
			if !ok {
				t.Fatal("the merged stream ended")
			}
			if strings.Contains(strings.ReplaceAll(ln, " ", ""), want) {
				return
			}
		case <-deadline:
			t.Fatal("no event from hub-c, which linked while the stream read its links")
		}
	}
}

// A lease or boot body over the router's limit is USAGE "request body too
// large", not cut short and routed (or decoded) as something else.
func TestMesh_RouterBodyTooLarge(t *testing.T) {
	a := startHub(t, "hub-a", testDevices("a"))
	big := append([]byte(`{"device":"a1","holder":"`), bytes.Repeat([]byte("x"), maxBody)...)
	big = append(big, `"}`...)
	for _, path := range []string{"/v1/leases", "/v1/devices"} {
		code, body := a.do(t, http.MethodPost, path, big)
		if code != http.StatusBadRequest || codeIn(body) != daemon.CodeUsage || !strings.Contains(string(body), "request body too large") {
			t.Fatalf("POST %s with %d bytes: %d %.200s", path, len(big), code, body)
		}
	}
	// At the limit the body is read whole.
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345"))
	if data, e := readBodyMax(r, 5); e != nil || string(data) != "12345" {
		t.Fatalf("at the limit: %q %v", data, e)
	}
	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("123456"))
	if _, e := readBodyMax(r, 5); e == nil || e.Code != daemon.CodeUsage {
		t.Fatalf("over the limit: %v", e)
	}
}

// A client's upload through a peer hub stalls under a lease that ends:
// the owner answers LEASE_EXPIRED, and the relaying hub passes it on
// while the client's body is still unread (full duplex). Neither hub's
// handler keeps reading after it returned, so the owner frees the device.
func TestMesh_StalledUploadThroughAPeerAnsweredWhenTheLeaseEnds(t *testing.T) {
	a, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a"})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", b.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hx := strings.Repeat("a", 64)
	sent := maxInlineBody + 64<<10
	fmt.Fprintf(conn, "PUT /v1/devices/a1@hub-a/blobs/%s HTTP/1.1\r\nHost: hub\r\nAuthorization: Bearer %s\r\n%s: %s\r\nContent-Length: %d\r\n\r\n",
		hx, testToken, daemon.HeaderLease, lr.Lease.ID, sent+(1<<20))
	if _, err := conn.Write(make([]byte, sent)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.count("PUT /v1/devices/a1/blobs/"+hx) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the upload never reached the owner")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.ReleaseLease(ctx, lr.Lease.ID); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer through the peer while the upload stalls: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if codeIn(data) != daemon.CodeLeaseExpired {
		t.Fatalf("answer %s: %s", resp.Status, data)
	}
	// With the client still connected and silent, the owner's request
	// unwound and its lease's teardown freed the device.
	deadline = time.Now().Add(5 * time.Second)
	for {
		l, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a"})
		if err == nil {
			_, _ = c.ReleaseLease(ctx, l.Lease.ID)
			break
		}
		if !isCode(err, daemon.CodeDeviceInUse) || time.Now().After(deadline) {
			t.Fatalf("the device was not freed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMesh_SelectorSkipsAStoppingHub: a hub that is stopping stays linked
// while it ends its leases and answers DAEMON_UNAVAILABLE; a selector lease
// through a peer passes over it like an unreachable hub instead of failing.
func TestMesh_SelectorSkipsAStoppingHub(t *testing.T) {
	a, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	for _, id := range []string{"b1@hub-b", "b2@hub-b"} {
		if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: id}); err != nil {
			t.Fatalf("lease %s: %v", id, err)
		}
	}
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/leases" {
			writeError(w, 0, daemon.Errorf(daemon.CodeDaemonUnavailable, "daemon is shutting down"))
			return true
		}
		return false
	})
	_, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock"}})
	if !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("selector with a stopping hub: %v (want DEVICE_IN_USE from hub-b, not hub-a's DAEMON_UNAVAILABLE)", err)
	}
	// Pinned to the stopping hub, the answer is a retryable
	// PEER_UNAVAILABLE naming it, not "matches nothing".
	_, err = c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock", Hub: "hub-a"}})
	if !isCode(err, daemon.CodePeerUnavailable) || !strings.Contains(err.Error(), "hub-a") {
		t.Fatalf("selector pinned to the stopping hub: %v", err)
	}
	a.setHook(func(http.ResponseWriter, *http.Request) bool { return false })
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock"}})
	if err != nil || !strings.HasSuffix(lr.Lease.Device, "@hub-a") {
		t.Fatalf("after the hub is back: %+v %v", lr, err)
	}
}
