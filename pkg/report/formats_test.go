package report

import (
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"os"
	"path/filepath"
	"testing"
)

func skeleton(t *testing.T, dir string) (*Index, []FlowDetail) {
	t.Helper()
	flows := []flow.Flow{{
		SourcePath: "f.yaml",
		Config:     flow.Config{Name: "F"},
		Steps:      []flow.Step{&flow.TapOnStep{BaseStep: flow.BaseStep{StepType: flow.StepTapOn}, Selector: flow.Selector{ID: "b"}}},
	}}
	index, details, err := BuildSkeleton(flows, BuilderConfig{OutputDir: dir, Device: Device{ID: "d", Platform: "android"}, App: App{ID: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	return index, details
}

func TestDaemonFormatsSkipHTMLAndAllure(t *testing.T) {
	dir := t.TempDir()
	index, details := skeleton(t, dir)
	if err := WriteSkeletonFormats(dir, index, details, DaemonFormats); err != nil {
		t.Fatal(err)
	}
	w := NewIndexWriterFormats(dir, index, DaemonFormats)
	w.Start()
	w.Close()

	for _, name := range []string{"report.json", "junit-report.xml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("daemon formats lack %s: %v", name, err)
		}
	}
	for _, name := range []string{"report.html", "allure-results"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("daemon formats wrote %s", name)
		}
	}

	all := t.TempDir()
	index, details = skeleton(t, all)
	if err := WriteSkeleton(all, index, details); err != nil {
		t.Fatal(err)
	}
	wa := NewIndexWriter(all, index)
	wa.Start()
	wa.Close()
	for _, name := range []string{"report.json", "report.html", "junit-report.xml", "allure-results"} {
		if _, err := os.Stat(filepath.Join(all, name)); err != nil {
			t.Errorf("all formats lack %s: %v", name, err)
		}
	}
	if !AllFormats.Has(FormatHTML) || DaemonFormats.Has(FormatHTML) || !DaemonFormats.Has(FormatJUnit) {
		t.Error("format sets changed")
	}
}
