package netshape

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfig_AIBlock(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"hubDevices": [{"id": "a"}], "ai": {"jevKeyFile": "jev.key", "anthropicKeyFile": "/abs/anthropic.key", "maxBudgetUsdPerCall": 0.5}}`))
	if err != nil || cfg.AI == nil || cfg.AI.JevKeyFile != "jev.key" || cfg.AI.MaxBudgetUSDPerCall != 0.5 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if cfg, err := ParseConfig([]byte(`{"hubDevices": []}`)); err != nil || cfg.AI != nil {
		t.Fatalf("%+v %v", cfg, err)
	}
	for body, want := range map[string]string{
		`{"ai": {"jevKeyFile": "a", "jevKey": "x"}}`: `unknown field "jevKey"`,
		`{"ai": {}}`: "names neither",
		`{"ai": {"jevKeyFile": "a", "maxBudgetUsdPerCall": -1}}`: "must not be negative",
	} {
		if _, err := ParseConfig([]byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", body, err)
		}
	}
	// network.json takes no ai block.
	if _, err := Parse([]byte(`{"devices": {}, "ai": {"jevKeyFile": "a"}}`)); err == nil {
		t.Fatal("network.json took an ai block")
	}
}

func TestAIKeys_ReadFromFilesBesideTheConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("jev.key", "JEV-FAKE\n", 0o600)
	anth := write("anthropic.key", " ANTH-FAKE ", 0o600)
	cfgPath := write(ConfigFileName, `{"ai": {"jevKeyFile": "jev.key", "anthropicKeyFile": "`+anth+`", "maxBudgetUsdPerCall": 0.5}}`, 0o600)
	cfg, err := LoadConfig(cfgPath)
	if err != nil || cfg.AI.JevKeyFile != filepath.Join(dir, "jev.key") {
		t.Fatalf("%+v %v", cfg, err)
	}
	k, err := cfg.AI.ReadKeys()
	if err != nil || k.Jev != "JEV-FAKE" || k.Anthropic != "ANTH-FAKE" {
		t.Fatalf("%+v %v", k, err)
	}
	// One file is enough.
	if k, err := (&AI{AnthropicKeyFile: anth}).ReadKeys(); err != nil || k.Jev != "" || k.Anthropic != "ANTH-FAKE" {
		t.Fatalf("%+v %v", k, err)
	}
	open := write("open.key", "SECRET-OPEN", 0o644)
	empty := write("empty.key", "  \n", 0o600)
	for _, a := range []*AI{{JevKeyFile: open}, {JevKeyFile: empty}, {JevKeyFile: filepath.Join(dir, "missing.key")}, {AnthropicKeyFile: open}} {
		_, err := a.ReadKeys()
		if err == nil || strings.Contains(err.Error(), "SECRET-OPEN") {
			t.Fatalf("%+v: %v", a, err)
		}
	}
	// Windows has no mode bits to check.
	defer func(g string) { goos = g }(goos)
	goos = "windows"
	if k, err := (&AI{JevKeyFile: open}).ReadKeys(); err != nil || k.Jev != "SECRET-OPEN" {
		t.Fatalf("%+v %v", k, err)
	}
	// An unreadable file (a directory) is an error.
	if _, err := (&AI{JevKeyFile: dir}).ReadKeys(); err == nil {
		t.Fatal("directory read as a key")
	}
}
