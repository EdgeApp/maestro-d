package emulator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDataSize(t *testing.T) {
	for _, tc := range []struct {
		in    string
		bytes int64
		norm  string
	}{
		{DefaultDataSize, 6 << 30, "6G"},
		{"8G", 8 << 30, "8G"},
		{"8gb", 8 << 30, "8G"},
		{"2048M", 2048 << 20, "2048M"},
		{"10G", 10 << 30, "10G"},
		{"1048576K", 1 << 30, "1048576K"},
		{"4294967296", 4 << 30, "4294967296"},
	} {
		b, norm, err := ParseDataSize(tc.in)
		if err != nil || b != tc.bytes || norm != tc.norm {
			t.Errorf("ParseDataSize(%q) = %d, %q, %v; want %d, %q", tc.in, b, norm, err, tc.bytes, tc.norm)
		}
	}
	for _, in := range []string{"12G", "10241M", "10737418241", "11g"} {
		if _, _, err := ParseDataSize(in); err == nil || !strings.Contains(err.Error(), "above the 10G limit") {
			t.Errorf("ParseDataSize(%q) = %v, want the 10G limit", in, err)
		}
	}
	for _, in := range []string{"", "0G", "big", "6T", "-1G", "99999999999999999999"} {
		if _, _, err := ParseDataSize(in); err == nil {
			t.Errorf("ParseDataSize(%q) accepted", in)
		}
	}
}

func TestValidAVDName(t *testing.T) {
	if !ValidAVDName("202611080630-maestroDHub") || !ValidAVDName("Pixel_8.API-35") {
		t.Error("valid names refused")
	}
	if ValidAVDName("has space") || ValidAVDName("") || ValidAVDName("a/b") {
		t.Error("invalid names accepted")
	}
}

func TestAVDHome(t *testing.T) {
	t.Setenv("ANDROID_AVD_HOME", "/avd-home")
	t.Setenv("ANDROID_USER_HOME", "/user-home")
	if got := avdHome(); got != "/avd-home" {
		t.Errorf("ANDROID_AVD_HOME: %s", got)
	}
	t.Setenv("ANDROID_AVD_HOME", "")
	if got := avdHome(); got != filepath.Join("/user-home", "avd") {
		t.Errorf("ANDROID_USER_HOME: %s", got)
	}
	t.Setenv("ANDROID_USER_HOME", "")
	t.Setenv("HOME", "/h")
	if got := avdHome(); got != filepath.Join("/h", ".android", "avd") {
		t.Errorf("default: %s", got)
	}
}

func TestAVDConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANDROID_AVD_HOME", home)
	if got := AVDConfigPath("a"); got != filepath.Join(home, "a.avd", "config.ini") {
		t.Errorf("without an ini file: %s", got)
	}
	elsewhere := filepath.Join(t.TempDir(), "moved.avd")
	if err := os.WriteFile(filepath.Join(home, "b.ini"), []byte("avd.ini.encoding=UTF-8\npath="+elsewhere+"\ntarget=android-35\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := AVDConfigPath("b"); got != filepath.Join(elsewhere, "config.ini") {
		t.Errorf("from the ini's path=: %s", got)
	}
}

func TestSetConfigValue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(p, []byte("hw.ramSize=2048\ndisk.dataPartition.size=6442450944\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setConfigValue(p, dataSizeKey, "8G"); err != nil {
		t.Fatal(err)
	}
	if err := setConfigValue(p, "hw.keyboard", "yes"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "hw.ramSize=2048\ndisk.dataPartition.size=8G\nhw.keyboard=yes\n" {
		t.Errorf("config.ini = %q", data)
	}
	if err := setConfigValue(filepath.Join(t.TempDir(), "missing.ini"), "k", "v"); err == nil {
		t.Error("a missing file must fail")
	}
}

// fakeAVD creates <home>/<name>.avd/config.ini as avdmanager would.
func fakeAVD(t *testing.T, name string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ANDROID_AVD_HOME", home)
	dir := filepath.Join(home, name+".avd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.ini")
	if err := os.WriteFile(cfg, []byte("AvdId="+name+"\ndisk.dataPartition.size=6442450944\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCreateAVD_WritesDataSize(t *testing.T) {
	sdkTools(t)
	cfg := fakeAVD(t, "pix")
	s := scriptExec(t, func(string, []string) (string, bool) { return "", true })
	err := CreateAVD(AVDSpec{Name: "pix", Image: "system-images;android-35;google_apis;arm64-v8a", Profile: "pixel_8", DataSize: "8G"})
	if err != nil {
		t.Fatalf("CreateAVD: %v", err)
	}
	if s.calls[0] != "avdmanager create avd -n pix -k system-images;android-35;google_apis;arm64-v8a -d pixel_8" {
		t.Errorf("calls = %v", s.calls)
	}
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), "disk.dataPartition.size=8G\n") || strings.Contains(string(data), "6442450944") {
		t.Errorf("config.ini = %q", data)
	}
}

func TestCreateAVD_Failures(t *testing.T) {
	sdkTools(t)
	if err := CreateAVD(AVDSpec{Name: "bad name"}); err == nil || !strings.Contains(err.Error(), "invalid AVD name") {
		t.Errorf("bad name: %v", err)
	}
	scriptExec(t, func(string, []string) (string, bool) { return "line1\nError: Package path is not valid.", false })
	if err := CreateAVD(AVDSpec{Name: "pix", Image: "x", Profile: "pixel_8"}); err == nil || !strings.Contains(err.Error(), "Package path is not valid") {
		t.Errorf("avdmanager failure should carry its output: %v", err)
	}
	// avdmanager succeeded but no config.ini appeared.
	t.Setenv("ANDROID_AVD_HOME", t.TempDir())
	scriptExec(t, func(string, []string) (string, bool) { return "", true })
	if err := CreateAVD(AVDSpec{Name: "pix", Image: "x", Profile: "pixel_8", DataSize: "8G"}); err == nil || !strings.Contains(err.Error(), "was created, but setting") {
		t.Errorf("missing config.ini: %v", err)
	}
	withFakeLookPath(t, func(string) (string, error) { return "", os.ErrNotExist })
	if err := CreateAVD(AVDSpec{Name: "pix"}); err == nil || !strings.Contains(err.Error(), "avdmanager not found") {
		t.Errorf("no avdmanager: %v", err)
	}
}

func TestDeleteAVD(t *testing.T) {
	sdkTools(t)
	s := scriptExec(t, func(string, []string) (string, bool) { return "", true })
	if err := DeleteAVD("pix"); err != nil || s.calls[0] != "avdmanager delete avd -n pix" {
		t.Errorf("DeleteAVD: %v %v", err, s.calls)
	}
	scriptExec(t, func(string, []string) (string, bool) {
		return "Error: There is no Android Virtual Device named 'pix'.", false
	})
	if err := DeleteAVD("pix"); err == nil || !strings.Contains(err.Error(), "no Android Virtual Device") {
		t.Errorf("failure: %v", err)
	}
	withFakeLookPath(t, func(string) (string, error) { return "", os.ErrNotExist })
	if err := DeleteAVD("pix"); err == nil || !strings.Contains(err.Error(), "avdmanager not found") {
		t.Errorf("no avdmanager: %v", err)
	}
}

func TestInstallSystemImage(t *testing.T) {
	sdkTools(t)
	s := scriptExec(t, func(string, []string) (string, bool) { return "", true })
	if err := InstallSystemImage("system-images;android-36;google_apis;arm64-v8a"); err != nil ||
		s.calls[0] != "sdkmanager --install system-images;android-36;google_apis;arm64-v8a" {
		t.Errorf("install: %v %v", err, s.calls)
	}
	scriptExec(t, func(string, []string) (string, bool) { return "Warning: Failed to find package", false })
	if err := InstallSystemImage("x"); err == nil || !strings.Contains(err.Error(), "Failed to find package") {
		t.Errorf("failure: %v", err)
	}
	withFakeLookPath(t, func(string) (string, error) { return "", os.ErrNotExist })
	if err := InstallSystemImage("x"); err == nil || !strings.Contains(err.Error(), "sdkmanager not found") {
		t.Errorf("no sdkmanager: %v", err)
	}
}

func TestLastLines(t *testing.T) {
	if got := lastLines([]byte("a\nb\nc\nd\n"), 2); got != "c\nd" {
		t.Errorf("lastLines = %q", got)
	}
}
