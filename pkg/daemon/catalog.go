package daemon

// catalog.go: device management — what can be created (GET /v1/catalog),
// creating a simulator or AVD (POST /v1/devices with create) and deleting
// one (DELETE /v1/devices/{id}?delete=true). The platform tools are
// reached through the Deps hooks; created.json records what was created.

import (
	"context"
	"net/http"
	"runtime"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/emulator"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

// hostArch picks the default Android image's ABI (a seam for tests).
var hostArch = runtime.GOARCH

// shutdownForDelete stops a running device before it is deleted (a seam
// for tests).
var shutdownForDelete = shutdownDevice

// CatalogResult is the body of GET /v1/catalog.
type CatalogResult struct {
	Envelope
	IOS     *simulator.Catalog `json:"ios,omitempty"`
	Android *emulator.Catalog  `json:"android,omitempty"`
	// Errors names, per platform, why its catalog is missing when no
	// platform was asked for (a Linux host has no simctl, a Mac may have
	// no Android SDK).
	Errors map[string]string `json:"errors,omitempty"`
}

// Catalog lists the iOS runtimes and device types, and the Android system
// images and hardware profiles. With available, Android also lists images
// that sdkmanager can download.
func (r *Registry) Catalog(platform string, available bool) (CatalogResult, *Error) {
	platform = strings.ToLower(platform)
	res := CatalogResult{Envelope: Envelope{OK: true}}
	var want []string
	switch platform {
	case "":
		want = []string{"ios", "android"}
	case "ios", "android":
		want = []string{platform}
	default:
		return res, Errorf(CodeUsage, "device catalog: platform must be ios or android (got %q)", platform)
	}
	for _, p := range want {
		var err error
		if p == "ios" {
			if r.deps.IOSCatalog == nil {
				return res, Errorf(CodeInternal, "device catalog: no iOS catalog configured")
			}
			res.IOS, err = r.deps.IOSCatalog()
		} else {
			if r.deps.AndroidCatalog == nil {
				return res, Errorf(CodeInternal, "device catalog: no Android catalog configured")
			}
			res.Android, err = r.deps.AndroidCatalog(available)
		}
		if err == nil {
			continue
		}
		if platform != "" {
			return res, WrapErr(CodeDeviceError, err)
		}
		if res.Errors == nil {
			res.Errors = map[string]string{}
		}
		res.Errors[p] = err.Error()
	}
	return res, nil
}

// Create makes a simulator or AVD as req.Create describes, records it in
// created.json, and boots it unless req.Boot is false.
func (r *Registry) Create(ctx context.Context, req BootRequest) (DeviceInfo, *Error) {
	spec := *req.Create
	if spec.Name == "" {
		spec.Name = req.Name
	}
	if strings.TrimSpace(spec.Name) == "" {
		return DeviceInfo{}, Errorf(CodeUsage, "device create: --name is required")
	}
	platform := strings.ToLower(req.Platform)
	var info DeviceInfo
	var e *Error
	switch platform {
	case "ios":
		info, e = r.createSimulator(spec)
	case "android":
		info, e = r.createAVD(spec)
	default:
		return DeviceInfo{}, Errorf(CodeUsage, "device create: --platform ios|android is required (got %q)", req.Platform)
	}
	if e != nil {
		return DeviceInfo{}, e
	}
	if req.Boot != nil && !*req.Boot {
		return info, nil
	}
	booted, err := r.Boot(ctx, BootRequest{Platform: platform, Name: info.ID, BootTimeout: req.BootTimeout})
	if err != nil {
		de := WrapErr(CodeDeviceError, err)
		de.Message = spec.Name + " was created as " + info.ID + " but did not boot: " + de.Message
		return DeviceInfo{}, de.WithDetails("created", info.ID)
	}
	booted.CreatedBy = CreatedByMaestroD
	return booted, nil
}

// refuseFlags fails USAGE for create fields of the other platform.
func refuseFlags(platform string, set map[string]bool) *Error {
	for _, f := range []string{"type", "runtime", "image", "profile", "data-size", "install-image", "accept-licenses"} {
		if set[f] {
			return Errorf(CodeUsage, "device create: --%s does not apply to %s", f, platform)
		}
	}
	return nil
}

func (r *Registry) createSimulator(spec CreateSpec) (DeviceInfo, *Error) {
	if e := refuseFlags("ios", map[string]bool{"image": spec.Image != "", "profile": spec.Profile != "",
		"data-size": spec.DataSize != "", "install-image": spec.InstallImage, "accept-licenses": spec.AcceptLicenses}); e != nil {
		return DeviceInfo{}, e
	}
	for _, d := range r.Live("ios") {
		if d.Kind == "simulator" && strings.EqualFold(d.Name, spec.Name) {
			return DeviceInfo{}, Errorf(CodeUsage, "device create: a simulator named %q already exists (%s)", spec.Name, d.ID).
				WithDetails("existing", d.ID)
		}
	}
	if r.deps.IOSCatalog == nil || r.deps.CreateSimulator == nil {
		return DeviceInfo{}, Errorf(CodeInternal, "device create: simulator creation is not configured")
	}
	cat, err := r.deps.IOSCatalog()
	if err != nil {
		return DeviceInfo{}, WrapErr(CodeDeviceError, err)
	}
	var rt *simulator.CatalogRuntime
	if spec.Runtime == "" {
		rt, err = cat.DefaultRuntime()
	} else {
		rt, err = cat.ResolveRuntime(spec.Runtime)
	}
	if err != nil {
		return DeviceInfo{}, Errorf(CodeUsage, "device create: %v", err)
	}
	var dt *simulator.CatalogDeviceType
	if spec.Type == "" {
		dt, err = cat.DefaultDeviceType(rt)
	} else {
		dt, err = cat.ResolveDeviceType(spec.Type, rt)
	}
	if err != nil {
		return DeviceInfo{}, Errorf(CodeUsage, "device create: %v", err)
	}
	udid, err := r.deps.CreateSimulator(spec.Name, dt.ID, rt.ID)
	if err != nil {
		return DeviceInfo{}, WrapErr(CodeDeviceError, err)
	}
	if err := recordCreated(CreatedRecord{Platform: "ios", ID: udid, Name: spec.Name, Daemon: r.name}); err != nil {
		logger.Warn("created.json: %v", err)
	}
	return DeviceInfo{ID: udid, Platform: "ios", Kind: "simulator", Name: spec.Name, OSVersion: rt.Version,
		State: "Shutdown", CreatedBy: CreatedByMaestroD}, nil
}

func (r *Registry) createAVD(spec CreateSpec) (DeviceInfo, *Error) {
	if e := refuseFlags("android", map[string]bool{"type": spec.Type != "", "runtime": spec.Runtime != ""}); e != nil {
		return DeviceInfo{}, e
	}
	if !emulator.ValidAVDName(spec.Name) {
		return DeviceInfo{}, Errorf(CodeUsage, "device create: invalid AVD name %q: use letters, digits, '.', '_' or '-'", spec.Name)
	}
	size := spec.DataSize
	if size == "" {
		size = emulator.DefaultDataSize
	}
	_, dataSize, err := emulator.ParseDataSize(size)
	if err != nil {
		return DeviceInfo{}, Errorf(CodeUsage, "device create: --data-size: %v", err)
	}
	if spec.Image != "" && !strings.HasPrefix(spec.Image, "system-images;") {
		return DeviceInfo{}, Errorf(CodeUsage, "device create: --image must be an sdkmanager package path like %q", "system-images;android-35;google_apis;"+emulator.HostABI(hostArch))
	}
	for _, d := range r.Live("android") {
		if d.ID == AVDPrefix+spec.Name || (emulator.IsEmulator(d.ID) && d.Name == spec.Name) {
			return DeviceInfo{}, Errorf(CodeUsage, "device create: an AVD named %q already exists", spec.Name).WithDetails("existing", d.ID)
		}
	}
	if r.deps.AndroidCatalog == nil || r.deps.CreateAVD == nil {
		return DeviceInfo{}, Errorf(CodeInternal, "device create: AVD creation is not configured")
	}
	cat, err := r.deps.AndroidCatalog(false)
	if err != nil {
		return DeviceInfo{}, WrapErr(CodeDeviceError, err)
	}
	image := spec.Image
	if image == "" {
		img, ok := cat.DefaultImage(hostArch)
		if !ok {
			want := "system-images;android-35;" + emulator.DefaultImageTag + ";" + emulator.HostABI(hostArch)
			return DeviceInfo{}, Errorf(CodeUsage, "device create: no %s system image for %s is installed; install one with `sdkmanager --install %q`, or pass --image",
				emulator.DefaultImageTag, emulator.HostABI(hostArch), want)
		}
		image = img.ID
	}
	if img, ok := cat.Image(image); !ok || !img.Installed {
		switch {
		case !spec.InstallImage:
			return DeviceInfo{}, Errorf(CodeUsage, "device create: system image %s is not installed; install it with `sdkmanager --install %q`, or pass --install-image --accept-licenses",
				image, image).WithDetails("image", image)
		case !spec.AcceptLicenses:
			return DeviceInfo{}, Errorf(CodeUsage, "device create: --install-image needs --accept-licenses: installing %s accepts the Android SDK licenses for it", image)
		case r.deps.InstallSystemImage == nil:
			return DeviceInfo{}, Errorf(CodeInternal, "device create: system image installation is not configured")
		}
		if err := r.deps.InstallSystemImage(image); err != nil {
			return DeviceInfo{}, WrapErr(CodeDeviceError, err)
		}
	}
	profile := spec.Profile
	if profile == "" {
		profile = emulator.DefaultProfile
	}
	if len(cat.Profiles) > 0 && !cat.HasProfile(profile) {
		return DeviceInfo{}, Errorf(CodeUsage, "device create: no hardware profile %q; see `avdmanager list device -c`", profile)
	}
	if err := r.deps.CreateAVD(emulator.AVDSpec{Name: spec.Name, Image: image, Profile: profile, DataSize: dataSize}); err != nil {
		return DeviceInfo{}, WrapErr(CodeDeviceError, err)
	}
	if err := recordCreated(CreatedRecord{Platform: "android", ID: spec.Name, Name: spec.Name, Daemon: r.name}); err != nil {
		logger.Warn("created.json: %v", err)
	}
	return DeviceInfo{ID: AVDPrefix + spec.Name, Platform: "android", Kind: "emulator", Name: spec.Name,
		State: "Shutdown", CreatedBy: CreatedByMaestroD}, nil
}

// resolveManaged finds a simulator or emulator by id, by avd:<name>, or by
// name.
func (r *Registry) resolveManaged(id string) (DeviceInfo, *Error) {
	list := r.List("")
	for _, d := range list {
		if d.ID == id || d.ID == AVDPrefix+id {
			return d, nil
		}
	}
	var matches []DeviceInfo
	for _, d := range list {
		if d.Name != "" && strings.EqualFold(d.Name, id) && (d.Kind == "simulator" || d.Kind == "emulator") {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return DeviceInfo{}, Errorf(CodeDeviceNotFound, "device %s not found", id)
	case 1:
		return matches[0], nil
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m.ID)
	}
	return DeviceInfo{}, Errorf(CodeUsage, "%q names %d devices (%s); pass an id", id, len(matches), strings.Join(ids, ", ")).
		WithDetails("candidates", ids)
}

// Delete deletes a simulator or AVD. One that `device create` did not make
// is refused with DEVICE_EXTERNAL unless force. Once the checks pass,
// detach (when set) closes any driver on it, and a running one is shut
// down before it is deleted.
func (r *Registry) Delete(id string, force bool, detach func(id string)) (DeviceInfo, *Error) {
	info, e := r.resolveManaged(id)
	if e != nil {
		return DeviceInfo{}, e
	}
	key := createdKey(info)
	if key == "" {
		if info.Kind == "emulator" {
			return DeviceInfo{}, Errorf(CodeDeviceError, "cannot read the AVD name of %s; delete it by name", info.ID)
		}
		return DeviceInfo{}, Errorf(CodeUsage, "%s is a physical device and cannot be deleted", info.ID)
	}
	if info.CreatedBy != CreatedByMaestroD && !force {
		return DeviceInfo{}, Errorf(CodeDeviceExternal, "%s was not created by maestro-d; use --force to delete it", id).
			WithDetails("device", info.ID)
	}
	var del func(string) error
	if info.Platform == "ios" {
		del = r.deps.DeleteSimulator
	} else {
		del = r.deps.DeleteAVD
	}
	if del == nil {
		return DeviceInfo{}, Errorf(CodeInternal, "device delete: deleting %s devices is not configured", info.Platform)
	}
	if detach != nil {
		detach(info.ID)
	}
	running := !strings.HasPrefix(info.ID, AVDPrefix) && info.State != "Shutdown"
	if running {
		logger.Info("device delete: shutting down %s first", info.ID)
		if err := shutdownForDelete(info); err != nil {
			return DeviceInfo{}, WrapErr(CodeDeviceError, err)
		}
	}
	if err := del(key); err != nil {
		return DeviceInfo{}, WrapErr(CodeDeviceError, err)
	}
	if err := forgetCreated(info.Platform, key); err != nil {
		logger.Warn("created.json: %v", err)
	}
	r.mu.Lock()
	delete(r.records, info.ID)
	r.persistLocked()
	r.mu.Unlock()
	return info, nil
}

// ---------------------------------------------------------------------------
// Handlers

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, e := s.registry.Catalog(q.Get("platform"), q.Get("available") == "true")
	if e != nil {
		writeError(w, e)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCreateDevice(w http.ResponseWriter, r *http.Request, req BootRequest) {
	s.idle.Hold()
	defer s.idle.Release()
	info, e := s.registry.Create(r.Context(), req)
	if e != nil {
		writeError(w, e)
		return
	}
	s.events.Publish(Event{Type: "device.created", Device: info.ID, Message: info.Name})
	if info.Ready {
		s.events.Publish(Event{Type: "device.booted", Device: info.ID, Message: info.Name})
	}
	writeJSON(w, http.StatusOK, DeviceResult{Envelope: Envelope{OK: true}, Device: info})
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, id string, force bool) {
	info, e := s.registry.Delete(id, force, s.detachForStop)
	if e != nil {
		writeError(w, e)
		return
	}
	s.events.Publish(Event{Type: "device.deleted", Device: info.ID, Message: info.Name})
	writeJSON(w, http.StatusOK, DeviceResult{Envelope: Envelope{OK: true}, Device: info})
}
