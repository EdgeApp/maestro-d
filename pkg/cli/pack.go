package cli

// pack.go: `maestro-d pack`, which turns local files into the
// content-addressed units a client uploads to a hub, and the upload itself.
// A file is one unit; a directory (an iOS .app) is zipped deterministically
// first — sorted entries, fixed timestamps — so the same tree always hashes
// the same. The JavaScript library runs this command for its own uploads,
// so both clients pack identically.

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// PackUnit is one upload unit.
type PackUnit struct {
	// Path is the caller's file or directory, absolute.
	Path string `json:"path"`
	// Name is the basename the blob ref carries.
	Name string `json:"name"`
	// SHA256 is the hash of the bytes uploaded.
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// File holds the bytes to upload: Path itself, or the zip written for
	// a directory.
	File string `json:"file"`
	// Dir is set for a zipped directory; the daemon unpacks it.
	Dir bool `json:"dir,omitempty"`
	// Ref is blob:sha256:<hex>/<name>.
	Ref string `json:"ref"`
}

// zipEpoch is the timestamp every packed entry carries.
var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// packPath packs one file or directory.
func packPath(path string) (*PackUnit, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "pack %s: %v", path, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "pack %s: %v", path, err)
	}
	u := &PackUnit{Path: abs, Name: filepath.Base(abs), File: abs}
	if fi.IsDir() {
		zipPath, err := zipDir(abs)
		if err != nil {
			return nil, daemon.Errorf(daemon.CodeUsage, "pack %s: %v", path, err)
		}
		u.File, u.Dir = zipPath, true
	}
	sum, size, err := hashFile(u.File)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "pack %s: %v", path, err)
	}
	u.SHA256, u.Size = sum, size
	u.Ref = "blob:sha256:" + sum + "/" + u.Name
	return u, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// packDir is where directory zips are written.
func packDir() string { return filepath.Join(os.TempDir(), "maestro-d-pack") }

// zipDir writes dir as a deterministic zip and returns its path, named by
// its hash so repeated packs reuse it.
func zipDir(dir string) (string, error) {
	if err := os.MkdirAll(packDir(), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(packDir(), "pack-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	zw := zip.NewWriter(tmp)
	// WalkDir visits entries in lexical order, so the zip is the same for
	// the same tree.
	werr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Modified = zipEpoch
		switch {
		case fi.IsDir():
			hdr.Name += "/"
			hdr.Method = zip.Store
			_, err = zw.CreateHeader(hdr)
			return err
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Method = zip.Store
			w, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			_, err = io.WriteString(w, target)
			return err
		case !fi.Mode().IsRegular():
			return nil
		}
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if werr == nil {
		werr = zw.Close()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}
	sum, _, err := hashFile(tmp.Name())
	if err != nil {
		return "", err
	}
	final := filepath.Join(packDir(), sum+".zip")
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}

// uploadPath packs path and makes sure the device's daemon holds it,
// returning its blob ref. It says on stderr whether it uploaded.
func uploadPath(ctx context.Context, c *daemon.Client, device, path string, quiet bool) (*PackUnit, error) {
	u, err := packPath(path)
	if err != nil {
		return nil, err
	}
	uploaded, err := c.UploadBlob(ctx, device, u.SHA256, u.File, u.Size)
	if err != nil {
		return nil, err
	}
	if !quiet {
		if uploaded {
			progress("Uploaded %s (%d bytes, sha256 %s)", u.Name, u.Size, u.SHA256[:12])
		} else {
			progress("%s is already on the target (HEAD 200, sha256 %s): not uploaded", u.Name, u.SHA256[:12])
		}
	}
	return u, nil
}

var packFlags = []dflag{
	boolFlag("json", "Print the units as JSON", nil, func(o *daemonOpts, v bool) { o.JSON = v }),
}

var packCommand = &cli.Command{
	Name:      "pack",
	Usage:     "Hash local files into the units a client uploads to a hub",
	ArgsUsage: "PATH…",
	Description: flagsDoc(`Each file is one unit; a directory (an iOS .app) is zipped first, with
sorted entries and fixed timestamps, so the same tree always gives the same
sha256. A targeted client (--target-hub, or the JavaScript library on a hub)
packs this way, uploads what the daemon lacks, and names each file
blob:sha256:<hex>/<basename>.

  maestro-d pack app.apk photo.png --json`, packFlags),
	SkipFlagParsing: true,
	Action: withArgs(packFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) == 0 {
			return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d pack PATH…")
		}
		units := make([]*PackUnit, 0, len(args))
		for _, a := range args {
			u, err := packPath(a)
			if err != nil {
				return err
			}
			units = append(units, u)
		}
		if o.JSON {
			printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "units": units})
			return nil
		}
		for _, u := range units {
			kind := ""
			if u.Dir {
				kind = " (directory, zipped)"
			}
			fmt.Printf("%s  %d  %s%s\n", u.SHA256, u.Size, u.Ref, kind)
		}
		return nil
	}),
}

// isBlobRef reports whether v already names an uploaded file.
func isBlobRef(v string) bool {
	return strings.HasPrefix(v, "blob:") || strings.HasPrefix(v, "bundle:")
}
