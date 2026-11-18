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
}

// InstallResult answers an install.
type InstallResult struct {
	Envelope
	Device string `json:"device"`
	// AppID is the installed app's bundle id or package name; empty when
	// it cannot be read (an .apk on a host without aapt2).
	AppID string `json:"appId,omitempty"`
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
	if e := decodeBody(r, &req); e != nil {
		writeError(w, e)
		return
	}
	if req.App == "" {
		writeError(w, Errorf(CodeUsage, "install: app is required (a blob: ref, or a path for local callers)"))
		return
	}
	if s.deps.InstallApp == nil {
		writeError(w, Errorf(CodeInternal, "install: no installer configured"))
		return
	}
	path := req.App
	if strings.HasPrefix(path, "blob:") {
		if _, _, err := executor.ParseBlobRef(path); err != nil {
			e := forbidden("app %s: %v", path, err).WithDetails("field", "app")
			s.logForbidden(r, e)
			writeError(w, e)
			return
		}
		_ = os.MkdirAll(s.cfg.ReportsDir, 0o755)
		dir, err := os.MkdirTemp(s.cfg.ReportsDir, ".install-")
		if err != nil {
			writeError(w, WrapErr(CodeInternal, err))
			return
		}
		defer os.RemoveAll(dir)
		local, err := s.blobs.Materialize(path, dir)
		if err != nil {
			writeError(w, Errorf(CodeUsage, "app %s: %v", path, err).WithDetails("field", "app"))
			return
		}
		path = local
	} else if CallerFrom(r.Context()).Remote {
		e := forbidden("app %s is a path on the daemon's host; a remote caller must upload it (a blob: ref)", path).WithDetails("field", "app")
		s.logForbidden(r, e)
		writeError(w, e)
		return
	}
	platform := strings.ToLower(req.Platform)
	if platform == "" {
		platform = appPlatform(path)
	}
	if platform == "" {
		platform = s.devicePlatform(id)
	}
	appID := ""
	if s.deps.AppID != nil {
		if v, err := s.deps.AppID(path); err == nil {
			appID = v
		} else {
			logger.Info("install %s: app id not read: %v", filepath.Base(path), err)
		}
	}
	s.idle.Hold()
	defer s.idle.Release()
	if req.Reinstall && appID != "" && s.deps.UninstallApp != nil {
		if err := s.deps.UninstallApp(r.Context(), id, platform, appID); err != nil {
			logger.Info("reinstall %s on %s: uninstall first failed (continuing): %v", appID, id, err)
		}
	}
	logger.Info("Installing %s (%s) on %s for %s", filepath.Base(path), appID, id, describeCaller(CallerFrom(r.Context())))
	if err := s.deps.InstallApp(r.Context(), id, platform, path); err != nil {
		writeError(w, WrapErr(CodeDeviceError, err))
		return
	}
	s.events.Publish(Event{Type: "app.installed", Device: id, Message: appID})
	writeJSON(w, http.StatusOK, InstallResult{Envelope: Envelope{OK: true}, Device: id, AppID: appID})
}

func (s *Server) handleUninstallApp(w http.ResponseWriter, r *http.Request) {
	id, appID := r.PathValue("id"), r.PathValue("appId")
	if appID == "" {
		writeError(w, Errorf(CodeUsage, "uninstall: app id is required"))
		return
	}
	if s.deps.UninstallApp == nil {
		writeError(w, Errorf(CodeInternal, "uninstall: no uninstaller configured"))
		return
	}
	platform := strings.ToLower(r.URL.Query().Get("platform"))
	if platform == "" {
		platform = s.devicePlatform(id)
	}
	if err := s.deps.UninstallApp(r.Context(), id, platform, appID); err != nil {
		writeError(w, WrapErr(CodeDeviceError, err))
		return
	}
	s.events.Publish(Event{Type: "app.uninstalled", Device: id, Message: appID})
	writeJSON(w, http.StatusOK, InstallResult{Envelope: Envelope{OK: true}, Device: id, AppID: appID})
}
