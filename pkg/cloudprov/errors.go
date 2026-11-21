package cloudprov

import (
	"errors"
	"fmt"
)

// Kind classifies a provider failure so the daemon can map it to an error
// code without knowing the provider.
type Kind string

const (
	// Auth: missing or rejected credentials, or a denied action.
	Auth Kind = "auth"
	// NoCapacity: the account's concurrency or limits are exhausted.
	NoCapacity Kind = "no-capacity"
	// DeviceBusy: the chosen model stayed busy.
	DeviceBusy Kind = "device-busy"
	// NotFound: no model, session or upload matched.
	NotFound Kind = "not-found"
	// Ended: the session is over.
	Ended Kind = "ended"
	// Other: anything else.
	Other Kind = "other"
)

// Error is what every provider method returns on failure, already classified,
// so the daemon maps Kind to an error code without knowing the provider.
type Error struct {
	Kind   Kind
	Action string // the denied action, for Auth
	Msg    string // already passed through the provider's Redact
	Err    error
}

func (e *Error) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Kind)
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds an *Error of kind k.
func Errorf(k Kind, format string, args ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, args...)}
}

// KindOf reports the Kind of err: the kind of the *Error it wraps, else Other.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Other
}
