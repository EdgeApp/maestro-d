package cli

// remote_files.go: the files of a one-shot sent to another machine's
// daemon (--target-hub). Its input-file fields (FieldSpec.Path) are packed,
// uploaded when the daemon lacks them, and sent as blob: refs; its output
// fields (FieldSpec.Output) are sent as names of the client's own,
// <first 8 hex of sha256(caller path)>/<basename>, and the files the result
// lists in `outputs` are fetched back to the caller's paths. Its flow-file
// fields (FieldSpec.Flow: runFlow's file and else, retry's file) are packed
// with their whole closure into one bundle (pack --flow) and sent as
// bundle:sha256:<manifest>/<root> refs; the outputs the subflow writes come
// back relative to that bundle's common directory.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// hubFiles rewrites one command's file fields for a targeted daemon.
type hubFiles struct {
	ctx    context.Context
	client *daemon.Client
	device string
	cwd    string
	quiet  bool
	// env expands ${VAR} in a packed flow's paths (-e).
	env map[string]string
	// bundleDir is the common directory of the bundle the command's flow
	// fields were packed into: outputs the command did not name itself
	// are fetched there.
	bundleDir string
	// fetch maps an outputs entry the result may list (outputs/<name>) to
	// the caller's path it is written to.
	fetch map[string]string
}

func newHubFiles(ctx context.Context, c *daemon.Client, device string, env map[string]string, quiet bool) *hubFiles {
	cwd, _ := os.Getwd()
	return &hubFiles{ctx: ctx, client: c, device: device, cwd: cwd, env: env, quiet: quiet, fetch: map[string]string{}}
}

func (h *hubFiles) abs(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(h.cwd, p)
	}
	return filepath.Clean(p)
}

// withPNG appends ".png" to a path with no extension, as the executor does
// to screenshot paths.
func withPNG(p string) string {
	if filepath.Ext(p) == "" {
		return p + ".png"
	}
	return p
}

// hash8 is the first 8 hex digits of sha256(s).
func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// output names an output file for the daemon and records where it goes.
func (h *hubFiles) output(v string) string {
	target := h.abs(v)
	name := hash8(target) + "/" + filepath.Base(target)
	h.fetch["outputs/"+name] = target
	if filepath.Ext(name) == "" {
		// takeScreenshot and an assertScreenshot baseline add .png.
		h.fetch["outputs/"+name+".png"] = target + ".png"
	}
	return name
}

// apply rewrites one value of a marked field.
func (h *hubFiles) apply(cmd string, f daemon.FieldSpec, v string) (string, error) {
	if v == "" || isBlobRef(v) || (f.PathSuffix != "" && !strings.HasSuffix(v, f.PathSuffix)) {
		return v, nil
	}
	if f.Output {
		return h.output(v), nil
	}
	if !f.Path {
		return v, nil
	}
	path := h.abs(v)
	if cmd == "assertScreenshot" {
		// A missing reference is a baseline the run writes, as a local
		// first run does beside the flow; the executor adds .png to an
		// extensionless one, so look for that file.
		path = withPNG(path)
		if _, err := os.Stat(path); err != nil {
			return h.output(v), nil
		}
	}
	u, err := uploadPath(h.ctx, h.client, h.device, path, h.quiet)
	if err != nil {
		return "", err
	}
	if cmd == "assertScreenshot" {
		// A mismatch writes the diff under the blob's name; it belongs
		// beside the caller's reference.
		h.fetch["outputs/"+u.SHA256[:8]+"/"+core.DiffScreenshotPath(u.Name)] = core.DiffScreenshotPath(path)
	}
	return u.Ref, nil
}

// applyAny rewrites a string, or each string of a list.
func (h *hubFiles) applyAny(cmd string, f daemon.FieldSpec, v any) (any, error) {
	switch x := v.(type) {
	case string:
		return h.apply(cmd, f, x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			s, ok := e.(string)
			if !ok {
				out[i] = e
				continue
			}
			r, err := h.apply(cmd, f, s)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// flows packs the command's flow-file fields (string values only: a
// runFlow else may be inline steps) into one bundle, uploads it with its
// manifest, and returns each field's bundle: ref.
func (h *hubFiles) flows(spec daemon.CommandSpec, value any) (map[string]string, error) {
	vals := map[string]string{}
	env := h.env
	switch v := value.(type) {
	case map[string]any:
		for _, f := range spec.Fields {
			if s, ok := v[f.Key].(string); ok && f.Flow && s != "" && !isBlobRef(s) {
				vals[f.Key] = s
			}
		}
		if e, ok := v["env"].(map[string]any); ok {
			extra := map[string]string{}
			for k, x := range e {
				extra[k] = fmt.Sprint(x)
			}
			env = mergeEnv(env, extra)
		}
	case string:
		for _, f := range spec.Fields {
			if f.Key == spec.BareField && f.Flow && v != "" && !isBlobRef(v) {
				vals[f.Key] = v
			}
		}
	}
	if len(vals) == 0 {
		return nil, nil
	}
	// file first, so it is the manifest's root; then the rest (else).
	keys := make([]string, 0, len(vals))
	for k := range vals {
		if k != "file" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if _, ok := vals["file"]; ok {
		keys = append([]string{"file"}, keys...)
	}
	roots := make([]string, len(keys))
	for i, k := range keys {
		roots[i] = h.abs(vals[k])
	}
	b, err := packFlow(roots, packFlowOpts{Env: env})
	if err != nil {
		return nil, err
	}
	if err := uploadBundle(h.ctx, h.client, h.device, b, true, h.quiet); err != nil {
		return nil, err
	}
	h.bundleDir = b.Dir
	refs := make(map[string]string, len(keys))
	for i, k := range keys {
		refs[k] = b.Ref(roots[i])
	}
	return refs, nil
}

// rewrite returns the command's value with its file fields rewritten. A
// bare value (a scalar, or a sequence) fills spec.BareField and gets that
// field's mark.
func (h *hubFiles) rewrite(spec daemon.CommandSpec, value any) (any, error) {
	fields := map[string]daemon.FieldSpec{}
	for _, f := range spec.Fields {
		if f.Path || f.Output {
			fields[f.Key] = f
		}
	}
	refs, err := h.flows(spec, value)
	if err != nil {
		return nil, err
	}
	if _, ok := value.(string); ok && refs[spec.BareField] != "" {
		return refs[spec.BareField], nil
	}
	if m, ok := value.(map[string]any); ok && len(refs) > 0 {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		for k, r := range refs {
			out[k] = r
		}
		value = out
	}
	if len(fields) == 0 || value == nil {
		return value, nil
	}
	if m, ok := value.(map[string]any); ok {
		out := make(map[string]any, len(m))
		for k, v := range m {
			if f, marked := fields[k]; marked {
				r, err := h.applyAny(spec.Name, f, v)
				if err != nil {
					return nil, err
				}
				v = r
			}
			out[k] = v
		}
		return out, nil
	}
	f, marked := fields[spec.BareField]
	if !marked {
		return value, nil
	}
	return h.applyAny(spec.Name, f, value)
}

// fetchOutputs writes each listed output this command named to the
// caller's path.
func (h *hubFiles) fetchOutputs(outputs []string) error {
	var rest []string
	for _, entry := range outputs {
		target, ok := h.fetch[entry]
		if !ok {
			rest = append(rest, entry)
			continue
		}
		if err := fetchArtifact(h.ctx, h.client, h.device, entry, target); err != nil {
			return err
		}
		if !h.quiet {
			progress("Fetched %s → %s", entry, target)
		}
	}
	if h.bundleDir != "" && len(rest) > 0 {
		// A packed subflow's outputs, named relative to its bundle root.
		return fetchBundleOutputs(h.ctx, h.client, h.device, h.bundleDir, rest, h.quiet)
	}
	return nil
}

// fetchArtifact downloads one artifact to target, through a temporary file
// beside it.
func fetchArtifact(ctx context.Context, c *daemon.Client, device, entry, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*")
	if err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	defer os.Remove(tmp.Name())
	if err := c.Artifact(ctx, device, entry, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	if err := os.Rename(tmp.Name(), target); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	return nil
}

// uploadAttachFiles replaces a targeted attach's local appFile with a blob
// ref, uploading it first (checkTargeted refuses --caps there).
func uploadAttachFiles(ctx context.Context, c *daemon.Client, o *daemonOpts, device string, cfg *daemon.AttachConfig) error {
	if o.Target == "" {
		return nil
	}
	if cfg.AppFile != "" && !isBlobRef(cfg.AppFile) {
		u, err := uploadPath(ctx, c, device, cfg.AppFile, o.JSON)
		if err != nil {
			return err
		}
		cfg.AppFile = u.Ref
	}
	return nil
}
