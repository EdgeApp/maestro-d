package daemon

import (
	"encoding/json"
	"time"
)

// AcquisitionReplay retains a successful acquisition through its retry window.
type AcquisitionReplay struct {
	RequestID   string          `json:"requestId"`
	LeaseID     string          `json:"leaseId"`
	Device      string          `json:"device"`
	CompletedAt time.Time       `json:"completedAt"`
	Status      int             `json:"status"`
	Response    json.RawMessage `json:"response"`
}

// SelectorAttempt identifies one mesh dispatch whose outcome may be unknown.
type SelectorAttempt struct {
	RequestID     string          `json:"requestId"`
	Owner         string          `json:"owner"`
	AttemptID     string          `json:"attemptId"`
	Pass          string          `json:"pass"`
	Path          string          `json:"path,omitempty"`
	OriginalBody  json.RawMessage `json:"originalBody,omitempty"`
	ForwardedBody json.RawMessage `json:"forwardedBody"`
	StartedAt     time.Time       `json:"startedAt"`
}

// ReplayLimit and ReplayTTL apply to both in-memory and durable outcomes.
const (
	ReplayLimit = idemPerDevice
	ReplayTTL   = idemTTL
)
