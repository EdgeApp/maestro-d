package device

// maestro-d: device listings carry each ready Android device's name, model
// and Android version, read once per adb connection and cached here.

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/execbound"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// DeviceWithProps is a device from `adb devices -l` with the properties
// ListDevicesWithProps read from it. Name, Model and OSVersion are blank
// for a device that is not in state "device", or whose read failed.
type DeviceWithProps struct {
	ConnectedDevice
	// TransportID is adb's transport_id, which changes on every reconnect;
	// "" from an adb that does not print it.
	TransportID string
	// Name is the user-set device name (`settings get global
	// device_name`), else the model.
	Name      string
	Model     string // ro.product.model, e.g. "Pixel 8"
	OSVersion string // ro.build.version.release, e.g. "16"
}

// propsReadTimeout bounds one device's read. A hub gives each peer 3 s to
// answer a listing, and that also covers adb, simctl and devicectl.
var propsReadTimeout = time.Second

// propsFailFresh is how long a failed or partial read stands before the
// device is read again, so a phone that keeps hanging costs one bounded
// read per window rather than one per listing, and a phone read while it
// was still booting gets its name within a window.
const propsFailFresh = 30 * time.Second

// now is the cache's clock, swappable in tests.
var now = time.Now

// propsScript reads, one per line, the model, the Android version, whether
// boot has completed, and the user-set name, in one adb call. Before
// system_server is up (a device that has just booted) `settings` fails or
// answers slowly; it then prints nameUnknown, and `true` keeps adb from
// returning its failure as the call's.
const propsScript = "getprop ro.product.model; getprop ro.build.version.release; getprop sys.boot_completed; " +
	"settings get global device_name 2>/dev/null || echo " + nameUnknown + "; true"

// nameUnknown is propsScript's line when `settings` did not answer.
const nameUnknown = "maestro-d:no-settings"

type deviceProps struct {
	name, model, osVersion string
}

// propsEntry is the last read of a serial on one connection. A complete
// read stands for the connection. A partial one (the name was unknown, or
// the read was cut off after the model and version) or a failed one (props
// blank) stands for propsFailFresh from at.
type propsEntry struct {
	transportID string
	props       deviceProps
	complete    bool
	at          time.Time
}

// propsCache maps a serial to what was last read from it.
var propsCache = struct {
	mu sync.Mutex
	m  map[string]propsEntry
}{m: map[string]propsEntry{}}

// ListDevicesWithProps lists the devices adb sees and fills each ready
// one's name, model and Android version, from the cache when the device's
// connection has not changed and otherwise by reading it. Reads run in
// parallel, so a listing costs at most one propsReadTimeout however many
// devices miss the cache. Devices that are not ready are listed blank, and
// dropped from the cache so their return reads them again.
func ListDevicesWithProps() ([]DeviceWithProps, error) {
	adbPath, err := findADB()
	if err != nil {
		return nil, err
	}
	cmd := execCommand(adbPath, "devices", "-l")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	out := stdout.String()
	transports := parseTransportIDs(out)
	connected := parseDeviceList(out)
	devices := make([]DeviceWithProps, len(connected))
	for i, d := range connected {
		devices[i] = DeviceWithProps{ConnectedDevice: d, TransportID: transports[d.Serial]}
	}

	var wg sync.WaitGroup
	ready := map[string]bool{}
	for i := range devices {
		d := &devices[i]
		if d.State != "device" {
			continue
		}
		ready[d.Serial] = true
		props, fresh := cachedProps(d.Serial, d.TransportID)
		if fresh {
			d.setProps(props)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			props, complete, err := readProps(adbPath, d.Serial)
			d.setProps(storeProps(d.Serial, d.TransportID, props, complete, err))
		}()
	}
	wg.Wait()
	evictProps(ready)
	return devices, nil
}

func (d *DeviceWithProps) setProps(p deviceProps) {
	d.Name, d.Model, d.OSVersion = p.name, p.model, p.osVersion
}

// parseTransportIDs maps each serial in `adb devices -l` output to its
// transport_id field.
func parseTransportIDs(output string) map[string]string {
	ids := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, f := range fields[2:] {
			if id, ok := strings.CutPrefix(f, "transport_id:"); ok {
				ids[fields[0]] = id
			}
		}
	}
	return ids
}

// cachedProps returns the cached read for serial on this connection and
// whether it stands: a complete read, or a partial or failed one inside
// propsFailFresh.
func cachedProps(serial, transportID string) (deviceProps, bool) {
	propsCache.mu.Lock()
	defer propsCache.mu.Unlock()
	e, ok := propsCache.m[serial]
	if !ok || e.transportID != transportID {
		return deviceProps{}, false
	}
	return e.props, e.complete || now().Sub(e.at) < propsFailFresh
}

// storeProps records a read and returns the props to list. A failed read
// keeps what an earlier read on the same connection found.
func storeProps(serial, transportID string, props deviceProps, complete bool, err error) deviceProps {
	propsCache.mu.Lock()
	defer propsCache.mu.Unlock()
	if err != nil {
		if e, ok := propsCache.m[serial]; ok && e.transportID == transportID {
			props = e.props
		} else {
			props = deviceProps{}
		}
		complete = false
	}
	propsCache.m[serial] = propsEntry{transportID: transportID, props: props, complete: complete, at: now()}
	return props
}

// evictProps drops every cached serial that is not in ready.
func evictProps(ready map[string]bool) {
	propsCache.mu.Lock()
	defer propsCache.mu.Unlock()
	for serial := range propsCache.m {
		if !ready[serial] {
			delete(propsCache.m, serial)
		}
	}
}

// readProps reads serial's model, Android version and name in one adb
// call, killed at propsReadTimeout, and says whether the read is complete.
// A call that failed or was cut off after printing the model and version
// still returns those, as a partial read. Each read logs one debug line.
func readProps(adbPath, serial string) (props deviceProps, complete bool, err error) {
	start := now()
	cmd := execCommand(adbPath, "-s", serial, "shell", propsScript)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	runErr := execbound.Run(cmd, propsReadTimeout)
	elapsed := now().Sub(start).Milliseconds()
	props, complete = parseProps(stdout.String())
	if runErr != nil {
		complete = false
		if props.model == "" {
			logger.Debug("adb props %s: %v in %dms", serial, runErr, elapsed)
			return deviceProps{}, false, fmt.Errorf("adb props %s: %w", serial, runErr)
		}
	}
	logger.Debug("adb props %s: %s/%s/%s (complete %v) in %dms", serial, props.model, props.osVersion, props.name, complete, elapsed)
	return props, complete, nil
}

// parseProps reads propsScript's output: the model, the Android version,
// sys.boot_completed and the device name, one per line. The read is
// complete when boot has completed and `settings` answered. A blank,
// "null" or unknown name falls back to the model.
func parseProps(output string) (deviceProps, bool) {
	lines := strings.Split(output, "\n")
	line := func(i int) string {
		if i >= len(lines) {
			return ""
		}
		return strings.TrimSpace(lines[i])
	}
	p := deviceProps{model: line(0), osVersion: line(1), name: line(3)}
	complete := line(2) == "1" && strings.Count(output, "\n") >= 4 && p.name != nameUnknown
	if p.name == "null" || p.name == "" || p.name == nameUnknown {
		p.name = p.model
	}
	return p, complete
}
