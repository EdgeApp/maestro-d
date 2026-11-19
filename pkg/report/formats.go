package report

// Formats selects the report files a run writes beside report.json and its
// flows/ and assets/, which every run writes.
type Formats uint8

const (
	// FormatJSON is report.json, always written; named for clarity at
	// call sites.
	FormatJSON Formats = 1 << iota
	// FormatHTML is report.html.
	FormatHTML
	// FormatJUnit is junit-report.xml.
	FormatJUnit
	// FormatAllure is allure-results/.
	FormatAllure

	// AllFormats is what `maestro-runner test` writes.
	AllFormats = FormatJSON | FormatHTML | FormatJUnit | FormatAllure
	// DaemonFormats is what a daemon session writes: the JSON its clients
	// read and the JUnit a remote caller may fetch through the artifacts
	// route. No one opens a daemon session's HTML or Allure output.
	DaemonFormats = FormatJSON | FormatJUnit
)

// Has reports whether f includes all of g.
func (f Formats) Has(g Formats) bool { return f&g == g }
