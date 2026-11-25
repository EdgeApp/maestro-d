package cli

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestBinaryImports(t *testing.T) {
	for _, name := range []string{"maestro-d", "maestro-d-hub"} {
		cmd := exec.Command("go", "list", "-deps", "./cmd/"+name)
		cmd.Dir = "../.."
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %s (%v)", name, out, err)
		}
		deps := strings.Fields(string(out))
		for _, pkg := range []string{"hub", "hubd", "hubweb"} {
			want := name == "maestro-d-hub" && pkg != "hubweb"
			if got := slices.Contains(deps, "github.com/devicelab-dev/maestro-runner/pkg/"+pkg); got != want {
				t.Errorf("%s imports %s: %t, want %t", name, pkg, got, want)
			}
		}
	}
}
