package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// tlsFiles writes a self-signed certificate for 127.0.0.1 and its key, and
// returns a pool that trusts it.
func tlsFiles(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	kder, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return certFile, keyFile, pool
}

func withTLS(certFile, keyFile string, pool *x509.CertPool) hubOpt {
	return func(c *Config, d *daemon.Config) {
		d.TLSCert, d.TLSKey = certFile, keyFile
		c.PeerCA = pool
	}
}

// plainRecorder is a plain HTTP listener that records every request (and
// its Authorization header) that reaches it.
type plainRecorder struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
}

func newPlainRecorder(t *testing.T) *plainRecorder {
	t.Helper()
	p := &plainRecorder{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.reqs = append(p.reqs, r.URL.Path+" auth="+r.Header.Get("Authorization"))
		p.mu.Unlock()
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *plainRecorder) addr() string { return strings.TrimPrefix(p.srv.URL, "http://") }

func (p *plainRecorder) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.reqs...)
}

// A hub that serves TLS never dials a peer over plain ws://: an mDNS answer
// with tls=0, an address from a peer's list, an explicit http:// --peer and
// a bulk dial all go over TLS, so a plain listener never sees the peer
// token. A real TLS peer still links.
func TestMesh_TLSHubNeverDialsPlain(t *testing.T) {
	setupHome(t)
	certFile, keyFile, pool := tlsFiles(t)
	b := startHub(t, "hub-b", testDevices("b"), withTLS(certFile, keyFile, pool))

	mdnsRogue, listRogue, staticRogue, bulkRogue := newPlainRecorder(t), newPlainRecorder(t), newPlainRecorder(t), newPlainRecorder(t)
	disco := &fakeDisco{found: []Found{
		{Name: "rogue", Addr: mdnsRogue.addr()}, // tls=0 in its TXT
		{Name: "hub-b", Addr: "https://" + b.addr},
	}}
	a := startHub(t, "hub-a", testDevices("a"), withTLS(certFile, keyFile, pool), withDisco(disco),
		withPeers("http://"+staticRogue.addr()))

	waitLinked(t, a, "hub-b")
	if st, err := b.mesh.link("hub-a"); err != nil || st == nil {
		t.Fatalf("hub-b not linked to hub-a: %v", err)
	}

	// A peer's list names a plain address.
	a.mesh.learn([]PeerRef{{Name: "rogue2", Addr: listRogue.addr()}})
	// A bulk dial to a plain address (a dialed-in peer's).
	if l, err := a.mesh.dial(bulkRogue.addr(), "rogue3", true); err == nil {
		l.close(1000, "test")
		t.Fatal("bulk dial to a plain listener succeeded")
	}

	time.Sleep(300 * time.Millisecond) // let the dialers try (and retry)
	for name, r := range map[string]*plainRecorder{"mdns": mdnsRogue, "list": listRogue, "static": staticRogue, "bulk": bulkRogue} {
		if got := r.seen(); len(got) != 0 {
			t.Errorf("%s: a plain listener got a request from a TLS hub: %v", name, got)
		}
	}
	for _, r := range []*plainRecorder{mdnsRogue, listRogue, staticRogue} {
		if a.logged(r.addr()+" is a plain address; dialing https://"+r.addr()) != 1 {
			t.Errorf("no upgrade log line for %s", r.addr())
		}
	}
	st := a.mesh.Status(context.Background())
	for _, p := range st.Peers {
		if p.Addr != "" && !strings.HasPrefix(p.Addr, "https://") {
			t.Errorf("status lists a plain peer address on a TLS hub: %+v", p)
		}
	}
}

// --peer-ca alone (a hub that does not serve TLS) also dials only https://;
// a hub with neither keeps plain addresses.
func TestMesh_SecureAddr(t *testing.T) {
	m := New(Config{Name: "x", PeerCA: x509.NewCertPool()})
	if got, up := m.secureAddr("h:1"); got != "https://h:1" || !up {
		t.Fatalf("peer-ca hub: %s %v", got, up)
	}
	if got, up := m.secureAddr("https://h:1"); got != "https://h:1" || up {
		t.Fatalf("already https: %s %v", got, up)
	}
	if got, up := m.secureAddr(""); got != "" || up {
		t.Fatalf("empty: %q %v", got, up)
	}
	plain := New(Config{Name: "y"})
	if got, up := plain.secureAddr("h:1"); got != "h:1" || up {
		t.Fatalf("plain hub: %s %v", got, up)
	}
}
