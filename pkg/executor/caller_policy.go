package executor

// maestro-d addition. This file is not part of upstream maestro-runner.
//
// A CallerPolicy confines what one daemon call may touch on the host. The
// daemon decides it per request from who sent the request: a remote caller
// (a TCP client of a hub, or a non-loopback client of `serve --http`) gets an
// active policy; a unix-socket caller gets an inactive one that only maps
// blob refs. Session.Execute, ExecuteAll and RunFlow install it on the
// session's ScriptEngine for the call and clear it afterwards, so every step
// the call runs — nested runFlow/retry/repeat commands and hooks included —
// is checked.
//
// It is enforced at three upstream seams, each a marked `// maestro-d` line:
//   - the end of ScriptEngine.ExpandStep (scripting.go), which walks the
//     step's input, flow-file and output fields (policyStep);
//   - the start of ScriptEngine.ResolvePath (scripting.go), where the
//     executor turns a path into a file it reads (policyResolve);
//   - the start of executeRunShell (runshell.go) (policyDeniesShell).
//
// Both path hooks apply the same idempotent rule, because upstream calls
// ResolvePath on fields ExpandStep has already handled. See
// docs/daemon/UPSTREAM.md.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// BlobResolver maps `blob:` and `bundle:` refs to files on the daemon's
// host. Dir is the session's blob-copy directory, one of the policy's roots.
type BlobResolver interface {
	Dir() string
	Resolve(ref string) (string, error)
}

// CallerPolicy is the per-call confinement the daemon puts in a request's
// context (WithCallerPolicy).
type CallerPolicy struct {
	// Remote marks a caller that must not reach the host: its host paths
	// are refused and its runShell steps fail unless AllowShell.
	Remote bool
	// AllowShell lets a remote caller run runShell (--allow-remote-shell).
	AllowShell bool
	// BundleDir is the materialized flow bundle the call runs from, if any.
	BundleDir string
	// OutputsDir receives the files a remote call's output fields write
	// (takeScreenshot.path, saveAuthState.path).
	OutputsDir string
	// Blobs maps blob refs; nil when the daemon has no blob store.
	Blobs BlobResolver

	// Per-call state, set by the Session that installs the policy.
	roots     []string
	deniedDir string
	cancel    context.CancelFunc
	denial    *PolicyDenial
}

// PolicyDenial records why a call was stopped.
type PolicyDenial struct {
	// Step is the description of the step that was denied, when known.
	Step string `json:"step,omitempty"`
	// Command and Field name the YAML command and field, when known.
	Command string `json:"command,omitempty"`
	Field   string `json:"field,omitempty"`
	// Path is the value that was refused (empty for runShell).
	Path string `json:"path,omitempty"`
	// Reason is the human explanation.
	Reason string `json:"reason"`
}

type callerPolicyKey struct{}

// WithCallerPolicy returns ctx carrying p for Session calls made under it.
func WithCallerPolicy(ctx context.Context, p *CallerPolicy) context.Context {
	return context.WithValue(ctx, callerPolicyKey{}, p)
}

// CallerPolicyFrom returns the policy in ctx, or nil.
func CallerPolicyFrom(ctx context.Context) *CallerPolicy {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(callerPolicyKey{}).(*CallerPolicy)
	return p
}

// fieldKind classifies a path-bearing step field.
type fieldKind int

const (
	fieldInput  fieldKind = iota // a file the executor or driver reads
	fieldFlow                    // a flow file (runFlow, retry)
	fieldOutput                  // a file the executor or driver writes
)

// policyFieldTable is every path-bearing field the typed switch in
// policyStep handles, by YAML command and field name. PolicyFields exposes
// it so pkg/daemon can check it against the generated command registry.
var policyFieldTable = map[string]map[string]fieldKind{
	"addMedia":         {"files": fieldInput},
	"runScript":        {"script": fieldInput, "file": fieldInput},
	"assertScreenshot": {"path": fieldInput},
	"runBrowserScript": {"file": fieldInput},
	"runWebViewScript": {"file": fieldInput},
	"uploadFile":       {"path": fieldInput, "paths": fieldInput},
	"loadAuthState":    {"path": fieldInput},
	"runFlow":          {"file": fieldFlow, "else": fieldFlow},
	"retry":            {"file": fieldFlow},
	"takeScreenshot":   {"path": fieldOutput},
	"saveAuthState":    {"path": fieldOutput},
}

// PolicyFields returns the path-bearing fields the policy walks, as YAML
// command name → sorted field names.
func PolicyFields() map[string][]string {
	out := make(map[string][]string, len(policyFieldTable))
	for cmd, fields := range policyFieldTable {
		names := make([]string, 0, len(fields))
		for f := range fields {
			names = append(names, f)
		}
		sort.Strings(names)
		out[cmd] = names
	}
	return out
}

// beginCall prepares a copy of p for one Session call: its roots, its empty
// denied/ directory under sessionDir, and the cancel func of the call's
// context. It returns nil for a nil policy.
func (p *CallerPolicy) beginCall(sessionDir string, cancel context.CancelFunc) *CallerPolicy {
	if p == nil {
		return nil
	}
	c := &CallerPolicy{
		Remote:     p.Remote,
		AllowShell: p.AllowShell,
		BundleDir:  p.BundleDir,
		OutputsDir: p.OutputsDir,
		Blobs:      p.Blobs,
		cancel:     cancel,
	}
	for _, r := range []string{p.BundleDir, p.OutputsDir} {
		c.AddRoot(r)
	}
	if p.Blobs != nil {
		c.AddRoot(p.Blobs.Dir())
	}
	if c.Remote {
		c.deniedDir = filepath.Join(sessionDir, "denied")
		// An empty, read-only directory: a denied path points into it, so
		// nothing on the host is read, and nothing can be written there.
		if err := os.MkdirAll(c.deniedDir, 0o755); err == nil {
			_ = os.Chmod(c.deniedDir, 0o555)
		}
	}
	return c
}

// AddRoot adds a directory under which a remote call's paths are allowed
// (a bundle materialized during the call).
func (p *CallerPolicy) AddRoot(dir string) {
	if dir == "" {
		return
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	p.roots = append(p.roots, filepath.Clean(dir))
}

// Denial returns the reason the call was stopped, or nil.
func (p *CallerPolicy) Denial() *PolicyDenial {
	if p == nil {
		return nil
	}
	return p.denial
}

// deny records the first denial, cancels the call and returns a path in
// the empty denied/ directory to stand in for the refused one.
func (p *CallerPolicy) deny(d PolicyDenial) string {
	if p.denial == nil {
		p.denial = &d
		if p.cancel != nil {
			p.cancel()
		}
	}
	base := filepath.Base(d.Path)
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = "denied"
	}
	return filepath.Join(p.deniedDir, base)
}

// isDenied reports whether path is one deny handed out.
func (p *CallerPolicy) isDenied(path string) bool {
	return p.denial != nil && p.deniedDir != "" && filepath.Dir(path) == p.deniedDir
}

// fill completes a denial recorded without its step and field.
func (d *PolicyDenial) fill(from PolicyDenial) {
	if d.Command == "" {
		d.Command, d.Field = from.Command, from.Field
	}
	if d.Step == "" {
		d.Step = from.Step
	}
}

// isRef reports whether s is a blob or bundle ref.
func isRef(s string) bool {
	return strings.HasPrefix(s, "blob:") || strings.HasPrefix(s, "bundle:")
}

// within reports whether path (absolute, cleaned) lies under one of the
// policy's roots.
func (p *CallerPolicy) within(path string) bool {
	for _, root := range p.roots {
		if path == root {
			return true
		}
		if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// absolute resolves path as upstream's ResolvePath would.
func absolute(path, flowDir string) string {
	if !filepath.IsAbs(path) && flowDir != "" {
		path = filepath.Join(flowDir, path)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

// mapRef resolves a blob or bundle ref, denying it when the daemon cannot.
func (p *CallerPolicy) mapRef(d PolicyDenial) string {
	if p.Blobs == nil {
		d.Reason = fmt.Sprintf("%s: this daemon has no blob store", d.Path)
		return p.deny(d)
	}
	local, err := p.Blobs.Resolve(d.Path)
	if err != nil {
		d.Reason = fmt.Sprintf("%s: %v", d.Path, err)
		return p.deny(d)
	}
	if dir := filepath.Dir(local); !p.within(filepath.Clean(dir)) {
		p.AddRoot(dir)
	}
	return local
}

// checkInput applies the rule to a path the executor or a driver reads.
// flowDir is the directory upstream resolves relative paths against.
func (p *CallerPolicy) checkInput(d PolicyDenial, flowDir string) string {
	if isRef(d.Path) {
		if !p.Remote && p.Blobs == nil {
			return d.Path
		}
		return p.mapRef(d)
	}
	if !p.Remote {
		return d.Path
	}
	if p.isDenied(d.Path) {
		// Already denied by the other hook (upstream's ExpandStep resolves
		// addMedia files through ResolvePath first): name the field now.
		p.denial.fill(d)
		return d.Path
	}
	if p.within(absolute(d.Path, flowDir)) {
		return d.Path
	}
	d.Reason = fmt.Sprintf("%s is a path on the daemon's host; a remote caller may only use files it uploaded", d.Path)
	return p.deny(d)
}

// checkOutput applies the rule to a path the executor or a driver writes:
// a remote caller's path must be relative with no "..", and is rewritten
// under OutputsDir. A path already there (the hook ran before) passes.
func (p *CallerPolicy) checkOutput(d PolicyDenial) string {
	if !p.Remote {
		return d.Path
	}
	if p.isDenied(d.Path) {
		p.denial.fill(d)
		return d.Path
	}
	if filepath.IsAbs(d.Path) {
		if p.OutputsDir != "" && underDir(filepath.Clean(d.Path), p.OutputsDir) {
			return d.Path
		}
		d.Reason = fmt.Sprintf("%s: a remote caller's output path must be relative", d.Path)
		return p.deny(d)
	}
	if isRef(d.Path) {
		d.Reason = fmt.Sprintf("%s: an output path cannot be a blob ref", d.Path)
		return p.deny(d)
	}
	for _, el := range strings.Split(filepath.ToSlash(d.Path), "/") {
		if el == ".." {
			d.Reason = fmt.Sprintf("%s: a remote caller's output path may not contain ..", d.Path)
			return p.deny(d)
		}
	}
	if p.OutputsDir == "" {
		d.Reason = fmt.Sprintf("%s: this call has no outputs directory", d.Path)
		return p.deny(d)
	}
	return filepath.Join(p.OutputsDir, filepath.Clean(d.Path))
}

func underDir(path, dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(abs), path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// shellDenied reports (and records) whether a runShell may not run.
func (p *CallerPolicy) shellDenied(step flow.Step) bool {
	if !p.Remote || p.AllowShell {
		return false
	}
	p.deny(PolicyDenial{Step: step.Describe(), Command: string(flow.StepRunShell),
		Reason: "runShell is not allowed for remote callers (start the hub with --allow-remote-shell)"})
	return true
}

// policyStep is hook 1, called at the end of ScriptEngine.ExpandStep. It
// walks the step's input, flow-file and output fields (the typed switch
// pkg/executor needs, since it cannot import pkg/daemon's CommandSpecs).
func (se *ScriptEngine) policyStep(step flow.Step) {
	p := se.policy
	if p == nil || step == nil {
		return
	}
	desc := step.Describe()
	den := func(cmd, field, path string) PolicyDenial {
		return PolicyDenial{Step: desc, Command: cmd, Field: field, Path: path}
	}
	in := func(cmd, field string, v *string) {
		if *v != "" {
			*v = p.checkInput(den(cmd, field, *v), se.flowDir)
		}
	}
	out := func(cmd, field string, v *string) {
		if *v != "" {
			*v = p.checkOutput(den(cmd, field, *v))
		}
	}
	switch s := step.(type) {
	case *flow.AddMediaStep:
		for i := range s.Files {
			in("addMedia", "files", &s.Files[i])
		}
	case *flow.RunScriptStep:
		if strings.HasSuffix(s.File, ".js") {
			in("runScript", "file", &s.File)
		}
		if strings.HasSuffix(s.Script, ".js") {
			in("runScript", "script", &s.Script)
		}
	case *flow.AssertScreenshotStep:
		in("assertScreenshot", "path", &s.Path)
	case *flow.RunBrowserScriptStep:
		in("runBrowserScript", "file", &s.File)
	case *flow.RunWebViewScriptStep:
		in("runWebViewScript", "file", &s.File)
	case *flow.UploadFileStep:
		in("uploadFile", "path", &s.Path)
		for i := range s.Paths {
			in("uploadFile", "paths", &s.Paths[i])
		}
	case *flow.LoadAuthStateStep:
		in("loadAuthState", "path", &s.Path)
	case *flow.RunFlowStep:
		in("runFlow", "file", &s.File)
		in("runFlow", "else", &s.ElseFile)
	case *flow.RetryStep:
		in("retry", "file", &s.File)
	case *flow.TakeScreenshotStep:
		out("takeScreenshot", "path", &s.Path)
	case *flow.SaveAuthStateStep:
		out("saveAuthState", "path", &s.Path)
	case *flow.RunShellStep:
		p.shellDenied(s)
	}
}

// policyResolve is hook 2, called at the start of ScriptEngine.ResolvePath.
// It returns the path to use and true when the policy decided it; false
// leaves the path to upstream's rule.
func (se *ScriptEngine) policyResolve(path string) (string, bool) {
	p := se.policy
	if p == nil || path == "" {
		return path, false
	}
	if isRef(path) {
		return p.checkInput(PolicyDenial{Field: "path", Path: path}, se.flowDir), true
	}
	if !p.Remote {
		return path, false
	}
	abs := absolute(path, se.flowDir)
	if p.within(abs) {
		return abs, true
	}
	return p.checkInput(PolicyDenial{Field: "path", Path: path}, se.flowDir), true
}

// policyDeniesShell is hook 3, called at the start of executeRunShell: a
// remote call without AllowShell gets a failed result instead of a shell.
func (fr *FlowRunner) policyDeniesShell(step *flow.RunShellStep) *core.CommandResult {
	p := fr.script.policy
	if p == nil || !p.shellDenied(step) {
		return nil
	}
	return &core.CommandResult{
		Success: false,
		Message: "runShell is not allowed for remote callers",
		Error:   fmt.Errorf("runShell is not allowed for remote callers"),
	}
}
