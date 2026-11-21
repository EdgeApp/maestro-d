package devicefarm

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/aws/smithy-go"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// reDeniedAction finds the action in an AccessDeniedException message:
// "... is not authorized to perform: devicefarm:CreateRemoteAccessSession on ...".
var reDeniedAction = regexp.MustCompile(`perform:\s*([A-Za-z0-9-]+:[A-Za-z0-9*]+)`)

// Classify maps an error from this package or the AWS SDK to a
// *cloudprov.Error with a redacted message. A *cloudprov.Error passes
// through; nil stays nil.
func Classify(err error) *cloudprov.Error {
	if err == nil {
		return nil
	}
	var ce *cloudprov.Error
	if errors.As(err, &ce) {
		out := *ce
		out.Msg = Redact(out.Msg)
		return &out
	}
	if errors.Is(err, ErrNoCredentials) {
		return &cloudprov.Error{Kind: cloudprov.Auth, Msg: Redact(err.Error()), Err: err}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &cloudprov.Error{Kind: cloudprov.Other, Msg: Redact("Device Farm: " + err.Error()), Err: err}
	}
	var api smithy.APIError
	if !errors.As(err, &api) {
		return &cloudprov.Error{Kind: cloudprov.Other, Msg: Redact(err.Error()), Err: err}
	}
	op := "Device Farm"
	var oe *smithy.OperationError
	if errors.As(err, &oe) {
		op = "Device Farm " + oe.Operation()
	}
	msg := fmt.Sprintf("%s: %s: %s", op, api.ErrorCode(), api.ErrorMessage())
	out := &cloudprov.Error{Kind: cloudprov.Other, Err: err}
	switch api.ErrorCode() {
	case "AccessDeniedException":
		out.Kind = cloudprov.Auth
		if m := reDeniedAction.FindStringSubmatch(api.ErrorMessage()); m != nil {
			out.Action = m[1]
			msg = fmt.Sprintf("%s: AWS denied %s to these credentials; allow it in their IAM policy (%s)", op, m[1], api.ErrorMessage())
		}
	case "UnrecognizedClientException", "InvalidClientTokenId", "InvalidSignatureException",
		"SignatureDoesNotMatch", "ExpiredTokenException", "IncompleteSignature", "MissingAuthenticationToken":
		out.Kind = cloudprov.Auth
	case "LimitExceededException", "ServiceAccountException":
		out.Kind = cloudprov.NoCapacity
	case "NotFoundException":
		out.Kind = cloudprov.NotFound
	}
	out.Msg = Redact(msg)
	return out
}

// isCode reports whether err is an AWS API error with the given code.
func isCode(err error, code string) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == code
}
