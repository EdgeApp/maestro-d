package emulator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scriptExec fakes every command: reply returns its stdout and whether it
// succeeds. Each call is recorded as "name arg …" and its stdin kept.
type scripted struct {
	calls []string
}

func scriptExec(t *testing.T, reply func(name string, args []string) (string, bool)) *scripted {
	t.Helper()
	s := &scripted{}
	withFakeExec(t, func(name string, args ...string) *exec.Cmd {
		s.calls = append(s.calls, strings.TrimSpace(filepath.Base(name)+" "+strings.Join(args, " ")))
		out, ok := reply(filepath.Base(name), args)
		if ok {
			return exec.Command("printf", "%s", out)
		}
		return exec.Command("sh", "-c", `printf '%s' "$0"; exit 1`, out)
	})
	return s
}

// sdkTools puts fake sdkmanager and avdmanager under a temporary
// ANDROID_HOME, so discovery finds them without PATH.
func sdkTools(t *testing.T) string {
	t.Helper()
	withFakeLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	home := t.TempDir()
	bin := filepath.Join(home, "cmdline-tools", "latest", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sdkmanager", "avdmanager"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ANDROID_HOME", home)
	return bin
}

const listInstalled = "WARNING: The SDK Manager CLI tool (sdkmanager) is deprecated.\n" +
	"Loading package information...    \r[=====    ] 25% Loading local repository...   \rInstalled packages:\n" +
	"  Path                                           | Version | Description                         | Location\n" +
	"  -------                                        | ------- | -------                             | -------\n" +
	"  build-tools;35.0.0                             | 35.0.0  | Android SDK Build-Tools 35          | build-tools/35.0.0\n" +
	"  system-images;android-35;google_apis;arm64-v8a | 9       | Google APIs ARM 64 v8a System Image | system-images/android-35/google_apis/arm64-v8a\n" +
	"  system-images;android-34;google_apis;arm64-v8a | 3       | Google APIs ARM 64 v8a System Image | system-images/android-34/google_apis/arm64-v8a\n" +
	"  system-images;android-36;google_apis_playstore;arm64-v8a | 1 | Google Play ARM 64 v8a System Image | x\n" +
	"  system-images;android-36;google_apis;x86_64    | 1       | Google APIs Intel x86_64 Atom       | x\n" +
	"  system-images;broken                           | 1       | malformed                           | x\n"

const listAll = "Installed packages:\n" +
	"  system-images;android-35;google_apis;arm64-v8a | 9 | Google APIs ARM 64 v8a System Image | system-images/android-35/google_apis/arm64-v8a\n" +
	"\nAvailable Packages:\n" +
	"  system-images;android-35;google_apis;arm64-v8a | 9 | Google APIs ARM 64 v8a System Image\n" +
	"  system-images;android-36;google_apis;arm64-v8a | 2 | Google APIs ARM 64 v8a System Image\n" +
	"\nAvailable Updates:\n" +
	"  system-images;android-34;default;arm64-v8a | 2 | 3\n"

func TestParseSDKImages_ListInstalled(t *testing.T) {
	imgs := parseSDKImages(listInstalled)
	if len(imgs) != 4 {
		t.Fatalf("images = %+v, want the four well-formed system images", imgs)
	}
	got := imgs[0]
	if got.ID != "system-images;android-35;google_apis;arm64-v8a" || got.API != "android-35" || got.Tag != "google_apis" ||
		got.ABI != "arm64-v8a" || !got.Installed || got.Description != "Google APIs ARM 64 v8a System Image" {
		t.Errorf("first image = %+v", got)
	}
	for _, img := range imgs {
		if !img.Installed {
			t.Errorf("--list_installed rows are installed: %+v", img)
		}
	}
}

func TestParseSDKImages_ListMarksAvailable(t *testing.T) {
	imgs := parseSDKImages(listAll)
	if len(imgs) != 3 {
		t.Fatalf("images = %+v, want 3 (the installed one listed once)", imgs)
	}
	want := map[string]bool{
		"system-images;android-35;google_apis;arm64-v8a": true,
		"system-images;android-36;google_apis;arm64-v8a": false,
		"system-images;android-34;default;arm64-v8a":     false,
	}
	for _, img := range imgs {
		if img.Installed != want[img.ID] {
			t.Errorf("%s installed = %v", img.ID, img.Installed)
		}
	}
}

func TestParseSDKImages_Empty(t *testing.T) {
	if imgs := parseSDKImages(""); imgs == nil || len(imgs) != 0 {
		t.Errorf("empty output = %#v, want an empty list", imgs)
	}
}

func TestParseProfiles(t *testing.T) {
	out := "Error: Could not load devices from /sdk/system-images/x/devices.xml\nWarning: x\n\npixel_8\nNexus 5X\n  pixel_9  \n"
	got := parseProfiles(out)
	if strings.Join(got, ",") != "pixel_8,Nexus 5X,pixel_9" {
		t.Errorf("profiles = %q", got)
	}
	if p := parseProfiles(""); p == nil || len(p) != 0 {
		t.Errorf("empty = %#v", p)
	}
}

func TestFindSDKManager(t *testing.T) {
	bin := sdkTools(t)
	if p, err := findSDKManager(); err != nil || p != filepath.Join(bin, "sdkmanager") {
		t.Errorf("ANDROID_HOME layout: %q, %v", p, err)
	}

	// The old tools/bin layout.
	old := t.TempDir()
	if err := os.MkdirAll(filepath.Join(old, "tools", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "tools", "bin", "sdkmanager"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANDROID_HOME", old)
	if p, err := findSDKManager(); err != nil || p != filepath.Join(old, "tools", "bin", "sdkmanager") {
		t.Errorf("tools/bin layout: %q, %v", p, err)
	}

	// PATH.
	t.Setenv("ANDROID_HOME", "")
	withFakeLookPath(t, func(s string) (string, error) { return "/usr/local/bin/" + s, nil })
	if p, err := findSDKManager(); err != nil || p != "/usr/local/bin/sdkmanager" {
		t.Errorf("PATH: %q, %v", p, err)
	}
}

func TestFindTools_MissingNamesToolAndInstall(t *testing.T) {
	withFakeLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	for name, find := range map[string]func() (string, error){"sdkmanager": findSDKManager, "avdmanager": findAVDManager} {
		_, err := find()
		if err == nil || !strings.Contains(err.Error(), name+" not found") || !strings.Contains(err.Error(), "command-line tools") ||
			!strings.Contains(err.Error(), "ANDROID_HOME") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ListCatalog(false); err == nil || !strings.Contains(err.Error(), "sdkmanager not found") {
		t.Errorf("ListCatalog without sdkmanager: %v", err)
	}
	// sdkmanager present, avdmanager missing.
	withFakeLookPath(t, func(s string) (string, error) {
		if s == "sdkmanager" {
			return "/x/sdkmanager", nil
		}
		return "", exec.ErrNotFound
	})
	if _, err := ListCatalog(false); err == nil || !strings.Contains(err.Error(), "avdmanager not found") {
		t.Errorf("ListCatalog without avdmanager: %v", err)
	}
}

func TestListCatalog(t *testing.T) {
	sdkTools(t)
	s := scriptExec(t, func(name string, args []string) (string, bool) {
		switch {
		case name == "sdkmanager" && args[0] == "--list_installed":
			return listInstalled, true
		case name == "sdkmanager" && args[0] == "--list":
			return listAll, true
		case name == "avdmanager":
			return "pixel_8\npixel_9\n", true
		}
		return "", false
	})
	cat, err := ListCatalog(false)
	if err != nil {
		t.Fatalf("ListCatalog: %v", err)
	}
	if len(cat.Images) != 4 || strings.Join(cat.Profiles, ",") != "pixel_8,pixel_9" {
		t.Errorf("catalog = %+v", cat)
	}
	if s.calls[0] != "sdkmanager --list_installed" || s.calls[1] != "avdmanager list device -c" {
		t.Errorf("calls = %v", s.calls)
	}
	cat, err = ListCatalog(true)
	if err != nil || len(cat.Images) != 3 || s.calls[2] != "sdkmanager --list" {
		t.Errorf("available: %+v, %v, %v", cat, err, s.calls)
	}
}

func TestListCatalog_ToolFails(t *testing.T) {
	sdkTools(t)
	scriptExec(t, func(name string, args []string) (string, bool) { return "", false })
	if _, err := ListCatalog(false); err == nil || !strings.Contains(err.Error(), "sdkmanager --list_installed") {
		t.Errorf("sdkmanager failure: %v", err)
	}
	scriptExec(t, func(name string, args []string) (string, bool) { return "", name == "sdkmanager" })
	if _, err := ListCatalog(false); err == nil || !strings.Contains(err.Error(), "avdmanager list device") {
		t.Errorf("avdmanager failure: %v", err)
	}
}

func TestHostABI(t *testing.T) {
	for arch, want := range map[string]string{"arm64": "arm64-v8a", "amd64": "x86_64", "386": "x86", "arm": "armeabi-v7a", "riscv64": "riscv64"} {
		if got := HostABI(arch); got != want {
			t.Errorf("HostABI(%s) = %s, want %s", arch, got, want)
		}
	}
}

func TestCatalog_DefaultImageForHostABI(t *testing.T) {
	cat := &Catalog{Images: parseSDKImages(listInstalled + "  system-images;android-Baklava;google_apis;arm64-v8a | 1 | preview | x\n")}
	img, ok := cat.DefaultImage("arm64")
	if !ok || img.ID != "system-images;android-35;google_apis;arm64-v8a" {
		t.Errorf("arm64 default = %+v, %v; want the newest numbered google_apis arm64-v8a image", img, ok)
	}
	img, ok = cat.DefaultImage("amd64")
	if !ok || img.ID != "system-images;android-36;google_apis;x86_64" {
		t.Errorf("amd64 default = %+v, %v", img, ok)
	}
	if _, ok := cat.DefaultImage("386"); ok {
		t.Error("no x86 image is installed")
	}
	available := &Catalog{Images: parseSDKImages(listAll)}
	if img, _ := available.DefaultImage("arm64"); img.API != "android-35" {
		t.Errorf("an image that is only available is never the default: %+v", img)
	}
}

func TestCatalog_ImageAndProfile(t *testing.T) {
	cat := &Catalog{Images: parseSDKImages(listAll), Profiles: []string{"pixel_8"}}
	if img, ok := cat.Image("system-images;android-36;google_apis;arm64-v8a"); !ok || img.Installed {
		t.Errorf("Image = %+v, %v", img, ok)
	}
	if _, ok := cat.Image("system-images;android-99;x;y"); ok {
		t.Error("unknown image found")
	}
	if !cat.HasProfile("pixel_8") || cat.HasProfile("pixel_99") {
		t.Error("HasProfile")
	}
}

func TestAPILevel(t *testing.T) {
	if apiLevel("android-36.1") <= apiLevel("android-36") || apiLevel("android-Baklava") >= apiLevel("android-21") {
		t.Error("API levels out of order")
	}
}
