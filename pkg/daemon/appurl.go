package daemon

// appurl.go: installApp from a URL. The daemon that owns the device
// downloads the build itself, with the caller's headers on that request
// only: a redirect (Mason's to its Space) is followed without them. The
// ETag of the build last installed on each device, per app id, is kept in
// <home>/installs.json and the build itself in <home>/builds (appbuild.go),
// so a second install of the same build is answered cached without a
// download, and clearState can reinstall it.
//
// The hosts a build may come from are config.json's installApp.allowHosts:
// with that block, the first request and every redirect must go to one of
// them, except a redirect to a DigitalOcean Space (Mason's signed one).
// Without it a local caller may name any URL, and a remote caller none, so
// a client of a hub cannot make it fetch an arbitrary URL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// installsFileName is the ETag record's name in the daemon's home.
const installsFileName = "installs.json"

// appHTTPClient downloads builds; a variable so tests can trust their
// TLS servers.
var appHTTPClient = &http.Client{}

// installsMu serializes the ETag record's read-modify-write.
var installsMu sync.Mutex

// fetchedApp is a build fetchApp downloaded, or found already installed.
type fetchedApp struct {
	dir, path string
	etag      string
	cached    bool
}

// redactedURL is u without its query and fragment, which may sign it, for
// messages and the log.
func redactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

// fetchApp downloads req.URL for device id into a new directory under the
// reports dir, unless the ETag it answers is the one last installed there
// (and req.Reinstall is not set).
func (s *Server) fetchApp(ctx context.Context, id string, req InstallRequest) (*fetchedApp, *Error) {
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, Errorf(CodeUsage, "install: url %s is not an absolute http or https URL", redactedURL(req.URL)).WithDetails("field", "url")
	}
	if strings.TrimSpace(req.AppID) == "" {
		return nil, Errorf(CodeUsage, "install: appId is required with url").WithDetails("field", "appId")
	}
	allow, e := s.installAllowHosts(ctx, u)
	if e != nil {
		return nil, e
	}
	// A build counts as installed only while it is kept, so clearState
	// can reinstall it.
	known := ""
	if !req.Reinstall && keptBuild(id, req.AppID) != "" {
		known = s.installedETag(id, req.AppID)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, Errorf(CodeUsage, "install: url %s: %v", redactedURL(req.URL), err).WithDetails("field", "url")
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	if known != "" {
		hreq.Header.Set("If-None-Match", known)
	}
	client := *appHTTPClient
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if allow != nil && !allow.AllowsRedirect(next.URL.Scheme, next.URL.Hostname()) {
			return &refusedRedirect{host: next.URL.Hostname()}
		}
		// The caller's headers (a bearer key) are for the URL it named,
		// never for where that sends the download.
		next.Header.Del("Authorization")
		for k := range req.Headers {
			next.Header.Del(k)
		}
		return nil
	}
	logger.Info("Downloading %s for %s on %s", redactedURL(req.URL), req.AppID, id)
	resp, err := client.Do(hreq)
	if err != nil {
		// The error quotes the URL it failed on, which may be signed.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		var rr *refusedRedirect
		if errors.As(err, &rr) {
			return nil, Forbidden("install: download %s redirects to %s, which installApp.allowHosts does not list", redactedURL(req.URL), rr.host).WithDetails("field", "url")
		}
		return nil, Errorf(CodeDeviceError, "install: download %s: %v", redactedURL(req.URL), err)
	}
	defer resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified && known != "" {
		return &fetchedApp{etag: known, cached: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, Errorf(CodeUsage, "install: download %s: %s", redactedURL(req.URL), resp.Status).WithDetails("status", resp.StatusCode)
	}
	if etag != "" && etag == known {
		return &fetchedApp{etag: etag, cached: true}, nil
	}
	_ = os.MkdirAll(s.cfg.ReportsDir, 0o755)
	dir, err := os.MkdirTemp(s.cfg.ReportsDir, ".install-")
	if err != nil {
		return nil, WrapErr(CodeInternal, err)
	}
	file := filepath.Join(dir, appFileName(resp, s.devicePlatform(id)))
	if err := writeDownload(file, resp.Body); err != nil {
		os.RemoveAll(dir)
		return nil, Errorf(CodeDeviceError, "install: download %s: %v", redactedURL(req.URL), err)
	}
	return &fetchedApp{dir: dir, path: file, etag: etag}, nil
}

// refusedRedirect stops a download redirected off the allowed hosts.
type refusedRedirect struct{ host string }

func (r *refusedRedirect) Error() string { return "redirect to " + r.host + " is not allowed" }

// installAllowHosts is the allowHosts policy for a download from u: the
// config's installApp block, which u's host must be in, or nil (anything
// goes) for a local caller of a daemon without one. A remote caller of a
// daemon without one is refused.
func (s *Server) installAllowHosts(ctx context.Context, u *url.URL) (*netshape.InstallApp, *Error) {
	cfg, _, err := s.deviceConfig().Load()
	if err != nil {
		return nil, Errorf(CodeInternal, "install: device config: %v", err)
	}
	allow := cfg.InstallApp
	if allow == nil {
		if CallerFrom(ctx).Remote {
			return nil, Forbidden("install: a remote caller may download only from installApp.allowHosts, and this daemon's config.json lists none").WithDetails("field", "url")
		}
		return nil, nil
	}
	if !allow.Allows(u.Hostname()) {
		return nil, Forbidden("install: url host %s is not in installApp.allowHosts", u.Hostname()).WithDetails("field", "url")
	}
	return allow, nil
}

func writeDownload(file string, body io.Reader) error {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// appFileName names a download so its extension tells the installer what
// it is: Content-Disposition's filename, else the last path segment of
// the URL the download came from, else app plus the device platform's
// usual package (.apk, .ipa).
func appFileName(resp *http.Response, platform string) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if name := filepath.Base(params["filename"]); appPlatform(name) != "" {
			return name
		}
	}
	if resp.Request != nil {
		if name := path.Base(resp.Request.URL.Path); appPlatform(name) != "" {
			return name
		}
	}
	switch platform {
	case "ios", "tvos":
		return "app.ipa"
	}
	return "app.apk"
}

// installs is the ETag record: device id → app id → ETag.
type installs map[string]map[string]string

func installsPath() string { return filepath.Join(Home(), installsFileName) }

func readInstalls() installs {
	data, err := os.ReadFile(installsPath())
	if err != nil {
		return installs{}
	}
	var rec installs
	if json.Unmarshal(data, &rec) != nil || rec == nil {
		return installs{}
	}
	return rec
}

// installedETag is the ETag last installed for appID on device id.
func (s *Server) installedETag(id, appID string) string {
	installsMu.Lock()
	defer installsMu.Unlock()
	return readInstalls()[id][appID]
}

// noteInstalled records the ETag just installed for appID on device id; an
// install without one forgets the earlier one.
func (s *Server) noteInstalled(id, appID, etag string) {
	installsMu.Lock()
	defer installsMu.Unlock()
	rec := readInstalls()
	if etag == "" {
		delete(rec[id], appID)
	} else {
		if rec[id] == nil {
			rec[id] = map[string]string{}
		}
		rec[id][appID] = etag
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	tmp := installsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		err = os.Rename(tmp, installsPath())
		if err != nil {
			logger.Warn("install record: %v", err)
		}
	} else {
		logger.Warn("install record: %v", fmt.Errorf("write: %w", err))
	}
}
