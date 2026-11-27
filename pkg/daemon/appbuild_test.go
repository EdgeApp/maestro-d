package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
)

// TestApps_InstallKeepsBuild: an install from a URL keeps the build per
// device and app id, a new build replaces it, an ETag counts as installed
// only while its build is kept, and an uninstall forgets both.
func TestApps_InstallKeepsBuild(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	var calls appCalls
	appDeps(env, &calls)
	b := newBuildServers(t)
	allowInstallHosts(t, "127.0.0.1")
	ctx := context.Background()
	const id, app = "00008120-PHONE", "app.teamstation.develop"
	req := InstallRequest{URL: b.mason.URL + "/api/v2/builds/7/download", AppID: app,
		Headers: map[string]string{"Authorization": "Bearer mason-key"}}

	if got := keptBuild(id, app); got != "" {
		t.Fatalf("kept before any install: %q", got)
	}
	if res, err := env.remote.InstallApp(ctx, id, req); err != nil || res.Cached {
		t.Fatalf("install: %+v %v", res, err)
	}
	kept := keptBuild(id, app)
	if data, err := os.ReadFile(kept); err != nil || string(data) != b.body || filepath.Base(kept) != "app-release.apk" {
		t.Fatalf("kept build %q: %q %v", kept, data, err)
	}
	if filepath.Dir(kept) != filepath.Join(Home(), buildsDirName, id, app) {
		t.Fatalf("kept in %s", kept)
	}

	// The same build is cached while it is kept; once it is gone the
	// download and install run again, and keep it again.
	if res, err := env.remote.InstallApp(ctx, id, req); err != nil || !res.Cached || len(calls.installs) != 1 {
		t.Fatalf("cached: %+v %v", res, err)
	}
	if err := os.RemoveAll(filepath.Dir(kept)); err != nil {
		t.Fatal(err)
	}
	if res, err := env.remote.InstallApp(ctx, id, req); err != nil || res.Cached || len(calls.installs) != 2 || keptBuild(id, app) == "" {
		t.Fatalf("not kept: %+v %v %+v", res, err, calls)
	}

	// A new build replaces the kept one: one file, the new build's.
	b.mu.Lock()
	b.etag, b.body, b.disposition = `"v2"`, "PK v2", `attachment; filename="TeamStation.ipa"`
	b.mu.Unlock()
	if res, err := env.remote.InstallApp(ctx, id, req); err != nil || res.Cached || res.ETag != `"v2"` {
		t.Fatalf("new build: %+v %v", res, err)
	}
	entries, _ := os.ReadDir(filepath.Join(Home(), buildsDirName, id, app))
	if len(entries) != 1 || entries[0].Name() != "TeamStation.ipa" {
		t.Fatalf("kept files: %v", entries)
	}
	if data, _ := os.ReadFile(keptBuild(id, app)); string(data) != "PK v2" {
		t.Fatalf("kept %q", data)
	}

	// An uninstall forgets the ETag and the build: the next install runs.
	if err := env.remote.UninstallApp(ctx, id, app); err != nil {
		t.Fatal(err)
	}
	if keptBuild(id, app) != "" || env.s.installedETag(id, app) != "" {
		t.Fatalf("uninstall kept %q %q", keptBuild(id, app), env.s.installedETag(id, app))
	}
	if res, err := env.remote.InstallApp(ctx, id, req); err != nil || res.Cached {
		t.Fatalf("after uninstall: %+v %v", res, err)
	}
}

// A device or app id never names a directory outside builds/.
func TestBuildDir_SafeNames(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	root := filepath.Join(Home(), buildsDirName)
	for _, c := range []struct{ id, app, want string }{
		{"..", ".", filepath.Join(root, "_", "_")},
		{"", "a/b", filepath.Join(root, "_", "a_b")},
		{"emulator-5554", "app.teamstation.develop", filepath.Join(root, "emulator-5554", "app.teamstation.develop")},
	} {
		if got := buildDir(c.id, c.app); got != c.want {
			t.Errorf("buildDir(%q, %q) = %s, want %s", c.id, c.app, got, c.want)
		}
	}
}

// keepBuild copies a build it cannot rename (another volume), and logs a
// build it cannot keep without failing.
func TestKeepBuild_MovesAndFailures(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	src := filepath.Join(t.TempDir(), "app.ipa")
	if err := os.WriteFile(src, []byte("PK"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := renameFile
	t.Cleanup(func() { renameFile = saved })
	renameFile = func(from, to string) error {
		if filepath.Ext(from) == ".ipa" {
			return errors.New("cross-device link")
		}
		return saved(from, to)
	}
	keepBuild("d", "a", src)
	if data, err := os.ReadFile(keptBuild("d", "a")); err != nil || string(data) != "PK" {
		t.Fatalf("copied: %q %v", data, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source left: %v", err)
	}

	// A source that is gone is not kept, and the earlier build stays.
	keepBuild("d", "a", filepath.Join(t.TempDir(), "gone.ipa"))
	if keptBuild("d", "a") == "" {
		t.Fatal("earlier build lost")
	}
	// The kept directory cannot be swapped in: logged.
	renameFile = func(string, string) error { return errors.New("busy") }
	other := filepath.Join(t.TempDir(), "b.apk")
	_ = os.WriteFile(other, []byte("PK"), 0o600)
	if err := moveFile(other, filepath.Join(t.TempDir(), "missing", "b.apk")); err == nil {
		t.Fatal("copy into a missing directory succeeded")
	}
	keepBuild("d", "b", other)
	if keptBuild("d", "b") != "" {
		t.Fatal("kept a build whose directory was not swapped in")
	}
	// The builds directory cannot be made: logged.
	blocked := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	t.Setenv(EnvHome, blocked)
	keepBuild("d", "a", other)
}

type innerDriver struct{ core.Driver }

func (w innerDriver) Inner() core.Driver { return w.Driver }

type buildUser struct {
	*mock.Driver
	src core.AppBuildSource
}

func (b *buildUser) SetAppBuildSource(src core.AppBuildSource) { b.src = src }

// A driver that reinstalls from a build, wrapped or not, gets the builds
// kept for its own device.
func TestGiveBuildSource(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	src := filepath.Join(t.TempDir(), "app.ipa")
	_ = os.WriteFile(src, []byte("PK"), 0o600)
	keepBuild("phone-1", "a", src)
	u := &buildUser{Driver: mock.New(mock.Config{})}
	giveBuildSource("phone-1", innerDriver{u})
	if u.src == nil || u.src("a") != keptBuild("phone-1", "a") || u.src("other") != "" {
		t.Fatalf("source: %v", u.src)
	}
	giveBuildSource("phone-1", mock.New(mock.Config{})) // no AppBuildUser: nothing to give
}
