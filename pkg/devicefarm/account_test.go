package devicefarm

import (
	"context"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

func TestAccount(t *testing.T) {
	c, f := newTestClient(t)
	f.SetTrialMinutes(1000, 987.5)
	acct, err := c.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	trial := acct["trialMinutes"].(map[string]any)
	if trial["total"] != 1000.0 || trial["remaining"] != 987.5 {
		t.Fatalf("trial = %v", trial)
	}
	if _, ok := acct["maxSlots"]; ok {
		t.Fatal("maxSlots exposed")
	}
	if acct["maxJobTimeoutMinutes"] != int32(150) {
		t.Fatalf("maxJobTimeoutMinutes = %#v", acct["maxJobTimeoutMinutes"])
	}
	for _, k := range []string{"unmeteredDevices", "unmeteredRemoteAccessDevices"} {
		if m, ok := acct[k].(map[string]int32); !ok || len(m) != 2 {
			t.Fatalf("%s = %#v", k, acct[k])
		}
	}
	f.FailOp("GetAccountSettings", "UnrecognizedClientException", "The security token included in the request is invalid")
	if _, err := c.Account(context.Background()); Classify(err).Kind != cloudprov.Auth {
		t.Fatalf("err = %v", err)
	}
}
