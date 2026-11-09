package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// fakeLaunchd replaces launchctl and the hub wait for a test.
type fakeLaunchd struct {
	calls []string
	fail  map[string]string
}

func useFakeLaunchd(t *testing.T) *fakeLaunchd {
	t.Helper()
	f := &fakeLaunchd{fail: map[string]string{}}
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv(daemon.EnvHome, home)
	oldGOOS, oldDir, oldCtl, oldUID, oldWait := serviceGOOS, launchAgentsDir, launchctl, serviceUID, waitForHub
	serviceGOOS = "darwin"
	launchAgentsDir = func() string { return filepath.Join(dir, "LaunchAgents") }
	launchctl = func(args ...string) ([]byte, error) {
		f.calls = append(f.calls, strings.Join(args, " "))
		if out, ok := f.fail[args[0]]; ok {
			return []byte(out), os.ErrPermission
		}
		if args[0] == "print" {
			return []byte("gui/501/app.edge.maestro-d.hub = {\n\tstate = running\n\tpid = 4242\n}\n"), nil
		}
		return nil, nil
	}
	serviceUID = func() int { return 501 }
	waitForHub = func(ctx context.Context, name string, timeout time.Duration) (*daemon.Client, error) {
		return &daemon.Client{Info: &daemon.Info{Name: name, Hub: "lab-a", PID: 4242, HTTPAddr: "[::]:7433"}}, nil
	}
	t.Cleanup(func() {
		serviceGOOS, launchAgentsDir, launchctl, serviceUID, waitForHub = oldGOOS, oldDir, oldCtl, oldUID, oldWait
	})
	return f
}

func hubFlags(t *testing.T, args ...string) *daemonOpts {
	t.Helper()
	o, _, err := parseDaemonArgs(args, hubStartFlags, nil)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestHubService_InstallPlistAndSecrets(t *testing.T) {
	f := useFakeLaunchd(t)
	t.Setenv("ANDROID_HOME", "/opt/android")
	t.Setenv("MAESTRO_TEAM_ID", "TEAM123")
	t.Setenv(daemon.EnvBin, "/opt/maestro-d/bin/maestro-d")
	o := hubFlags(t, "--name", "lab-a", "--listen", ":7433", "--share-device", "phone", "--token", "s3cret-client", "--peer-token", "s3cret-peer", "--no-mdns")
	if err := runServiceInstall(context.Background(), o, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(servicePlistPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("s3cret")) {
		t.Fatalf("a token is in the plist:\n%s", data)
	}
	var p struct {
		Label            string
		ProgramArguments []string
		RunAtLoad        bool
		KeepAlive        map[string]bool
		Env              map[string]string `plist:"EnvironmentVariables"`
		Out              string            `plist:"StandardOutPath"`
		Err              string            `plist:"StandardErrorPath"`
	}
	if _, err := plist.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(p.ProgramArguments, " ")
	home := os.Getenv(daemon.EnvHome)
	secrets := filepath.Join(home, "hub", "secrets")
	for _, want := range []string{"/opt/maestro-d/bin/maestro-d hub start --foreground --daemon hub --name lab-a --listen :7433",
		"--no-mdns", "--share-device phone", "--token-file " + filepath.Join(secrets, "token"), "--peer-token-file " + filepath.Join(secrets, "peer-token")} {
		if !strings.Contains(args, want) {
			t.Errorf("ProgramArguments %q lack %q", args, want)
		}
	}
	if p.Label != ServiceLabel || !p.RunAtLoad || p.KeepAlive["SuccessfulExit"] != false || len(p.KeepAlive) != 1 {
		t.Fatalf("plist: %+v", p)
	}
	if p.Env[daemon.EnvHome] != home || p.Env["ANDROID_HOME"] != "/opt/android" || p.Env["MAESTRO_TEAM_ID"] != "TEAM123" || p.Env["PATH"] == "" {
		t.Fatalf("environment: %v", p.Env)
	}
	if p.Out != filepath.Join(home, "run", "hub", "daemon.log") || p.Err != p.Out {
		t.Fatalf("log paths: %s %s", p.Out, p.Err)
	}
	for name, want := range map[string]string{"token": "s3cret-client\n", "peer-token": "s3cret-peer\n"} {
		st, err := os.Stat(filepath.Join(secrets, name))
		got, _ := os.ReadFile(filepath.Join(secrets, name))
		if err != nil || st.Mode().Perm() != 0o600 || string(got) != want {
			t.Fatalf("%s: %v %v %q", name, st, err, got)
		}
	}
	if len(f.calls) != 1 || f.calls[0] != "bootstrap gui/501 "+servicePlistPath() {
		t.Fatalf("launchctl: %v", f.calls)
	}
	// Installing again boots the old agent out first.
	if err := runServiceInstall(context.Background(), hubFlags(t, "--token", "a", "--peer-token", "b"), nil); err != nil {
		t.Fatal(err)
	}
	if f.calls[1] != "bootout gui/501/"+ServiceLabel || !strings.HasPrefix(f.calls[2], "bootstrap ") {
		t.Fatalf("reinstall: %v", f.calls)
	}

	// hub start goes through launchd while the service is installed.
	f.calls = nil
	if err := runHubStart(context.Background(), hubFlags(t), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != "kickstart gui/501/"+ServiceLabel {
		t.Fatalf("kickstart: %v", f.calls)
	}
	f.fail["kickstart"] = "boom"
	if err := runHubStart(context.Background(), hubFlags(t), nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("kickstart failure: %v", err)
	}

	// status prints the launchd state.
	out := captureStdout(t, func() {
		if err := runServiceStatus(context.Background(), hubFlags(t), nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "launchd state running, pid 4242") || !strings.Contains(out, `hub "hub" is not running`) {
		t.Fatalf("status: %s", out)
	}

	// uninstall without --purge keeps the secrets; with it they go.
	f.calls = nil
	o = hubFlags(t)
	if err := runServiceUninstall(context.Background(), o, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(servicePlistPath()); !os.IsNotExist(err) {
		t.Fatal("plist left")
	}
	if _, err := os.Stat(filepath.Join(secrets, "token")); err != nil {
		t.Fatal("secrets removed without --purge")
	}
	if f.calls[0] != "bootout gui/501/"+ServiceLabel {
		t.Fatalf("bootout: %v", f.calls)
	}
	out = captureStdout(t, func() {
		o, _, _ := parseDaemonArgs([]string{"--purge"}, hubServiceUninstallFlags, nil)
		if err := runServiceUninstall(context.Background(), o, nil); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(secrets); !os.IsNotExist(err) || !strings.Contains(out, "was not installed") || !strings.Contains(out, "deleted") {
		t.Fatalf("purge: %v\n%s", err, out)
	}
	out = captureStdout(t, func() {
		if err := runServiceStatus(context.Background(), hubFlags(t), nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "not installed") {
		t.Fatalf("status after uninstall: %s", out)
	}
	// A failing bootstrap is reported.
	f.fail["bootstrap"] = "Bootstrap failed: 5"
	if err := runServiceInstall(context.Background(), hubFlags(t, "--token", "a", "--peer-token", "b"), nil); err == nil || !strings.Contains(err.Error(), "Bootstrap failed") {
		t.Fatalf("bootstrap failure: %v", err)
	}
	f.fail["print"] = "Could not find service"
	if err := runServiceStatus(context.Background(), hubFlags(t), nil); err != nil {
		t.Fatal(err)
	}
	uo, _, _ := parseDaemonArgs([]string{"--json"}, hubServiceUninstallFlags, nil)
	if err := runServiceUninstall(context.Background(), uo, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHubService_RefusedOffMacOS(t *testing.T) {
	useFakeLaunchd(t)
	serviceGOOS = "linux"
	for _, run := range []func(context.Context, *daemonOpts, []string) error{runServiceInstall, runServiceUninstall, runServiceStatus} {
		if err := run(context.Background(), hubFlags(t, "--token", "a", "--peer-token", "b"), nil); err == nil || !strings.Contains(err.Error(), "systemd") {
			t.Fatalf("linux: %v", err)
		}
	}
	if serviceInstalled() {
		t.Fatal("installed on linux")
	}
	if st, pid := launchdState("x\n\tstate = waiting\n"); st != "waiting" || pid != "" {
		t.Fatal("launchdState")
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r)
		done <- b.String()
	}()
	fn()
	os.Stdout = old
	_ = w.Close()
	return <-done
}

func TestHub_FlagsAndTokens(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "peer")
	_ = os.WriteFile(f, []byte("p33r\n"), 0o600)
	o := hubFlags(t, "--token", "t", "--peer-token-file", f)
	tok, peer, err := checkHubFlags(o)
	if err != nil || tok != "t" || peer != "p33r" || o.HTTPAddr != ":7433" || o.hub.name == "" {
		t.Fatalf("defaults: %q %q %v %+v", tok, peer, err, o.hub)
	}
	for _, args := range [][]string{
		{"--token", "t"},
		{"--token", "t", "--peer-token", "p", "--peer-token-file", f},
		{"--token", "t", "--peer-token", "p", "--name", "Not Valid"},
		{"--token", "t", "--peer-token", "p", "--tls-cert", "c"},
		{"--token", "t", "--peer-token-file", filepath.Join(dir, "missing")},
	} {
		if _, _, err := checkHubFlags(hubFlags(t, args...)); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	_ = os.Chmod(f, 0o644)
	if _, err := readSecretFile("peer-token-file", f); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("readable: %v", err)
	}
	_ = os.Chmod(f, 0o200)
	if _, err := readSecretFile("peer-token-file", f); err == nil {
		t.Fatal("unreadable file")
	}
	if p, err := peerCAPool(""); p != nil || err != nil {
		t.Fatal("no --peer-ca")
	}
	if _, err := peerCAPool(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("missing CA")
	}
	notPEM := filepath.Join(dir, "x.pem")
	_ = os.WriteFile(notPEM, []byte("x"), 0o600)
	if _, err := peerCAPool(notPEM); err == nil {
		t.Fatal("not PEM")
	}
	caFile := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(caFile, selfSignedPEM(t), 0o600)
	if p, err := peerCAPool(caFile); err != nil || p == nil {
		t.Fatalf("CA: %v", err)
	}
	// A bad CA stops a foreground hub before it starts.
	o = hubFlags(t, "--token", "t", "--peer-token", "p", "--peer-ca", notPEM, "--foreground", "--daemon", "never")
	if err := runHubStart(context.Background(), o, nil); err == nil {
		t.Fatal("bad --peer-ca")
	}
}

func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "hub-test"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestHub_StatusPrinting(t *testing.T) {
	two := 2
	var b bytes.Buffer
	printHubStatus(&b, daemon.HubStatus{Name: "lab-a", Version: "1.0", APIVersion: "1", Listen: "[::]:7433", MDNS: false, Addr: "10.0.0.1:7433",
		Peers: []daemon.HubPeer{
			{Name: "lab-b", Addr: "10.0.0.2:7433", State: "linked", Direction: "outbound", RTTMs: 3.25, Devices: &two},
			{Addr: "10.0.0.9:7433", State: "refused", Error: "hub name taken"},
		}})
	out := b.String()
	for _, want := range []string{"hub lab-a: maestro-d 1.0, API 1, listening on [::]:7433 (no mDNS)", "advertised as 10.0.0.1:7433",
		"lab-b", "linked", "outbound", "3.2ms", "?", "refused", "(hub name taken)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	printHubStatus(&b, daemon.HubStatus{Name: "solo", MDNS: true})
	if !strings.Contains(b.String(), "mDNS at launch") || !strings.Contains(b.String(), "No peers.") {
		t.Fatalf("solo: %s", b.String())
	}
	// hub status with no hub running is DAEMON_UNAVAILABLE.
	t.Setenv(daemon.EnvHome, t.TempDir())
	o, _, _ := parseDaemonArgs(nil, hubStatusFlags, nil)
	if err := runHubStatus(context.Background(), o, nil); !isCode(err, daemon.CodeDaemonUnavailable) {
		t.Fatalf("no hub: %v", err)
	}
}
