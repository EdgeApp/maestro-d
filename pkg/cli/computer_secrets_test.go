package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb/rfbtest"
)

// TestComputerSecrets drives a computer through a real in-process daemon
// and then looks for either password everywhere the daemon keeps or
// answers anything: its home (reports, devices.json), the log, events, the
// device routes and the session's variables. The daemon never receives
// them — the driver reads computer.json itself — and this proves it.
func TestComputerSecrets(t *testing.T) {
	const vncPW, macPW = "vnc-zebra-91", "mac-quokka-37"
	s, cfgPath := computerEnv(t, rfbtest.Config{User: "paul", Password: macPW},
		`{"vncPassword": "`+vncPW+`", "macosUser": "paul", "macosPassword": "`+macPW+`"}`, 0o600)
	home := filepath.Dir(cfgPath)
	t.Setenv(daemon.EnvHome, home)
	if err := logger.Init(filepath.Join(home, "daemon.log")); err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	srv := daemon.New(daemon.Config{Name: "secrets", ReportsDir: filepath.Join(home, "reports")}, daemonDeps())
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	c := daemon.NewHTTPClient(hs.URL, "", nil)
	ctx := context.Background()

	var events bytes.Buffer
	evCtx, stopEvents := context.WithCancel(ctx)
	evDone := make(chan struct{})
	go func() {
		defer close(evDone)
		_ = c.Events(evCtx, "", 0, func(e daemon.Event) bool {
			b, _ := json.Marshal(e)
			events.Write(b)
			return true
		})
	}()

	if _, err := c.Attach(ctx, "mac", daemon.AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Command(ctx, "mac", "inputText", "hello", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Screenshot(ctx, "mac"); err != nil {
		t.Fatal(err)
	}
	var answers bytes.Buffer
	for _, f := range []func() (any, error){
		func() (any, error) { return c.Devices(ctx, "") },
		func() (any, error) { return c.Device(ctx, "mac") },
		func() (any, error) { return c.Vars(ctx, "mac") },
		func() (any, error) { return c.State(ctx, "mac") },
		func() (any, error) { return c.PlatformInfo(ctx, "mac") },
	} {
		v, err := f()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		answers.Write(b)
	}
	if err := c.Detach(ctx, "mac"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	stopEvents()
	<-evDone
	if s.Logins() != 1 {
		t.Fatalf("logins %d", s.Logins())
	}

	check := func(where string, data []byte) {
		for _, pw := range []string{vncPW, macPW} {
			if bytes.Contains(data, []byte(pw)) {
				t.Errorf("%s holds a password", where)
			}
		}
	}
	check("the answers", answers.Bytes())
	check("the events", events.Bytes())
	_ = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || p == cfgPath {
			return nil
		}
		data, _ := os.ReadFile(p)
		check(strings.TrimPrefix(p, home), data)
		return nil
	})
	if !strings.Contains(events.String(), "device.attached") {
		t.Errorf("no attach event seen: %s", events.String())
	}
}
