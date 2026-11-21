package cli

// daemon_devicefarm_e2e_test.go: the maestro-d binary against a fake AWS
// Device Farm (pkg/devicefarm/dftest), with no credentials or network:
// locations and filters, the cloud catalog, and (from M3) launching,
// driving and releasing cloud devices.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
)

// awsEnvDrop are the inherited variables that could hand a test daemon real
// AWS credentials.
var awsEnvDrop = []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_*", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE",
	"MAESTRO_D_DEVICEFARM_PROJECT"}

// dfEnv pins the AWS credential chain of every command e runs: the
// inherited AWS variables are dropped, the shared files are empty files in
// the test's temp dir, instance metadata is off, and fake points the
// provider at the fake. It returns the variables to add, with the fake's
// keys when withKey.
func dfEnv(t *testing.T, e *e2e, fake *dftest.Fake, withKey bool) []string {
	t.Helper()
	e.drop = append(e.drop, awsEnvDrop...)
	dir := t.TempDir()
	creds, conf := filepath.Join(dir, "credentials"), filepath.Join(dir, "config")
	for _, f := range []string{creds, conf} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"AWS_SHARED_CREDENTIALS_FILE=" + creds, "AWS_CONFIG_FILE=" + conf, "AWS_EC2_METADATA_DISABLED=true",
		"MAESTRO_D_DEVICEFARM_ENDPOINT=" + fake.URL()}
	if withKey {
		env = append(env, "AWS_ACCESS_KEY_ID="+fakeKeyID, "AWS_SECRET_ACCESS_KEY=fake-secret-for-tests")
	}
	return env
}

// fakeKeyID is the access key id the fake-backed tests use; it must never
// appear in a run file, a log or an answer.
const fakeKeyID = "AKIAFAKEFAKEFAKE0001"

func TestE2E_DeviceFarmCatalog(t *testing.T) {
	e := newE2E(t)
	fake := dftest.New(t)
	env := append(dfEnv(t, e, fake, true), "MAESTRO_D_MOCK_DEVICES=mock-1")
	dflag := []string{"--daemon", "dfcat"}

	// Every entry has a location; the mock device is on this machine.
	r := e.runEnv(env, append([]string{"device", "list", "--json"}, dflag...)...)
	var listed daemon.DevicesResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &listed) != nil {
		t.Fatalf("device list: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	var mock *daemon.DeviceInfo
	for i := range listed.Devices {
		if listed.Devices[i].Location == "" {
			t.Fatalf("%s has no location", listed.Devices[i].ID)
		}
		if listed.Devices[i].ID == "mock-1" {
			mock = &listed.Devices[i]
		}
	}
	if mock == nil || mock.Location != daemon.LocationLocal || mock.Machine != daemon.MachineName() {
		t.Fatalf("mock-1: %+v", mock)
	}
	r = e.runEnv(env, append([]string{"device", "list", "--location", "mesh", "--json"}, dflag...)...)
	if r.code != 0 || strings.Contains(r.stdout, `"mock-1"`) {
		t.Fatalf("--location mesh: exit %d\n%s", r.code, r.stdout)
	}
	r = e.runEnv(env, append([]string{"device", "list", "--name", "^mock", "--location", "local"}, dflag...)...)
	if r.code != 0 || !strings.Contains(r.stdout, "LOCATION") || !strings.Contains(r.stdout, "mock-1") {
		t.Fatalf("table: %s%s", r.stdout, r.stderr)
	}
	if r = e.runEnv(env, append([]string{"device", "list", "--name", "(["}, dflag...)...); r.code != 2 || envelopeCode(t, r.stderr) != daemon.CodeUsage {
		t.Fatalf("bad regexp: exit %d\n%s", r.code, r.stderr)
	}

	// The cloud catalog, filtered on the daemon.
	r = e.runEnv(env, append([]string{"device", "catalog", "--location", "cloud", "--platform", "ios", "--os-min", "16", "--os-max", "18", "--json"}, dflag...)...)
	var cat daemon.CatalogResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &cat) != nil || len(cat.Cloud) != 1 {
		t.Fatalf("catalog: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if ms := cat.Cloud[0].Models; len(ms) != 1 || ms[0].Name != "Apple iPhone 15" || ms[0].ID != "df:ios:apple-iphone-15:17.4" || cat.Cloud[0].Provider != "devicefarm" {
		t.Fatalf("catalog models: %+v", cat.Cloud)
	}
	r = e.runEnv(env, append([]string{"device", "catalog", "--location", "cloud", "--platform", "android", "--name", "pixel", "--available"}, dflag...)...)
	if r.code != 0 || !strings.Contains(r.stdout, "Google Pixel 8") || !strings.Contains(r.stdout, "AVAILABILITY") || strings.Contains(r.stdout, "Galaxy") {
		t.Fatalf("catalog table: %s%s", r.stdout, r.stderr)
	}
	for _, op := range []string{"ListDevices"} {
		if fake.CountOp(op) == 0 {
			t.Fatalf("the fake never saw %s: %v", op, fake.Ops())
		}
	}

	// Another daemon without AWS credentials answers USAGE naming them.
	noKey := dfEnv(t, e, fake, false)
	r = e.runEnv(noKey, "device", "catalog", "--location", "cloud", "--daemon", "dfnokey")
	if r.code != 2 || envelopeCode(t, r.stderr) != daemon.CodeUsage || !strings.Contains(r.stderr, "AWS_ACCESS_KEY_ID") {
		t.Fatalf("without credentials: exit %d\n%s", r.code, r.stderr)
	}
	// Without --location cloud the catalog asks no provider.
	before := fake.CountOp("ListDevices")
	e.runEnv(noKey, "device", "catalog", "--platform", "android", "--daemon", "dfnokey")
	if fake.CountOp("ListDevices") != before {
		t.Fatal("a local catalog asked Device Farm")
	}
}
