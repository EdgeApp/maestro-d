package devicefarm

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveAccount reads the real account's trial minutes: the minute ledger
// for live runs. It runs only with MAESTRO_D_DEVICEFARM_LIVE=1, takes
// credentials from the environment (or MAESTRO_D_DEVICEFARM_CREDENTIALS_FILE)
// and logs nothing but the trial minutes.
//
//	MAESTRO_D_DEVICEFARM_LIVE=1 go test -run TestLiveAccount -v ./pkg/devicefarm/
func TestLiveAccount(t *testing.T) {
	if os.Getenv("MAESTRO_D_DEVICEFARM_LIVE") != "1" {
		t.Skip("set MAESTRO_D_DEVICEFARM_LIVE=1 to read the live Device Farm account")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p, err := NewProvider(ctx, Options{CredentialsFile: os.Getenv("MAESTRO_D_DEVICEFARM_CREDENTIALS_FILE")})
	if err != nil {
		t.Fatal(err) // already classified and redacted
	}
	acct, err := p.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trial, _ := acct["trialMinutes"].(map[string]any)
	t.Logf("trialMinutes total=%v remaining=%v", trial["total"], trial["remaining"])
}
