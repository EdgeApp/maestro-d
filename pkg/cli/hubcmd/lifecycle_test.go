package hubcmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestHubStart_Helpers(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	o := &daemonOpts{Name: "hub", HTTPAddr: ":7433", TLSCert: "c.pem", TLSKey: "k.pem", PassEnv: []string{"A"},
		hub: hubOpts{name: "lab-a", share: []string{"x", "y"}, remoteShell: true, peers: []string{"hub-b:7433"}, noMDNS: true,
			advertise: "10.0.0.1:7433", peerCA: "ca.pem", peerToken: "p3er"}}
	args := strings.Join(hubChildArgs(o), " ")
	for _, want := range []string{"hub start --foreground --daemon hub --name lab-a --listen :7433", "--share-device x --share-device y",
		"--allow-remote-shell", "--pass-env A", "--tls-cert /", "--peer hub-b:7433", "--no-mdns", "--advertise 10.0.0.1:7433", "--peer-ca /"} {
		if !strings.Contains(args, want) {
			t.Errorf("child args %q lack %q", args, want)
		}
	}
	if strings.Contains(args, "token") {
		t.Fatalf("child args carry a token: %s", args)
	}
	got := redactArgs([]string{"hub", "start", "--token", "s3cret", "--token=s3cret", "--peer-token", "s3cret", "--peer-token=s3cret", "--listen", ":1"})
	if strings.Contains(strings.Join(got, " "), "s3cret") {
		t.Fatalf("redact: %v", got)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	_ = os.WriteFile(f, []byte(" tok\n"), 0o600)
	o = &daemonOpts{hub: hubOpts{tokenFile: f}, explicit: map[string]bool{}}
	if tok, err := hubToken(o); err != nil || tok != "tok" {
		t.Fatalf("token file: %q %v", tok, err)
	}
	o.explicit["token"] = true
	if _, err := hubToken(o); err == nil {
		t.Fatal("--token and --token-file together")
	}
	o = &daemonOpts{hub: hubOpts{tokenFile: filepath.Join(dir, "missing")}}
	if _, err := hubToken(o); err == nil {
		t.Fatal("missing token file")
	}
	if _, err := hubToken(&daemonOpts{}); err == nil {
		t.Fatal("no token")
	}
	o = &daemonOpts{explicit: map[string]bool{}}
	hubName(o)
	if o.Name != DefaultHubDaemon {
		t.Fatal(o.Name)
	}
	if err := runHubStart(context.Background(), &daemonOpts{Token: "t", TLSCert: "c", explicit: map[string]bool{}}, nil); err == nil {
		t.Fatal("--tls-cert without --tls-key")
	}
}

func TestHubLeaseDurations(t *testing.T) {
	hubMax, devs, err := parseLeaseMax([]string{"2h", "emulator-5554=10m", "Pixel 8=1800", "4h"})
	if err != nil || hubMax != 4*time.Hour || len(devs) != 2 || devs[0] != (daemon.LeaseMax{Device: "emulator-5554", Max: 10 * time.Minute}) ||
		devs[1] != (daemon.LeaseMax{Device: "Pixel 8", Max: 30 * time.Minute}) {
		t.Fatalf("lease-max: %v %+v %v", hubMax, devs, err)
	}
	for _, bad := range []string{"=10m", "x=", "x=never", "never"} {
		if _, _, err := parseLeaseMax([]string{bad}); err == nil {
			t.Errorf("--lease-max %q accepted", bad)
		}
	}
	args := strings.Join(hubChildArgs(&daemonOpts{hub: hubOpts{leaseMax: []string{"1h", "a=5m"}}}), " ")
	if !strings.Contains(args, "--lease-max 1h --lease-max a=5m") {
		t.Fatalf("child args: %s", args)
	}
	if _, _, err := checkHubFlags(&daemonOpts{Token: "t", hub: hubOpts{peerToken: "p", leaseMax: []string{"x=y"}}, explicit: map[string]bool{}}); err == nil {
		t.Fatal("a bad --lease-max starts a hub")
	}
}
