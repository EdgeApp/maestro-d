//go:build darwin

package computer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/config"
)

const (
	textHelperName   = "MaestroDText"
	textHelperSource = "main.swift"
	textBuildTimeout = 5 * time.Minute
	textRunTimeout   = 30 * time.Second
)

// The helper's source ships in drivers/macos and its binary is cached in
// the runner home's cache, keyed by the source and compiler, exactly as
// MaestroDCapture is. Variables so tests can point them elsewhere.
var (
	helperSourceDir = func() string { return filepath.Join(config.GetDriversDir("macos"), textHelperName) }
	helperCacheDir  = func() string { return filepath.Join(config.GetCacheDir(), "text-helper") }
)

var (
	textBuildMu sync.Mutex
	textBuilt   string
)

// VisionRecognizer runs MaestroDText, building it with xcrun swiftc on first
// use.
type VisionRecognizer struct{}

func (VisionRecognizer) Recognize(ctx context.Context, image []byte) ([]TextBox, error) {
	bin, err := ensureTextHelper(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, textRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Stdin = bytes.NewReader(image)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", textHelperName, err, strings.TrimSpace(stderr.String()))
	}
	return parseBoxes(out)
}

func ensureTextHelper(ctx context.Context) (string, error) {
	textBuildMu.Lock()
	defer textBuildMu.Unlock()
	if textBuilt != "" {
		return textBuilt, nil
	}
	ctx, cancel := context.WithTimeout(ctx, textBuildTimeout)
	defer cancel()
	bin, err := buildTextHelper(ctx, helperSourceDir(), helperCacheDir())
	if err != nil {
		return "", fmt.Errorf("build %s: %w", textHelperName, err)
	}
	textBuilt = bin
	return bin, nil
}

func buildTextHelper(ctx context.Context, srcDir, cacheDir string) (string, error) {
	source, err := os.ReadFile(filepath.Join(srcDir, textHelperSource))
	if err != nil {
		return "", err
	}
	version, err := exec.CommandContext(ctx, "xcrun", "swiftc", "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("xcrun swiftc --version: %w: %s", err, strings.TrimSpace(string(version)))
	}
	h := sha256.New()
	h.Write(source)
	h.Write([]byte{0})
	h.Write(version)
	key := hex.EncodeToString(h.Sum(nil))[:12]
	bin := filepath.Join(cacheDir, key, textHelperName)
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(cacheDir, ".build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, textHelperName)
	if msg, err := exec.CommandContext(ctx, "xcrun", "swiftc", "-O", "-o", out, filepath.Join(srcDir, textHelperSource)).CombinedOutput(); err != nil {
		return "", fmt.Errorf("xcrun swiftc: %w: %s", err, strings.TrimSpace(string(msg)))
	}
	final := filepath.Join(cacheDir, key)
	if err := os.Rename(tmp, final); err != nil && !os.IsExist(err) {
		// Another process built the same key first; use its binary.
		if _, statErr := os.Stat(bin); statErr != nil {
			return "", err
		}
	}
	return bin, nil
}
