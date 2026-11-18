package cdp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

func networkBrowserServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/probe" {
			fmt.Fprint(w, "observed response")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><title>Network fixture</title><p>Ready</p>`)
	}))
}

// Read the raw event stream: EachEvent would itself enable Network and hide
// precisely the idle-domain regression this probe is intended to detect.
func assertNetworkCapture(t *testing.T, d *Driver, page *rod.Page, url string, want bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(page.GetContext(), 5*time.Second)
	defer cancel()
	messages := d.browser.Context(ctx).Event()
	barrier := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		count := 0
		for msg := range messages {
			if msg.SessionID != page.SessionID {
				continue
			}
			request := proto.NetworkRequestWillBeSent{}
			if msg.Load(&request) && request.Request.URL == url+"/probe" {
				count++
			}
			entry := proto.RuntimeConsoleAPICalled{}
			if msg.Load(&entry) && len(entry.Args) > 0 && entry.Args[0].Value.Str() == "network-probe-complete" {
				barrier <- count
				return
			}
		}
	}()
	defer func() { cancel(); <-done }()
	result, err := page.Timeout(5*time.Second).Eval(`async url => {
  const body=await (await fetch(url+'/probe',{cache:'no-store'})).text();
  console.log('network-probe-complete');return body;
 }`, url)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value.Str() != "observed response" {
		t.Fatal("fixture response was not delivered")
	}
	select {
	case count := <-barrier:
		if (count > 0) != want {
			t.Fatalf("Network events = %d, capture wanted %t", count, want)
		}
	case <-ctx.Done():
		t.Fatal("console barrier was not delivered")
	}
}
func TestBrowserNetworkScopesAndTabs(t *testing.T) {
	server := networkBrowserServer()
	defer server.Close()
	d := newTestDriver(t, server.URL)
	defer d.Close()
	initial := d.page
	d.tabLabels["initial"] = initial
	assertNetworkCapture(t, d, initial, server.URL, false)
	first := observeForTest(t, d, initial)
	second := observeForTest(t, d, initial)
	assertNetworkCapture(t, d, initial, server.URL, true)
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	assertNetworkCapture(t, d, initial, server.URL, true)
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	assertNetworkCapture(t, d, initial, server.URL, false)
	if r := d.openTab(&flow.OpenTabStep{URL: server.URL, TabLabel: "other"}); !r.Success {
		t.Fatal(r.Error)
	}
	// Runtime is needed only for the console barrier on this new page.
	if err := (proto.RuntimeEnable{}).Call(d.page); err != nil {
		t.Fatal(err)
	}
	assertNetworkCapture(t, d, d.page, server.URL, false)
	if r := d.switchTab(&flow.SwitchTabStep{TabLabel: "initial"}); !r.Success {
		t.Fatal(r.Error)
	}
	if r := d.openBrowser(&flow.OpenBrowserStep{URL: server.URL}); !r.Success {
		t.Fatal(r.Error)
	}
	assertNetworkCapture(t, d, initial, server.URL, false)
}
func TestBrowserNetworkBlankConstruction(t *testing.T) {
	server := networkBrowserServer()
	defer server.Close()
	d := newTestDriver(t, "")
	defer d.Close()
	if d.page.LoadState(&proto.NetworkEnable{}) {
		t.Fatal("blank construction enabled Network")
	}
	if r := d.openLink(&flow.OpenLinkStep{Link: server.URL}); !r.Success {
		t.Fatal(r.Error)
	}
	assertNetworkCapture(t, d, d.page, server.URL, false)
}
func TestBrowserNetworkNavigationWaitsForFetch(t *testing.T) {
	for _, command := range []string{"initial", "launch", "link", "browser"} {
		t.Run(command, func(t *testing.T) {
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				if r.URL.Path == "/delayed" {
					started <- struct{}{}
					<-release
					fmt.Fprint(w, "ready")
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<!doctype html><p id="ready">waiting</p><script>fetch('/delayed').then(r=>r.text()).then(text=>document.querySelector('#ready').textContent=text)</script>`)
			}))
			defer server.Close()
			var d *Driver
			done := make(chan error, 1)
			if command == "initial" {
				go func() { var err error; d, err = New(Config{Headless: true, URL: server.URL}); done <- err }()
			} else {
				d = newTestDriver(t, "")
				go func() {
					var r *core.CommandResult
					switch command {
					case "launch":
						r = d.launchApp(&flow.LaunchAppStep{AppID: server.URL})
					case "link":
						r = d.openLink(&flow.OpenLinkStep{Link: server.URL})
					default:
						r = d.openBrowser(&flow.OpenBrowserStep{URL: server.URL})
					}
					done <- r.Error
				}()
			}
			select {
			case <-started:
			case <-time.After(15 * time.Second):
				close(release)
				t.Fatal("navigation did not start fetch")
			}
			early := false
			select {
			case <-done:
				early = true
			case <-time.After(800 * time.Millisecond):
			}
			close(release)
			if !early {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("navigation did not settle")
				}
			}
			if d != nil {
				defer d.Close()
			}
			if early {
				t.Fatal("navigation returned while its asynchronous fetch was pending")
			}
			got, err := d.page.Eval(`() => document.querySelector('#ready').textContent`)
			if err != nil || got.Value.Str() != "ready" {
				t.Fatalf("navigation readiness = %v, %v", got, err)
			}
		})
	}
}
func clearConnectionEvents(t *testing.T, page *rod.Page) {
	t.Helper()
	_, err := page.Eval(`async () => {
  if (!window.networkEvents) {
   window.networkEvents=[];
   for (const name of ['online','offline']) window.addEventListener(name,()=>networkEvents.push(name));
   navigator.connection?.addEventListener('change',()=>networkEvents.push('connection'));
  }
  await new Promise(r=>setTimeout(r,50));networkEvents=[];
 }`)
	if err != nil {
		t.Fatal(err)
	}
}
func assertConnectionUnchanged(t *testing.T, page *rod.Page, online bool) {
	t.Helper()
	result, err := page.Eval(`async () => {await new Promise(r=>setTimeout(r,50));return {online:navigator.onLine,events:networkEvents};}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value.Get("online").Bool() != online || len(result.Value.Get("events").Arr()) != 0 {
		t.Fatalf("observer changed connectivity: %s", result.Value.JSON("", ""))
	}
}
func TestBrowserNetworkEmulationSurvivesWaitCleanup(t *testing.T) {
	server := networkBrowserServer()
	defer server.Close()
	d := newTestDriver(t, server.URL)
	defer d.Close()
	if r := d.setNetworkConditions(&flow.SetNetworkConditionsStep{Offline: true}); !r.Success {
		t.Fatal(r.Error)
	}
	clearConnectionEvents(t, d.page)
	r := d.waitForRequest(&flow.WaitForRequestStep{BaseStep: flow.BaseStep{TimeoutMs: 50}, URL: "*/missing"})
	if r.Success {
		t.Fatal("missing request unexpectedly matched")
	}
	assertConnectionUnchanged(t, d.page, false)
	offline, err := d.page.Eval(`async url=>{try {await fetch(url+'/probe');return false} catch {return true}}`, server.URL)
	if err != nil || !offline.Value.Bool() {
		t.Fatalf("offline request unexpectedly succeeded: %v, %v", offline, err)
	}

	// A matching POST still completes its waiter in offline mode, without a
	// cleanup-generated reconnect notification.
	done := make(chan *core.CommandResult, 1)
	go func() {
		done <- d.waitForRequest(&flow.WaitForRequestStep{BaseStep: flow.BaseStep{TimeoutMs: 2000}, URL: "*/probe", Method: "POST"})
	}()
	for {
		_, err := d.page.Eval(`async url=>{try {await fetch(url+'/probe',{method:'POST',body:'payload'})} catch {}}`, server.URL)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-done:
			if !r.Success || r.Data != "payload" {
				t.Fatalf("POST match: %#v", r)
			}
			goto matched
		default:
		}
	}
matched:
	assertConnectionUnchanged(t, d.page, false)
	active := observeForTest(t, d, d.page)
	if r := d.setNetworkConditions(&flow.SetNetworkConditionsStep{Latency: 100, DownloadSpeed: 128, UploadSpeed: 64}); !r.Success {
		t.Fatal(r.Error)
	}
	clearConnectionEvents(t, d.page)
	before, err := d.page.Eval(`()=>({rtt:navigator.connection.rtt,downlink:navigator.connection.downlink})`)
	if err != nil {
		t.Fatal(err)
	}
	if err := active.close(); err != nil {
		t.Fatal(err)
	}
	assertConnectionUnchanged(t, d.page, true)
	after, err := d.page.Eval(`()=>({rtt:navigator.connection.rtt,downlink:navigator.connection.downlink})`)
	if err != nil || before.Value.JSON("", "") != after.Value.JSON("", "") {
		t.Fatalf("throttle changed: %v -> %v (%v)", before, after, err)
	}
	if r := d.openBrowser(&flow.OpenBrowserStep{URL: server.URL}); !r.Success {
		t.Fatal(r.Error)
	}
	after, err = d.page.Eval(`()=>({rtt:navigator.connection.rtt,downlink:navigator.connection.downlink})`)
	if err != nil || before.Value.JSON("", "") != after.Value.JSON("", "") {
		t.Fatalf("navigation reset throttle: %v -> %v (%v)", before, after, err)
	}
	clearConnectionEvents(t, d.page)
	timed := d.waitForRequest(&flow.WaitForRequestStep{BaseStep: flow.BaseStep{TimeoutMs: 50}, URL: "*/missing"})
	if timed.Success {
		t.Fatal("missing throttled request matched")
	}
	assertConnectionUnchanged(t, d.page, true)
	if r := d.clearNetworkMocks(); !r.Success {
		t.Fatal(r.Error)
	}
	assertNetworkCapture(t, d, d.page, server.URL, false)
}
func TestBrowserNetworkEmulatedTabClosureReleasesScopes(t *testing.T) {
	server := networkBrowserServer()
	defer server.Close()
	d := newTestDriver(t, server.URL)
	defer d.Close()
	original := d.page
	o := observeForTest(t, d, original)
	if r := d.openTab(&flow.OpenTabStep{URL: server.URL}); !r.Success {
		t.Fatal(r.Error)
	}
	if r := d.setNetworkConditions(&flow.SetNetworkConditionsStep{Offline: true}); !r.Success {
		t.Fatal(r.Error)
	}
	if err := o.close(); err != nil {
		t.Fatal(err)
	}
	if !original.LoadState(&proto.NetworkEnable{}) {
		t.Fatal("scope released while another tab was emulated")
	}
	if r := d.closeTab(); !r.Success {
		t.Fatal(r.Error)
	}
	awaitNetwork(t, func() bool { return !original.LoadState(&proto.NetworkEnable{}) })
	assertNetworkCapture(t, d, original, server.URL, false)
}
func TestBrowserNetworkMocksSurviveNavigationCleanup(t *testing.T) {
	server := networkBrowserServer()
	defer server.Close()
	d := newTestDriver(t, server.URL)
	defer d.Close()
	if r := d.mockNetwork(&flow.MockNetworkStep{URL: "*/mock", Response: flow.MockResponseSpec{Status: 200, Body: "mock body"}}); !r.Success {
		t.Fatal(r.Error)
	}
	if r := d.blockNetwork(&flow.BlockNetworkStep{Patterns: []string{"*/blocked"}}); !r.Success {
		t.Fatal(r.Error)
	}
	if r := d.openBrowser(&flow.OpenBrowserStep{URL: server.URL}); !r.Success {
		t.Fatal(r.Error)
	}
	result, err := d.page.Eval(`async()=>{const body=await(await fetch('/mock')).text();let blocked=false;try{await fetch('/blocked')}catch{blocked=true};return {body,blocked}}`)
	if err != nil || result.Value.Get("body").Str() != "mock body" || !result.Value.Get("blocked").Bool() {
		t.Fatalf("network rules changed: %v, %v", result, err)
	}
	if r := d.clearNetworkMocks(); !r.Success {
		t.Fatal(r.Error)
	}
	result, err = d.page.Eval(`async()=>await(await fetch('/mock')).text()`)
	if err != nil || strings.Contains(result.Value.Str(), "mock body") {
		t.Fatal("mock was not cleared")
	}
	assertNetworkCapture(t, d, d.page, server.URL, false)
}
