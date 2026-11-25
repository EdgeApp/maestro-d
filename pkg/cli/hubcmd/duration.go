package hubcmd

import (
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// parseReservationLead reads hub start's --reservation-lead: "" is the
// default, DefaultReservationLead.
func parseReservationLead(v string) (time.Duration, error) {
	if strings.TrimSpace(v) == "" {
		return daemon.DefaultReservationLead, nil
	}
	return common.ParseLeaseDuration("reservation-lead", v)
}

// parseLeaseMax reads hub start's --lease-max values: DURATION sets the
// hub's maximum window (the last one wins), DEVICE=DURATION one device's.
func parseLeaseMax(vals []string) (time.Duration, []daemon.LeaseMax, error) {
	var hubMax time.Duration
	var devs []daemon.LeaseMax
	for _, v := range vals {
		dev, dur, perDevice := "", v, false
		if i := strings.LastIndexByte(v, '='); i >= 0 {
			dev, dur, perDevice = strings.TrimSpace(v[:i]), v[i+1:], true
			if dev == "" {
				return 0, nil, daemon.Errorf(daemon.CodeUsage, "--lease-max %q: want DURATION or DEVICE=DURATION", v)
			}
		}
		d, err := common.ParseLeaseDuration("lease-max", dur)
		if err != nil {
			return 0, nil, err
		}
		if perDevice {
			devs = append(devs, daemon.LeaseMax{Device: dev, Max: d})
		} else {
			hubMax = d
		}
	}
	return hubMax, devs, nil
}
