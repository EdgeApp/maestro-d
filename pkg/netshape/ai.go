package netshape

// ai.go: config.json's ai block, the daemon's own keys for assertWithAI and
// assertNoDefectsWithAI. The block names files; their contents are read at
// check time and never logged or returned.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AI is config.json's {"ai": {"jevKeyFile", "anthropicKeyFile",
// "maxBudgetUsdPerCall"}}. A relative key file is relative to the config's
// directory.
type AI struct {
	JevKeyFile       string `json:"jevKeyFile,omitempty"`
	AnthropicKeyFile string `json:"anthropicKeyFile,omitempty"`
	// MaxBudgetUSDPerCall caps what one check may spend on these keys, and
	// is its budget when the caller names none (0 = no cap).
	MaxBudgetUSDPerCall float64 `json:"maxBudgetUsdPerCall,omitempty"`
}

func (a *AI) validate() error {
	if strings.TrimSpace(a.JevKeyFile) == "" && strings.TrimSpace(a.AnthropicKeyFile) == "" {
		return errors.New("names neither jevKeyFile nor anthropicKeyFile")
	}
	if a.MaxBudgetUSDPerCall < 0 {
		return fmt.Errorf("maxBudgetUsdPerCall must not be negative, got %v", a.MaxBudgetUSDPerCall)
	}
	return nil
}

func (a *AI) resolve(dir string) {
	for _, p := range []*string{&a.JevKeyFile, &a.AnthropicKeyFile} {
		if v := strings.TrimSpace(*p); v != "" && !filepath.IsAbs(v) {
			*p = filepath.Join(dir, v)
		} else {
			*p = v
		}
	}
}

// AIKeys are the keys the block's files hold.
type AIKeys struct{ Jev, Anthropic string }

// ReadKeys reads the key files the block names. Each must be readable by
// its owner only; an error names the file, never its content.
func (a *AI) ReadKeys() (AIKeys, error) {
	var k AIKeys
	var err error
	if k.Jev, err = readKeyFile(a.JevKeyFile); err != nil {
		return AIKeys{}, err
	}
	if k.Anthropic, err = readKeyFile(a.AnthropicKeyFile); err != nil {
		return AIKeys{}, err
	}
	return k, nil
}

func readKeyFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("ai key file: %w", err)
	}
	if perm := info.Mode().Perm(); goos != "windows" && perm&0o077 != 0 {
		return "", fmt.Errorf("ai key file %s: mode %04o is readable by others; chmod 600 it", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("ai key file: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("ai key file %s is empty", path)
	}
	return key, nil
}
