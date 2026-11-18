package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReleaseReportDirDaemonOwned(t *testing.T) {
	s := &Server{}
	kept := filepath.Join(t.TempDir(), "mock-1", "20261118-120000")
	mkdirs(t, filepath.Join(kept, "assets"))
	s.addReportUser(kept)
	blob := filepath.Join(kept, "blobs", "u1")
	mkdirs(t, blob, filepath.Join(kept, "bundles", "abc"))
	s.releaseReportDir(kept, blob, false)
	if !present(filepath.Join(kept, "assets")) {
		t.Error("a kept report lost its assets")
	}
	if present(filepath.Join(kept, "blobs")) || present(filepath.Join(kept, "bundles")) {
		t.Error("blobs/ or bundles/ survived the last detach")
	}

	discarded := filepath.Join(t.TempDir(), "mock-2", "20261118-120001")
	mkdirs(t, filepath.Join(discarded, "assets"))
	s.addReportUser(discarded)
	blob2 := filepath.Join(discarded, "blobs", "u2")
	mkdirs(t, blob2)
	s.releaseReportDir(discarded, blob2, true)
	if present(discarded) {
		t.Error("discardReports left the report directory")
	}
}

// Two sessions share a caller's outputDir; the second is still starting
// (counted, inputs materialized) when the first detaches.
func TestReleaseReportDirSharedOutputDir(t *testing.T) {
	s := &Server{}
	out := t.TempDir()
	mkdirs(t, filepath.Join(out, "assets"))
	s.addReportUser(out) // first session
	b1 := filepath.Join(out, "blobs", "u1")
	mkdirs(t, b1, filepath.Join(out, "bundles", "abc"))
	s.addReportUser(out) // second session, still starting its driver
	b2 := filepath.Join(out, "blobs", "u2")
	mkdirs(t, b2)

	s.releaseReportDir(out, b1, false)
	if present(b1) {
		t.Error("the first session's blobs survived its detach")
	}
	if !present(b2) || !present(filepath.Join(out, "bundles", "abc")) {
		t.Fatal("the first detach removed what the starting session uses")
	}
	s.releaseReportDir(out, b2, false)
	if present(filepath.Join(out, "blobs")) || present(filepath.Join(out, "bundles")) {
		t.Error("the last detach left the blobs/ or bundles/ the daemon made")
	}
	if !present(filepath.Join(out, "assets")) {
		t.Error("a caller's outputDir lost its files")
	}
	if len(s.reportUsers) != 0 {
		t.Errorf("reportUsers = %v, want empty", s.reportUsers)
	}
}

// A caller's outputDir that already had folders named blobs/ and bundles/
// keeps them.
func TestReleaseReportDirKeepsCallersOwnInputDirs(t *testing.T) {
	s := &Server{}
	out := t.TempDir()
	mine := filepath.Join(out, "blobs", "callers-file")
	mkdirs(t, mine, filepath.Join(out, "bundles", "callers"))
	s.addReportUser(out)
	blob := filepath.Join(out, "blobs", "u1")
	mkdirs(t, blob)
	s.releaseReportDir(out, blob, false)
	if present(blob) {
		t.Error("the session's own blobs survived")
	}
	if !present(mine) || !present(filepath.Join(out, "bundles", "callers")) {
		t.Error("the caller's own blobs/ or bundles/ was removed")
	}
	s.releaseReportDir(out, filepath.Join(out, "blobs", "none"), false) // unbalanced release is harmless
}

func TestAttachConfigDiscardReportsWire(t *testing.T) {
	var cfg AttachConfig
	if err := json.Unmarshal([]byte(`{"discardReports":true,"outputDir":"/o"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.DiscardReports || cfg.OutputDir != "/o" {
		t.Errorf("decoded %+v", cfg)
	}
	cfg.ReportDir = "/secret/report"
	data, _ := json.Marshal(cfg)
	if strings.Contains(string(data), "/secret/report") || strings.Contains(string(data), "reportDir") {
		t.Errorf("ReportDir leaked into the wire format: %s", data)
	}
}

func TestCanonicalDirJoinsSpellings(t *testing.T) {
	out := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(out, link); err != nil {
		t.Fatal(err)
	}
	want := canonicalDir(out)
	for _, spelling := range []string{out + "/", out + "/.", link, link + "/"} {
		if got := canonicalDir(spelling); got != want {
			t.Errorf("canonicalDir(%q) = %q, want %q", spelling, got, want)
		}
	}
	s := &Server{}
	s.addReportUser(canonicalDir(out))
	s.addReportUser(canonicalDir(out + "/"))
	if len(s.reportUsers) != 1 || s.reportUsers[want].n != 2 {
		t.Errorf("two spellings counted apart: %v", s.reportUsers)
	}
}

func present(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
