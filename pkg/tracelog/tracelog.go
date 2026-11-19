// Package tracelog is the opt-in, size-capped, redacting trace the Android
// driver clients write. It is off unless MAESTRO_D_ANDROID_TRACE=1, and then
// lives in the session's output (report) directory.
package tracelog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// EnvVar turns the Android driver traces on when it is "1".
	EnvVar = "MAESTRO_D_ANDROID_TRACE"
	// FileName is the trace's name inside the output directory.
	FileName = "client.log"
	// MaxBytes is the size at which the trace file is truncated.
	MaxBytes int64 = 50 << 20
)

// Discard is the logger a client uses when no trace was asked for.
func Discard() *log.Logger { return log.New(io.Discard, "", 0) }

// Path is <outputDir>/client.log when the trace is on and the session has
// an output directory, else "".
func Path(outputDir string) string {
	if os.Getenv(EnvVar) != "1" || outputDir == "" {
		return ""
	}
	return filepath.Join(outputDir, FileName)
}

// Open returns a logger writing to path, truncated whenever it passes max
// bytes, and the file to close when the trace ends (a deleted report frees
// its space only once the file is closed); nil, nil when path cannot be
// opened.
func Open(path string, max int64) (*log.Logger, io.Closer) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil
	}
	size := int64(0)
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	return log.New(&capped{f: f, size: size, max: max}, "", log.Ltime|log.Lmicroseconds), f
}

// capped truncates its file to empty when a write takes it past max. The
// file is opened O_APPEND, so writes after the truncation start at 0.
type capped struct {
	mu   sync.Mutex
	f    *os.File
	size int64
	max  int64
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.size+int64(len(p)) > c.max {
		if err := c.f.Truncate(0); err == nil {
			c.size = 0
		}
	}
	n, err := c.f.Write(p)
	c.size += int64(n)
	return n, err
}

// redactedKeys are the JSON fields that carry typed or pasted text:
// element values, per-key action values and clipboard content.
var redactedKeys = map[string]bool{"text": true, "value": true, "content": true}

// Redact returns body (a JSON request body) for the trace, with every
// string under a text, value or content field, at any depth, replaced by
// "<redacted N chars>". A body that is not JSON is not echoed at all.
func Redact(body []byte) string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Sprintf("<%d bytes>", len(body))
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redact(v, false)); err != nil {
		return fmt.Sprintf("<%d bytes>", len(body))
	}
	return strings.TrimSuffix(out.String(), "\n")
}

func redact(v any, secret bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = redact(child, secret || redactedKeys[k])
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = redact(child, secret)
		}
		return t
	case string:
		if secret {
			return fmt.Sprintf("<redacted %d chars>", len([]rune(t)))
		}
	}
	return v
}
