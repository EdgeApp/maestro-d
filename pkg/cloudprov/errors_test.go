package cloudprov

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorMessage(t *testing.T) {
	cause := errors.New("cause")
	for _, tc := range []struct {
		err  *Error
		want string
	}{
		{&Error{Kind: Auth, Msg: "denied", Err: cause}, "denied"},
		{&Error{Kind: Other, Err: cause}, "cause"},
		{&Error{Kind: Ended}, "ended"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
	e := &Error{Kind: NotFound, Err: cause}
	if !errors.Is(e, cause) || e.Unwrap() != cause {
		t.Fatal("Unwrap")
	}
}

func TestErrorfAndKindOf(t *testing.T) {
	e := Errorf(DeviceBusy, "busy %d", 3)
	if e.Kind != DeviceBusy || e.Msg != "busy 3" || e.Err != nil {
		t.Fatalf("Errorf = %+v", e)
	}
	if KindOf(fmt.Errorf("wrapped: %w", e)) != DeviceBusy {
		t.Fatal("KindOf wrapped")
	}
	if KindOf(errors.New("plain")) != Other || KindOf(nil) != Other {
		t.Fatal("KindOf plain")
	}
	for _, k := range []Kind{Auth, NoCapacity, DeviceBusy, NotFound, Ended, Other} {
		if KindOf(&Error{Kind: k}) != k {
			t.Errorf("KindOf(%s)", k)
		}
	}
}
