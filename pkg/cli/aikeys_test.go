package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

const cliFakeJevKey = "jev-FAKE-CLI-KEY"

func writeKeys(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ai-keys.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAttachAIKeysFile(t *testing.T) {
	good := writeKeys(t, `{"jevKey": "`+cliFakeJevKey+`", "anthropicKey": "sk-ant-FAKE"}`, 0o600)
	o, _, err := parseDaemonArgs([]string{"--ai-keys-file", good, "--device", "d"}, attachCmdFlags, nil)
	if err != nil || o.Attach.AI == nil || o.Attach.AI.JevKey != cliFakeJevKey || o.Attach.AI.AnthropicKey != "sk-ant-FAKE" {
		t.Fatalf("%+v %v", o, err)
	}
	// One-shots take it too, for the attach they make.
	if o, _, err := parseDaemonArgs([]string{"--ai-keys-file", good, "--device", "d"}, oneShotFlags, nil); err != nil || o.Attach.AI == nil {
		t.Fatalf("one-shot: %v", err)
	}
	for _, tc := range []struct {
		path, want string
	}{
		{writeKeys(t, `{"jevKey": "`+cliFakeJevKey+`"}`, 0o644), "chmod 600"},
		{writeKeys(t, `{"jevKey": "`+cliFakeJevKey+`", "other": 1}`, 0o600), "not a JSON object"},
		{writeKeys(t, `{`+cliFakeJevKey, 0o600), "not a JSON object"},
		{writeKeys(t, `{}`, 0o600), "holds neither"},
		{filepath.Join(t.TempDir(), "missing.json"), "--ai-keys-file"},
		{t.TempDir(), "not a regular file"},
	} {
		_, _, err := parseDaemonArgs([]string{"--ai-keys-file", tc.path, "--device", "d"}, attachCmdFlags, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), cliFakeJevKey) {
			t.Fatalf("%s: %v", tc.path, err)
		}
	}
}

// The one-shot assertWithAI takes nested thresholds as numbers.
func TestOneShot_AssertWithAIThresholds(t *testing.T) {
	_, p := oneShotParse(t, "assertWithAI", "--assertion", "A sign-in form", "--thresholds.jevPass", "1.0", "--thresholds.haiku", "1.01", "--budgetUsd", "0.5", "--device", "d")
	v, err := buildStepValue(daemon.CommandSpecs["assertWithAI"], p)
	m, ok := v.(map[string]any)
	if err != nil || !ok || m["assertion"] != "A sign-in form" || m["budgetUsd"] != 0.5 {
		t.Fatalf("%#v %v", v, err)
	}
	th, _ := m["thresholds"].(map[string]any)
	if th["jevPass"] != 1.0 || th["haiku"] != 1.01 {
		t.Fatalf("%#v", th)
	}
}

// assertNoDefectsWithAI's assertion is built in, so a bare call is a step.
func TestBuildStepValue_AssertNoDefectsWithAINeedsNoValue(t *testing.T) {
	v, err := buildStepValue(daemon.CommandSpecs["assertNoDefectsWithAI"], &parsedArgs{})
	if err != nil || v != nil {
		t.Fatalf("got %v, %v", v, err)
	}
}
