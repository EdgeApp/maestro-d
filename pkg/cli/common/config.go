package common

import (
	"errors"
	"os"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// CheckConfigPath diagnoses a missing explicit config on startup.
func CheckConfigPath(deviceConfig string, explicit map[string]bool) error {
	if deviceConfig == "" {
		return nil
	}
	if _, err := os.Stat(deviceConfig); errors.Is(err, os.ErrNotExist) {
		from := "--config"
		if !explicit["config"] {
			from = "MAESTRO_D_CONFIG"
		}
		return daemon.Errorf(daemon.CodeUsage, "%s %s does not exist", from, deviceConfig)
	}
	return nil
}

// ConfigParser adds migration diagnostics to commands that start a daemon
// or install its service. Only a removed share flag triggers discovery.
func ConfigParser[T any](parse ArgParser[T], deps func() daemon.Deps) ArgParser[T] {
	return func(args []string, flags []Flag[T], keys map[string]string) (*T, *ParsedArgs, error) {
		o, p, err := parse(args, flags, keys)
		if err != nil {
			return nil, nil, err
		}
		for _, flag := range p.Step {
			switch flag.Key {
			case "share-device", "share-tv":
				id := flag.Value
				if id == "" {
					id = "<id>"
				} else {
					for _, d := range deps().ListDevices("") {
						if d.ID == id || strings.EqualFold(d.Name, id) {
							id = d.ID
							if d.Kind == "emulator" && d.Name != "" {
								id = daemon.AVDPrefix + d.Name
							}
							break
						}
					}
				}
				return nil, nil, removedDeviceFlag("--"+flag.Key, id)
			case "network-config":
				return nil, nil, removedDeviceFlag("--network-config", "<id>")
			}
		}
		if os.Getenv("MAESTRO_D_NETWORK_CONFIG") != "" {
			return nil, nil, removedDeviceFlag("MAESTRO_D_NETWORK_CONFIG", "<id>")
		}
		return o, p, nil
	}
}

func removedDeviceFlag(name, id string) error {
	return daemon.Errorf(daemon.CodeUsage, "%s was removed in 4.0: add the device to config.json with maestro-d-hub devices add %s", name, id)
}
