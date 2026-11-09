package daemon

// blobs.go: the content-addressed blob store every daemon keeps, and the
// `blob:` refs that name its files.
//
// Files live at <home>/blobs/sha256/<hex>. A client uploads one with
// `PUT /v1/devices/{id}/blobs/{sha256}` (resumable with Content-Range) after
// `HEAD` says the daemon lacks it, then names it in a step, an install or an
// attach as blob:sha256:<hex>/<basename>. The routes are device-scoped so
// that, on a mesh, they reach the device's owner like every other device
// route; the store itself is per daemon home.
//
// Every caller-supplied hash must be 64 lowercase hex digits and every
// basename its own filepath.Base, checked before any file is opened, so no
// hash or name can be a path into the daemon's disk.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/executor"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// HeaderUploadOffset reports how many bytes of a blob the daemon holds.
const HeaderUploadOffset = "Upload-Offset"

// BlobExpiry is how long an unused blob is kept; older ones are deleted at
// daemon start.
const BlobExpiry = 7 * 24 * time.Hour

// BlobStore is the daemon's content-addressed file store.
type BlobStore struct {
	root string // <home>/blobs

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewBlobStore returns the store under root (<home>/blobs).
func NewBlobStore(root string) *BlobStore {
	return &BlobStore{root: root, locks: map[string]*sync.Mutex{}}
}

// DefaultBlobRoot is <home>/blobs.
func DefaultBlobRoot() string { return filepath.Join(Home(), "blobs") }

func (b *BlobStore) path(hex string) string    { return filepath.Join(b.root, "sha256", hex) }
func (b *BlobStore) partial(hex string) string { return filepath.Join(b.root, "partial", hex) }

// lock serialises uploads of one hash.
func (b *BlobStore) lock(hex string) func() {
	b.mu.Lock()
	l := b.locks[hex]
	if l == nil {
		l = &sync.Mutex{}
		b.locks[hex] = l
	}
	b.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Has reports whether the blob is complete, touching it as used.
func (b *BlobStore) Has(hex string) (int64, bool) {
	fi, err := os.Stat(b.path(hex))
	if err != nil || fi.IsDir() {
		return 0, false
	}
	now := time.Now()
	_ = os.Chtimes(b.path(hex), now, now)
	return fi.Size(), true
}

// Offset is the size of a partial upload (0 when none).
func (b *BlobStore) Offset(hex string) int64 {
	fi, err := os.Stat(b.partial(hex))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// contentRange parses "bytes S-E/T" (E and T may be *).
var contentRangeRe = regexp.MustCompile(`^bytes (\d+)-(\d+|\*)/(\d+|\*)$`)

// PutResult describes the store after one PUT.
type PutResult struct {
	Complete bool
	Offset   int64
	Created  bool
}

// Put writes body as the bytes of hex starting at the Content-Range start
// (or as the whole file when cr is empty). When the file is complete — the
// range's total reached, or the end of a body with no range — its hash is
// verified before it becomes visible.
func (b *BlobStore) Put(hex, cr string, body io.Reader) (PutResult, *Error) {
	unlock := b.lock(hex)
	defer unlock()
	if size, ok := b.Has(hex); ok {
		_, _ = io.Copy(io.Discard, body)
		return PutResult{Complete: true, Offset: size}, nil
	}
	start, total := int64(0), int64(-1)
	if cr != "" {
		m := contentRangeRe.FindStringSubmatch(strings.TrimSpace(cr))
		if m == nil {
			return PutResult{}, Errorf(CodeUsage, "Content-Range %q: expected bytes START-END/TOTAL", cr)
		}
		start, _ = strconv.ParseInt(m[1], 10, 64)
		if m[3] != "*" {
			total, _ = strconv.ParseInt(m[3], 10, 64)
		}
	}
	if err := os.MkdirAll(filepath.Dir(b.partial(hex)), 0o755); err != nil {
		return PutResult{}, WrapErr(CodeInternal, err)
	}
	if err := os.MkdirAll(filepath.Dir(b.path(hex)), 0o755); err != nil {
		return PutResult{}, WrapErr(CodeInternal, err)
	}
	have := b.Offset(hex)
	if start > have {
		return PutResult{Offset: have}, Errorf(CodeUsage, "blob %s: upload resumes at byte %d, not %d", hex, have, start).
			WithDetails("offset", have)
	}
	f, err := os.OpenFile(b.partial(hex), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return PutResult{}, WrapErr(CodeInternal, err)
	}
	// A retried chunk may overlap what is already here: keep the prefix.
	if err := f.Truncate(start); err != nil {
		f.Close()
		return PutResult{}, WrapErr(CodeInternal, err)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		f.Close()
		return PutResult{}, WrapErr(CodeInternal, err)
	}
	n, copyErr := io.Copy(f, body)
	closeErr := f.Close()
	offset := start + n
	if copyErr != nil {
		// What arrived stays: HEAD reports it as Upload-Offset.
		return PutResult{Offset: offset}, Errorf(CodeUsage, "blob %s: upload interrupted at byte %d: %v", hex, offset, copyErr).
			WithDetails("offset", offset)
	}
	if closeErr != nil {
		return PutResult{Offset: offset}, WrapErr(CodeInternal, closeErr)
	}
	if total >= 0 && offset < total {
		return PutResult{Offset: offset}, nil
	}
	if total >= 0 && offset > total {
		_ = os.Remove(b.partial(hex))
		return PutResult{}, Errorf(CodeUsage, "blob %s: %d bytes is more than the range's total %d", hex, offset, total)
	}
	sum, err := fileSHA256(b.partial(hex))
	if err != nil {
		return PutResult{}, WrapErr(CodeInternal, err)
	}
	if sum != hex {
		_ = os.Remove(b.partial(hex))
		return PutResult{}, Errorf(CodeUsage, "blob hash mismatch: the body's sha256 is %s, not %s", sum, hex).
			WithDetails("sha256", sum)
	}
	if err := os.Rename(b.partial(hex), b.path(hex)); err != nil {
		return PutResult{}, WrapErr(CodeInternal, err)
	}
	return PutResult{Complete: true, Offset: offset, Created: true}, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Expire deletes complete and partial blobs unused for longer than maxAge.
func (b *BlobStore) Expire(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, dir := range []string{"sha256", "partial"} {
		entries, err := os.ReadDir(filepath.Join(b.root, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil || fi.IsDir() || !fi.ModTime().Before(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(b.root, dir, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed
}

// Materialize places the blob a ref names at <dir>/<hex>/<basename>, keeping
// the basename and its extension (simctl addmedia and adb push need them),
// and returns that path. A blob that is a zip of a directory (an iOS .app
// packed by `maestro-d pack`) is extracted there instead. A file already in
// place is reused.
func (b *BlobStore) Materialize(ref, dir string) (string, error) {
	hex, base, err := executor.ParseBlobRef(ref)
	if err != nil {
		return "", err
	}
	src := b.path(hex)
	if _, ok := b.Has(hex); !ok {
		return "", fmt.Errorf("blob %s has not been uploaded to this daemon (HEAD/PUT …/blobs/%s first)", hex, hex)
	}
	dest := filepath.Join(dir, hex, base)
	if _, err := os.Lstat(dest); err == nil {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if isDirZip(src, base) {
		if err := unzipDir(src, dest); err != nil {
			_ = os.RemoveAll(dest)
			return "", fmt.Errorf("unpack %s: %w", base, err)
		}
		return dest, nil
	}
	if err := os.Link(src, dest); err != nil {
		if err := copyFile(src, dest); err != nil {
			return "", err
		}
	}
	return dest, nil
}

// zipFileExts are file types that are zips themselves; a blob with one of
// them is used as the file it is, not unpacked.
var zipFileExts = map[string]bool{".zip": true, ".ipa": true, ".apk": true, ".aab": true, ".apks": true, ".xapk": true, ".jar": true}

// isDirZip reports whether a blob is a packed directory: zip bytes under a
// name that is not itself a zip file type.
func isDirZip(path, base string) bool {
	if zipFileExts[strings.ToLower(filepath.Ext(base))] {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return false
	}
	return bytes.Equal(magic, []byte("PK\x03\x04"))
}

// unzipDir extracts a packed directory into dest, refusing entries that
// would land outside it. Every check is made on the path as the file
// system resolves it, so a chain of symlinks the zip itself creates (a/l ->
// .., then a/l/m -> ..) cannot carry a later entry, or a link, out of dest.
func unzipDir(src, dest string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	realDest, err := executor.RealPath(dest)
	if err != nil {
		return err
	}
	sep := string(filepath.Separator)
	inside := func(p string) (string, bool) {
		real, err := executor.RealPath(p)
		return real, err == nil && underDir(real, realDest)
	}
	for _, f := range zr.File {
		name := filepath.FromSlash(f.Name)
		if name == "" || filepath.IsAbs(name) || hasDotDot(name) {
			return fmt.Errorf("entry %q leaves the directory", f.Name)
		}
		name = strings.TrimSuffix(name, sep)
		if name == "" {
			continue
		}
		// The entry's parent, resolved: every write below goes through it.
		parent, ok := inside(realDest + sep + filepath.Dir(name))
		if !ok {
			return fmt.Errorf("entry %q leaves the directory", f.Name)
		}
		target := filepath.Join(parent, filepath.Base(name))
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if _, ok := inside(target); !ok {
				return fmt.Errorf("entry %q leaves the directory", f.Name)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			link, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			l := string(link)
			if _, ok := inside(parent + sep + l); filepath.IsAbs(l) || !ok {
				return fmt.Errorf("symlink %q points outside the directory", f.Name)
			}
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return err
			}
			if err := os.Symlink(l, target); err != nil {
				return err
			}
		default:
			if _, ok := inside(target); !ok {
				return fmt.Errorf("entry %q leaves the directory", f.Name)
			}
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return err
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm()|0o600)
			if err != nil {
				rc.Close()
				return err
			}
			_, cerr := io.Copy(out, rc)
			rc.Close()
			if err := out.Close(); cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return cerr
			}
		}
	}
	// A later link can change where an earlier one leads (a -> z/.., then
	// z -> ...): check every link in the finished tree.
	return filepath.WalkDir(realDest, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		if _, ok := inside(p); !ok {
			rel, _ := filepath.Rel(realDest, p)
			return fmt.Errorf("symlink %q points outside the directory", filepath.ToSlash(rel))
		}
		return nil
	})
}

// hasDotDot reports whether a relative path has a ".." component.
func hasDotDot(name string) bool {
	for _, c := range strings.Split(filepath.ToSlash(name), "/") {
		if c == ".." {
			return true
		}
	}
	return false
}

// underDir reports whether path lies in (or is) dir.
func underDir(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sessionBlobs is the executor's view of the store for one session: refs
// are materialized under the session's blob-copy directory.
type sessionBlobs struct {
	store *BlobStore
	dir   string
}

func (s sessionBlobs) Dir() string { return s.dir }

func (s sessionBlobs) Resolve(ref string) (string, error) {
	if strings.HasPrefix(ref, "bundle:") {
		return "", fmt.Errorf("flow bundles are not supported by this daemon yet")
	}
	return s.store.Materialize(ref, s.dir)
}

// ---------------------------------------------------------------------------
// Routes

// blobHash returns the route's {sha256}, or writes FORBIDDEN.
func (s *Server) blobHash(w http.ResponseWriter, r *http.Request) (string, bool) {
	h := r.PathValue("sha256")
	if !executor.ValidBlobHash(h) {
		e := forbidden("blob hash %q is not 64 lowercase hex digits", h).WithDetails("field", "sha256")
		s.logForbidden(r, e)
		writeError(w, e)
		return "", false
	}
	return h, true
}

func (s *Server) handleBlobHead(w http.ResponseWriter, r *http.Request) {
	h, ok := s.blobHash(w, r)
	if !ok {
		return
	}
	if size, ok := s.blobs.Has(h); ok {
		w.Header().Set(HeaderUploadOffset, strconv.FormatInt(size, 10))
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set(HeaderUploadOffset, strconv.FormatInt(s.blobs.Offset(h), 10))
	w.WriteHeader(http.StatusNotFound)
}

// BlobResult answers a blob PUT.
type BlobResult struct {
	Envelope
	SHA256   string `json:"sha256"`
	Offset   int64  `json:"offset"`
	Complete bool   `json:"complete"`
}

func (s *Server) handleBlobPut(w http.ResponseWriter, r *http.Request) {
	h, ok := s.blobHash(w, r)
	if !ok {
		return
	}
	s.idle.Hold()
	defer s.idle.Release()
	res, e := s.blobs.Put(h, r.Header.Get("Content-Range"), r.Body)
	w.Header().Set(HeaderUploadOffset, strconv.FormatInt(res.Offset, 10))
	if e != nil {
		writeError(w, e)
		return
	}
	status := http.StatusOK
	switch {
	case res.Created:
		status = http.StatusCreated
		logger.Info("blob %s stored (%d bytes) from %s", h, res.Offset, describeCaller(CallerFrom(r.Context())))
	case !res.Complete:
		status = http.StatusAccepted
	}
	writeJSON(w, status, BlobResult{Envelope: Envelope{OK: true}, SHA256: h, Offset: res.Offset, Complete: res.Complete})
}
