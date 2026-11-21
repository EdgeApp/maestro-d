package devicefarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
)

func TestNewRegionAndEndpointOption(t *testing.T) {
	c, f := newTestClient(t)
	if got := c.api.Options().Region; got != Region || Region != "us-west-2" {
		t.Fatalf("region = %q", got)
	}
	if _, err := c.Account(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.CountOp("GetAccountSettings") != 1 {
		t.Fatalf("ops = %v", f.Ops())
	}
	if c.project != DefaultProject {
		t.Fatalf("project = %q", c.project)
	}
}

func TestNewEndpointFromEnvironment(t *testing.T) {
	pinAWS(t)
	withKeys(t)
	t.Setenv("AWS_REGION", "eu-west-1") // ignored: Device Farm is us-west-2 only
	f := dftest.New(t)
	t.Setenv(EnvEndpoint, f.URL())
	t.Setenv(EnvProject, "other-project")
	c, err := New(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.api.Options().Region != Region {
		t.Fatalf("region = %q", c.api.Options().Region)
	}
	if _, err := c.Account(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.CountOp("GetAccountSettings") != 1 {
		t.Fatalf("ops = %v", f.Ops())
	}
	if c.project != "other-project" {
		t.Fatalf("project = %q", c.project)
	}
	c2, err := New(context.Background(), Options{Project: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if c2.project != "explicit" {
		t.Fatalf("project = %q, want the option over the environment", c2.project)
	}
}

func TestNewMissingCredentials(t *testing.T) {
	pinAWS(t)
	f := dftest.New(t)
	_, err := New(context.Background(), Options{Endpoint: f.URL()})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	for _, want := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_PROFILE",
		"~/.aws/credentials", "~/.aws/config", "own environment", "maestro-d stop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	if len(f.Requests()) != 0 {
		t.Fatalf("a client without credentials called the API: %v", f.Ops())
	}
	// A named profile that does not exist is the same failure.
	_, err = New(context.Background(), Options{Endpoint: f.URL(), Profile: "nope"})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("missing profile: err = %v", err)
	}
	if ce := Classify(err); ce.Kind != "auth" {
		t.Fatalf("kind = %q", ce.Kind)
	}
}

func TestNewCredentialPrecedence(t *testing.T) {
	pinAWS(t)
	f := dftest.New(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "df-credentials")
	if err := os.WriteFile(file, []byte("[default]\naws_access_key_id = FILEKEYID\naws_secret_access_key = FILESECRET\n\n"+
		"[ci]\naws_access_key_id = CIKEYID\naws_secret_access_key = CISECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if err := os.WriteFile(shared, []byte("[named]\naws_access_key_id = PROFILEKEYID\naws_secret_access_key = PROFILESECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "ENVKEYID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ENVSECRET")

	knows := func(c *Client, secret string) bool { return c.Redactor().Redact(secret) == redacted }
	cases := []struct {
		name string
		opts Options
		want string
		not  string
	}{
		{"environment chain", Options{}, "ENVKEYID", "FILEKEYID"},
		{"file beats environment", Options{CredentialsFile: file}, "FILEKEYID", "ENVKEYID"},
		{"file and profile", Options{CredentialsFile: file, Profile: "ci"}, "CIKEYID", "FILEKEYID"},
		{"profile beats environment", Options{Profile: "named"}, "PROFILEKEYID", "ENVKEYID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Endpoint = f.URL()
			c, err := New(context.Background(), tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !knows(c, tc.want) || knows(c, tc.not) {
				t.Fatalf("loaded the wrong key: knows %s=%v %s=%v", tc.want, knows(c, tc.want), tc.not, knows(c, tc.not))
			}
		})
	}
}
