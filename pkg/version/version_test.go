package version_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBinariesReportBuildVersion(t *testing.T) {
	for _, name := range []string{"maestro-d", "maestro-d-hub"} {
		cmd := exec.Command("go", "run", "-ldflags", "-X github.com/devicelab-dev/maestro-runner/pkg/version.Version=4.0.0-check", "./cmd/"+name, "version")
		cmd.Dir = "../.."
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "4.0.0-check" {
			t.Errorf("%s version: %s (%v)", name, out, err)
		}
	}
}
