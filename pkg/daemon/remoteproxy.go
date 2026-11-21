package daemon

// remoteproxy.go: a cloud device's Appium endpoint behind a loopback proxy.
// A cloud provider's endpoint URL is itself a credential (Device Farm's is
// unsigned), and the Appium driver puts the URL it talks to in its log lines
// and in the errors that steps return to clients. The driver is therefore
// pointed at http://127.0.0.1:<port>/<token>, which this proxy forwards to
// the real endpoint, so neither ever holds the provider's URL.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

type remoteProxy struct {
	srv *http.Server
	ln  net.Listener
	// url is the Appium base URL the driver uses.
	url string
}

// newRemoteProxy listens on a loopback port and forwards
// /<token>/<rest> to target/<rest>.
func newRemoteProxy(target string) (*remoteProxy, error) {
	tu, err := url.Parse(target)
	if err != nil || tu.Scheme == "" || tu.Host == "" {
		return nil, Errorf(CodeDeviceError, "the cloud provider returned an Appium endpoint that is not a URL")
	}
	tok := make([]byte, 12)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(tok)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	prefix := "/" + token
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest := strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.Scheme, pr.Out.URL.Host = tu.Scheme, tu.Host
			pr.Out.URL.Path = strings.TrimSuffix(tu.Path, "/") + rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = tu.Host
			// WebDriver POSTs carry a JSON object, {} when there is
			// nothing to send. The Appium client sends some (an element
			// click) with no body, which a provider's managed endpoint
			// refuses ("Value null at 'payload'").
			if pr.Out.Method == http.MethodPost && pr.Out.ContentLength == 0 {
				pr.Out.Body = io.NopCloser(strings.NewReader("{}"))
				pr.Out.ContentLength = 2
				pr.Out.Header.Set("Content-Type", "application/json")
			}
		},
		// The default logs the outbound URL; this one names no URL.
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			msg := "the cloud device's Appium endpoint did not answer"
			if r.Context().Err() != nil {
				msg = "the request was cancelled"
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"value":{"error":"unknown error","message":"`+msg+`"}}`)
		},
	}
	p := &remoteProxy{ln: ln, url: "http://" + ln.Addr().String() + prefix}
	p.srv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
				http.NotFound(w, r)
				return
			}
			rp.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// close stops the proxy; requests in flight are cut off.
func (p *remoteProxy) close() {
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.srv.Shutdown(ctx)
	_ = p.srv.Close()
}
