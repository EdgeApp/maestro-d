package devicelab_ios

import (
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
