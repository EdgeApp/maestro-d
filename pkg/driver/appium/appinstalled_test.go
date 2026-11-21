package appium

// maestro-d: AppInstalled, which the daemon polls after installing into a
// cloud session.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppInstalled(t *testing.T) {
	var lastArgs map[string]interface{}
	answer := interface{}(true)
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/execute/sync") {
			var body struct {
				Script string                   `json:"script"`
				Args   []map[string]interface{} `json:"args"`
			}
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &body)
			if body.Script != "mobile: isAppInstalled" || len(body.Args) != 1 {
				t.Errorf("script %q args %v", body.Script, body.Args)
			}
			lastArgs = body.Args[0]
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				writeJSON(w, map[string]interface{}{"value": map[string]interface{}{"error": "unknown method", "message": "no such command"}})
				return
			}
			writeJSON(w, map[string]interface{}{"value": answer})
			return
		}
		writeJSON(w, map[string]interface{}{"value": nil})
	}))
	defer server.Close()

	d := createTestAppiumDriver(server)
	d.platform = "android"
	if ok, err := d.AppInstalled("org.b"); err != nil || !ok || lastArgs["appId"] != "org.b" {
		t.Fatalf("android: %v %v %v", ok, err, lastArgs)
	}
	d.platform = "ios"
	answer = false
	if ok, err := d.AppInstalled("org.b"); err != nil || ok || lastArgs["bundleId"] != "org.b" {
		t.Fatalf("ios: %v %v %v", ok, err, lastArgs)
	}
	answer = nil
	if _, err := d.AppInstalled("org.b"); err == nil {
		t.Fatal("a null answer is not an error")
	}
	fail = true
	if _, err := d.AppInstalled("org.b"); err == nil {
		t.Fatal("a failed command is not an error")
	}
}
