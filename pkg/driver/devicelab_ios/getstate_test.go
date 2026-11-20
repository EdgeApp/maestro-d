package devicelab_ios

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
)

func stateServer(t *testing.T, appState string) *Driver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"data": map[string]any{"appState": appState},
		})
	}))
	t.Cleanup(srv.Close)
	d := NewDriver(&Client{baseURL: srv.URL, httpClient: srv.Client()}, nil, "test-udid", nil)
	d.SetAppID("app.teamstation.app")
	return d
}

func TestGetStateReadsTheRunnersAppState(t *testing.T) {
	cases := map[string][2]string{
		"runningForeground":          {"foreground", "app.teamstation.app"},
		"runningBackground":          {"background", ""},
		"runningBackgroundSuspended": {"background", ""},
		"notRunning":                 {"not_running", ""},
		"unknown":                    {"", ""},
	}
	for runner, want := range cases {
		state := stateServer(t, runner).GetState()
		if state.AppState != want[0] || state.ForegroundApp != want[1] {
			t.Errorf("%s: got %+v", runner, state)
		}
	}
}

func TestGetStateWithNoAppMakesNoCall(t *testing.T) {
	d := NewDriver(nil, nil, "test-udid", nil)
	if state := d.GetState(); state.AppState != "" {
		t.Errorf("got %+v", state)
	}
}

// The runner call is made under the foreground lookup's two-second limit, not
// callTimeout's ten seconds, so it is cancelled when GetState gives up.
func TestGetStateCancelsTheRunnerCallAtTheLimit(t *testing.T) {
	cancelled := make(chan time.Duration, 1)
	release := make(chan struct{})
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices a dropped connection only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			cancelled <- time.Since(start)
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	defer close(release)
	d := NewDriver(&Client{baseURL: srv.URL, httpClient: srv.Client()}, nil, "test-udid", nil)
	d.SetAppID("app.teamstation.app")
	if state := d.GetState(); state.AppState != "" {
		t.Errorf("a timed-out lookup leaves appState absent: %+v", state)
	}
	select {
	case after := <-cancelled:
		if after > core.ForegroundLookupLimit+time.Second {
			t.Errorf("the call was cancelled only after %v", after)
		}
	case <-time.After(core.ForegroundLookupLimit + 2*time.Second):
		t.Fatal("the runner call outlived the lookup's limit")
	}
}

// GetState reads the app's state with the runner's appState command, a
// lifecycle command the runner answers without activating the app: a
// snapshot would bring a backgrounded app to the front (and relaunch a crashed
// one) before reading it, so the state would always read foreground.
func TestGetStateReadsWithoutActivatingTheApp(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		// A stand-in for the runner: a snapshot activates the app first and
		// so reports it in front; appState reads it where it is.
		appState := "runningBackground"
		if sent["command"] == string(CmdSnapshot) {
			appState = "runningForeground"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"data": map[string]any{"appState": appState},
		})
	}))
	t.Cleanup(srv.Close)
	d := NewDriver(&Client{baseURL: srv.URL, httpClient: srv.Client()}, nil, "test-udid", nil)
	d.SetAppID("app.teamstation.app")
	if state := d.GetState(); state.AppState != "background" || state.ForegroundApp != "" {
		t.Errorf("a backgrounded app should read background: %+v", state)
	}
	if sent["command"] != string(CmdAppState) || sent["appBundleId"] != "app.teamstation.app" {
		t.Errorf("sent %v, want the appState command for the session's app", sent)
	}
}
