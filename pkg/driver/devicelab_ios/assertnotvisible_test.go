package devicelab_ios

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func fastNotVisiblePoll(t *testing.T) {
	t.Helper()
	prev := notVisiblePollInterval
	notVisiblePollInterval = 10 * time.Millisecond
	t.Cleanup(func() { notVisiblePollInterval = prev })
}

// The element leaves the screen on the fourth snapshot; assertNotVisible must
// keep looking until then instead of failing on the first one.
func TestAssertNotVisible_WaitsForElementToGo(t *testing.T) {
	fastNotVisiblePoll(t)
	var calls int64
	d := snapshotServer(t, func(call int64) []SnapshotNode {
		atomic.StoreInt64(&calls, call)
		if call <= 3 {
			return []SnapshotNode{visibleRow("spotlight")}
		}
		return []SnapshotNode{visibleRow("other")}
	})

	step := &flow.AssertNotVisibleStep{Selector: flow.Selector{ID: "spotlight"}}
	step.TimeoutMs = 5000
	res := d.handleAssertNotVisible(step)
	if !res.Success {
		t.Fatalf("expected success once the element went, got: %s", res.Message)
	}
	if got := atomic.LoadInt64(&calls); got != 4 {
		t.Errorf("snapshots = %d, want 4", got)
	}
}

func TestAssertNotVisible_FailsAfterTimeout(t *testing.T) {
	fastNotVisiblePoll(t)
	d := snapshotServer(t, func(int64) []SnapshotNode {
		return []SnapshotNode{visibleRow("spotlight")}
	})

	step := &flow.AssertNotVisibleStep{Selector: flow.Selector{ID: "spotlight"}}
	step.TimeoutMs = 200
	start := time.Now()
	res := d.handleAssertNotVisible(step)
	elapsed := time.Since(start)
	if res.Success {
		t.Fatal("expected failure while the element stays visible")
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("failed after %v, want it to wait the 200 ms timeout", elapsed)
	}
	if !strings.Contains(res.Message, "id=spotlight") || !strings.Contains(res.Message, "200 ms") {
		t.Errorf("Message = %q, want the selector and the timeout", res.Message)
	}
}

func TestAssertNotVisible_AbsentPassesOnFirstSnapshot(t *testing.T) {
	fastNotVisiblePoll(t)
	var calls int64
	d := snapshotServer(t, func(call int64) []SnapshotNode {
		atomic.StoreInt64(&calls, call)
		return []SnapshotNode{
			visibleRow("other"),
			{Type: "Cell", Identifier: "spotlight"}, // zero-size: matched but not displayed
		}
	})

	res := d.handleAssertNotVisible(&flow.AssertNotVisibleStep{Selector: flow.Selector{ID: "spotlight"}})
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Message)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("snapshots = %d, want 1", got)
	}
}

// truncatedServer answers every command with nodes and the runner's
// truncated flag set.
func truncatedServer(t *testing.T, nodes []SnapshotNode) *Driver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"ok":   true,
			"data": map[string]any{"nodes": nodes, "truncated": true},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	client := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	return NewDriver(client, nil, "test-udid", nil)
}

// The runner's truncated flag reaches the hierarchy instead of being dropped.
func TestHierarchyJSON_KeepsTruncatedFlag(t *testing.T) {
	d := truncatedServer(t, []SnapshotNode{visibleRow("row")})
	raw, err := d.hierarchyJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Nodes     []SnapshotNode `json:"nodes"`
		Truncated bool           `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Truncated || len(doc.Nodes) != 1 {
		t.Errorf("hierarchy = %s, want one node and truncated:true", raw)
	}

	full := snapshotServer(t, func(int64) []SnapshotNode { return []SnapshotNode{visibleRow("row")} })
	raw, err = full.hierarchyJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "truncated") {
		t.Errorf("a complete snapshot must not be marked truncated: %s", raw)
	}
}

// A miss or an absence on a truncated snapshot says the element may lie past
// the runner's cap instead of reading like a plain answer.
func TestTruncatedSnapshotIsReported(t *testing.T) {
	fastNotVisiblePoll(t)
	d := truncatedServer(t, []SnapshotNode{visibleRow("row"), visibleRow("row")})

	res := d.handleAssertNotVisible(&flow.AssertNotVisibleStep{Selector: flow.Selector{ID: "chat-composer"}})
	if !res.Success {
		t.Fatalf("absent element should still pass, got: %s", res.Message)
	}
	if !strings.Contains(res.Message, "cap of 2 nodes") {
		t.Errorf("Message = %q, want the truncation noted", res.Message)
	}

	_, err := d.findElement(flow.Selector{ID: "chat-composer"}, false, 100)
	if err == nil || !strings.Contains(err.Error(), "cap of 2 nodes") {
		t.Errorf("findElement error = %v, want the truncation noted", err)
	}

	step := &flow.AssertVisibleStep{Selector: flow.Selector{ID: "row"}, Count: "3"}
	step.TimeoutMs = 100
	if res := d.handleAssertVisible(step); res.Success || !strings.Contains(res.Message, "cap of 2 nodes") {
		t.Errorf("count Message = %q, want a failure that notes the truncation", res.Message)
	}

	full := snapshotServer(t, func(int64) []SnapshotNode { return []SnapshotNode{visibleRow("row")} })
	res = full.handleAssertNotVisible(&flow.AssertNotVisibleStep{Selector: flow.Selector{ID: "chat-composer"}})
	if !res.Success || res.Message != "not visible" {
		t.Errorf("complete snapshot Message = %q, want plain %q", res.Message, "not visible")
	}
}
