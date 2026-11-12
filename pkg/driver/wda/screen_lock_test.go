package wda

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func TestPressKeyScreenLock(t *testing.T) {
	for _, key := range []string{"LOCK", "lock", "UNLOCK", "unlock"} {
		t.Run(key, func(t *testing.T) {
			locked := strings.EqualFold(key, "lock")
			calls := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if r.Method == "GET" {
					jsonResponse(w, map[string]interface{}{"value": locked})
				} else {
					jsonResponse(w, map[string]interface{}{"value": nil})
				}
			}))
			defer server.Close()
			d := createTestDriver(server)
			result := d.Execute(&flow.PressKeyStep{Key: key})
			if !result.Success {
				t.Fatalf("%s: %v", result.Message, result.Error)
			}
			want := "POST /session/test-session/wda/" + strings.ToLower(key) + ",GET /session/test-session/wda/locked"
			if strings.Join(calls, ",") != want {
				t.Fatalf("calls=%v, want %s", calls, want)
			}
		})
	}
}

func TestScreenLockFailures(t *testing.T) {
	for _, tc := range []struct {
		name, key           string
		postError, getError bool
		value               interface{}
	}{
		{name: "lock action fails", key: "LOCK", postError: true},
		{name: "unlock action fails", key: "UNLOCK", postError: true},
		{name: "state query fails", key: "LOCK", getError: true},
		{name: "lock did not happen", key: "LOCK", value: false},
		{name: "passcode still locked", key: "UNLOCK", value: true},
		{name: "invalid state", key: "UNLOCK", value: "false"},
		{name: "missing state", key: "UNLOCK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gets := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					gets++
				}
				if (r.Method == "POST" && tc.postError) || (r.Method == "GET" && tc.getError) {
					w.WriteHeader(http.StatusInternalServerError)
					jsonResponse(w, map[string]interface{}{"value": map[string]interface{}{"error": "unknown error", "message": "device failure"}})
					return
				}
				jsonResponse(w, map[string]interface{}{"value": tc.value})
			}))
			defer server.Close()
			result := createTestDriver(server).Execute(&flow.PressKeyStep{Key: tc.key})
			if result.Success {
				t.Fatal("unexpected success")
			}
			if tc.postError && gets != 0 {
				t.Fatal("queried state after failed action")
			}
		})
	}
}

func TestScreenLockRejectsTV(t *testing.T) {
	for _, key := range []string{"LOCK", "UNLOCK"} {
		// A nil client ensures rejection occurs before any device access.
		d := &Driver{isTV: true}
		if d.Execute(&flow.PressKeyStep{Key: key}).Success {
			t.Fatal("TV lock accepted")
		}
	}
}
