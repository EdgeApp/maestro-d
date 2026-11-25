package common

import (
	"errors"
	"os"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// CheckConfigFlags settles which device config file is read: only one
// ever is. A flag on the command line wins over the other's environment
// variable; the two flags together, or the two variables, are USAGE. A
// --config file that does not exist is USAGE too, so a mistyped path does
// not start a daemon that shares and describes nothing.
func CheckConfigFlags(deviceConfig, networkConfig *string, explicit map[string]bool) error {
	if *deviceConfig != "" && *networkConfig != "" {
		switch c, n := explicit["config"], explicit["network-config"]; {
		case c && !n:
			*networkConfig = ""
		case n && !c:
			*deviceConfig = ""
		case c && n:
			return daemon.Errorf(daemon.CodeUsage, "--config and --network-config name one file each; give one (--network-config is the older format)")
		default:
			return daemon.Errorf(daemon.CodeUsage, "MAESTRO_D_CONFIG and MAESTRO_D_NETWORK_CONFIG name one file each; unset one (MAESTRO_D_NETWORK_CONFIG is the older format)")
		}
	}
	if *deviceConfig == "" {
		return nil
	}
	if _, err := os.Stat(*deviceConfig); errors.Is(err, os.ErrNotExist) {
		from := "--config"
		if !explicit["config"] {
			from = "MAESTRO_D_CONFIG"
		}
		return daemon.Errorf(daemon.CodeUsage, "%s %s does not exist", from, *deviceConfig)
	}
	return nil
}
