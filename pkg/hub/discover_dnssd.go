package hub

// discover_dnssd.go: browsing through the system's mDNS on macOS. There
// mDNSResponder owns UDP 5353, and zeroconf's own browser never hears the
// answers of hubs on other machines (it sees only its own host). The
// system's stack does, so on macOS the launch browse runs `dns-sd`: -B
// lists the instances, -L resolves each to host, port and TXT, and -G
// gives the host's IPv4 addresses. Advertising stays with zeroconf, whose
// announcements the system stack (and every other host) hears.

import (
	"bufio"
	"context"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dnssd runs dns-sd until ctx ends or each line it prints makes stop true.
var dnssd = func(ctx context.Context, stop func(line string) bool, args ...string) error {
	cmd := exec.CommandContext(ctx, "dns-sd", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if stop(sc.Text()) {
			break
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return nil
}

var (
	// 21:43:37.891  Add        3  14 local.               _maestro-d._tcp.     hub-a
	browseLine = regexp.MustCompile(`\sAdd\s+\d+\s+\d+\s+\S+\s+_maestro-d\._tcp\.\s+(\S+)\s*$`)
	// …  hub-a._maestro-d._tcp.local. can be reached at hub-a.local.:7433 (interface 14)
	reachLine = regexp.MustCompile(`can be reached at (\S+):(\d+)\b`)
	// 21:43:44.250  Add  40000002  14  hub-a.local.  10.0.0.2  120
	addrLine = regexp.MustCompile(`\sAdd\s+\S+\s+\d+\s+(\S+)\s+(\d+\.\d+\.\d+\.\d+)\s`)
)

// browseDNSSD lists the hubs the system's mDNS sees within d, resolved.
func browseDNSSD(ctx context.Context, d time.Duration) ([]Found, error) {
	bctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var names []string
	seen := map[string]bool{}
	err := dnssd(bctx, func(line string) bool {
		if m := browseLine.FindStringSubmatch(line); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
		return false
	}, "-B", ServiceType, "local.")
	if err != nil {
		return nil, err
	}
	out := make([]Found, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			out[i], _ = resolveDNSSD(ctx, name)
		}(i, name)
	}
	wg.Wait()
	var found []Found
	for _, f := range out {
		if f.Name != "" {
			found = append(found, f)
		}
	}
	return found, nil
}

// resolveDNSSD resolves one instance to a Found.
func resolveDNSSD(ctx context.Context, instance string) (Found, bool) {
	lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	host, port := "", 0
	var txt []string
	_ = dnssd(lctx, func(line string) bool {
		if m := reachLine.FindStringSubmatch(line); m != nil {
			host = m[1]
			port, _ = strconv.Atoi(m[2])
			return false
		}
		if host != "" {
			txt = strings.Fields(line)
			return true
		}
		return false
	}, "-L", instance, ServiceType, "local.")
	if host == "" {
		return Found{}, false
	}
	gctx, gcancel := context.WithTimeout(ctx, 2*time.Second)
	defer gcancel()
	var ips []net.IP
	_ = dnssd(gctx, func(line string) bool {
		if m := addrLine.FindStringSubmatch(line); m != nil {
			if ip := net.ParseIP(m[2]); ip != nil {
				ips = append(ips, ip)
			}
		}
		// dns-sd -G prints every address at once, then waits for more.
		return len(ips) > 0 && !strings.Contains(line, "Flags")
	}, "-G", "v4", host)
	return foundFromEntry(instance, ips, port, txt)
}
