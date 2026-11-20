//go:build darwin

package quicktime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const findIdentityOutput = `  1) 1111111111111111111111111111111111111111 "Apple Distribution: Example Org (FGHIJ67890)"
  2) abcdefabcdefabcdefabcdefabcdefabcdefabcd "Apple Development: Example (ABCDE12345)"
  3) 2222222222222222222222222222222222222222 "Apple Development: Other (XXXXXXXXXX)"
     3 valid identities found
`

// fakeTools replaces runCommand with fakes of xcrun, security and
// codesign, recording every call.
type fakeTools struct {
	calls      [][]string
	identities string
	swiftc     string
	failSwiftc bool
	failSign   bool
	signedAt   []string
}

func (f *fakeTools) install(t *testing.T) {
	old := runCommand
	t.Cleanup(func() { runCommand = old })
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		switch {
		case name == "security":
			return []byte(f.identities), nil
		case name == "xcrun" && len(args) == 2 && args[1] == "--version":
			return []byte(f.swiftc), nil
		case name == "xcrun" && args[0] == "swiftc":
			if f.failSwiftc {
				return []byte("error: boom"), errors.New("exit status 1")
			}
			for i, a := range args {
				if a == "-o" {
					return nil, os.WriteFile(args[i+1], []byte("binary"), 0o755)
				}
			}
			return nil, errors.New("no -o")
		case name == "codesign":
			if f.failSign {
				return []byte("errSecInternalComponent"), errors.New("exit status 1")
			}
			f.signedAt = append(f.signedAt, args[len(args)-1])
			return nil, nil
		}
		return nil, errors.New("unexpected " + name)
	}
}

func (f *fakeTools) count(name, arg string) int {
	n := 0
	for _, c := range f.calls {
		if c[0] == name && strings.Contains(strings.Join(c, " "), arg) {
			n++
		}
	}
	return n
}

func helperSources(t *testing.T) string {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, helperSource), []byte("print(1)"), 0o644)
	os.WriteFile(filepath.Join(dir, helperPlist), []byte("<plist/>"), 0o644)
	return dir
}

// U36: the signing identity: the environment, else the first Apple
// Development identity, else ad-hoc.
func TestSigningIdentity(t *testing.T) {
	f := &fakeTools{identities: findIdentityOutput}
	f.install(t)
	t.Setenv(signIdentityEnv, "")
	if id, label := signingIdentity(context.Background()); id != "ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD" || label != "Apple Development: Example (ABCDE12345)" {
		t.Fatalf("%s %s", id, label)
	}
	t.Setenv(signIdentityEnv, "Developer ID Application: X")
	if id, _ := signingIdentity(context.Background()); id != "Developer ID Application: X" {
		t.Fatal(id)
	}
	t.Setenv(signIdentityEnv, "")
	f.identities = "     0 valid identities found\n"
	if id, label := signingIdentity(context.Background()); id != "-" || !strings.Contains(label, "ad-hoc") {
		t.Fatal(id, label)
	}
}

// U36: the cache key covers the source, the plist, the compiler and the
// identity.
func TestCacheKey(t *testing.T) {
	base := cacheKey([]byte("s"), []byte("p"), []byte("v"), "i")
	for _, k := range []string{
		cacheKey([]byte("s2"), []byte("p"), []byte("v"), "i"),
		cacheKey([]byte("s"), []byte("p2"), []byte("v"), "i"),
		cacheKey([]byte("s"), []byte("p"), []byte("v2"), "i"),
		cacheKey([]byte("s"), []byte("p"), []byte("v"), "-"),
		cacheKey([]byte("sp"), []byte(""), []byte("v"), "i"),
	} {
		if k == base {
			t.Fatal("key ignores an input")
		}
	}
	if len(base) != 64 {
		t.Fatal(base)
	}
}

// U36: the helper builds into a temporary directory, is signed there,
// renamed into <cache>/<key[:12]>, then reused; the cache keeps the
// newest two builds.
func TestBuildHelper(t *testing.T) {
	f := &fakeTools{identities: findIdentityOutput, swiftc: "Swift version 6.2"}
	f.install(t)
	t.Setenv(signIdentityEnv, "")
	src := helperSources(t)
	cache := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for i, name := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc"} {
		p := filepath.Join(cache, name)
		os.MkdirAll(p, 0o755)
		at := old.Add(time.Duration(i) * time.Minute)
		os.Chtimes(p, at, at)
	}
	os.MkdirAll(filepath.Join(cache, ".build-other"), 0o755)

	app, err := buildHelper(context.Background(), src, cache)
	if err != nil {
		t.Fatal(err)
	}
	key := cacheKey([]byte("print(1)"), []byte("<plist/>"), []byte("Swift version 6.2"), "ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD")
	if want := filepath.Join(cache, key[:12], "MaestroDCapture.app"); app != want {
		t.Fatalf("app %s, want %s", app, want)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "Contents", "Info.plist")); string(b) != "<plist/>" {
		t.Fatal("Info.plist not in the bundle")
	}
	if len(f.signedAt) != 1 || !strings.Contains(f.signedAt[0], string(filepath.Separator)+".build-") {
		t.Fatalf("signed %v, want the temporary build", f.signedAt)
	}
	if f.count("codesign", "--sign ABCDEF") != 1 || f.count("codesign", "--timestamp=none") != 1 {
		t.Fatalf("codesign calls %v", f.calls)
	}
	if f.count("xcrun", "-framework AVFoundation -framework CoreMediaIO -framework CoreMedia") != 1 {
		t.Fatalf("swiftc calls %v", f.calls)
	}
	entries, _ := os.ReadDir(cache)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, " "); got != ".build-other "+key[:12]+" cccccccccccc" && got != ".build-other cccccccccccc "+key[:12] {
		t.Fatalf("cache keeps %q", got)
	}

	again, err := buildHelper(context.Background(), src, cache)
	if err != nil || again != app || f.count("xcrun", "-O") != 1 {
		t.Fatalf("second build: %v %s (swiftc runs %d)", err, again, f.count("xcrun", "-O"))
	}
}

func TestBuildHelperFailures(t *testing.T) {
	t.Setenv(signIdentityEnv, "")
	src := helperSources(t)
	f := &fakeTools{failSwiftc: true}
	f.install(t)
	if _, err := buildHelper(context.Background(), src, t.TempDir()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("swiftc: %v", err)
	}
	f.failSwiftc, f.failSign = false, true
	cache := t.TempDir()
	if _, err := buildHelper(context.Background(), src, cache); err == nil || !strings.Contains(err.Error(), "errSecInternalComponent") {
		t.Fatalf("codesign: %v", err)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Fatalf("a failed build leaves %v", entries)
	}
	if _, err := buildHelper(context.Background(), t.TempDir(), cache); err == nil {
		t.Fatal("missing source")
	}
	os.Remove(filepath.Join(src, helperPlist))
	if _, err := buildHelper(context.Background(), src, cache); err == nil {
		t.Fatal("missing plist")
	}
	runCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no xcrun") }
	if _, err := buildHelper(context.Background(), helperSources(t), cache); err == nil || !strings.Contains(err.Error(), "no xcrun") {
		t.Fatalf("no compiler: %v", err)
	}
}

// ensureHelperApp builds once per process.
func TestEnsureHelperApp(t *testing.T) {
	f := &fakeTools{swiftc: "v"}
	f.install(t)
	t.Setenv(signIdentityEnv, "-")
	src, cache := helperSources(t), t.TempDir()
	oldSrc, oldCache := helperSourceDir, helperCacheDir
	helperSourceDir = func() string { return src }
	helperCacheDir = func() string { return cache }
	t.Cleanup(func() { helperSourceDir, helperCacheDir, builtApp = oldSrc, oldCache, "" })
	builtApp = ""
	a, err := ensureHelperApp()
	if err != nil {
		t.Fatal(err)
	}
	b, err := ensureHelperApp()
	if err != nil || a != b || f.count("xcrun", "--version") != 1 {
		t.Fatalf("%v %s %s %d", err, a, b, f.count("xcrun", "--version"))
	}
	builtApp = ""
	helperSourceDir = func() string { return t.TempDir() }
	if _, err := ensureHelperApp(); err == nil {
		t.Fatal("missing source")
	}
}
