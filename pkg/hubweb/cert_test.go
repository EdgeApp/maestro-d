package hubweb

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestCertificateNames(t *testing.T) {
	var addrs []net.Addr
	for _, prefix := range []string{"127.0.0.1/8", "::1/128", "192.0.2.42/24", "2001:db8::8/64", "192.0.2.42/32"} {
		ip, network, err := net.ParseCIDR(prefix)
		if err != nil {
			t.Fatal(err)
		}
		network.IP = ip
		addrs = append(addrs, network)
	}
	dns, ips := certificateNames("Bench", "Mesh.Example:7433", addrs)
	wantIPs := []netip.Addr{netip.MustParseAddr("192.0.2.42"), netip.MustParseAddr("2001:db8::8")}
	if !slices.Equal(dns, []string{"bench", "bench.local", "mesh.example"}) || !slices.Equal(ips, wantIPs) {
		t.Fatal(dns, ips)
	}
	dns, ips = certificateNames("bench.local", "[2001:db8::8]:7433", addrs)
	if !slices.Equal(dns, []string{"bench.local"}) || !slices.Equal(ips, wantIPs) {
		t.Fatal(dns, ips)
	}
	dns, ips = certificateNames("bench", "https://mesh.example:7433", addrs)
	if !slices.Equal(dns, []string{"bench", "bench.local", "mesh.example"}) || !slices.Equal(ips, wantIPs) {
		t.Fatal(dns, ips)
	}

}

func TestStoredCertificate(t *testing.T) {
	dir := t.TempDir()
	dns := []string{"hub", "hub.local"}
	ips := []netip.Addr{netip.MustParseAddr("192.0.2.4")}
	now := time.Now().UTC().Truncate(time.Second)
	read := func(when time.Time) tls.Certificate {
		t.Helper()
		cert, err := storedCertificate(dir, dns, ips, when)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	first := read(now)
	leaf, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() || leaf.NotAfter.Sub(leaf.NotBefore) != 825*24*time.Hour || leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) != nil {
		t.Fatal(leaf)
	}
	for _, host := range append(slices.Clone(dns), ips[0].String()) {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"cert.pem", "key.pem"} {
		stat, err := os.Stat(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && stat.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o", file, stat.Mode().Perm())
		}
	}
	if !bytes.Equal(first.Certificate[0], read(leaf.NotAfter.Add(-31 * 24 * time.Hour)).Certificate[0]) {
		t.Fatal("replaced an unexpired matching certificate")
	}
	second := read(leaf.NotAfter.Add(-30 * 24 * time.Hour))
	if bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("renewal did not replace the certificate")
	}
	when := leaf.NotAfter.Add(-29 * 24 * time.Hour)
	ips[0] = netip.MustParseAddr("192.0.2.5")
	third := read(when)
	if bytes.Equal(second.Certificate[0], third.Certificate[0]) {
		t.Fatal("IP change did not regenerate certificate")
	}
	dns[0] = "different"
	if bytes.Equal(third.Certificate[0], read(when).Certificate[0]) {
		t.Fatal("hostname change did not regenerate certificate")
	}
}

func TestWebTLSConfig(t *testing.T) {
	home := t.TempDir()
	config, fingerprint, err := webTLSConfig(home, "bench.example:9400", "", "")
	if err != nil {
		t.Fatal(err)
	}
	der := config.Certificates[0].Certificate[0]
	sum := sha256.Sum256(der)
	if config.MinVersion != tls.VersionTLS13 || fingerprint != hex.EncodeToString(sum[:]) {
		t.Fatal(config.MinVersion, fingerprint)
	}
	dir := filepath.Join(home, "hub", "web")
	configured, fp, err := webTLSConfig(t.TempDir(), "", filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil || fp != fingerprint || !bytes.Equal(der, configured.Certificates[0].Certificate[0]) {
		t.Fatal(fp, err)
	}
	if _, _, err := webTLSConfig(home, "", filepath.Join(dir, "missing"), ""); err == nil {
		t.Fatal("missing configured certificate accepted")
	}
	if _, _, err := webTLSConfig(filepath.Join(dir, "key.pem"), "", "", ""); err == nil {
		t.Fatal("non-directory certificate home accepted")
	}
}

func TestInterruptedCertificateRotation(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old, err := storedCertificate(dir, []string{"hub"}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("interrupted write"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Concurrent hub starts must serve the same replacement fingerprint.
	results := make(chan tls.Certificate, 4)
	errs := make(chan error, 4)
	for range cap(results) {
		go func() {
			pair, err := storedCertificate(dir, []string{"hub"}, nil, now)
			if err != nil {
				errs <- err
				return
			}
			results <- pair
		}()
	}
	var replacement []byte
	for range cap(results) {
		select {
		case err := <-errs:
			t.Fatal(err)
		case pair := <-results:
			if bytes.Equal(pair.Certificate[0], old.Certificate[0]) || (replacement != nil && !bytes.Equal(pair.Certificate[0], replacement)) {
				t.Fatal("inconsistent replacement")
			}
			replacement = pair.Certificate[0]
		}
	}
}

func TestCertificateStorageErrors(t *testing.T) {
	for _, path := range []string{".cert.lock", "cert.pem", "key.pem"} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, path), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := storedCertificate(dir, []string{"hub"}, nil, time.Now()); err == nil {
			t.Fatalf("replaced directory %s", path)
		}
	}
	if err := writePEM(filepath.Join(t.TempDir(), "absent", "cert.pem"), nil); err == nil {
		t.Fatal("wrote to absent directory")
	}
}
