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
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/hub"
)

// oneShotParse parses a one-shot command line the way runOneShot does.
func oneShotParse(t *testing.T, name string, args ...string) (*daemonOpts, *parsedArgs) {
	t.Helper()
	keys := map[string]string{}
	for _, f := range daemon.CommandSpecs[name].Fields {
		keys[f.Key] = f.Type
	}
	o, p, err := parseDaemonArgs(args, oneShotFlags, keys)
	if err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return o, p
}

func clearTargetEnv(t *testing.T) {
	for _, k := range []string{"MAESTRO_D_HUB", "MAESTRO_D_TOKEN", "MAESTRO_D_CA", "MAESTRO_OUTPUT", "APPIUM_CAPS"} {
		t.Setenv(k, "")
	}
}

func TestTargetHub_OptionalValue(t *testing.T) {
	clearTargetEnv(t)
	cases := []struct {
		args    []string
		base    string
		tls     bool
		textArg string
	}{
		{[]string{"--target-hub"}, "http://localhost:7433", false, ""},
		{[]string{"--target-hub=h"}, "http://h:7433", false, ""},
		{[]string{"--target-hub=h:9"}, "http://h:9", false, ""},
		{[]string{"--target-hub=https://h:9"}, "https://h:9", true, ""},
		{[]string{"--target-hub=https://h"}, "https://h:7433", true, ""},
		{[]string{"--target-hub=[::1]:9"}, "http://[::1]:9", false, ""},
		{[]string{"--target-hub=::1"}, "http://[::1]:7433", false, ""},
		{[]string{"--target-hub", "Login"}, "http://localhost:7433", false, "Login"},
		{[]string{"Login", "--target-hub"}, "http://localhost:7433", false, "Login"},
	}
	for _, tc := range cases {
		o, p := oneShotParse(t, "tapOn", tc.args...)
		base, useTLS, err := parseTarget(o.Target)
		if err != nil || base != tc.base || useTLS != tc.tls {
			t.Errorf("%v: %s tls=%v err=%v", tc.args, base, useTLS, err)
		}
		if got := strings.Join(p.positional, " "); got != tc.textArg {
			t.Errorf("%v: positional %q, want %q", tc.args, got, tc.textArg)
		}
	}
	// tapOn --target-hub Login: the text is the tapOn value, not the hub.
	o, p := oneShotParse(t, "tapOn", "--target-hub", "Login", "--device", "d")
	v, err := buildStepValue(daemon.CommandSpecs["tapOn"], p)
	if err != nil || v != "Login" || o.Device != "d" {
		t.Fatalf("value %v err %v device %q", v, err, o.Device)
	}
	if !strings.Contains(flagsUsage(targetFlags), "--target-hub[=HOST[:PORT]]") {
		t.Fatalf("usage:\n%s", flagsUsage(targetFlags))
	}
}

func TestTargetHub_EnvironmentPrecedence(t *testing.T) {
	clearTargetEnv(t)
	t.Setenv("MAESTRO_D_HUB", "envhub:1")
	t.Setenv("MAESTRO_D_TOKEN", "envtok")
	o, _ := oneShotParse(t, "tapOn", "X")
	if o.Target != "envhub:1" || o.Token != "envtok" {
		t.Fatalf("env: %q %q", o.Target, o.Token)
	}
	o, _ = oneShotParse(t, "tapOn", "X", "--target-hub=flaghub", "--token", "flagtok")
	if o.Target != "flaghub" || o.Token != "flagtok" {
		t.Fatalf("the flag wins: %q %q", o.Target, o.Token)
	}
	clearTargetEnv(t)
	o, _ = oneShotParse(t, "tapOn", "X")
	if o.Target != "" {
		t.Fatal("no flag, no variable: the local daemon")
	}
}

func TestParseTarget_Errors(t *testing.T) {
	for _, v := range []string{"ftp://h", "", "h/path", "h:0", "h:99999", "h:x", "user@h"} {
		if _, _, err := parseTarget(v); err == nil {
			t.Errorf("%q: expected an error", v)
		}
	}
	// --target-hub and --peer read an address one way.
	for _, v := range []string{"h", "h:9", "https://h", "HTTP://h:1/", "::1", "[::1]:9", "user@h", "h h", "h:0", "ftp://h", ":9"} {
		_, _, terr := parseTarget(v)
		_, perr := hub.ParseAddr(v, hub.DefaultPort)
		if (terr == nil) != (perr == nil) {
			t.Errorf("%q: --target-hub %v, --peer %v", v, terr, perr)
		}
	}
}

func TestTargetHub_TLSTrustRoot(t *testing.T) {
	if c, err := targetTLSConfig(""); c != nil || err != nil {
		t.Fatal("no --ca: system roots")
	}
	if _, err := targetTLSConfig("/no/such/ca.pem"); err == nil {
		t.Fatal("missing file")
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	_ = os.WriteFile(junk, []byte("not a certificate"), 0o644)
	if _, err := targetTLSConfig(junk); err == nil {
		t.Fatal("no PEM in the file")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	c, err := targetTLSConfig(ca)
	if err != nil || c == nil || c.RootCAs == nil {
		t.Fatalf("with --ca: %v", err)
	}
	o := &daemonOpts{Target: "https://h:9", CAFile: ca, Verbose: true}
	client, err := connectTarget(o)
	if err != nil || client.BaseURL() != "https://h:9" {
		t.Fatalf("connectTarget: %v", err)
	}
	daemon.RetryLog = nil
	if _, err := connectTarget(&daemonOpts{Target: "https://h", CAFile: "/no/such"}); err == nil {
		t.Fatal("a missing --ca fails before any request")
	}
	if _, err := connectTarget(&daemonOpts{Target: "ftp://h"}); err == nil {
		t.Fatal("bad target")
	}
	if _, err := connectTarget(&daemonOpts{Target: "h", CAFile: ca, Verbose: true}); err != nil {
		t.Fatal("--ca with http:// is only noted")
	}
	daemon.RetryLog = nil
}

func TestTargetHub_NeverSpawns(t *testing.T) {
	clearTargetEnv(t)
	t.Setenv(daemon.EnvHome, t.TempDir())
	o := &daemonOpts{Name: "default", Target: "127.0.0.1:1"}
	c, err := connectDaemon(context.Background(), o, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("nothing listens on port 1")
	} else if de, ok := err.(*daemon.Error); !ok || de.ExitCode() != 3 {
		t.Fatalf("unreachable target: %v", err)
	}
	if entries, _ := daemon.ListDaemons(); len(entries) != 0 {
		t.Fatalf("a targeted command spawned a daemon: %+v", entries)
	}
	if got := targetLabel(o); got != "http://127.0.0.1:1" {
		t.Fatalf("label %q", got)
	}
	if got := targetLabel(&daemonOpts{Target: "ftp://x"}); got != "ftp://x" {
		t.Fatalf("label %q", got)
	}
	if got := targetLabel(&daemonOpts{Name: "n"}); got != `daemon "n"` {
		t.Fatalf("label %q", got)
	}
}

func TestTargetHub_RefusesHostDirectories(t *testing.T) {
	clearTargetEnv(t)
	for _, flag := range []string{"--output-dir", "--user-data-dir"} {
		_, _, err := parseDaemonArgs([]string{"--target-hub", flag, "/tmp/x"}, attachCmdFlags, nil)
		if err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("%s: %v", flag, err)
		}
		if _, _, err := parseDaemonArgs([]string{flag, "/tmp/x"}, attachCmdFlags, nil); err != nil {
			t.Errorf("%s without a target: %v", flag, err)
		}
	}
	// MAESTRO_OUTPUT from a shell set up for local runs is dropped, with a notice.
	t.Setenv("MAESTRO_OUTPUT", "/tmp/reports")
	stderr := captureStderr(t, func() {
		o, _, err := parseDaemonArgs([]string{"--target-hub=h", "--app-id", "x"}, attachCmdFlags, nil)
		if err != nil {
			t.Fatal(err)
		}
		if o.Attach.OutputDir != "" {
			t.Fatalf("outputDir %q kept", o.Attach.OutputDir)
		}
		for _, n := range o.AttachSet {
			if n == "output-dir" {
				t.Fatal("output-dir still marked as set")
			}
		}
	})
	if !strings.Contains(stderr, "ignoring MAESTRO_OUTPUT") {
		t.Fatalf("notice: %q", stderr)
	}
	o, _, _ := parseDaemonArgs(nil, attachCmdFlags, nil)
	if o.Attach.OutputDir != "/tmp/reports" {
		t.Fatal("a local command keeps MAESTRO_OUTPUT")
	}
}

// A hub attaches no Appium for a remote caller (5.harness-review.1), so a
// targeted command refuses an Appium capabilities file, from the flag or
// the environment, before it sends anything; a local command keeps it.
func TestTargetHub_RefusesAppiumCaps(t *testing.T) {
	clearTargetEnv(t)
	for _, flags := range [][]dflag{attachCmdFlags, leaseFlags, oneShotFlags} {
		_, _, err := parseDaemonArgs([]string{"--target-hub=h", "--caps", "caps.json"}, flags, nil)
		if de, ok := err.(*daemon.Error); !ok || de.Code != daemon.CodeUsage || !strings.Contains(err.Error(), "--caps") {
			t.Errorf("--caps: %v", err)
		}
	}
	o, _, err := parseDaemonArgs([]string{"--caps", "caps.json"}, attachCmdFlags, nil)
	if err != nil || o.Attach.Extra["capsFile"] != "caps.json" {
		t.Fatalf("a local --caps: %v", err)
	}
	t.Setenv("APPIUM_CAPS", "caps.json")
	_, _, err = parseDaemonArgs([]string{"--target-hub=h"}, attachCmdFlags, nil)
	if de, ok := err.(*daemon.Error); !ok || de.Code != daemon.CodeUsage || !strings.Contains(err.Error(), "APPIUM_CAPS") {
		t.Errorf("APPIUM_CAPS: %v", err)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	_ = w.Close()
	var b bytes.Buffer
	_, _ = io.Copy(&b, r)
	return b.String()
}

func TestFlagSet_ListsAFlagOnce(t *testing.T) {
	n := 0
	for _, f := range oneShotFlags {
		if f.name == "token" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("token listed %d times", n)
	}
	for _, flags := range [][]dflag{statusFlags, deviceListFlags, deviceStartFlags, deviceStopFlags, attachCmdFlags,
		detachFlags, runFlags, getFlags, setFlags, evalFlags, oneShotFlags, deviceCatalogFlags, deviceCreateFlags, deviceDeleteFlags} {
		found := false
		for _, f := range flags {
			found = found || f.name == "target-hub"
		}
		if !found {
			t.Errorf("a client command lacks --target-hub: %v", flags[0].name)
		}
	}
}

func TestServeFlags_TLSAndPassEnv(t *testing.T) {
	clearTargetEnv(t)
	o, _ := parseFor(t, serveFlags, "--http", "0.0.0.0:7433", "--token", "t", "--tls-cert", "c.pem", "--tls-key", "k.pem",
		"--pass-env", "A", "--pass-env", "B")
	if o.TLSCert != "c.pem" || o.TLSKey != "k.pem" || strings.Join(o.PassEnv, ",") != "A,B" || o.HTTPAddr != "0.0.0.0:7433" {
		t.Fatalf("%+v", o)
	}
}
