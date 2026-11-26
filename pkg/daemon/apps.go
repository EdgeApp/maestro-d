package daemon

// apps.go: installing and uninstalling apps as operations of their own
// (`POST /v1/devices/{id}/apps`, `DELETE /v1/devices/{id}/apps/{appId}`),
// with the installers pkg/cli injects through Deps. The device need not be
// attached.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/executor"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// InstallRequest is the body of POST /v1/devices/{id}/apps.
type InstallRequest struct {
	// App is a blob:sha256:<hex>/<basename> ref, or (local callers only) a
	// path on the daemon's host: an .apk, .app or .ipa.
	App string `json:"app"`
	// Reinstall uninstalls the app first when its id is known.
	Reinstall bool `json:"reinstall,omitempty"`
	// Platform overrides the platform inferred from the file and device.
	Platform string `json:"platform,omitempty"`
	// URL, in place of App, is a build the device's own daemon downloads
	// (appurl.go): Headers go with that request only, never to a redirect
	// target, and AppID, required with it, keys the ETag cache.
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	AppID   string            `json:"appId,omitempty"`
}

// InstallResult answers an install.
type InstallResult struct {
	Envelope
	Device string `json:"device"`
	// AppID is the installed app's bundle id or package name; empty when
	// it cannot be read (an .apk on a host without aapt2).
	AppID string `json:"appId,omitempty"`
	// Cached, for an install from a URL, is true when the build's ETag is
	// the one last installed on the device, so nothing was downloaded or
	// installed; ETag is the build's.
	Cached bool   `json:"cached,omitempty"`
	ETag   string `json:"etag,omitempty"`
}

// appPlatform infers the platform an app file is for.
func appPlatform(path string) string {
	switch strings.ToLower(filepath.Ext(strings.TrimRight(path, "/"))) {
	case ".apk", ".aab", ".apks":
		return "android"
	case ".app", ".ipa":
		return "ios"
	}
	return ""
}

// devicePlatform is the device's platform as the registry knows it.
func (s *Server) devicePlatform(id string) string {
	if a := s.getAttached(id); a != nil && a.info.Platform != "" {
		return a.info.Platform
	}
	if info, ok := s.registry.Lookup(id); ok {
		return info.Platform
	}
	return ""
}

func (s *Server) handleInstallApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req InstallRequest
	if e := DecodeBody(r, &req); e != nil {
		WriteError(w, e)
		return
	}
	var fetched *fetchedApp
	switch {
	case req.URL != "" && req.App != "":
		WriteError(w, Errorf(CodeUsage, "install: give app or url, not both").WithDetails("field", "url"))
		return
	case req.URL != "":
		f, e := s.fetchApp(r.Context(), id, req)
		if e != nil {
			if e.Code == CodeForbidden {
				s.LogForbidden(r, e)
			}
			WriteError(w, e)
			return
		}
		if f.cached {
			logger.Info("Install of %s on %s skipped: ETag %s is installed", req.AppID, id, f.etag)
			WriteJSON(w, http.StatusOK, InstallResult{Envelope: Envelope{OK: true}, Device: id, AppID: req.AppID, Cached: true, ETag: f.etag})
			return
		}
		defer os.RemoveAll(f.dir)
		fetched = f
		req.App = f.path
	case req.App == "":
		WriteError(w, Errorf(CodeUsage, "install: app is required (a blob: ref, or a path for local callers), or url"))
		return
	}
	path := req.App
	if fetched != nil {
		// The daemon's own download: neither a blob nor the caller's path.
	} else if strings.HasPrefix(path, "blob:") {
		if _, _, err := executor.ParseBlobRef(path); err != nil {
			e := Forbidden("app %s: %v", path, err).WithDetails("field", "app")
			s.LogForbidden(r, e)
			WriteError(w, e)
			return
		}
		_ = os.MkdirAll(s.cfg.ReportsDir, 0o755)
		dir, err := os.MkdirTemp(s.cfg.ReportsDir, ".install-")
		if err != nil {
			WriteError(w, WrapErr(CodeInternal, err))
			return
		}
		defer os.RemoveAll(dir)
		local, err := s.blobs.Materialize(path, dir)
		if err != nil {
			WriteError(w, Errorf(CodeUsage, "app %s: %v", path, err).WithDetails("field", "app"))
			return
		}
		path = local
	} else if CallerFrom(r.Context()).Remote {
		e := Forbidden("app %s is a path on the daemon's host; a remote caller must upload it (a blob: ref)", path).WithDetails("field", "app")
		s.LogForbidden(r, e)
		WriteError(w, e)
		return
	}
	if rd := s.remote(id); rd != nil {
		// A cloud device: the provider installs it (remote.go), before and
		// instead of the reinstall's uninstall, which it has no use for.
		// A download from a URL is not cached for it: each session is a
		// new device.
		s.handleInstallCloud(w, r, rd, path)
		return
	}
	if s.deps.InstallApp == nil {
		WriteError(w, Errorf(CodeInternal, "install: no installer configured"))
		return
	}
	platform := strings.ToLower(req.Platform)
	if platform == "" {
		platform = appPlatform(path)
	}
	if platform == "" {
		platform = s.devicePlatform(id)
	}
	appID := req.AppID
	if appID == "" && s.deps.AppID != nil {
		if v, err := s.deps.AppID(path); err == nil {
			appID = v
		} else {
			logger.Info("install %s: app id not read: %v", filepath.Base(path), err)
		}
	}
	s.idle.Hold()
	defer s.idle.Release()
	if req.Reinstall && appID != "" && s.deps.UninstallApp != nil {
		if e := s.verifyBinding(r.Context(), id); e != nil {
			WriteError(w, e)
			return
		}
		if err := s.deps.UninstallApp(r.Context(), id, platform, appID); err != nil {
			logger.Info("reinstall %s on %s: uninstall first failed (continuing): %v", appID, id, err)
		}
	}
	if e := s.verifyBinding(r.Context(), id); e != nil {
		WriteError(w, e)
		return
	}
	logger.Info("Installing %s (%s) on %s for %s", filepath.Base(path), appID, id, DescribeCaller(CallerFrom(r.Context())))
	if err := s.deps.InstallApp(r.Context(), id, platform, path); err != nil {
		WriteError(w, WrapErr(CodeDeviceError, err))
		return
	}
	res := InstallResult{Envelope: Envelope{OK: true}, Device: id, AppID: appID}
	if fetched != nil {
		s.noteInstalled(id, req.AppID, fetched.etag)
		keepBuild(id, req.AppID, fetched.path)
		res.ETag = fetched.etag
	}
	s.events.Publish(Event{Type: "app.installed", Device: id, Message: appID})
	WriteJSON(w, http.StatusOK, res)
}

func (s *Server) handleUninstallApp(w http.ResponseWriter, r *http.Request) {
	id, appID := r.PathValue("id"), r.PathValue("appId")
	if appID == "" {
		WriteError(w, Errorf(CodeUsage, "uninstall: app id is required"))
		return
	}
	if s.remote(id) != nil {
		WriteError(w, Errorf(CodeUsage, "Device Farm has no uninstall during a session; release the device"))
		return
	}
	if s.deps.UninstallApp == nil {
		WriteError(w, Errorf(CodeInternal, "uninstall: no uninstaller configured"))
		return
	}
	platform := strings.ToLower(r.URL.Query().Get("platform"))
	if platform == "" {
		platform = s.devicePlatform(id)
	}
	if e := s.verifyBinding(r.Context(), id); e != nil {
		WriteError(w, e)
		return
	}
	if err := s.deps.UninstallApp(r.Context(), id, platform, appID); err != nil {
		WriteError(w, WrapErr(CodeDeviceError, err))
		return
	}
	s.forgetInstall(id, appID)
	s.events.Publish(Event{Type: "app.uninstalled", Device: id, Message: appID})
	WriteJSON(w, http.StatusOK, InstallResult{Envelope: Envelope{OK: true}, Device: id, AppID: appID})
}
