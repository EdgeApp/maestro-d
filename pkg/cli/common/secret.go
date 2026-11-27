package common

import (
	"os"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// ReadSecretFile reads a token or key file that must not be readable by others.
func ReadSecretFile(flag, path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", daemon.Errorf(daemon.CodeUsage, "--%s: %v", flag, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", daemon.Errorf(daemon.CodeUsage, "--%s %s is readable by others (mode %04o); chmod 600 it", flag, path, st.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", daemon.Errorf(daemon.CodeUsage, "--%s: %v", flag, err)
	}
	return strings.TrimSpace(string(data)), nil
}
