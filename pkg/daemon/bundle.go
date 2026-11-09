package daemon

// bundle.go: flow bundles. A client that targets another machine's daemon
// packs a flow with its dependency closure (`maestro-d pack --flow`) into a
// manifest, {root, files: {relPath: sha256}}, relative to the files'
// common directory, and uploads each file as a blob. It then posts the
// manifest to `/flows {bundle}` or `/steps {bundle}`, or names it in a
// one-shot's flow field as bundle:sha256:<manifest>/<root> (the manifest
// itself uploaded as a blob). The owner builds the directory under the
// session's report directory, bundles/<manifest sha>/, with hard links from
// the blob store, and runs the root file from there, so every relative
// reference resolves inside the bundle as it does beside the file on the
// client.
//
// Every name and hash a caller supplies is checked before anything is
// created (FORBIDDEN otherwise), so a caller-built manifest can never place
// a file outside bundles/<sha>/ or pull a daemon file into one.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/executor"
)

// BundleManifest names a flow bundle's files: Root is the file the call
// runs and Files maps each bundle-relative path to its blob's sha256.
type BundleManifest struct {
	Root  string            `json:"root"`
	Files map[string]string `json:"files"`
}

// maxBundleFiles bounds a manifest; maxManifestBytes bounds an uploaded one.
const (
	maxBundleFiles   = 10000
	maxManifestBytes = 8 << 20
)

// Validate checks every name and hash in the manifest. A failure is
// FORBIDDEN, with the offending field in details.
func (m *BundleManifest) Validate() *Error {
	if m == nil {
		return forbidden("bundle: no manifest").WithDetails("field", "bundle")
	}
	if err := executor.ValidBundlePath(m.Root); err != nil {
		return forbidden("bundle root: %v", err).WithDetails("field", "bundle.root")
	}
	if len(m.Files) > maxBundleFiles {
		return Errorf(CodeUsage, "bundle: %d files is more than %d", len(m.Files), maxBundleFiles)
	}
	keys := make([]string, 0, len(m.Files))
	for k := range m.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := executor.ValidBundlePath(k); err != nil {
			return forbidden("bundle file: %v", err).WithDetails("field", "bundle.files")
		}
		if !executor.ValidBlobHash(m.Files[k]) {
			return forbidden("bundle file %s: hash %q is not 64 lowercase hex digits", k, m.Files[k]).WithDetails("field", "bundle.files")
		}
	}
	if _, ok := m.Files[m.Root]; !ok {
		return forbidden("bundle root %s is not one of the bundle's files", m.Root).WithDetails("field", "bundle.root")
	}
	return nil
}

// Hash is the sha256 of the manifest's JSON encoding (keys sorted), which
// names its directory; `maestro-d pack --flow` uploads exactly these bytes
// as the manifest blob.
func (m *BundleManifest) Hash() string {
	data, _ := json.Marshal(m)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReadManifest reads an uploaded manifest blob.
func (b *BlobStore) ReadManifest(hash string) (*BundleManifest, error) {
	if !executor.ValidBlobHash(hash) {
		return nil, fmt.Errorf("bundle manifest hash %q is not 64 lowercase hex digits", hash)
	}
	if _, ok := b.Has(hash); !ok {
		return nil, fmt.Errorf("bundle manifest %s has not been uploaded to this daemon (HEAD/PUT …/blobs/%s first)", hash, hash)
	}
	f, err := os.Open(b.path(hash))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("bundle manifest %s is larger than %d bytes", hash, maxManifestBytes)
	}
	var m BundleManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("bundle manifest %s: %v", hash, err)
	}
	return &m, nil
}

// MaterializeBundle builds <parent>/<hash>/ from a validated manifest,
// hard-linking each file from the store (copying where links fail), and
// returns the directory. An existing directory is reused: its name is the
// manifest's hash, so its contents are the same.
func (b *BlobStore) MaterializeBundle(m *BundleManifest, hash, parent string) (string, *Error) {
	dest := filepath.Join(parent, hash)
	if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
		return dest, nil
	}
	keys := make([]string, 0, len(m.Files))
	var missing []string
	for k, h := range m.Files {
		keys = append(keys, k)
		if _, ok := b.Has(h); !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(keys)
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", Errorf(CodeUsage, "bundle: %d file(s) not uploaded to this daemon: %s (HEAD/PUT …/blobs/<sha256> first)",
			len(missing), strings.Join(missing, ", ")).WithDetails("missing", missing)
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", WrapErr(CodeInternal, err)
	}
	tmp, err := os.MkdirTemp(parent, "."+hash[:12]+"-")
	if err != nil {
		return "", WrapErr(CodeInternal, err)
	}
	for _, k := range keys {
		target := filepath.Join(tmp, filepath.FromSlash(k))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = os.RemoveAll(tmp)
			return "", Errorf(CodeUsage, "bundle file %s: %v", k, err)
		}
		src := b.path(m.Files[k])
		if err := os.Link(src, target); err != nil {
			if err := copyFile(src, target); err != nil {
				_ = os.RemoveAll(tmp)
				return "", Errorf(CodeUsage, "bundle file %s: %v", k, err)
			}
		}
		// The link shares the blob's inode: a step writing through it
		// would change the stored blob.
		_ = os.Chmod(target, 0o444)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp)
		if fi, serr := os.Stat(dest); serr == nil && fi.IsDir() {
			return dest, nil
		}
		return "", WrapErr(CodeInternal, err)
	}
	return dest, nil
}

// resolveBundleRef materializes the bundle a bundle:sha256:<manifest>/<root>
// ref names under parent and returns the root file's path.
func (b *BlobStore) resolveBundleRef(ref, parent string) (string, error) {
	hash, root, err := executor.ParseBundleRef(ref)
	if err != nil {
		return "", err
	}
	m, err := b.ReadManifest(hash)
	if err != nil {
		return "", err
	}
	if e := m.Validate(); e != nil {
		return "", fmt.Errorf("bundle manifest %s: %s", hash, e.Message)
	}
	if _, ok := m.Files[root]; !ok {
		return "", fmt.Errorf("%s is not a file of bundle %s", root, hash)
	}
	dir, e := b.MaterializeBundle(m, hash, parent)
	if e != nil {
		return "", fmt.Errorf("%s", e.Message)
	}
	return filepath.Join(dir, filepath.FromSlash(root)), nil
}

// bundleCall checks a /flows or /steps bundle and materializes it in the
// session's bundles/ directory, returning the bundle directory and the root
// file's path.
func (s *Server) bundleCall(a *attached, m *BundleManifest) (dir, root string, e *Error) {
	if e := m.Validate(); e != nil {
		return "", "", e
	}
	dir, e = s.blobs.MaterializeBundle(m, m.Hash(), filepath.Join(a.reportDir, "bundles"))
	if e != nil {
		return "", "", e
	}
	return dir, filepath.Join(dir, filepath.FromSlash(m.Root)), nil
}
