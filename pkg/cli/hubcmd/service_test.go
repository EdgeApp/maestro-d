package hubcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"
)

func TestLegacyServiceWarning(t *testing.T) {
	useFakeLaunchd(t)
	p := servicePlistPath()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		warn bool
	}{
		{[]string{"/opt/maestro-d", "hub", "start", "--foreground"}, true},
		{[]string{"/opt/maestro-d-hub", "start", "--foreground"}, false},
	} {
		data, err := plist.Marshal(map[string]any{"ProgramArguments": tc.args}, plist.XMLFormat)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "stderr")
		file, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stderr
		os.Stderr = file
		warnLegacyService()
		os.Stderr = previous
		_ = file.Close()
		data, err = os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "maestro-d-hub service install") != tc.warn {
			t.Fatalf("%v: %q", tc.args, data)
		}
	}
}
