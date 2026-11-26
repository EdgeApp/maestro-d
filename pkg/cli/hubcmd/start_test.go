package hubcmd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestHubWebPort(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	t.Setenv("MAESTRO_D_HUB_WEB_PORT", "9876")
	flags := []string{"--name", "bench", "--token", "test", "--peer-token", "peer", "--no-mdns", "--listen", "127.0.0.1:0", "--foreground"}
	if p := hubFlags(t, flags...).hub.webPort; p == nil || *p != 9876 {
		t.Fatal("environment did not set web port")
	}
	for _, invalid := range []string{"-1", "65536"} {
		o := hubFlags(t, append(flags, "--web-port", invalid)...)
		if _, _, err := checkHubFlags(o); err == nil || !strings.Contains(err.Error(), "--web-port") {
			t.Fatal(invalid, err)
		}
	}
	o := hubFlags(t, append(flags, "--web-port", "0")...)
	if p := o.hub.webPort; p == nil || *p != 0 {
		t.Fatal("explicit zero did not override environment")
	}
	log := filepath.Join(t.TempDir(), "stdout")
	out, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	prior := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = prior; out.Close() }()
	start := make(chan error, 1)
	go func() { start <- runHubStart(context.Background(), o, nil) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var client *daemon.Client
	for client == nil {
		select {
		case err := <-start:
			t.Fatal("start returned", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
			client, err = daemon.DialHub()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	defer func() {
		if err := client.ShutdownHub(ctx, false); err != nil {
			t.Error(err)
		}
		select {
		case err := <-start:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error(ctx.Err())
		}
	}()
	addr := client.Info.WebAddr
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || port == "0" || port == "9400" {
		t.Fatal(addr, err)
	}
	if err := runHubStatus(ctx, &daemonOpts{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := printHub(&daemonOpts{}, client.Info, "running"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(body), "web=https://"+addr) || strings.Count(string(body), "dashboard https://"+daemon.MachineName()+":"+port+"/") != 2 {
		t.Fatalf("port %s; %s; %v", port, body, err)
	}
}

func TestHubDefaultWebPortOccupied(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	t.Setenv("MAESTRO_D_HUB_WEB_PORT", "")
	occupied, err := net.Listen("tcp", ":9400")
	if err != nil {
		t.Skipf("cannot reserve default port for occupied-port test: %v", err)
	}
	defer occupied.Close()
	o := hubFlags(t, "--token", "test", "--peer-token", "peer", "--no-mdns", "--listen", "127.0.0.1:0", "--foreground")
	err = runHubStart(t.Context(), o, nil)
	requireCode(t, err, daemon.CodeUsage, 2)
	if !strings.Contains(err.Error(), "--web-port 9400") {
		t.Fatal(err)
	}
}
