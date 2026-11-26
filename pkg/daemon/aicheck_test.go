package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/aicheck"
	"github.com/devicelab-dev/maestro-runner/pkg/aicheck/aichecktest"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

const (
	ownJevKey        = "jev-FAKE-OWN-KEY"
	ownAnthropicKey  = "sk-ant-FAKE-OWN-KEY"
	sentJevKey       = "jev-FAKE-SENT-KEY"
	sentAnthropicKey = "sk-ant-FAKE-SENT-KEY"
)

// aiFake points the daemon's AI checks at a fake answering with Jev only.
func aiFake(t *testing.T) *aichecktest.Server {
	t.Helper()
	fake := aichecktest.New(t, map[string]aichecktest.Answer{
		aicheck.ModelJev:    {Noul: aichecktest.Noul(0.5)},
		aicheck.ModelHaiku:  {Verdict: aicheck.Pass, Confidence: 0.9, Observed: "a sign-in form", InputTokens: 1000, OutputTokens: 100},
		aicheck.ModelSonnet: {Verdict: aicheck.Pass, Confidence: 0.9},
	})
	prevJ, prevA := AIJevURL, AIAnthropicURL
	AIJevURL, AIAnthropicURL = fake.JevURL(), fake.AnthropicURL()
	t.Cleanup(func() { AIJevURL, AIAnthropicURL = prevJ, prevA })
	return fake
}

// writeAIConfig gives the daemon its own keys in its config.json.
func writeAIConfig(t *testing.T, budget float64) {
	t.Helper()
	dir := Home()
	_ = os.MkdirAll(dir, 0o755)
	for name, key := range map[string]string{"jev.key": ownJevKey, "anthropic.key": ownAnthropicKey} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(map[string]any{"ai": map[string]any{"jevKeyFile": "jev.key", "anthropicKeyFile": "anthropic.key", "maxBudgetUsdPerCall": budget}})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func attachWithKeys(t *testing.T, env *testEnv) *DeviceInfo {
	t.Helper()
	info, err := env.c.Attach(context.Background(), "mock-1", AttachConfig{Platform: "mock",
		AI: &AttachAI{JevKey: sentJevKey, AnthropicKey: sentAnthropicKey}})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func aiResult(t *testing.T, res *StepResult) aicheck.Result {
	t.Helper()
	data, _ := json.Marshal(res.Data)
	var out aicheck.Result
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func usedKeys(fake *aichecktest.Server) (jev, anthropic []string) {
	for _, r := range fake.Requests() {
		if v := r.Header.Get("Authorization"); v != "" {
			jev = append(jev, strings.TrimPrefix(v, "Bearer "))
		}
		if v := r.Header.Get("x-api-key"); v != "" {
			anthropic = append(anthropic, v)
		}
	}
	return
}

// A daemon with no ai block spends the keys its attach carried.
func TestAI_AttachKeysWithoutAnAIBlock(t *testing.T) {
	env := newTestEnv(t, 0)
	fake := aiFake(t)
	attachWithKeys(t, env)
	res, err := env.c.Command(context.Background(), "mock-1", "assertWithAI", "A sign-in form is on screen", true)
	if err != nil {
		t.Fatal(err)
	}
	if r := aiResult(t, res); r.Verdict != aicheck.Pass || r.Model != aicheck.ModelHaiku {
		t.Fatalf("%+v", r)
	}
	jev, anth := usedKeys(fake)
	if len(jev) != 1 || jev[0] != sentJevKey || len(anth) != 1 || anth[0] != sentAnthropicKey {
		t.Fatalf("keys used: %v %v", jev, anth)
	}
}

// The daemon's own block wins over the keys a client sent, and caps the
// client's budget at maxBudgetUsdPerCall.
func TestAI_OwnBlockWinsAndCapsTheBudget(t *testing.T) {
	env := newTestEnv(t, 0)
	fake := aiFake(t)
	writeAIConfig(t, 0.005)
	attachWithKeys(t, env)
	res, err := env.c.Command(context.Background(), "mock-1", "assertWithAI", map[string]any{"assertion": "A sign-in form is on screen", "budgetUsd": 5}, true)
	if err != nil {
		t.Fatal(err)
	}
	r := aiResult(t, res)
	jev, anth := usedKeys(fake)
	if len(jev) != 1 || jev[0] != ownJevKey || len(anth) != 1 || anth[0] != ownAnthropicKey {
		t.Fatalf("keys used: %v %v", jev, anth)
	}
	// Haiku answered at 0.9, so the waterfall stopped before the cap
	// mattered; ask for a stricter Haiku to reach Sonnet, which the cap
	// refuses.
	res, err = env.c.Command(context.Background(), "mock-1", "assertWithAI",
		map[string]any{"assertion": "A sign-in form is on screen", "budgetUsd": 5, "thresholds": map[string]any{"haiku": 1.01}}, true)
	if err != nil {
		t.Fatal(err)
	}
	r = aiResult(t, res)
	last := r.Escalations[len(r.Escalations)-1]
	if r.Verdict != aicheck.Unsure || last.Model != aicheck.ModelSonnet || last.Reason != aicheck.ReasonBudget {
		t.Fatalf("%+v", r)
	}
}

// A broken config or key file fails the step without naming a key.
func TestAI_BadKeyFileFailsTheStep(t *testing.T) {
	env := newTestEnv(t, 0)
	aiFake(t)
	writeAIConfig(t, 0)
	if err := os.Chmod(filepath.Join(Home(), "jev.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.attach("mock-1")
	_, err := env.c.Command(context.Background(), "mock-1", "assertNoDefectsWithAI", nil, true)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), ownJevKey) {
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(filepath.Join(Home(), "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.c.Command(context.Background(), "mock-1", "assertNoDefectsWithAI", nil, true); err == nil {
		t.Fatal("broken config accepted")
	}
}

// No key appears in the attach answer, the device listing, the session's
// variables, the step's answer, the report or the daemon's log.
func TestAI_NoKeyAnywhere(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	if err := logger.Init(logPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.Close)
	env := newTestEnv(t, 0)
	aiFake(t)
	info := attachWithKeys(t, env)
	ctx := context.Background()
	var seen []string
	add := func(v any) {
		data, _ := json.Marshal(v)
		seen = append(seen, string(data))
	}
	add(info)
	res, err := env.c.Command(ctx, "mock-1", "assertWithAI", "A sign-in form is on screen", true)
	if err != nil {
		t.Fatal(err)
	}
	add(res)
	devs, _ := env.c.Devices(ctx, "")
	add(devs)
	vars, _ := env.c.Vars(ctx, "mock-1")
	add(vars)
	a := env.s.getAttached("mock-1")
	add(a.cfg)
	if a.cfg.AI != nil {
		t.Fatal("attach keys stored in the session's config")
	}
	reportDir := a.reportDir
	if err := env.c.Detach(ctx, "mock-1"); err != nil {
		t.Fatal(err)
	}
	logData, _ := os.ReadFile(logPath)
	seen = append(seen, string(logData))
	_ = filepath.Walk(reportDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			data, _ := os.ReadFile(p)
			seen = append(seen, string(data))
		}
		return nil
	})
	for i, s := range seen {
		for _, k := range []string{sentJevKey, sentAnthropicKey} {
			if strings.Contains(s, k) {
				t.Fatalf("key in output %d: %.300s", i, s)
			}
		}
	}
	if !strings.Contains(string(logData), "mock-1") {
		t.Fatalf("log not captured: %s", logData)
	}
}
