package devicefarm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
)

// Fake credentials. The key id has the AKIA shape so the pattern redaction
// is exercised too.
const (
	testKeyID  = "AKIATESTTESTTESTTEST"
	testSecret = "testSecretKeyNeverReal0000000000000000000"
)

// pinAWS isolates the AWS credential chain from the machine running the
// test: no profile, no keys, empty shared files, no instance metadata.
func pinAWS(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_DEVICE_FARM",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_ACCESS_KEY", "AWS_SECRET_KEY",
		EnvEndpoint, EnvProject,
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")
	conf := filepath.Join(dir, "config")
	for _, p := range []string{creds, conf} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWS_CONFIG_FILE", conf)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// withKeys puts the fake keys in the environment.
func withKeys(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", testKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", testSecret)
}

// fast shortens every wait in the package for the test.
func fast(t *testing.T) {
	t.Helper()
	saved := []any{uploadPollInterval, uploadPollTimeout, sessionPollInterval, pendingDeviceTimeout, bootTimeoutUnit, stopTimeout,
		finalMinutesWait, finalMinutesPoll}
	uploadPollInterval = 2 * time.Millisecond
	uploadPollTimeout = 2 * time.Second
	sessionPollInterval = 2 * time.Millisecond
	pendingDeviceTimeout = 30 * time.Millisecond
	bootTimeoutUnit = 100 * time.Millisecond
	stopTimeout = 5 * time.Second
	finalMinutesWait, finalMinutesPoll = 200*time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() {
		uploadPollInterval = saved[0].(time.Duration)
		uploadPollTimeout = saved[1].(time.Duration)
		sessionPollInterval = saved[2].(time.Duration)
		pendingDeviceTimeout = saved[3].(time.Duration)
		bootTimeoutUnit = saved[4].(time.Duration)
		stopTimeout = saved[5].(time.Duration)
		finalMinutesWait = saved[6].(time.Duration)
		finalMinutesPoll = saved[7].(time.Duration)
	})
}

// newTestClient starts a fake and a client on it with the fake keys.
func newTestClient(t *testing.T) (*Client, *dftest.Fake) {
	t.Helper()
	pinAWS(t)
	withKeys(t)
	fast(t)
	f := dftest.New(t)
	c, err := New(context.Background(), Options{Endpoint: f.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, f
}

// lastBody decodes the body of the last request of op.
func lastBody(t *testing.T, f *dftest.Fake, op string) map[string]any {
	t.Helper()
	reqs := f.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Op == op {
			var m map[string]any
			if err := json.Unmarshal(reqs[i].Body, &m); err != nil {
				t.Fatalf("%s body: %v", op, err)
			}
			return m
		}
	}
	t.Fatalf("no %s request", op)
	return nil
}

// writeFile writes content to name in a temp dir and returns the path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
