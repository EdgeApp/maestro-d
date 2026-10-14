package wda

// endpoint.go decides how the host reaches WebDriverAgent on a physical
// device. The usbmuxd port forward is the usual path, but a device can be
// cabled and paired without usbmuxd listing it: an iPad on a USB-C network
// link is reachable only through CoreDevice. So when usbmuxd does not know
// the device, the runner falls back to a go-ios tunnel, then to the
// CoreDevice tunnel address devicectl reports, then to the address WDA
// printed when it started, and logs the path it used.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/forward"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// The paths the runner can reach WDA by, in the order it tries them.
const (
	reachUSBMux      = "usbmuxd port forward"
	reachGoIOSTunnel = "go-ios tunnel"
	reachCoreDevice  = "CoreDevice tunnel address"
	reachReported    = "address WDA reported"
)

// probeTimeout bounds one /status request against a candidate address.
const probeTimeout = 3 * time.Second

// wdaEndpoint is where the WDA client sends its requests, the path that
// address came from, and what to close when the runner stops.
type wdaEndpoint struct {
	baseURL string
	path    string
	closer  io.Closer
}

// goIOSTunnel is the go-ios agent's description of a device tunnel
// (GET /tunnel/<udid> on the agent's HTTP API).
type goIOSTunnel struct {
	Address          string `json:"address"`
	Udid             string `json:"udid"`
	UserspaceTUN     bool   `json:"userspaceTun"`
	UserspaceTUNPort int    `json:"userspaceTunPort"`
}

// reachDeps holds the device-facing operations reachWDA chooses between,
// so the decision can be tested without a device.
type reachDeps struct {
	// usbmuxForward forwards localhost:port to the device's port through
	// usbmuxd. It fails when usbmuxd does not list the device.
	usbmuxForward func(udid string, port uint16) (io.Closer, error)
	// agentTunnel asks a running go-ios agent for the device's tunnel.
	agentTunnel func(udid string) (goIOSTunnel, error)
	// tunnelForward forwards 127.0.0.1:port to the device's port through
	// a go-ios tunnel.
	tunnelForward func(t goIOSTunnel, port uint16) (io.Closer, error)
	// coreDeviceIP returns the device's CoreDevice tunnel address.
	coreDeviceIP func(udid string) (string, error)
	// probe checks WDA answers at baseURL.
	probe func(baseURL string) error
}

// defaultReachDeps returns the real operations.
func defaultReachDeps() reachDeps {
	return reachDeps{
		usbmuxForward: usbmuxForward,
		agentTunnel:   agentTunnel,
		tunnelForward: tunnelForward,
		coreDeviceIP:  coreDeviceIP,
		probe:         probeWDA,
	}
}

// reachWDA picks how to reach WDA listening on port on the device udid.
// reportedURL is the address WDA printed at startup ("" when none).
func reachWDA(udid string, port uint16, reportedURL string, d reachDeps) (wdaEndpoint, error) {
	var tried []string
	fail := func(path string, err error) {
		logger.Info("WDA on %s: %s failed: %v", udid, path, err)
		tried = append(tried, fmt.Sprintf("%s: %v", path, err))
	}
	chosen := func(ep wdaEndpoint) (wdaEndpoint, error) {
		logger.Info("WDA on %s reached over %s at %s", udid, ep.path, ep.baseURL)
		fmt.Printf("  WebDriverAgent reached over %s (%s)\n", ep.path, ep.baseURL)
		return ep, nil
	}

	closer, err := d.usbmuxForward(udid, port)
	if err == nil {
		return chosen(wdaEndpoint{fmt.Sprintf("http://localhost:%d", port), reachUSBMux, closer})
	}
	fail(reachUSBMux, err)

	if t, err := d.agentTunnel(udid); err != nil {
		fail(reachGoIOSTunnel, err)
	} else if closer, err := d.tunnelForward(t, port); err != nil {
		fail(reachGoIOSTunnel, err)
	} else {
		local := fmt.Sprintf("http://127.0.0.1:%d", port)
		if err := d.probe(local); err != nil {
			_ = closer.Close()
			fail(reachGoIOSTunnel, err)
		} else {
			return chosen(wdaEndpoint{local, reachGoIOSTunnel, closer})
		}
	}

	if ip, err := d.coreDeviceIP(udid); err != nil {
		fail(reachCoreDevice, err)
	} else {
		u := "http://" + net.JoinHostPort(ip, strconv.Itoa(int(port)))
		if err := d.probe(u); err != nil {
			fail(reachCoreDevice, err)
		} else {
			return chosen(wdaEndpoint{u, reachCoreDevice, nil})
		}
	}

	if reportedURL == "" {
		fail(reachReported, errors.New("WDA printed no address"))
	} else {
		u := strings.TrimRight(reportedURL, "/")
		if err := d.probe(u); err != nil {
			fail(reachReported, err)
		} else {
			return chosen(wdaEndpoint{u, reachReported, nil})
		}
	}

	return wdaEndpoint{}, fmt.Errorf("device %s not reachable: %s", udid, strings.Join(tried, "; "))
}

// serverURLRe matches the address WDA prints once its HTTP server listens.
var serverURLRe = regexp.MustCompile(`ServerURLHere->(\S+?)<-ServerURLHere`)

// reportedServerURL returns the last address WDA printed in log, or "".
// The runner log keeps every startup attempt, so the last one is current.
func reportedServerURL(log string) string {
	m := serverURLRe.FindAllStringSubmatch(log, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// probeWDA reports whether WDA answers GET /status at baseURL.
func probeWDA(baseURL string) error {
	c := http.Client{Timeout: probeTimeout}
	resp, err := c.Get(baseURL + "/status")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /status: HTTP %d", resp.StatusCode)
	}
	return nil
}

// usbmuxForward forwards the WDA port through usbmuxd.
func usbmuxForward(udid string, port uint16) (io.Closer, error) {
	entry, err := goios.GetDevice(udid)
	if err != nil {
		return nil, fmt.Errorf("usbmuxd does not list the device: %w", err)
	}
	listener, err := forward.Forward(entry, port, port)
	if err != nil {
		return nil, fmt.Errorf("port forward %d->%d failed: %w", port, port, err)
	}
	// Give the forward a moment to establish
	time.Sleep(500 * time.Millisecond)
	return listener, nil
}

// agentTunnel asks the go-ios agent (`ios tunnel start`) for udid's tunnel.
func agentTunnel(udid string) (goIOSTunnel, error) {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/tunnel/%s", goios.HttpApiPort(), udid))
	if err != nil {
		return goIOSTunnel{}, fmt.Errorf("no go-ios agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return goIOSTunnel{}, fmt.Errorf("go-ios agent has no tunnel for the device (HTTP %d)", resp.StatusCode)
	}
	var t goIOSTunnel
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return goIOSTunnel{}, fmt.Errorf("go-ios agent reply: %w", err)
	}
	if t.Address == "" {
		return goIOSTunnel{}, errors.New("go-ios agent has no tunnel for the device")
	}
	return t, nil
}

// tunnelForward forwards 127.0.0.1:port to port on the device through t.
func tunnelForward(t goIOSTunnel, port uint16) (io.Closer, error) {
	entry := goios.DeviceEntry{UserspaceTUN: t.UserspaceTUN, UserspaceTUNPort: t.UserspaceTUNPort}
	return forwardTCP(port, func() (net.Conn, error) {
		return goios.ConnectTUNDevice(t.Address, int(port), entry)
	})
}

// forwardTCP listens on 127.0.0.1:port and pipes each connection to one
// opened by dial. Closing the returned listener stops new connections.
func forwardTCP(port uint16, dial func() (net.Conn, error)) (io.Closer, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen on %d: %w", port, err)
	}
	go func() {
		for {
			local, err := ln.Accept()
			if err != nil {
				return
			}
			go pipeTo(local, dial)
		}
	}()
	return ln, nil
}

// pipeTo copies between local and a connection from dial until either
// side closes.
func pipeTo(local net.Conn, dial func() (net.Conn, error)) {
	defer func() { _ = local.Close() }()
	remote, err := dial()
	if err != nil {
		logger.Warn("WDA tunnel forward: %v", err)
		return
	}
	defer func() { _ = remote.Close() }()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(remote, local)
		closeWrite(remote)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(local, remote)
		closeWrite(local)
	}()
	wg.Wait()
}

// closeWrite half-closes c when it supports it, so the peer sees EOF.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// DevicectlDevice is what `devicectl device info details` says about a
// physical device that the runner and setup need.
type DevicectlDevice struct {
	Name      string
	OSVersion string
	// TunnelIP is the CoreDevice tunnel address, "" unless the tunnel is
	// connected.
	TunnelIP string
}

// devicectlInfoTimeout bounds one `devicectl device info details` call.
// devicectl hangs on a locked device or one still pairing, and attach would
// freeze with it.
var devicectlInfoTimeout = 10 * time.Second

// DevicectlDeviceInfo reads udid's details from devicectl. It works for
// devices usbmuxd does not list.
func DevicectlDeviceInfo(udid string) (DevicectlDevice, error) {
	f, err := os.CreateTemp("", "maestro-d-devicectl-*.json")
	if err != nil {
		return DevicectlDevice{}, err
	}
	path := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()

	ctx, cancel := context.WithTimeout(context.Background(), devicectlInfoTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "info", "details",
		"--device", udid, "--json-output", path, "-q")
	// A child that outlives xcrun and holds its output open must not keep
	// CombinedOutput waiting past the timeout.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return DevicectlDevice{}, fmt.Errorf("devicectl device info details timed out after %v", devicectlInfoTimeout)
	}
	if err != nil {
		return DevicectlDevice{}, fmt.Errorf("devicectl device info details: %w: %s", err, strings.TrimSpace(string(out)))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DevicectlDevice{}, err
	}
	return parseDevicectlDetails(data)
}

// parseDevicectlDetails reads devicectl's --json-output for one device.
func parseDevicectlDetails(data []byte) (DevicectlDevice, error) {
	var doc struct {
		Result struct {
			ConnectionProperties struct {
				TunnelIPAddress string `json:"tunnelIPAddress"`
				TunnelState     string `json:"tunnelState"`
			} `json:"connectionProperties"`
			DeviceProperties struct {
				Name            string `json:"name"`
				OSVersionNumber string `json:"osVersionNumber"`
			} `json:"deviceProperties"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return DevicectlDevice{}, fmt.Errorf("devicectl output: %w", err)
	}
	r := doc.Result
	dev := DevicectlDevice{
		Name:      r.DeviceProperties.Name,
		OSVersion: r.DeviceProperties.OSVersionNumber,
	}
	if r.ConnectionProperties.TunnelState == "connected" {
		dev.TunnelIP = r.ConnectionProperties.TunnelIPAddress
	}
	return dev, nil
}

// coreDeviceIP returns udid's CoreDevice tunnel address from devicectl.
func coreDeviceIP(udid string) (string, error) {
	dev, err := DevicectlDeviceInfo(udid)
	if err != nil {
		return "", err
	}
	if dev.TunnelIP == "" {
		return "", errors.New("devicectl reports no connected tunnel")
	}
	return dev.TunnelIP, nil
}
