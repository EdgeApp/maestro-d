package devicefarm

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
)

// Account reads the account settings: trialMinutes {total, remaining},
// unmeteredDevices, unmeteredRemoteAccessDevices and maxJobTimeoutMinutes.
// maxSlots is left out: it maps offering ids, not a concurrency number.
func (c *Client) Account(ctx context.Context) (map[string]any, error) {
	out, err := c.api.GetAccountSettings(ctx, &devicefarm.GetAccountSettingsInput{})
	if err != nil {
		return nil, err
	}
	trial := map[string]any{"total": float64(0), "remaining": float64(0)}
	acct := map[string]any{
		"trialMinutes":                 trial,
		"unmeteredDevices":             map[string]int32{},
		"unmeteredRemoteAccessDevices": map[string]int32{},
	}
	s := out.AccountSettings
	if s == nil {
		return acct, nil
	}
	if s.TrialMinutes != nil {
		trial["total"] = aws.ToFloat64(s.TrialMinutes.Total)
		trial["remaining"] = aws.ToFloat64(s.TrialMinutes.Remaining)
	}
	if s.UnmeteredDevices != nil {
		acct["unmeteredDevices"] = s.UnmeteredDevices
	}
	if s.UnmeteredRemoteAccessDevices != nil {
		acct["unmeteredRemoteAccessDevices"] = s.UnmeteredRemoteAccessDevices
	}
	if s.MaxJobTimeoutMinutes != nil {
		acct["maxJobTimeoutMinutes"] = aws.ToInt32(s.MaxJobTimeoutMinutes)
	}
	return acct, nil
}
