package uiautomator2

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TouchPath is one touch: move to the start, down, hold, a move per leg
// with its time, up.
func TestTouchPath(t *testing.T) {
	var seq []map[string]interface{}
	client, server := newTestClientWithSession(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/actions") {
			t.Errorf("path %s", r.URL.Path)
		}
		var req struct {
			Actions []struct {
				Parameters map[string]interface{}   `json:"parameters"`
				Actions    []map[string]interface{} `json:"actions"`
			} `json:"actions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Actions[0].Parameters["pointerType"] != "touch" {
			t.Errorf("not touch: %+v", req.Actions[0].Parameters)
		}
		seq = req.Actions[0].Actions
		_, _ = w.Write([]byte(`{"value": null}`))
	})
	defer server.Close()
	if err := client.TouchPath(10, 20, 300, [][3]int{{30, 40, 500}, {50, 60, 250}}); err != nil {
		t.Fatal(err)
	}
	if len(seq) != 6 || seq[1]["type"] != "pointerDown" || seq[2]["duration"].(float64) != 300 ||
		seq[3]["x"].(float64) != 30 || seq[3]["duration"].(float64) != 500 || seq[5]["type"] != "pointerUp" {
		t.Fatalf("sequence: %v", seq)
	}
	if err := client.TouchPath(10, 20, 0, [][3]int{{30, 40, 500}}); err != nil || len(seq) != 4 {
		t.Fatalf("no hold: %v %v", seq, err)
	}
}
