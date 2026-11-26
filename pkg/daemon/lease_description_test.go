package daemon

import (
	"strings"
	"testing"
)

func TestLeaseDescription(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"\x00  smoke\n\t run\u0080 \r", "smoke run"},
		{strings.Repeat("猫", 201), strings.Repeat("猫", 200)},
	} {
		if got := LeaseDescription(tc.in); got != tc.want {
			t.Errorf("got %q; want %q", got, tc.want)
		}
	}
}
