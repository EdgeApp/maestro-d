package devicefarm

import (
	"regexp"
	"strings"
)

// redacted replaces every secret.
const redacted = "<redacted>"

var (
	// The managed Appium endpoint: the path after remote-endpoint/ is the
	// credential.
	reRemoteEndpoint = regexp.MustCompile(`remote-endpoint/[^\s"'<>\x60]*`)
	// Interactive (video and control) endpoints: keep scheme and host.
	reInteractive = regexp.MustCompile(`((?:https?|wss?)://[^/\s"'<>?\x60]*interactive[^/\s"'<>?\x60]*)[/?][^\s"'<>\x60]*`)
	// S3 presigned upload URLs: everything after the ?.
	reS3Query = regexp.MustCompile(`(https?://[^/\s"'<>?\x60]*s3[^/\s"'<>?\x60]*amazonaws\.com[^\s"'<>?\x60]*)\?[^\s"'<>\x60]*`)
	// Any other presigned query string.
	rePresigned = regexp.MustCompile(`\?[^\s"'<>\x60]*X-Amz-[^\s"'<>\x60]*`)
	// Stray presigned parameters outside a URL.
	reAmzParam = regexp.MustCompile(`(X-Amz-[A-Za-z-]+)=[^&\s"'<>\x60]*`)
	// AWS access key ids: long-term (AKIA) and temporary (ASIA).
	reKeyID = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
)

// Redact removes Device Farm secrets from s: endpoint paths, presigned query
// strings and AWS access key ids.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = reRemoteEndpoint.ReplaceAllString(s, "remote-endpoint/"+redacted)
	s = reInteractive.ReplaceAllString(s, "$1/"+redacted)
	s = reS3Query.ReplaceAllString(s, "$1?"+redacted)
	s = rePresigned.ReplaceAllString(s, "?"+redacted)
	s = reAmzParam.ReplaceAllString(s, "$1="+redacted)
	s = reKeyID.ReplaceAllString(s, redacted)
	return s
}

// Redactor is Redact that also removes the literal credentials a client
// loaded, whatever their shape.
type Redactor struct {
	secrets []string
}

// NewRedactor returns a Redactor that removes the given secrets (empty ones
// are ignored) on top of Redact's patterns.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if s != "" {
			r.secrets = append(r.secrets, s)
		}
	}
	return r
}

// Redact removes the known secrets, then applies the package Redact.
func (r *Redactor) Redact(s string) string {
	if r != nil {
		for _, secret := range r.secrets {
			s = strings.ReplaceAll(s, secret, redacted)
		}
	}
	return Redact(s)
}
