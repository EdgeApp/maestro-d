package daemon

// artifacts.go: reading a session's report directory — screenshots, the
// files a remote call's output fields wrote under outputs/, recordings,
// hierarchy dumps, client.log — so a client on another machine can fetch
// them.

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/executor"
)

// ArtifactInfo is one file of a session's report directory.
type ArtifactInfo struct {
	// Path is relative to the report directory, with / separators.
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

// ArtifactsResult lists a session's artifacts.
type ArtifactsResult struct {
	Envelope
	Device    string         `json:"device"`
	ReportDir string         `json:"reportDir"`
	Artifacts []ArtifactInfo `json:"artifacts"`
}

// artifactSkipDirs are the inputs and scratch directories a session keeps
// beside its artifacts: the blob copies and the policy's denied/ stand-in.
var artifactSkipDirs = map[string]bool{"blobs": true, "denied": true}

// listArtifacts walks dir for regular files.
func listArtifacts(dir string) ([]ArtifactInfo, error) {
	out := []ArtifactInfo{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			if path != dir && artifactSkipDirs[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, ArtifactInfo{Path: filepath.ToSlash(rel), Size: fi.Size(), ModTime: fi.ModTime().UTC().Format(time.RFC3339)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	list, err := listArtifacts(a.reportDir)
	if err != nil {
		writeError(w, WrapErr(CodeInternal, err))
		return
	}
	writeJSON(w, http.StatusOK, ArtifactsResult{Envelope: Envelope{OK: true}, Device: a.id, ReportDir: a.reportDir, Artifacts: list})
}

// artifactPath resolves a caller's artifact path inside reportDir, refusing
// an absolute path, a ".." element, or a symlink that leads out.
func artifactPath(reportDir, rel string) (string, *Error) {
	if rel == "" {
		return "", Errorf(CodeUsage, "artifact path is required")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", forbidden("artifact path %q must be relative to the report directory", rel).WithDetails("field", "path")
	}
	for _, el := range strings.Split(filepath.ToSlash(rel), "/") {
		if el == ".." {
			return "", forbidden("artifact path %q may not contain ..", rel).WithDetails("field", "path")
		}
	}
	full := filepath.Join(reportDir, filepath.FromSlash(rel))
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", Errorf(CodeUsage, "no artifact %s", rel).WithDetails("path", rel)
		}
		return "", WrapErr(CodeInternal, err)
	}
	root, err := filepath.EvalSymlinks(reportDir)
	if err != nil {
		return "", WrapErr(CodeInternal, err)
	}
	if !executor.PathUnder(real, root) {
		return "", forbidden("artifact path %q leads outside the report directory", rel).WithDetails("field", "path")
	}
	return real, nil
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	path, e := artifactPath(a.reportDir, r.PathValue("path"))
	if e != nil {
		if e.Code == CodeForbidden {
			s.logForbidden(r, e)
		}
		writeError(w, e)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, WrapErr(CodeInternal, err))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		writeError(w, Errorf(CodeUsage, "artifact %s is not a file", r.PathValue("path")))
		return
	}
	ct := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		ct = "image/png"
	case ".json":
		ct = "application/json"
	case ".xml":
		ct = "application/xml"
	case ".log", ".txt":
		ct = "text/plain; charset=utf-8"
	case ".mp4":
		ct = "video/mp4"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}
