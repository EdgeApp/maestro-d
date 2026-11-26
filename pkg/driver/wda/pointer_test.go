package wda

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// dragPath is one W3C touch: move, down, hold, a move per leg, up.
func TestDragPath_W3CTouch(t *testing.T) {
	var _ core.PathDragger = (*Driver)(nil)
	var seq []map[string]interface{}
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions") {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Actions []struct {
					Parameters map[string]interface{}   `json:"parameters"`
					Actions    []map[string]interface{} `json:"actions"`
				} `json:"actions"`
			}
			_ = json.Unmarshal(body, &req)
			if req.Actions[0].Parameters["pointerType"] != "touch" {
				t.Errorf("not touch: %s", body)
			}
			seq = req.Actions[0].Actions
			if fail {
				jsonResponse(w, map[string]interface{}{"value": map[string]interface{}{"error": "invalid argument", "message": "bad action"}})
				return
			}
		}
		jsonResponse(w, map[string]interface{}{"value": nil})
	}))
	defer server.Close()
	d := createTestDriver(server) // 390 x 844
	res := d.DragPath(&flow.DragPathStep{Points: []string{"10%, 10%", "50%, 10%", "50%, 50%"}, DurationMs: 600, HoldDuration: 150})
	if !res.Success || len(seq) != 6 || seq[0]["x"].(float64) != 39 || seq[2]["duration"].(float64) != 150 ||
		seq[4]["x"].(float64) != 195 || seq[4]["y"].(float64) != 422 || seq[5]["type"] != "pointerUp" {
		t.Fatalf("%+v %v", res, seq)
	}
	if res := d.DragPath(&flow.DragPathStep{Points: []string{"1, 1", "bad"}}); res.Success {
		t.Fatal("bad point")
	}
	fail = true
	if res := d.DragPath(&flow.DragPathStep{Points: []string{"1, 1", "2, 2"}}); res.Success || !strings.Contains(res.Message, "bad action") {
		t.Fatalf("WDA error: %+v", res)
	}
	d.info = &core.PlatformInfo{}
	d.client.baseURL = "http://127.0.0.1:1"
	if res := d.DragPath(&flow.DragPathStep{Points: []string{"1, 1", "2, 2"}}); res.Success {
		t.Fatalf("no screen size: %+v", res)
	}
}
