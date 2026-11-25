package hubd

import (
	"net/http"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// fromAddr makes every request look as if it came from addr, as a TCP
// listener sees a LAN client.
func fromAddr(addr string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = addr
		h.ServeHTTP(w, r)
	})
}

func wantErrCode(t *testing.T, err error, code daemon.Code) *daemon.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got success", code)
	}
	if got := codeOf(t, err); got != code {
		t.Fatalf("code %s, want %s: %v", got, code, err)
	}
	de, _ := err.(*daemon.Error)
	return de
}
