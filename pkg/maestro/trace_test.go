package maestro

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientTraceIsOptIn(t *testing.T) {
	c := NewClientTCP(1)
	c.logger.Printf("→ %s id=%d", "deviceInfo", 1) // the default trace goes nowhere
	if _, err := os.Stat("/tmp/devicelab-driver-client.log"); err == nil {
		t.Log("an older binary's /tmp/devicelab-driver-client.log exists; this client did not write it")
	}

	path := filepath.Join(t.TempDir(), "client.log")
	c.SetLogPath(path)
	c.logger.Printf("→ %s id=%d", "deviceInfo", 2)
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		t.Fatalf("SetLogPath trace empty: %v", err)
	}
	before := c.logger
	c.SetLogPath(filepath.Join(path, "bad", "x.log"))
	if c.logger != before {
		t.Error("an unopenable path replaced the working trace")
	}
}

func TestCloseClosesTheTrace(t *testing.T) {
	c := NewClientTCP(1)
	c.SetLogPath(filepath.Join(t.TempDir(), "client.log"))
	if c.traceFile == nil {
		t.Fatal("no trace file")
	}
	_ = c.Close()
	if c.traceFile != nil {
		t.Error("Close left the trace file open")
	}
}

func TestUnknownFrameIsTracedBySizeOnly(t *testing.T) {
	c := NewClientTCP(1)
	path := filepath.Join(t.TempDir(), "client.log")
	c.SetLogPath(path)
	c.dispatch([]byte(`{"text":"secret-4321"}`))
	_ = c.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "unknown frame (22 bytes)") || strings.Contains(string(data), "secret") {
		t.Errorf("trace = %q", data)
	}
}
