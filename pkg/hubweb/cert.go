package hubweb

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
)

func webTLSConfig(homeDir, advertise, certFile, keyFile string) (*tls.Config, string, error) {
	pair, err := dashboardCertificate(homeDir, advertise, certFile, keyFile)
	if err != nil {
		return nil, "", err
	}
	fingerprint := sha256.Sum256(pair.Certificate[0])
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, hex.EncodeToString(fingerprint[:]), nil
}

func dashboardCertificate(homeDir, advertise, certFile, keyFile string) (tls.Certificate, error) {
	if certFile != "" || keyFile != "" {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}
	host, err := os.Hostname()
	if err != nil {
		return tls.Certificate{}, err
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return tls.Certificate{}, err
	}
	dns, ips := certificateNames(host, advertise, addresses)
	return storedCertificate(filepath.Join(homeDir, "hub", "web"), dns, ips, time.Now())
}

func certificateNames(host, advertise string, addresses []net.Addr) ([]string, []netip.Addr) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	local := strings.TrimSuffix(host, ".local") + ".local"
	if u, err := url.Parse(advertise); err == nil && u.Host != "" {
		advertise = u.Hostname()
	} else if h, _, err := net.SplitHostPort(advertise); err == nil {
		advertise = h
	}
	var dns []string
	var ips []netip.Addr
	for _, name := range []string{host, local, advertise} {
		if name == "" {
			continue
		}
		if ip, err := netip.ParseAddr(strings.Trim(name, "[]")); err == nil {
			ips = append(ips, ip.WithZone("").Unmap())
		} else {
			dns = append(dns, strings.TrimSuffix(strings.ToLower(name), "."))
		}
	}
	for _, addr := range addresses {
		if prefix, err := netip.ParsePrefix(addr.String()); err == nil && !prefix.Addr().IsLoopback() {
			ips = append(ips, prefix.Addr().Unmap())
		}
	}
	slices.Sort(dns)
	slices.SortFunc(ips, netip.Addr.Compare)
	return slices.Compact(dns), slices.Compact(ips)
}

func storedCertificate(dir string, dns []string, ips []netip.Addr, now time.Time) (tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	lock, err := fsage.LockFile(filepath.Join(dir, ".cert.lock"))
	if err != nil {
		return tls.Certificate{}, err
	}
	defer fsage.UnlockFile(lock)

	crt, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if pair, err := tls.LoadX509KeyPair(crt, key); err == nil {
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err == nil && !now.Before(leaf.NotBefore) && now.Add(30*24*time.Hour).Before(leaf.NotAfter) &&
			slices.Equal(leaf.DNSNames, dns) && slices.EqualFunc(leaf.IPAddresses, ips, func(a net.IP, b netip.Addr) bool { return a.Equal(net.IP(b.AsSlice())) }) {
			return pair, nil
		}
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	start := now.Add(-5 * time.Minute).UTC()
	leaf := &x509.Certificate{
		SerialNumber: serial, NotBefore: start, NotAfter: start.Add(825 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: dns,
	}
	for _, ip := range ips {
		leaf.IPAddresses = append(leaf.IPAddresses, net.IP(ip.AsSlice()))
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	pk, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: pk})
	if err := writePEM(key, keyPEM); err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(crt, certPEM); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// A crash between renames leaves a mismatched pair, regenerated on next start.
func writePEM(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pem-*") // mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("install dashboard certificate file: %w", err)
	}
	return nil
}
