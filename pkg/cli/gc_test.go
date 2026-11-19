package cli

import "testing"

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{
		0:          "0 B",
		1023:       "1023 B",
		1536:       "1.5 KB",
		5 << 20:    "5.0 MB",
		3 << 30:    "3.0 GB",
		5000 << 30: "5000.0 GB",
	} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
