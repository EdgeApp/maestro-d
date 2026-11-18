package cli

// bundle.go: `maestro-d pack --flow`, which packs a flow with its
// dependency closure into a bundle a hub runs whole, and the client side of
// running one (upload, then fetch the outputs back).
//
// The walk follows the references the validator follows — runFlow (file
// and else), retry's file, and the flows' onFlowStart/onFlowComplete hooks —
// plus the files the steps read: runScript (.js), addMedia,
// assertScreenshot, runBrowserScript, runWebViewScript, uploadFile and
// loadAuthState, and the root flow's workspace config.yaml. ${VAR} in a
// path is expanded with the -e values (and the flows' own env); a path that
// still cannot be resolved stops the pack, naming the step and field.
//
// The manifest is {root, files: {relPath: sha256}}, relative to the files'
// common directory, which is the bundle root on both sides: the owner
// materializes it as bundles/<manifest sha>/, and writes a bundled output
// field's file under outputs/ at the same path relative to that root, which
// the client fetches back into the common directory.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/executor"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// FlowBundle is a packed flow.
type FlowBundle struct {
	// Manifest is what the client posts (/flows, /steps) and uploads as
	// the manifest blob (bundle: refs).
	Manifest daemon.BundleManifest `json:"manifest"`
	// SHA256 is the manifest's hash: its blob, and its directory on the
	// owner, bundles/<sha256>/.
	SHA256 string `json:"sha256"`
	// Dir is the files' common directory on the client, the bundle root.
	Dir string `json:"dir"`
	// Roots maps each root file given, absolute, to its manifest path.
	Roots map[string]string `json:"roots"`
	// Units are the files to upload, one per distinct hash.
	Units []*PackUnit `json:"units"`
	// ManifestUnit is the manifest's own blob, for bundle: refs.
	ManifestUnit *PackUnit `json:"manifestUnit"`
}

// Ref is the bundle:sha256:<manifest>/<root> ref for one of its roots.
func (b *FlowBundle) Ref(root string) string {
	return "bundle:sha256:" + b.SHA256 + "/" + b.Roots[root]
}

// packFlowOpts are pack --flow's options.
type packFlowOpts struct {
	// Base, when set, places the (first) root as if it sat in this
	// directory: its references resolve there, and its manifest path is
	// its basename beside Base's files. The JS library uses it for the
	// temporary root file that stands for inline steps.
	Base string
	// Env expands ${VAR} in paths (-e).
	Env map[string]string
	// Include adds files flows open dynamically (globs relative to the
	// first root's directory).
	Include []string
}

// bundleWalk is one pack --flow's state.
type bundleWalk struct {
	env map[string]string
	// files maps each file's place in the bundle (absolute, on the client)
	// to the file holding its bytes (the same path, except for a --base
	// root).
	files map[string]string
	// flows are the flow files already walked.
	flows map[string]bool
}

var varRe = regexp.MustCompile(`\$\{[^}]*\}`)

// expand replaces ${NAME} with env values; ok is false when one is left.
func expand(v string, env map[string]string) (string, bool) {
	out := varRe.ReplaceAllStringFunc(v, func(m string) string {
		name := strings.TrimSpace(m[2 : len(m)-1])
		if val, ok := env[name]; ok {
			return val
		}
		return m
	})
	return out, !strings.Contains(out, "${")
}

func mergeEnv(base, over map[string]string) map[string]string {
	if len(over) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// packFlow packs the flow files roots (and their closures) into one
// bundle; the first root is the manifest's root.
func packFlow(roots []string, opts packFlowOpts) (*FlowBundle, error) {
	if len(roots) == 0 {
		return nil, daemon.Errorf(daemon.CodeUsage, "pack --flow: no flow file")
	}
	w := &bundleWalk{env: opts.Env, files: map[string]string{}, flows: map[string]bool{}}
	b := &FlowBundle{Roots: map[string]string{}}
	places := make([]string, len(roots))
	for i, r := range roots {
		actual, err := filepath.Abs(r)
		if err != nil {
			return nil, daemon.Errorf(daemon.CodeUsage, "pack %s: %v", r, err)
		}
		place := actual
		if i == 0 && opts.Base != "" {
			base, err := filepath.Abs(opts.Base)
			if err != nil {
				return nil, daemon.Errorf(daemon.CodeUsage, "pack --base %s: %v", opts.Base, err)
			}
			place = filepath.Join(base, filepath.Base(actual))
		}
		places[i] = place
		if err := w.flow(place, actual, w.env, true); err != nil {
			return nil, err
		}
	}
	rootDir := filepath.Dir(places[0])
	for _, g := range opts.Include {
		if !filepath.IsAbs(g) {
			g = filepath.Join(rootDir, g)
		}
		matches, err := filepath.Glob(g)
		if err != nil {
			return nil, daemon.Errorf(daemon.CodeUsage, "pack --include %s: %v", g, err)
		}
		n := 0
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
				w.files[filepath.Clean(m)] = filepath.Clean(m)
				n++
			}
		}
		if n == 0 {
			return nil, daemon.Errorf(daemon.CodeUsage, "pack --include %s: no file matches", g)
		}
	}
	b.Dir = commonDir(w.files)
	b.Manifest.Files = map[string]string{}
	byHash := map[string]bool{}
	places2 := make([]string, 0, len(w.files))
	for p := range w.files {
		places2 = append(places2, p)
	}
	sort.Strings(places2)
	for _, place := range places2 {
		u, err := packPath(w.files[place])
		if err != nil {
			return nil, err
		}
		if u.Dir {
			return nil, daemon.Errorf(daemon.CodeUsage, "pack --flow: %s is a directory, not a file", w.files[place])
		}
		rel, _ := filepath.Rel(b.Dir, place)
		rel = filepath.ToSlash(rel)
		b.Manifest.Files[rel] = u.SHA256
		if !byHash[u.SHA256] {
			byHash[u.SHA256] = true
			u.Name = filepath.Base(place)
			b.Units = append(b.Units, u)
		}
	}
	for i, r := range roots {
		abs, _ := filepath.Abs(r)
		rel, _ := filepath.Rel(b.Dir, places[i])
		b.Roots[abs] = filepath.ToSlash(rel)
	}
	b.Manifest.Root = b.Roots[mustAbs(roots[0])]
	data, err := json.Marshal(&b.Manifest)
	if err != nil {
		return nil, daemon.WrapErr(daemon.CodeInternal, err)
	}
	b.SHA256 = b.Manifest.Hash()
	if err := os.MkdirAll(packDir(), 0o755); err != nil {
		return nil, daemon.WrapErr(daemon.CodeUsage, err)
	}
	mpath := filepath.Join(packDir(), b.SHA256+".json")
	if err := os.WriteFile(mpath, data, 0o644); err != nil {
		return nil, daemon.WrapErr(daemon.CodeUsage, err)
	}
	b.ManifestUnit = &PackUnit{Path: mpath, Name: "manifest.json", SHA256: b.SHA256, Size: int64(len(data)), File: mpath,
		Ref: "blob:sha256:" + b.SHA256 + "/manifest.json"}
	return b, nil
}

func mustAbs(p string) string {
	abs, _ := filepath.Abs(p)
	return abs
}

// commonDir is the deepest directory holding every file.
func commonDir(files map[string]string) string {
	common := ""
	for p := range files {
		d := filepath.Dir(p)
		if common == "" {
			common = d
			continue
		}
		for !executor.PathUnder(d, common) {
			parent := filepath.Dir(common)
			if parent == common {
				break
			}
			common = parent
		}
	}
	return common
}

// flow walks one flow file: place is where it sits in the bundle, actual
// the file holding its bytes. root marks a root flow, whose workspace
// config.yaml is packed too.
func (w *bundleWalk) flow(place, actual string, env map[string]string, root bool) error {
	if w.flows[place] {
		return nil
	}
	w.flows[place] = true
	data, err := os.ReadFile(actual)
	if err != nil {
		return daemon.Errorf(daemon.CodeUsage, "pack --flow: %v", err)
	}
	f, err := flow.Parse(data, place)
	if err != nil {
		return daemon.Errorf(daemon.CodeUsage, "pack --flow: %v", err)
	}
	w.files[place] = actual
	env = mergeEnv(f.Config.Env, env)
	dir := filepath.Dir(place)
	if root {
		for _, name := range []string{"config.yaml", "config.yml"} {
			if p := filepath.Join(dir, name); fileExists(p) && p != place {
				w.files[p] = p
			}
		}
	}
	for _, steps := range [][]flow.Step{f.Config.OnFlowStart, f.Steps, f.Config.OnFlowComplete} {
		if err := w.steps(steps, place, dir, env); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// stepErr names the step and field a path came from.
func stepErr(file string, step flow.Step, field, format string, args ...any) error {
	return daemon.Errorf(daemon.CodeUsage, "pack --flow: %s: step %q, %s: %s", file, step.Describe(), field, fmt.Sprintf(format, args...)).
		WithDetails("step", step.Describe()).WithDetails("field", field)
}

// resolve expands and checks one path a step names, relative to dir.
func (w *bundleWalk) resolve(file, dir string, step flow.Step, field, v string, env map[string]string) (string, error) {
	x, ok := expand(v, env)
	if !ok {
		return "", stepErr(file, step, field, "%s cannot be resolved (pass the variable with -e NAME=VALUE)", v)
	}
	if filepath.IsAbs(x) {
		return "", stepErr(file, step, field, "%s is absolute; a bundled flow's paths must be relative to the flow", x)
	}
	return filepath.Join(dir, x), nil
}

// input adds a file a step reads.
func (w *bundleWalk) input(file, dir string, step flow.Step, field, v string, env map[string]string) error {
	if v == "" || isBlobRef(v) {
		return nil
	}
	p, err := w.resolve(file, dir, step, field, v, env)
	if err != nil {
		return err
	}
	if !fileExists(p) {
		return stepErr(file, step, field, "%s: no such file", p)
	}
	w.files[p] = p
	return nil
}

// output checks a file a step writes: it must be relative, inside the
// bundle, and is not packed.
func (w *bundleWalk) output(file string, step flow.Step, field, v string, env map[string]string) error {
	if v == "" {
		return nil
	}
	x, _ := expand(v, env)
	if filepath.IsAbs(x) {
		return stepErr(file, step, field, "%s is absolute; a bundled flow's output paths must be relative (they are written on the hub and fetched back)", x)
	}
	for _, el := range strings.Split(filepath.ToSlash(x), "/") {
		if el == ".." {
			return stepErr(file, step, field, "%s leaves the flow's directory; a bundled flow's output paths may not contain ..", x)
		}
	}
	return nil
}

// subflow walks a flow file a step runs.
func (w *bundleWalk) subflow(file, dir string, step flow.Step, field, v string, env map[string]string) error {
	if v == "" || isBlobRef(v) {
		return nil
	}
	p, err := w.resolve(file, dir, step, field, v, env)
	if err != nil {
		return err
	}
	if !fileExists(p) {
		return stepErr(file, step, field, "%s: no such flow file", p)
	}
	return w.flow(p, p, env, false)
}

// steps walks a step list of the flow file at file (dir is its directory).
func (w *bundleWalk) steps(steps []flow.Step, file, dir string, env map[string]string) error {
	for _, step := range steps {
		var err error
		switch s := step.(type) {
		case *flow.DefineVariablesStep:
			env = mergeEnv(env, s.Env)
		case *flow.AddMediaStep:
			for _, f := range s.Files {
				if err = w.input(file, dir, s, "files", f, env); err != nil {
					break
				}
			}
		case *flow.RunScriptStep:
			senv := mergeEnv(env, s.Env)
			for field, v := range map[string]string{"file": s.File, "script": s.Script} {
				if strings.HasSuffix(v, ".js") && err == nil {
					err = w.input(file, dir, s, field, v, senv)
				}
			}
		case *flow.AssertScreenshotStep:
			err = w.screenshotRef(file, dir, s, env)
		case *flow.RunBrowserScriptStep:
			err = w.input(file, dir, s, "file", s.File, env)
		case *flow.RunWebViewScriptStep:
			err = w.input(file, dir, s, "file", s.File, env)
		case *flow.UploadFileStep:
			err = w.input(file, dir, s, "path", s.Path, env)
			for _, p := range s.Paths {
				if err == nil {
					err = w.input(file, dir, s, "paths", p, env)
				}
			}
		case *flow.LoadAuthStateStep:
			err = w.input(file, dir, s, "path", s.Path, env)
		case *flow.TakeScreenshotStep:
			err = w.output(file, s, "path", s.Path, env)
		case *flow.SaveAuthStateStep:
			err = w.output(file, s, "path", s.Path, env)
		case *flow.RunFlowStep:
			senv := mergeEnv(env, s.Env)
			if err = w.subflow(file, dir, s, "file", s.File, senv); err == nil {
				err = w.subflow(file, dir, s, "else", s.ElseFile, senv)
			}
			if err == nil {
				err = w.steps(s.Steps, file, dir, senv)
			}
			if err == nil {
				err = w.steps(s.ElseSteps, file, dir, senv)
			}
		case *flow.RetryStep:
			senv := mergeEnv(env, s.Env)
			if err = w.subflow(file, dir, s, "file", s.File, senv); err == nil {
				err = w.steps(s.Steps, file, dir, senv)
			}
		case *flow.RepeatStep:
			err = w.steps(s.Steps, file, dir, env)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// screenshotRef packs an assertScreenshot reference that exists (with
// ".png" added when it has no extension, as the executor does); a missing
// one is a baseline the run writes, so it is checked as an output instead.
func (w *bundleWalk) screenshotRef(file, dir string, s *flow.AssertScreenshotStep, env map[string]string) error {
	if s.Path == "" || isBlobRef(s.Path) {
		return nil
	}
	x, ok := expand(s.Path, env)
	if ok && !filepath.IsAbs(x) {
		if p := withPNG(filepath.Join(dir, x)); fileExists(p) {
			w.files[p] = p
			return nil
		}
	}
	if !ok {
		return stepErr(file, s, "path", "%s cannot be resolved (pass the variable with -e NAME=VALUE)", s.Path)
	}
	return w.output(file, s, "path", x, env)
}

// uploadBundle uploads the bundle's files the device's daemon lacks, and
// its manifest when withManifest (for bundle: refs). It reports on stderr
// how many it uploaded.
func uploadBundle(ctx context.Context, c *daemon.Client, device string, b *FlowBundle, withManifest, quiet bool) error {
	units := b.Units
	if withManifest {
		units = append(append([]*PackUnit{}, units...), b.ManifestUnit)
	}
	sent, have := 0, 0
	for _, u := range units {
		uploaded, err := c.UploadBlob(ctx, device, u.SHA256, u.File, u.Size)
		if err != nil {
			return err
		}
		if uploaded {
			sent++
		} else {
			have++
		}
	}
	if !quiet {
		what := "blobs"
		if withManifest {
			what = "blobs, manifest included"
		}
		progress("Bundle %s (%s, %d files): uploaded %d %s, %d already on the target", b.SHA256[:12], b.Manifest.Root, len(b.Manifest.Files), sent, what, have)
	}
	return nil
}

// fetchBundleOutputs writes each file a bundled call lists in outputs to
// the same path relative to the bundle root on the client, dir. An entry
// that would leave dir is skipped.
func fetchBundleOutputs(ctx context.Context, c *daemon.Client, device, dir string, outputs []string, quiet bool) error {
	for _, entry := range outputs {
		rel, ok := strings.CutPrefix(entry, "outputs/")
		if !ok || executor.ValidBundlePath(rel) != nil {
			progress("Skipped output %s: not a path inside the bundle", entry)
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := fetchArtifact(ctx, c, device, entry, target); err != nil {
			return err
		}
		if !quiet {
			progress("Fetched %s → %s", entry, target)
		}
	}
	return nil
}
