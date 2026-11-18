package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const diff = `diff --git a/pkg/a/a.go b/pkg/a/a.go
--- a/pkg/a/a.go
+++ b/pkg/a/a.go
@@ -3,0 +4,3 @@ func x() {
+	one()
+	two()
+	three()
@@ -20 +23 @@ func y() {
-	old()
+	new()
@@ -30,2 +33,0 @@ func z() {
diff --git a/pkg/a/a_test.go b/pkg/a/a_test.go
+++ b/pkg/a/a_test.go
@@ -1,0 +1,2 @@
+package a
+
diff --git a/pkg/daemon/commands_gen.go b/pkg/daemon/commands_gen.go
+++ b/pkg/daemon/commands_gen.go
@@ -1,0 +1,1 @@
+x
diff --git a/gone.go b/gone.go
+++ /dev/null
@@ -1,0 +1,1 @@
`

func TestParseDiff(t *testing.T) {
	out := map[string]map[int]bool{}
	if err := parseDiff(strings.NewReader(diff), out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("files = %v, want only pkg/a/a.go", out)
	}
	got := out["pkg/a/a.go"]
	for _, l := range []int{4, 5, 6, 23} {
		if !got[l] {
			t.Errorf("line %d not marked", l)
		}
	}
	if len(got) != 4 {
		t.Errorf("lines = %v; a pure deletion adds none", got)
	}
}

const profile = `mode: atomic
example.com/m/pkg/a/a.go:3.10,5.2 2 1
example.com/m/pkg/a/a.go:6.1,6.20 1 0
example.com/m/pkg/a/a.go:22.1,24.2 3 0
example.com/m/pkg/a/a.go:40.1,41.2 5 0
example.com/m/pkg/daemon/commands_gen.go:1.1,2.2 9 0
`

const e2eProfile = `mode: set
example.com/m/pkg/a/a.go:22.1,24.2 3 1
`

func TestReport(t *testing.T) {
	blocks := map[blockKey]*block{}
	for _, p := range []string{profile, e2eProfile} {
		if err := readProfile(strings.NewReader(p), "example.com/m", blocks); err != nil {
			t.Fatal(err)
		}
	}
	changed := map[string]map[int]bool{"pkg/a/a.go": {4: true, 6: true, 23: true}, "pkg/daemon/commands_gen.go": {1: true}}
	var out bytes.Buffer
	// Blocks 3–5 (2, covered), 6 (1, not), 22–24 (3, covered by e2e); 40–41 is unchanged.
	if code := report(blocks, changed, 80, &out); code != 0 || !strings.Contains(out.String(), "5/6") || !strings.Contains(out.String(), "83.3%") {
		t.Errorf("code %d:\n%s", code, out.String())
	}
	out.Reset()
	if code := report(blocks, changed, 90, &out); code != 1 || !strings.Contains(out.String(), "FAIL") {
		t.Errorf("below the bar: code %d:\n%s", code, out.String())
	}
	out.Reset()
	if code := report(blocks, map[string]map[int]bool{}, 90, &out); code != 0 || !strings.Contains(out.String(), "no measured statements") {
		t.Errorf("nothing changed: code %d:\n%s", code, out.String())
	}
}

func TestReadProfile_Bad(t *testing.T) {
	for _, p := range []string{"no-colon-here", "x.go:1.1,2 z"} {
		if err := readProfile(strings.NewReader(p), "m", map[blockKey]*block{}); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestModulePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(p, []byte("// c\nmodule example.com/m\n\ngo 1.23\n"), 0o644)
	if m, err := modulePath(p); err != nil || m != "example.com/m" {
		t.Errorf("module = %q, %v", m, err)
	}
	_ = os.WriteFile(p, []byte("go 1.23\n"), 0o644)
	if _, err := modulePath(p); err == nil {
		t.Error("no module line accepted")
	}
	if _, err := modulePath(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing go.mod accepted")
	}
}

func TestCounted(t *testing.T) {
	for path, want := range map[string]bool{
		"pkg/a/a.go": true, "pkg/a/a_test.go": false, "pkg/daemon/commands_gen.go": false,
		"pkg/hub/hubtest/proxy.go": false, "pkg/hub/link.go": true, "README.md": false,
	} {
		if counted(path) != want {
			t.Errorf("counted(%s) = %v", path, !want)
		}
	}
}
