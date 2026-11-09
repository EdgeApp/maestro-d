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
	"regexp"
	"sort"
	"strings"
	"time"

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
	// ClientOutputs marks a local caller that names its outputs itself (a
	// client targeting this daemon over loopback TCP): its relative output
	// paths are written under OutputsDir and reported, as a remote
	// caller's are, but nothing is confined.
	ClientOutputs bool
	// Blobs maps blob refs; nil when the daemon has no blob store.
	Blobs BlobResolver

	// Per-call state, set by the Session that installs the policy.
	roots     []string
	deniedDir string
	cancel    context.CancelFunc
	denial    *PolicyDenial
	// outputs are the paths the policy directed a write to (output fields,
	// baselines, diffs), in order, not yet reported, with what was there
	// before, so only files the call wrote are reported.
	outputs []outputRec
	// callerNames maps a blob copy to the caller's name for it,
	// <first 8 hex of its sha256>/<basename>, for its diff image.
	callerNames map[string]string
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
		Remote:        p.Remote,
		AllowShell:    p.AllowShell,
		BundleDir:     p.BundleDir,
		OutputsDir:    p.OutputsDir,
		ClientOutputs: p.ClientOutputs,
		Blobs:         p.Blobs,
		cancel:        cancel,
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

// within reports whether path (absolute) lies under one of the policy's
// roots, both as written and as the file system resolves it: a symlink in
// a root (an unpacked blob's, say) must not lead out of it.
func (p *CallerPolicy) within(path string) bool {
	clean := filepath.Clean(path)
	real, err := RealPath(path)
	if err != nil {
		return false
	}
	for _, root := range p.roots {
		if !lexicallyUnder(clean, root) {
			continue
		}
		if realRoot, err := RealPath(root); err == nil && lexicallyUnder(real, realRoot) {
			return true
		}
	}
	return false
}

// lexicallyUnder reports whether the cleaned path lies in (or is) dir.
func lexicallyUnder(path, dir string) bool {
	if path == dir {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RealPath resolves every symlink in path as the file system would on
// opening it, as filepath.EvalSymlinks does, except that a component that
// does not exist (and everything after it) is taken as written. path is
// not cleaned first: "l/.." leaves wherever the symlink l leads. A relative
// path is taken from the working directory.
func RealPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd + string(filepath.Separator) + path
	}
	vol := filepath.VolumeName(path)
	root := vol + string(filepath.Separator)
	resolved := root
	todo := strings.Split(filepath.ToSlash(path[len(vol):]), "/")
	links := 0
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, c)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if links++; links > 255 {
			return "", fmt.Errorf("%s: too many levels of symbolic links", path)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			tvol := filepath.VolumeName(target)
			resolved = tvol + string(filepath.Separator)
			target = target[len(tvol):]
		}
		todo = append(strings.Split(filepath.ToSlash(target), "/"), todo...)
	}
	return resolved, nil
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

// rawAbsolute is absolute without the cleaning: the path as the file
// system will open it, for within's resolved check.
func rawAbsolute(path, flowDir string) string {
	if !filepath.IsAbs(path) && flowDir != "" {
		path = flowDir + string(filepath.Separator) + path
	}
	if !filepath.IsAbs(path) {
		if wd, err := os.Getwd(); err == nil {
			path = wd + string(filepath.Separator) + path
		}
	}
	return path
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
	if hex, base, err := ParseBlobRef(d.Path); err == nil {
		if p.callerNames == nil {
			p.callerNames = map[string]string{}
		}
		p.callerNames[absolute(local, "")] = hex[:8] + "/" + base
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
	if p.within(rawAbsolute(d.Path, flowDir)) {
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
		return p.clientOutput(d.Path)
	}
	if p.isDenied(d.Path) {
		p.denial.fill(d)
		return d.Path
	}
	if filepath.IsAbs(d.Path) {
		if p.OutputsDir != "" && underDir(filepath.Clean(d.Path), p.OutputsDir) {
			p.recordOutput(d.Path)
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
	out := filepath.Join(p.OutputsDir, filepath.Clean(d.Path))
	p.recordOutput(out)
	return out
}

// clientOutput rewrites a ClientOutputs caller's relative output path
// under OutputsDir; any other local path is left as it is.
func (p *CallerPolicy) clientOutput(path string) string {
	if !p.ClientOutputs || p.OutputsDir == "" || isRef(path) {
		return path
	}
	if filepath.IsAbs(path) {
		if underDir(filepath.Clean(path), p.OutputsDir) {
			p.recordOutput(path)
		}
		return path
	}
	for _, el := range strings.Split(filepath.ToSlash(path), "/") {
		if el == ".." {
			return path
		}
	}
	out := filepath.Join(p.OutputsDir, filepath.Clean(path))
	p.recordOutput(out)
	return out
}

// outputRec is one recorded output path and the state of the files it may
// name (the path, and with ".png" added when it has no extension) before
// the call wrote them.
type outputRec struct {
	path   string
	before map[string]fileState
}

type fileState struct {
	mod  time.Time
	size int64
}

func statFile(path string) (fileState, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return fileState{}, false
	}
	return fileState{mod: fi.ModTime(), size: fi.Size()}, true
}

// outputCandidates are the files an output path may become: executeTake
// Screenshot and executeAssertScreenshot add ".png" to a path with none.
func outputCandidates(path string) []string {
	if filepath.Ext(path) == "" {
		return []string{path, path + ".png"}
	}
	return []string{path}
}

// recordOutput notes a path the call may write, for the result's outputs.
func (p *CallerPolicy) recordOutput(path string) {
	for _, o := range p.outputs {
		if o.path == path {
			return
		}
	}
	rec := outputRec{path: path, before: map[string]fileState{}}
	for _, c := range outputCandidates(path) {
		if st, ok := statFile(c); ok {
			rec.before[c] = st
		}
	}
	p.outputs = append(p.outputs, rec)
}

// takeOutputs returns the recorded output files the call wrote (created or
// changed), relative to reportDir (outputs/<path>), and forgets them.
func (p *CallerPolicy) takeOutputs(reportDir string) []string {
	if p == nil || len(p.outputs) == 0 {
		return nil
	}
	var out []string
	for _, rec := range p.outputs {
		for _, c := range outputCandidates(rec.path) {
			st, ok := statFile(c)
			if !ok {
				continue
			}
			if prev, had := rec.before[c]; had && prev == st {
				continue
			}
			rel, err := filepath.Rel(reportDir, c)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			out = append(out, filepath.ToSlash(rel))
		}
	}
	p.outputs = nil
	return out
}

// blobHashRe is the only form a caller-supplied hash may take, so no hash
// can be a path into the daemon's disk.
var blobHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidBlobHash reports whether s is 64 lowercase hex digits.
func ValidBlobHash(s string) bool { return blobHashRe.MatchString(s) }

// ParseBlobRef splits blob:sha256:<hex>/<basename>. The hex must be 64
// lowercase hex digits and the basename its own filepath.Base, not "." or
// ".."; it is checked before any file is opened.
func ParseBlobRef(ref string) (hex, base string, err error) {
	rest, ok := strings.CutPrefix(ref, "blob:sha256:")
	if !ok {
		return "", "", fmt.Errorf("not a blob ref (want blob:sha256:<hex>/<basename>)")
	}
	hex, base, ok = strings.Cut(rest, "/")
	if !ok {
		return "", "", fmt.Errorf("blob ref has no /<basename>")
	}
	if !ValidBlobHash(hex) {
		return "", "", fmt.Errorf("blob hash %q is not 64 lowercase hex digits", hex)
	}
	if base == "" || base == "." || base == ".." || base != filepath.Base(base) || strings.ContainsAny(base, "\\\x00") {
		return "", "", fmt.Errorf("blob basename %q is not a plain file name", base)
	}
	return hex, base, nil
}

// withPNG appends ".png" to a path with no extension, as upstream's
// executeAssertScreenshot does to a reference path.
func withPNG(path string) string {
	if filepath.Ext(path) == "" {
		return path + ".png"
	}
	return path
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// screenshotRef applies the remote assertScreenshot rule to its reference
// path: a blob ref, or a relative path naming a file inside the call's
// bundle, is an input; any other relative path is an output (a baseline
// written under OutputsDir, as a local first run writes it beside the
// flow). An absolute path is an input checked against the roots, unless it
// already lies under OutputsDir.
func (se *ScriptEngine) screenshotRef(d PolicyDenial, v *string) {
	p := se.policy
	switch {
	case isRef(*v) || (!p.Remote && !p.ClientOutputs):
		*v = p.checkInput(d, se.flowDir)
	case !p.Remote:
		// A ClientOutputs caller sends a blob ref for a reference it has,
		// and its own output name for a baseline it lacks.
		*v = p.clientOutput(*v)
	case filepath.IsAbs(*v):
		if p.OutputsDir != "" && underDir(filepath.Clean(*v), p.OutputsDir) {
			*v = p.checkOutput(d)
			return
		}
		*v = p.checkInput(d, se.flowDir)
	default:
		abs := absolute(withPNG(*v), se.flowDir)
		if p.within(rawAbsolute(withPNG(*v), se.flowDir)) && fileExists(abs) && (p.OutputsDir == "" || !underDir(abs, p.OutputsDir)) {
			*v = p.checkInput(d, se.flowDir)
			return
		}
		*v = p.checkOutput(d)
	}
}

// PolicyAllows reports whether the active caller policy allows path: it
// lies under one of the call's roots (the blob copies, the bundle, the
// outputs directory). Upstream's validateArtifactPath also accepts these.
func (se *ScriptEngine) PolicyAllows(path string) bool {
	p := se.policy
	return p != nil && p.within(rawAbsolute(path, se.flowDir))
}

// DiffPath is where executeAssertScreenshot writes (and clears) the diff
// image for referencePath. With no remote policy it is upstream's
// core.DiffScreenshotPath, beside the reference. For a remote call it lies
// under OutputsDir, named from the caller's own reference name — for a blob
// ref <first 8 hex>/<basename>, for a bundle file its bundle-relative path —
// so the client can fetch it next to its reference.
func (se *ScriptEngine) DiffPath(referencePath string) string {
	upstream := core.DiffScreenshotPath(referencePath)
	p := se.policy
	if p == nil || (!p.Remote && !p.ClientOutputs) || p.OutputsDir == "" {
		return upstream
	}
	abs := absolute(referencePath, se.flowDir)
	var out string
	switch name, ok := p.callerNames[abs]; {
	case ok:
		out = filepath.Join(p.OutputsDir, core.DiffScreenshotPath(name))
	case p.callerNames[strings.TrimSuffix(abs, ".png")] != "":
		out = filepath.Join(p.OutputsDir, core.DiffScreenshotPath(withPNG(p.callerNames[strings.TrimSuffix(abs, ".png")])))
	case underDir(abs, p.OutputsDir):
		out = core.DiffScreenshotPath(abs)
	case p.BundleDir != "" && underDir(abs, p.BundleDir):
		rel, _ := filepath.Rel(p.BundleDir, abs)
		out = filepath.Join(p.OutputsDir, core.DiffScreenshotPath(rel))
	default:
		out = filepath.Join(p.OutputsDir, core.DiffScreenshotPath(filepath.Base(abs)))
	}
	_ = os.MkdirAll(filepath.Dir(out), 0o755)
	p.recordOutput(out)
	return out
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
		if s.Path != "" {
			se.screenshotRef(den("assertScreenshot", "path", s.Path), &s.Path)
		}
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
	if p.within(rawAbsolute(path, se.flowDir)) {
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
