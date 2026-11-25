package common_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/hub"
)

func TestParseTarget_Errors(t *testing.T) {
	for _, v := range []string{"ftp://h", "", "h/path", "h:0", "h:99999", "h:x", "user@h"} {
		if _, _, err := common.ParseTarget(v); err == nil {
			t.Errorf("%q: expected an error", v)
		}
	}
	// --target-hub and --peer read an address one way.
	for _, v := range []string{"h", "h:9", "https://h", "HTTP://h:1/", "::1", "[::1]:9", "user@h", "h h", "h:0", "ftp://h", ":9"} {
		_, _, terr := common.ParseTarget(v)
		_, perr := hub.ParseAddr(v, hub.DefaultPort)
		if (terr == nil) != (perr == nil) {
			t.Errorf("%q: --target-hub %v, --peer %v", v, terr, perr)
		}
	}
}

func TestTargetHub_TLSTrustRoot(t *testing.T) {
	if c, err := common.TargetTLSConfig(""); c != nil || err != nil {
		t.Fatal("no --ca: system roots")
	}
	if _, err := common.TargetTLSConfig("/no/such/ca.pem"); err == nil {
		t.Fatal("missing file")
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	_ = os.WriteFile(junk, []byte("not a certificate"), 0o644)
	if _, err := common.TargetTLSConfig(junk); err == nil {
		t.Fatal("no PEM in the file")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	c, err := common.TargetTLSConfig(ca)
	if err != nil || c == nil || c.RootCAs == nil {
		t.Fatalf("with --ca: %v", err)
	}
	client, err := common.ConnectTarget("https://h:9", ca, "", true)
	if err != nil || client.BaseURL() != "https://h:9" {
		t.Fatalf("connectTarget: %v", err)
	}
	daemon.RetryLog = nil
	if _, err := common.ConnectTarget("https://h", "/no/such", "", false); err == nil {
		t.Fatal("a missing --ca fails before any request")
	}
	if _, err := common.ConnectTarget("ftp://h", "", "", false); err == nil {
		t.Fatal("bad target")
	}
	if _, err := common.ConnectTarget("h", ca, "", true); err != nil {
		t.Fatal("--ca with http:// is only noted")
	}
	daemon.RetryLog = nil
}
