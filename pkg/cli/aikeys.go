package cli

// aikeys.go: attach --ai-keys-file, the caller's keys for assertWithAI and
// assertNoDefectsWithAI, read from a file so no key is on a command line.

import (
	"bytes"
	"encoding/json"
	"os"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// readAIKeysFile reads {"jevKey", "anthropicKey"} from a file only its
// owner can read. Errors name the file, never its content.
func readAIKeysFile(path string) (*daemon.AttachAI, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "--ai-keys-file: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, daemon.Errorf(daemon.CodeUsage, "--ai-keys-file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, daemon.Errorf(daemon.CodeUsage, "--ai-keys-file %s has mode %04o; make it 0600 (chmod 600) so no one else can read it", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "--ai-keys-file: %v", err)
	}
	var keys daemon.AttachAI
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&keys); err != nil {
		// The decoder's message may quote the file; give only its kind.
		return nil, daemon.Errorf(daemon.CodeUsage, "--ai-keys-file %s is not a JSON object of jevKey and anthropicKey", path)
	}
	if keys.JevKey == "" && keys.AnthropicKey == "" {
		return nil, daemon.Errorf(daemon.CodeUsage, "--ai-keys-file %s holds neither jevKey nor anthropicKey", path)
	}
	return &keys, nil
}
