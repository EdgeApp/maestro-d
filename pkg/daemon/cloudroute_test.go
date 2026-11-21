package daemon

import "testing"

func TestRouteCloud(t *testing.T) {
	peers := []PeerCloud{
		{Hub: "zeta", Providers: []string{"devicefarm"}},
		{Hub: "beta", Providers: []string{"devicefarm"}},
		{Hub: "alpha"},
	}
	for _, c := range []struct {
		named, provider string
		self            []string
		peers           []PeerCloud
		want            string
	}{
		{named: "zeta", want: "zeta"}, // the hub the client names
		{self: []string{"devicefarm"}, peers: peers, provider: "devicefarm", want: "self"},            // this hub has it
		{peers: peers, provider: "devicefarm", want: "beta"},                                          // first peer by name
		{peers: peers, provider: "", want: "beta"},                                                    // any provider
		{self: []string{"devicefarm"}, peers: peers, provider: "browserstack", want: ""},              // nobody has it
		{peers: []PeerCloud{{Hub: "a", Providers: []string{"browserstack"}}}, provider: "devicefarm"}, // wrong provider
	} {
		got, e := RouteCloud(c.named, "self", c.self, c.peers, c.provider)
		if c.want == "" {
			if e == nil || e.Code != CodeUsage {
				t.Errorf("%+v: want USAGE, got %q %v", c, got, e)
			}
			continue
		}
		if e != nil || got != c.want {
			t.Errorf("%+v: got %q %v, want %q", c, got, e, c.want)
		}
	}
	// The peers' order on input does not matter.
	if peers[0].Hub != "zeta" {
		t.Fatal("RouteCloud reordered its input")
	}
	_, e := RouteCloud("", "self", nil, nil, "devicefarm")
	if e == nil || e.Message != "no hub in this mesh has Device Farm credentials" {
		t.Fatalf("message: %v", e)
	}
	_, e = RouteCloud("", "self", nil, nil, "browserstack")
	if e == nil || e.Message != "no hub in this mesh has browserstack credentials" {
		t.Fatalf("other provider message: %v", e)
	}
}
