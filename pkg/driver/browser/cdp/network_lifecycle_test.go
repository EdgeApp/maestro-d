package cdp

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/go-rod/rod"
	rodcdp "github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

type networkCall struct{ session, method string }
type networkTransport struct {
	mu       sync.Mutex
	events   chan *rodcdp.Event
	calls    []networkCall
	failures map[string]error
	sessions int
	hook     func(string)
}

func (f *networkTransport) Event() <-chan *rodcdp.Event { return f.events }
func (f *networkTransport) Call(ctx context.Context, session, method string, params interface{}) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(method)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, networkCall{session, method})
	if err := f.failures[method]; err != nil {
		return nil, err
	}
	switch method {
	case "Target.createTarget":
		return []byte(`{"targetId":"fixture-page"}`), nil
	case "Target.attachToTarget":
		f.sessions++
		return []byte(fmt.Sprintf(`{"sessionId":"session-%d"}`, f.sessions)), nil
	case "Runtime.evaluate", "Runtime.callFunctionOn":
		return []byte(`{"result":{"type":"object","objectId":"fixture-object","value":true}}`), nil
	case "DOMSnapshot.captureSnapshot":
		return []byte(`{"documents":[],"strings":["document"]}`), nil
	default:
		return []byte(`{}`), nil
	}
}
func (f *networkTransport) fail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = err
}
func (f *networkTransport) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}
func (f *networkTransport) emit(t *testing.T, page *rod.Page, event proto.Event) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case f.events <- &rodcdp.Event{SessionID: string(page.SessionID), Method: event.ProtoEvent(), Params: data}:
	case <-time.After(2 * time.Second):
		t.Fatal("event transport blocked")
	}
}
func fakeNetworkDriver(t *testing.T) (*Driver, *networkTransport) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &networkTransport{events: make(chan *rodcdp.Event, 64), failures: make(map[string]error)}
	b := rod.New().Context(ctx).NoDefaultDevice().Client(f)
	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}
	p, err := b.PageFromTarget("fixture-page")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); close(f.events) })
	return &Driver{browser: b, page: p, stopCh: make(chan struct{}), tabLabels: make(map[string]*rod.Page)}, f
}
func awaitNetwork(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("network lifecycle condition did not complete")
}
func observeForTest(t *testing.T, d *Driver, page *rod.Page) *networkObservation {
	t.Helper()
	o, err := d.observeNetwork(page, page.GetContext(), func(*proto.NetworkRequestWillBeSent) {})
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func TestNetworkObservationSharedListeners(t *testing.T) {
	d, f := fakeNetworkDriver(t)
	first := make(chan struct{}, 1)
	second := make(chan struct{}, 2)
	a, err := d.observeNetwork(d.page, d.page.GetContext(), func(*proto.NetworkRequestWillBeSent) { first <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.observeNetwork(d.page, d.page.GetContext(), func(*proto.NetworkRequestWillBeSent) { second <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	f.emit(t, d.page, &proto.NetworkRequestWillBeSent{})
	awaitNetwork(t, func() bool { return len(first) == 1 && len(second) == 1 })
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.done:
	default:
		t.Fatal("closed observation still has a listener")
	}
	if f.count("Network.enable") != 1 || f.count("Network.disable") != 0 {
		t.Fatal("overlapping observer lost its domain")
	}
	f.emit(t, d.page, &proto.NetworkRequestWillBeSent{})
	awaitNetwork(t, func() bool { return len(second) == 2 })
	if len(first) != 1 {
		t.Fatal("canceled listener received another event")
	}
	if err := b.close(); err != nil {
		t.Fatal(err)
	}
	if f.count("Network.disable") != 1 {
		t.Fatal("last consumer did not disable Network")
	}
	if d.page.LoadState(&proto.NetworkEnable{}) {
		t.Fatal("Network remains enabled")
	}
}
func TestNetworkObservationUncertainResponses(t *testing.T) {
	t.Run("enable cleanup retains ownership on failure", func(t *testing.T) {
		d, f := fakeNetworkDriver(t)
		cleanupErr := errors.New("cleanup unavailable")
		f.fail("Network.enable", context.DeadlineExceeded)
		f.fail("Network.disable", cleanupErr)
		_, err := d.observeNetwork(d.page, d.page.GetContext(), func(*proto.NetworkRequestWillBeSent) {})
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cleanupErr) {
			t.Fatalf("uncertain enable and cleanup errors = %v", err)
		}
		if f.count("Network.disable") != 1 || len(d.networkScopes) != 1 {
			t.Fatal("uncertain enable lost cleanup ownership")
		}
		f.fail("Network.enable", nil)
		f.fail("Network.disable", nil)
		retry := observeForTest(t, d, d.page)
		if err := retry.close(); err != nil {
			t.Fatal(err)
		}
		if f.count("Network.enable") != 2 || f.count("Network.disable") != 2 || len(d.networkScopes) != 0 {
			t.Fatal("recovered observation did not re-establish and release its domain")
		}
	})
	t.Run("enable cleanup preserves emulation until reset", func(t *testing.T) {
		d, f := fakeNetworkDriver(t)
		if result := d.setNetworkConditions(&flow.SetNetworkConditionsStep{Offline: true}); !result.Success {
			t.Fatal(result.Error)
		}
		f.fail("Network.enable", context.DeadlineExceeded)
		_, err := d.observeNetwork(d.page, d.page.GetContext(), func(*proto.NetworkRequestWillBeSent) {})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if f.count("Network.disable") != 0 {
			t.Fatal("failed enable cleanup changed active emulation")
		}
		if result := d.setNetworkConditions(&flow.SetNetworkConditionsStep{}); !result.Success {
			t.Fatal(result.Error)
		}
		if f.count("Network.disable") != 1 || len(d.networkScopes) != 0 {
			t.Fatal("reset did not release the uncertain enable")
		}
	})
	t.Run("disable timeout requires a confirmed enable", func(t *testing.T) {
		d, f := fakeNetworkDriver(t)
		o := observeForTest(t, d, d.page)
		f.fail("Network.disable", context.DeadlineExceeded)
		if err := o.close(); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		// Chrome may have disabled the domain before its response timed out.
		// Rod's cached state still says enabled, so it cannot confirm recovery.
		f.fail("Network.disable", nil)
		retryErr := errors.New("enable unavailable")
		f.fail("Network.enable", retryErr)
		retry, err := d.observeNetwork(d.page, d.page.GetContext(), func(*proto.NetworkRequestWillBeSent) {})
		if !errors.Is(err, retryErr) {
			if retry != nil {
				_ = retry.close()
			}
			t.Fatalf("reacquisition trusted uncertain cached state: %v", err)
		}
		f.fail("Network.enable", nil)
		retry = observeForTest(t, d, d.page)
		if err := retry.close(); err != nil {
			t.Fatal(err)
		}
		if f.count("Network.enable") != 3 || f.count("Network.disable") != 3 || len(d.networkScopes) != 0 {
			t.Fatal("recovered observation did not re-establish and release its domain")
		}
	})
}

func TestNetworkObservationProtocolFailures(t *testing.T) {
	d, f := fakeNetworkDriver(t)
	want := errors.New("protocol unavailable")
	f.fail("Network.enable", want)
	if _, err := d.observeNetwork(d.page, d.page.GetContext(), func(*proto.NetworkRequestWillBeSent) {}); !errors.Is(err, want) {
		t.Fatalf("enable error = %v", err)
	}
	f.fail("Network.enable", nil)
	o := observeForTest(t, d, d.page)
	f.fail("Network.disable", want)
	if err := o.close(); !errors.Is(err, want) {
		t.Fatalf("disable error = %v", err)
	}
	f.fail("Network.disable", nil)
	// A subsequent consumer re-establishes the domain, then retries cleanup.
	retry := observeForTest(t, d, d.page)
	if err := retry.close(); err != nil {
		t.Fatal(err)
	}
	// The failed initial enable also needed an independent cleanup attempt.
	if f.count("Network.disable") != 3 {
		t.Fatal("failed release was not retried")
	}
}
func TestNetworkObservationEmulationHoldAndReset(t *testing.T) {
	d, f := fakeNetworkDriver(t)
	o := observeForTest(t, d, d.page)
	for _, step := range []*flow.SetNetworkConditionsStep{{Offline: true}, {Latency: 100}, {DownloadSpeed: 10, UploadSpeed: 5}} {
		if result := d.setNetworkConditions(step); !result.Success {
			t.Fatal(result.Error)
		}
	}
	if err := o.close(); err != nil {
		t.Fatal(err)
	}
	if f.count("Network.disable") != 0 {
		t.Fatal("emulation was disabled by observation cleanup")
	}
	reused := observeForTest(t, d, d.page)
	if err := reused.close(); err != nil {
		t.Fatal(err)
	}
	if f.count("Network.enable") != 1 {
		t.Fatal("held scope was duplicated")
	}
	want := errors.New("reset failed")
	f.fail("Network.emulateNetworkConditions", want)
	if r := d.clearNetworkMocks(); r.Success || !errors.Is(r.Error, want) {
		t.Fatalf("failed reset = %#v", r)
	}
	if r := d.setNetworkConditions(&flow.SetNetworkConditionsStep{}); r.Success {
		t.Fatal("failed setting update succeeded")
	}
	if f.count("Network.disable") != 0 {
		t.Fatal("failed reset released emulation hold")
	}
	f.fail("Network.emulateNetworkConditions", nil)
	d.fetchEnabled = true
	if r := d.clearNetworkMocks(); !r.Success {
		t.Fatal(r.Error)
	}
	if f.count("Fetch.disable") != 1 || f.count("Network.disable") != 1 {
		t.Fatal("successful reset did not release interception and idle observation")
	}
	if r := d.setNetworkConditions(&flow.SetNetworkConditionsStep{}); !r.Success {
		t.Fatal(r.Error)
	}
}
func TestNetworkObservationResetKeepsActiveConsumer(t *testing.T) {
	d, f := fakeNetworkDriver(t)
	o := observeForTest(t, d, d.page)
	if r := d.setNetworkConditions(&flow.SetNetworkConditionsStep{Offline: true}); !r.Success {
		t.Fatal(r.Error)
	}
	if r := d.clearNetworkMocks(); !r.Success {
		t.Fatal(r.Error)
	}
	if f.count("Network.disable") != 0 {
		t.Fatal("reset disabled an active consumer")
	}
	if err := o.close(); err != nil {
		t.Fatal(err)
	}
	if f.count("Network.disable") != 1 {
		t.Fatal("consumer cleanup did not release reset scope")
	}
}
func TestNetworkObservationClosingEmulatedPageReleasesOtherScopes(t *testing.T) {
	d, f := fakeNetworkDriver(t)
	other, err := d.browser.PageFromTarget("other-page")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(other.GetContext())
	other = other.Context(ctx)
	a := observeForTest(t, d, d.page)
	b := observeForTest(t, d, other)
	d.networkMu.Lock()
	err = d.applyNetworkConditions(other, proto.NetworkEmulateNetworkConditions{Offline: true, DownloadThroughput: -1, UploadThroughput: -1})
	d.networkMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	if err := b.close(); err != nil {
		t.Fatal(err)
	}
	if f.count("Network.disable") != 0 {
		t.Fatal("another page's emulation was disturbed")
	}
	cancel()
	awaitNetwork(t, func() bool { return f.count("Network.disable") == 1 })
	d.networkMu.Lock()
	defer d.networkMu.Unlock()
	if len(d.networkScopes) != 0 || len(d.networkEmulation) != 0 {
		t.Fatal("page closure retained idle scopes or emulation")
	}
}
func TestNetworkObservationNavigationCommands(t *testing.T) {
	commands := map[string]func(*Driver) *core.CommandResult{
		"back": func(d *Driver) *core.CommandResult { return d.back(&flow.BackStep{}) },
		"launch": func(d *Driver) *core.CommandResult {
			return d.launchApp(&flow.LaunchAppStep{AppID: "https://example.test/"})
		},
		"link": func(d *Driver) *core.CommandResult {
			return d.openLink(&flow.OpenLinkStep{Link: "https://example.test/"})
		},
		"browser": func(d *Driver) *core.CommandResult {
			return d.openBrowser(&flow.OpenBrowserStep{URL: "https://example.test/"})
		},
	}
	for name, command := range commands {
		for _, failure := range []string{"", "Network.enable", "navigation"} {
			t.Run(name+"/"+failure, func(t *testing.T) {
				d, f := fakeNetworkDriver(t)
				method := failure
				if method == "navigation" {
					method = "Page.navigate"
					if name == "back" {
						method = "Runtime.callFunctionOn"
					}
				}
				if method != "" {
					f.fail(method, errors.New("expected protocol failure"))
				}
				r := command(d)
				if r.Success != (failure == "") {
					t.Fatalf("command result = %#v", r)
				}
				// Even an unconfirmed enable must release any domain Chrome
				// may have started before the protocol response failed.
				if f.count("Network.disable") != 1 {
					t.Fatal("navigation did not release its observer")
				}
			})
		}
	}
}
func TestNetworkObservationNavigationLifecycleEvents(t *testing.T) {
	d, f := fakeNetworkDriver(t)
	err := d.navigateAndWait(d.page, func() error {
		f.emit(t, d.page, &proto.NetworkRequestWillBeSent{RequestID: "empty"})
		f.emit(t, d.page, &proto.NetworkLoadingFailed{RequestID: "empty"})
		f.emit(t, d.page, &proto.NetworkRequestWillBeSent{RequestID: "request", Request: &proto.NetworkRequest{URL: "https://example.test/data"}})
		f.emit(t, d.page, &proto.NetworkLoadingFinished{RequestID: "request"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.count("Network.disable") != 1 {
		t.Fatal("navigation lifecycle did not clean up")
	}
}
func TestNetworkObservationWaitForRequest(t *testing.T) {
	for _, method := range []string{"", "post"} {
		t.Run(method, func(t *testing.T) {
			d, f := fakeNetworkDriver(t)
			done := make(chan *core.CommandResult, 1)
			go func() { done <- d.waitForRequest(&flow.WaitForRequestStep{URL: "*/match", Method: method}) }()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			for {
				select {
				case r := <-done:
					if !r.Success || r.Data != "payload" {
						t.Fatalf("match result = %#v", r)
					}
					if f.count("Network.disable") != 1 {
						t.Fatal("matched waiter retained observation")
					}
					return
				case <-ticker.C:
					f.emit(t, d.page, &proto.NetworkRequestWillBeSent{})
					f.emit(t, d.page, &proto.NetworkRequestWillBeSent{Request: &proto.NetworkRequest{URL: "https://example.test/other", Method: "POST"}})
					if method != "" {
						f.emit(t, d.page, &proto.NetworkRequestWillBeSent{Request: &proto.NetworkRequest{URL: "https://example.test/match", Method: "GET"}})
					}
					f.emit(t, d.page, &proto.NetworkRequestWillBeSent{Request: &proto.NetworkRequest{URL: "https://example.test/match", Method: "POST", PostData: "payload"}})
				case <-deadline.C:
					t.Fatal("request waiter did not complete")
				}
			}
		})
	}
}
func TestNetworkObservationWaitFailureAndCancellation(t *testing.T) {
	for _, failure := range []string{"timeout", "enable", "cleanup", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			d, f := fakeNetworkDriver(t)
			if failure == "enable" {
				f.fail("Network.enable", errors.New("enable failed"))
			}
			if failure == "cleanup" {
				f.fail("Network.disable", errors.New("disable failed"))
			}
			if failure == "canceled" {
				ctx, cancel := context.WithCancel(d.page.GetContext())
				d.page = d.page.Context(ctx)
				cancel()
			}
			result := d.waitForRequest(&flow.WaitForRequestStep{BaseStep: flow.BaseStep{TimeoutMs: 5}, URL: "*/missing"})
			if result.Success {
				t.Fatal("failed wait succeeded")
			}
			if failure == "timeout" && (!errors.Is(result.Error, context.DeadlineExceeded) || f.count("Network.disable") != 1) {
				t.Fatalf("timeout did not clean up: %#v", result)
			}
		})
	}
}

// Exercise constructor ownership through the real launcher/transport boundary,
// with a local protocol fixture instead of launching Chromium.
func TestNetworkObservationInitialNavigation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("launcher fixture uses a POSIX named pipe")
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("enableFailure=%t", fail), func(t *testing.T) {
			transport := &networkTransport{failures: make(map[string]error)}
			if fail {
				transport.fail("Network.enable", errors.New("initial observation failed"))
			}
			var endpoint string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/json/version" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"webSocketDebuggerUrl":%q}`, endpoint)
					return
				}
				// Rod uses a nonstandard handshake nonce accepted by Chrome.
				key := r.Header.Get("Sec-WebSocket-Key")
				digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				r.Header.Set("Sec-WebSocket-Key", "AAAAAAAAAAAAAAAAAAAAAA==")
				conn, err := websocket.Accept(networkHandshakeWriter{w, base64.StdEncoding.EncodeToString(digest[:])}, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				conn.SetReadLimit(4 << 20)
				for {
					_, wire, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					var request struct {
						ID        int             `json:"id"`
						SessionID string          `json:"sessionId"`
						Method    string          `json:"method"`
						Params    json.RawMessage `json:"params"`
					}
					if json.Unmarshal(wire, &request) != nil {
						return
					}
					result, callErr := transport.Call(r.Context(), request.SessionID, request.Method, request.Params)
					response := map[string]interface{}{"id": request.ID}
					if callErr != nil {
						response["error"] = map[string]interface{}{"code": -32000, "message": callErr.Error()}
					} else {
						response["result"] = json.RawMessage(result)
					}
					data, _ := json.Marshal(response)
					if conn.Write(r.Context(), websocket.MessageText, data) != nil || request.Method == "Browser.close" {
						return
					}
				}
			}))
			defer server.Close()
			endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/browser/fixture"
			dir := t.TempDir()
			pipe := filepath.Join(dir, "lifetime")
			pidFile := filepath.Join(dir, "process")
			if output, err := exec.Command("mkfifo", pipe).CombinedOutput(); err != nil {
				t.Fatalf("named pipe: %v %s", err, output)
			}
			t.Setenv("MAESTRO_CDP_TEST_PIPE", pipe)
			t.Setenv("MAESTRO_CDP_TEST_PID", pidFile)
			t.Setenv("MAESTRO_CDP_TEST_URL", endpoint)
			binary := filepath.Join(dir, "browser")
			script := `#!/bin/sh
printf '%s\n' "$$" > "$MAESTRO_CDP_TEST_PID"
printf 'DevTools listening on %s\n' "$MAESTRO_CDP_TEST_URL" >&2
read -r finished < "$MAESTRO_CDP_TEST_PIPE"
`
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if data, err := os.ReadFile(pidFile); err == nil {
					if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
						if process, err := os.FindProcess(pid); err == nil {
							_ = process.Kill()
						}
					}
				}
			})
			d, err := New(Config{Headless: true, ChromeBin: binary, UserDataDir: filepath.Join(dir, "profile"), URL: "https://example.test/initial"})
			if fail {
				if err == nil || d != nil || !strings.Contains(err.Error(), "initial observation failed") {
					t.Fatalf("constructor error = %v, driver = %v", err, d)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if transport.count("Network.disable") != 1 {
					t.Fatal("initial navigation retained Network")
				}
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if transport.count("Browser.close") != 1 {
				t.Fatal("constructor did not release browser ownership")
			}
		})
	}
}

type networkHandshakeWriter struct {
	http.ResponseWriter
	accept string
}

func (w networkHandshakeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w networkHandshakeWriter) WriteHeader(status int) {
	if status == http.StatusSwitchingProtocols {
		w.Header().Set("Sec-WebSocket-Accept", w.accept)
	}
	w.ResponseWriter.WriteHeader(status)
}

func TestNetworkObservationConcurrentEmulationAndRelease(t *testing.T) {
	for _, first := range []string{"setting", "release"} {
		t.Run(first, func(t *testing.T) {
			d, f := fakeNetworkDriver(t)
			o := observeForTest(t, d, d.page)
			entered := make(chan struct{})
			unblock := make(chan struct{})
			var once sync.Once
			blockedMethod := "Network.disable"
			if first == "setting" {
				blockedMethod = "Network.emulateNetworkConditions"
			}
			f.mu.Lock()
			f.hook = func(method string) {
				if method == blockedMethod {
					once.Do(func() { close(entered); <-unblock })
				}
			}
			f.mu.Unlock()
			settingDone := make(chan *core.CommandResult, 1)
			releaseDone := make(chan error, 1)
			set := func() { settingDone <- d.setNetworkConditions(&flow.SetNetworkConditionsStep{Offline: true}) }
			release := func() { releaseDone <- o.close() }
			if first == "setting" {
				go set()
			} else {
				go release()
			}
			<-entered
			started := make(chan struct{})
			if first == "setting" {
				go func() { close(started); release() }()
			} else {
				go func() { close(started); set() }()
			}
			<-started
			// The second operation cannot complete while the first protocol transition
			// is held. This checks the production mutex, not the transport's mutex.
			select {
			case <-settingDone:
				close(unblock)
				t.Fatal("setting escaped serialized cleanup")
			case <-releaseDone:
				close(unblock)
				t.Fatal("release escaped serialized setting")
			case <-time.After(20 * time.Millisecond):
			}
			close(unblock)
			if result := <-settingDone; !result.Success {
				t.Fatal(result.Error)
			}
			if err := <-releaseDone; err != nil {
				t.Fatal(err)
			}
			expected := 1
			if first == "setting" {
				expected = 0
			}
			if f.count("Network.disable") != expected {
				t.Fatal("release discarded the latest network state")
			}
			next := observeForTest(t, d, d.page)
			if err := next.close(); err != nil {
				t.Fatal(err)
			}
			if f.count("Network.disable") != expected {
				t.Fatal("subsequent cleanup lost the emulation hold")
			}
			if result := d.clearNetworkMocks(); !result.Success {
				t.Fatal(result.Error)
			}
		})
	}
}
