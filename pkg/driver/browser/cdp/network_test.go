package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

type networkClientFunc func(context.Context, string, string, interface{}) ([]byte, error)

func (f networkClientFunc) Call(ctx context.Context, session, method string, params interface{}) ([]byte, error) {
	return f(ctx, session, method, params)
}

func TestEnableNetworkEventsExplicitZeroLimits(t *testing.T) {
	calls := 0
	client := networkClientFunc(func(_ context.Context, _, method string, params interface{}) ([]byte, error) {
		calls++
		if method != "Network.enable" {
			t.Fatalf("method = %q", method)
		}
		wire, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]int
		if err := json.Unmarshal(wire, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != 2 {
			t.Fatalf("unexpected Network.enable parameters: %s", wire)
		}
		for _, name := range []string{"maxTotalBufferSize", "maxResourceBufferSize"} {
			if value, present := fields[name]; !present || value != 0 {
				t.Errorf("%s must be explicitly zero: %s", name, wire)
			}
		}
		return nil, nil
	})
	if err := enableNetworkEvents(client); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("Network.enable calls = %d", calls)
	}
}

func TestEnableNetworkEventsReturnsProtocolError(t *testing.T) {
	want := errors.New("connection closed")
	client := networkClientFunc(func(context.Context, string, string, interface{}) ([]byte, error) {
		return nil, want
	})
	if err := enableNetworkEvents(client); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// Response storage stays disabled even while a command consumes network events.
func TestNetworkEventsEvictResponseBodies(t *testing.T) {
	body := strings.Repeat("downloaded test response\n", 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><title>Network events</title>")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	d := newTestDriver(t, server.URL)
	defer d.Close()
	ctx, cancel := context.WithTimeout(d.page.GetContext(), 5*time.Second)
	defer cancel()
	var requestID proto.NetworkRequestID
	completed := make(chan proto.NetworkRequestID, 1)
	payload := make(chan string, 1)
	url := server.URL + "/observed"
	observation, err := d.observeNetwork(d.page, ctx,
		func(e *proto.NetworkRequestWillBeSent) {
			if e.Request.URL == url {
				payload <- e.Request.PostData
			}
		},
		func(e *proto.NetworkResponseReceived) {
			if e.Response.URL == url {
				requestID = e.RequestID
			}
		},
		func(e *proto.NetworkLoadingFinished) bool {
			if e.RequestID != requestID {
				return false
			}
			completed <- e.RequestID
			return true
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer observation.close()
	result, err := d.page.Timeout(5*time.Second).Eval(`async url => {
  const response=await fetch(url,{method:'POST',body:'request payload'});
  return await response.text();
 }`, url)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value.Str() != body {
		t.Fatal("the page did not receive the complete response")
	}
	select {
	case id := <-completed:
		_, err := (proto.NetworkGetResponseBody{RequestID: id}).Call(d.page)
		if err == nil || !strings.Contains(err.Error(), "evicted") {
			t.Fatalf("downloaded response must be evicted, got %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Network.loadingFinished was not delivered")
	}
	if got := <-payload; got != "request payload" {
		t.Fatalf("POST data = %q", got)
	}
	if err := observation.close(); err != nil {
		t.Fatal(err)
	}
}
