// Command wanproxy is a TCP proxy that makes a link slow and lossy, for
// end-to-end tests of maestro-d clients over a WAN. It runs where netem is
// not available (a container's kernel), on the client's side of the link:
//
//	wanproxy -listen 127.0.0.1:7434 -target 192.168.64.1:7433 -rtt 150ms -rate 20mbit -drop-after-request 3
//
// -rtt adds half the round trip to each direction; -rate caps each
// direction's bandwidth; -drop-after-request N reads the client's side as
// HTTP and closes the connection right after forwarding every Nth request,
// before its response, so the client must retry.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/devicelab-dev/maestro-runner/pkg/hub/hubtest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7434", "address clients connect to")
	target := flag.String("target", "", "HOST:PORT to forward to (required)")
	rtt := flag.Duration("rtt", 0, "round-trip time to add (half each way), e.g. 150ms")
	rate := flag.String("rate", "", "bandwidth cap per direction, e.g. 20mbit (default unlimited)")
	drop := flag.Int("drop-after-request", 0, "drop the connection after forwarding every Nth HTTP request (0 = never)")
	quiet := flag.Bool("quiet", false, "do not log connections and drops")
	flag.Parse()
	if *target == "" {
		fmt.Fprintln(os.Stderr, "wanproxy: -target is required")
		os.Exit(2)
	}
	bps, err := hubtest.ParseRate(*rate)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wanproxy:", err)
		os.Exit(2)
	}
	cfg := hubtest.ProxyConfig{Listen: *listen, Target: *target, RTT: *rtt, Rate: bps, DropAfterRequest: *drop}
	if !*quiet {
		cfg.Log = log.New(os.Stderr, "wanproxy: ", log.Ltime|log.Lmicroseconds)
	}
	p, err := hubtest.Start(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wanproxy:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wanproxy: %s -> %s rtt=%v rate=%s drop-after-request=%d\n", p.Addr(), *target, *rtt, *rate, *drop)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = p.Close()
}
