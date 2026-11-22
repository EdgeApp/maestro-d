package devicefarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

func apiError(op, code, msg string) error {
	return &smithy.OperationError{ServiceID: "Device Farm", OperationName: op,
		Err: &smithy.GenericAPIError{Code: code, Message: msg}}
}

func TestClassifyKinds(t *testing.T) {
	cases := []struct {
		err    error
		kind   cloudprov.Kind
		action string
	}{
		{apiError("CreateRemoteAccessSession", "AccessDeniedException",
			"User: arn:aws:iam::123456789012:user/ci-runner is not authorized to perform: devicefarm:CreateRemoteAccessSession on resource: arn:x"),
			cloudprov.Auth, "devicefarm:CreateRemoteAccessSession"},
		{apiError("ListDevices", "AccessDeniedException", "denied"), cloudprov.Auth, ""},
		{apiError("ListDevices", "UnrecognizedClientException", "bad token"), cloudprov.Auth, ""},
		{apiError("ListDevices", "InvalidSignatureException", "bad sig"), cloudprov.Auth, ""},
		{apiError("CreateRemoteAccessSession", "LimitExceededException", "limit"), cloudprov.NoCapacity, ""},
		{apiError("CreateRemoteAccessSession", "ServiceAccountException", "account"), cloudprov.NoCapacity, ""},
		{apiError("GetRemoteAccessSession", "NotFoundException", "none"), cloudprov.NotFound, ""},
		{apiError("CreateUpload", "ArgumentException", "bad"), cloudprov.Other, ""},
		{&smithy.GenericAPIError{Code: "NotFoundException", Message: "bare"}, cloudprov.NotFound, ""},
		{fmt.Errorf("wrapped: %w", ErrNoCredentials), cloudprov.Auth, ""},
		{context.Canceled, cloudprov.Other, ""},
		{errors.New("boom"), cloudprov.Other, ""},
		{cloudprov.Errorf(cloudprov.DeviceBusy, "busy"), cloudprov.DeviceBusy, ""},
		{fmt.Errorf("x: %w", cloudprov.Errorf(cloudprov.Ended, "ended")), cloudprov.Ended, ""},
	}
	for _, tc := range cases {
		ce := Classify(tc.err)
		if ce.Kind != tc.kind || ce.Action != tc.action {
			t.Errorf("%v: kind %s action %q, want %s %q", tc.err, ce.Kind, ce.Action, tc.kind, tc.action)
		}
		if ce.Msg == "" {
			t.Errorf("%v: no message", tc.err)
		}
	}
	if Classify(nil) != nil {
		t.Fatal("nil classified")
	}
	ce := Classify(cases[0].err)
	if !strings.Contains(ce.Msg, "CreateRemoteAccessSession") || !strings.Contains(ce.Msg, "IAM policy") || !errors.Is(ce, ce.Err) {
		t.Fatalf("msg = %q", ce.Msg)
	}
	if !strings.HasPrefix(Classify(cases[8].err).Msg, "Device Farm: NotFoundException") {
		t.Fatalf("bare msg = %q", Classify(cases[8].err).Msg)
	}
}

func TestClassifyRedacts(t *testing.T) {
	secret := "https://devicefarm.us-west-2.amazonaws.com/remote-endpoint/s3cr3t/wd/hub key AKIAABCDEFGHIJKLMNOP"
	for _, err := range []error{
		apiError("GetRemoteAccessSession", "ArgumentException", secret),
		errors.New(secret),
		&cloudprov.Error{Kind: cloudprov.Other, Msg: secret},
	} {
		msg := Classify(err).Msg
		if strings.Contains(msg, "s3cr3t") || strings.Contains(msg, "AKIAABCDEFGHIJKLMNOP") {
			t.Errorf("not redacted: %q", msg)
		}
	}
	if !isCode(apiError("X", "NotFoundException", ""), "NotFoundException") || isCode(errors.New("x"), "NotFoundException") {
		t.Fatal("isCode")
	}
}
