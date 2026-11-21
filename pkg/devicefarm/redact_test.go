package devicefarm

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"nothing secret here", "nothing secret here"},
		{"endpoint https://devicefarm-remote.us-west-2.api.aws/remote-endpoint/AbC123xyz/wd/hub/session failed",
			"endpoint https://devicefarm-remote.us-west-2.api.aws/remote-endpoint/<redacted> failed"},
		{`{"url":"http://127.0.0.1:5555/remote-endpoint/tok/wd/hub"}`,
			`{"url":"http://127.0.0.1:5555/remote-endpoint/<redacted>"}`},
		{"video wss://devicefarm-interactive-global.us-west-2.api.aws/websockify?token=abc&X-Amz-Signature=ff",
			"video wss://devicefarm-interactive-global.us-west-2.api.aws/<redacted>"},
		{"PUT https://prod-us-west-2-uploads.s3-us-west-2.amazonaws.com/arn%3Aaws/u.apk?AWSAccessKeyId=x&Expires=1&Signature=abc done",
			"PUT https://prod-us-west-2-uploads.s3-us-west-2.amazonaws.com/arn%3Aaws/u.apk?<redacted> done"},
		{"http://127.0.0.1:1/upload/tok?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=beef",
			"http://127.0.0.1:1/upload/tok?<redacted>"},
		{"signature X-Amz-Signature=beef&X-Amz-Credential=cred",
			"signature X-Amz-Signature=<redacted>&X-Amz-Credential=<redacted>"},
		{"key AKIAIOSFODNN7EXAMPLE and ASIAIOSFODNN7EXAMPLE", "key <redacted> and <redacted>"},
		{"not a key AKIAshort", "not a key AKIAshort"},
	}
	for _, tc := range cases {
		if got := Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactorKnowsLoadedKeys(t *testing.T) {
	r := NewRedactor("CUSTOMKEYID", "", "the-secret")
	got := r.Redact("id CUSTOMKEYID secret the-secret at /remote-endpoint/x")
	if strings.Contains(got, "CUSTOMKEYID") || strings.Contains(got, "the-secret") || strings.Contains(got, "/remote-endpoint/x") {
		t.Fatalf("got %q", got)
	}
	var nilR *Redactor
	if nilR.Redact("AKIAIOSFODNN7EXAMPLE") != redacted {
		t.Fatal("nil redactor")
	}
}
