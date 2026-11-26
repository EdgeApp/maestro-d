package hubcmd

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestHubStopEndLeases(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	dir, err := os.MkdirTemp("", "hs-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hub" {
			daemon.WriteJSON(w, 200, daemon.Envelope{OK: true})
			return
		}
		got <- r.Method + " " + r.URL.RequestURI()
		daemon.WriteError(w, daemon.Forbidden("test: stop here before waiting for process exit"))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	if err := os.MkdirAll(daemon.HubRunDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteInfo(&daemon.Info{Name: "hub", RunDir: daemon.HubRunDir(), SocketPath: path, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	o, args, err := parseDaemonArgs([]string{"--end-leases"}, hubStopFlags, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = runHubStop(t.Context(), o, args.Positional)
	requireCode(t, err, daemon.CodeForbidden, 2)
	if req := <-got; req != "POST /v1/shutdown?endLeases=true" {
		t.Fatal(req)
	}
}
