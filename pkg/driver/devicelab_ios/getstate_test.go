package devicelab_ios

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
