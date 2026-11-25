package hubcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
)

const fakeKeyID = "AKIAFAKEFAKEFAKE0001"

func writeCreds(t *testing.T, path string) {
	t.Helper()
	data := "[default]\naws_access_key_id = " + fakeKeyID + "\naws_secret_access_key = fake-secret-for-tests\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHubCloudFlags(t *testing.T) {
	creds := filepath.Join(t.TempDir(), "creds")
	writeCreds(t, creds)
	o := hubFlags(t, "--devicefarm", "--devicefarm-credentials-file", creds, "--devicefarm-profile", "p",
		"--devicefarm-project", "proj", "--cloud-max-devices", "3")
	if err := checkCloudFlags(o); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(hubChildArgs(o), " ")
	for _, want := range []string{"--devicefarm", "--devicefarm-credentials-file " + creds, "--devicefarm-profile p", "--devicefarm-project proj", "--cloud-max-devices 3"} {
		if !strings.Contains(args, want) {
			t.Errorf("child args lack %q: %s", want, args)
		}
	}
	// The child's args, which the service records, name the file, never a key.
	if strings.Contains(args, fakeKeyID) {
		t.Fatalf("a key in the child args: %s", args)
	}
	o = hubFlags(t, "--devicefarm")
	if err := checkCloudFlags(o); err != nil || o.hub.cloudMax != defaultCloudMaxDevices {
		t.Fatalf("default cap: %d %v", o.hub.cloudMax, err)
	}
	loose := filepath.Join(t.TempDir(), "loose")
	writeCreds(t, loose)
	_ = os.Chmod(loose, 0o640)
	for _, bad := range [][]string{
		{"--cloud-max-devices", "2"},
		{"--devicefarm", "--cloud-max-devices", "21"},
		{"--devicefarm", "--cloud-max-devices", "-1"},
		{"--devicefarm", "--devicefarm-credentials-file", "/nonexistent/creds"},
		{"--devicefarm", "--devicefarm-credentials-file", t.TempDir()},
		{"--devicefarm", "--devicefarm-credentials-file", loose},
	} {
		o := hubFlags(t, bad...)
		if err := checkCloudFlags(o); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
	// The start check: a provider that cannot be built, or an account call
	// that fails, stops the hub.
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	fake := dftest.New(t)
	if _, err := hubDeviceFarm(devicefarm.Options{Endpoint: fake.URL()}); err == nil || !strings.Contains(err.Error(), "--devicefarm") {
		t.Fatalf("no credentials: %v", err)
	}
	fake.FailOp("GetAccountSettings", "AccessDeniedException", "not authorized to perform: devicefarm:GetAccountSettings")
	if _, err := hubDeviceFarm(devicefarm.Options{Endpoint: fake.URL(), CredentialsFile: creds}); err == nil {
		t.Fatal("a failing start check")
	}
	fake2 := dftest.New(t)
	if p, err := hubDeviceFarm(devicefarm.Options{Endpoint: fake2.URL(), CredentialsFile: creds}); err != nil || p == nil {
		t.Fatalf("start check: %v", err)
	}
	o = hubFlags(t, "--devicefarm")
	o.hub.dfOpts = devicefarm.Options{Endpoint: fake2.URL(), CredentialsFile: creds}
	deps, err := hubDeps(o)
	if err != nil || deps.Providers["devicefarm"] == nil {
		t.Fatalf("hubDeps: %v", err)
	}
	if p, err := deps.Providers["devicefarm"](); err != nil || p.Name() != "devicefarm" {
		t.Fatalf("provider: %v", err)
	}
	if names := providerNames(deps); strings.Join(names, ",") != "devicefarm" {
		t.Fatalf("names %v", names)
	}
	if deps, _ := hubDeps(&daemonOpts{}); deps.Providers != nil {
		t.Fatal("a hub without --devicefarm has a provider")
	}
}
