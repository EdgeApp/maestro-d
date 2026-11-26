package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/aicheck"
	"github.com/devicelab-dev/maestro-runner/pkg/aicheck/aichecktest"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

const (
	hubOwnJevKey  = "jev-FAKE-HUB-A-KEY"
	clientJevKey  = "jev-FAKE-CLIENT-KEY"
	clientAnthKey = "sk-ant-FAKE-CLIENT-KEY"
)

// An AI check on a peer's device is answered by the hub that owns it,
// with its own keys; the keys the client sent to the other hub are never
// forwarded.
func TestMesh_AICheckRunsOnTheOwnerWithItsKeys(t *testing.T) {
	for _, ownKeys := range []bool{true, false} {
		setupHome(t)
		fake := aichecktest.New(t, map[string]aichecktest.Answer{aicheck.ModelJev: {Noul: aichecktest.Noul(0.9)}})
		prevJ, prevA := daemon.AIJevURL, daemon.AIAnthropicURL
		daemon.AIJevURL, daemon.AIAnthropicURL = fake.JevURL(), fake.AnthropicURL()
		t.Cleanup(func() { daemon.AIJevURL, daemon.AIAnthropicURL = prevJ, prevA })

		dir := t.TempDir()
		cfg := map[string]any{"hubDevices": []any{map[string]any{"id": "a1"}}}
		if ownKeys {
			if err := os.WriteFile(filepath.Join(dir, "jev.key"), []byte(hubOwnJevKey), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg["ai"] = map[string]any{"jevKeyFile": "jev.key"}
		}
		data, _ := json.Marshal(cfg)
		aConfig := filepath.Join(dir, "config.json")
		if err := os.WriteFile(aConfig, data, 0o600); err != nil {
			t.Fatal(err)
		}
		a := startHub(t, "hub-a", testDevices("a"), withDeviceConfig(aConfig))
		b := startHub(t, "hub-b", testDevices("b"), withPeers(a.addr), withDeviceConfig(filepath.Join(dir, "none.json")))
		waitLinked(t, a, "hub-b")
		waitLinked(t, b, "hub-a")

		var mu sync.Mutex
		var bodies []string
		a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPost && r.Body != nil {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				mu.Lock()
				bodies = append(bodies, r.URL.Path+" "+string(body))
				mu.Unlock()
			}
			return false
		})

		c := b.client()
		ctx := context.Background()
		keys := &daemon.AttachAI{JevKey: clientJevKey, AnthropicKey: clientAnthKey}
		// Through a lease that attaches, then a plain attach.
		lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a", Holder: "tester", Attach: &daemon.AttachConfig{Platform: "mock", AI: keys}})
		if err != nil {
			t.Fatal(err)
		}
		c.SetLease("a1@hub-a", lr.Lease.ID)
		if _, err := c.Attach(ctx, "a1@hub-a", daemon.AttachConfig{Platform: "mock", AI: keys}); err != nil {
			t.Fatal(err)
		}
		res, err := c.Command(ctx, "a1@hub-a", "assertWithAI", "A sign-in form is on screen", true)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(res)
		var r aicheck.Result
		raw, _ := json.Marshal(res.Data)
		_ = json.Unmarshal(raw, &r)
		reqs := fake.Requests()
		if ownKeys {
			if r.Verdict != aicheck.Pass || len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer "+hubOwnJevKey {
				t.Fatalf("own keys: %+v %d", r, len(reqs))
			}
		} else if r.Verdict != aicheck.Unsure || len(reqs) != 0 || r.Escalations[0].Reason != aicheck.ReasonNoKey {
			// hub-a has no keys of its own, and the client's stayed on hub-b.
			t.Fatalf("no keys: %+v %d", r, len(reqs))
		}
		mu.Lock()
		seen := strings.Join(bodies, "\n")
		mu.Unlock()
		if !strings.Contains(seen, "/attach") || !strings.Contains(seen, "/v1/leases") {
			t.Fatalf("owner saw: %s", seen)
		}
		for _, s := range []string{seen, string(out)} {
			if strings.Contains(s, clientJevKey) || strings.Contains(s, clientAnthKey) || strings.Contains(s, hubOwnJevKey) || strings.Contains(s, `"ai"`) {
				t.Fatalf("key forwarded or echoed: %s", s)
			}
		}
	}
}

func TestDropAIKeys(t *testing.T) {
	for in, want := range map[string]string{
		`{"platform":"mock","ai":{"jevKey":"k"}}`:        `{"platform":"mock"}`,
		`{"device":"a1","attach":{"ai":{"jevKey":"k"}}}`: `{"attach":{},"device":"a1"}`,
		`{"device":"a1","attach":{"platform":"mock"}}`:   `{"device":"a1","attach":{"platform":"mock"}}`,
		`not json`:                       `not json`,
		`{"device":"a1","attach":"odd"}`: `{"device":"a1","attach":"odd"}`,
	} {
		path := "/v1/devices/a1/attach"
		if strings.Contains(in, "device") {
			path = "/v1/leases"
		}
		if got := string(dropAIKeys(http.MethodPost, path, []byte(in))); got != want {
			t.Fatalf("%s: %s", in, got)
		}
	}
	if got := string(dropAIKeys(http.MethodPost, "/v1/devices/a1/commands/tapOn", []byte(`{"ai":1}`))); got != `{"ai":1}` {
		t.Fatal(got)
	}
}
