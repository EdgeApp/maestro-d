package hubtest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "0": 0, "20mbit": 20e6, "1.5kbit": 1500, "1gbit": 1e9, "64000": 64000, "8bit": 8} {
		got, err := ParseRate(in)
		if err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"fast", "-1mbit"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestProxy_DelaysAndDrops(t *testing.T) {
	var served atomic.Int32
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	defer hs.Close()
	p, err := Start(ProxyConfig{Listen: "127.0.0.1:0", Target: strings.TrimPrefix(hs.URL, "http://"), RTT: 100 * time.Millisecond,
		Rate: 10e6, DropAfterRequest: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	client := &http.Client{Transport: &http.Transport{}}
	url := "http://" + p.Addr() + "/x"

	start := time.Now()
	resp, err := client.Post(url, "text/plain", strings.NewReader("one"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("first request: %q after %v", body, time.Since(start))
	}
	// The second request reaches the server, but its connection is dropped
	// before the answer.
	if _, err := client.Post(url, "text/plain", strings.NewReader("two")); err == nil {
		t.Fatal("the second request should lose its connection")
	}
	time.Sleep(200 * time.Millisecond)
	if served.Load() != 2 || p.Requests() != 2 || p.Drops() != 1 {
		t.Fatalf("served %d, requests %d, drops %d", served.Load(), p.Requests(), p.Drops())
	}
	// A plain proxy (no drops) just forwards.
	plain, err := Start(ProxyConfig{Listen: "127.0.0.1:0", Target: strings.TrimPrefix(hs.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	resp, err = http.Get("http://" + plain.Addr() + "/y")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
