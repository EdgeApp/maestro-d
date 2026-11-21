package devicefarm

import (
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// Provider limits and timing; tests shorten the durations.
var (
	// maxCandidates is how many models Allocate tries.
	maxCandidates = 3
	// defaultBootTimeout bounds the wait for RUNNING when the spec sets none.
	defaultBootTimeout = 900 * time.Second
	// bootTimeoutUnit converts CloudSpec.BootTimeoutSec.
	bootTimeoutUnit = time.Second
	// stopTimeout bounds each StopRemoteAccessSession Allocate makes.
	stopTimeout = 60 * time.Second
	// finalMinutesWait and finalMinutesPoll bound the wait for a stopped
	// session's device minutes.
	finalMinutesWait = 10 * time.Second
	finalMinutesPoll = 2 * time.Second
)

// EnvFastCloud is the test-only seam (shared with the daemon) that shortens
// the waits: session and upload polls every 100 ms, and 1 s in
// PENDING_DEVICE before the next candidate.
const EnvFastCloud = "MAESTRO_D_TEST_FAST_CLOUD"

func init() {
	if os.Getenv(EnvFastCloud) == "1" {
		sessionPollInterval, uploadPollInterval = 100*time.Millisecond, 100*time.Millisecond
		pendingDeviceTimeout = time.Second
		finalMinutesWait, finalMinutesPoll = 2*time.Second, 50*time.Millisecond
	}
}

// SessionLimit is how long Device Farm lets a remote access session run.
const SessionLimit = 150 * time.Minute

// Provider is AWS Device Farm as a cloudprov.RemoteProvider. Every error it
// returns is a *cloudprov.Error with a redacted message.
type Provider struct {
	c *Client
}

var _ cloudprov.RemoteProvider = (*Provider)(nil)

// NewProvider builds a client from opts and wraps it. Missing credentials
// are an Auth error wrapping ErrNoCredentials.
func NewProvider(ctx context.Context, opts Options) (*Provider, error) {
	c, err := New(ctx, opts)
	if err != nil {
		return nil, Classify(err)
	}
	return &Provider{c: c}, nil
}

// NewProviderFromClient wraps an existing client.
func NewProviderFromClient(c *Client) *Provider { return &Provider{c: c} }

// Client returns the provider's client.
func (p *Provider) Client() *Client { return p.c }

// Name is "devicefarm".
func (p *Provider) Name() string { return "devicefarm" }

// IDPrefix is "df".
func (p *Provider) IDPrefix() string { return "df" }

// Redact removes endpoint paths, presigned queries and the loaded
// credentials from s.
func (p *Provider) Redact(s string) string { return p.c.redactor.Redact(s) }

// fail classifies err and redacts its message with the loaded credentials
// too. A nil err stays a nil error.
func (p *Provider) fail(err error) error {
	if err == nil {
		return nil
	}
	e := Classify(err)
	e.Msg = p.Redact(e.Msg)
	return e
}

// Catalog lists the remote-access models of platform ("" for both).
func (p *Provider) Catalog(ctx context.Context, platform string) ([]cloudprov.CloudModel, error) {
	models, err := p.c.Devices(ctx, platform)
	if err != nil {
		return nil, p.fail(err)
	}
	return models, nil
}

// Allocate resolves the apps, picks up to three candidate models and starts
// a session on the first that runs. Every session it creates is reported to
// h.Created before its first poll; every session it gives up is stopped,
// even when ctx is cancelled, and then reported to h.Abandoned.
func (p *Provider) Allocate(ctx context.Context, spec cloudprov.CloudSpec, h cloudprov.AllocateHooks) (*cloudprov.Allocation, error) {
	a, err := p.allocate(ctx, spec, h)
	if err != nil {
		return nil, p.fail(err)
	}
	return a, nil
}

func (p *Provider) allocate(ctx context.Context, spec cloudprov.CloudSpec, h cloudprov.AllocateHooks) (*cloudprov.Allocation, error) {
	platform := strings.ToLower(spec.Platform)
	if platform != "android" && platform != "ios" {
		return nil, cloudprov.Errorf(cloudprov.Other, "a cloud device needs platform android or ios, not %q", spec.Platform)
	}
	if len(spec.AuxiliaryApps) > 3 {
		return nil, cloudprov.Errorf(cloudprov.Other, "Device Farm takes at most 3 auxiliary apps, not %d", len(spec.AuxiliaryApps))
	}
	// Apps first: an upload that fails must not leave a session behind.
	var appARN string
	if spec.App != "" {
		arn, err := p.c.ResolveApp(ctx, spec.App)
		if err != nil {
			return nil, err
		}
		appARN = arn
	}
	var aux []string
	for _, ref := range spec.AuxiliaryApps {
		arn, err := p.c.ResolveApp(ctx, ref)
		if err != nil {
			return nil, err
		}
		aux = append(aux, arn)
	}
	models, err := p.c.Devices(ctx, platform)
	if err != nil {
		return nil, err
	}
	cands, err := Candidates(models, spec)
	if err != nil {
		return nil, err
	}
	if len(cands) > maxCandidates {
		cands = cands[:maxCandidates]
	}
	timeout := defaultBootTimeout
	if spec.BootTimeoutSec > 0 {
		timeout = time.Duration(spec.BootTimeoutSec) * bootTimeoutUnit
	}
	var busy []string
	for _, m := range cands {
		arn, err := p.c.Create(ctx, SessionSpec{
			DeviceARN:     m.ARN,
			Platform:      platform,
			OSVersion:     m.OSVersion,
			AppARN:        appARN,
			AuxiliaryApps: aux,
			AppiumVersion: spec.AppiumVersion,
		}, h.DeviceID)
		if err != nil {
			return nil, err
		}
		if h.Created != nil {
			if herr := h.Created(arn); herr != nil {
				msg := "cannot record the cloud session; it was stopped"
				if serr := p.abandon(ctx, arn, h); serr != nil {
					msg = "cannot record the cloud session, and stopping it failed: " + Redact(serr.Error())
				}
				return nil, &cloudprov.Error{Kind: cloudprov.Other, Msg: msg + " (" + Redact(herr.Error()) + ")", Err: herr}
			}
		}
		s, werr := p.c.Wait(ctx, arn, timeout, h.Progress)
		if werr == nil {
			return p.allocation(platform, m, s, spec), nil
		}
		if serr := p.abandon(ctx, arn, h); serr != nil {
			return nil, &cloudprov.Error{
				Kind: cloudprov.KindOf(werr),
				Msg:  fmt.Sprintf("%s; stopping the session failed (%s), so it stays in the cloud ledger until it is released", Classify(werr).Msg, Classify(serr).Msg),
				Err:  werr,
			}
		}
		if cloudprov.KindOf(werr) == cloudprov.DeviceBusy && ctx.Err() == nil {
			busy = append(busy, m.Name+" "+m.OSVersion)
			continue
		}
		return nil, werr
	}
	return nil, cloudprov.Errorf(cloudprov.DeviceBusy,
		"every matching Device Farm device stayed busy (PENDING_DEVICE for %s each): %s; "+
			"try again later or pick another model", pendingDeviceTimeout, strings.Join(busy, ", "))
}

// abandon stops a session Allocate gives up, on a context of its own so a
// cancelled request still stops it, then reports it to h.Abandoned. A stop
// that fails is returned and the session is not reported abandoned, so its
// record stays for the orphan pass.
func (p *Provider) abandon(ctx context.Context, arn string, h cloudprov.AllocateHooks) error {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	if _, err := p.stopRef(sctx, arn); err != nil {
		return err
	}
	if h.Abandoned != nil {
		h.Abandoned(arn)
	}
	return nil
}

func (p *Provider) allocation(platform string, m cloudprov.CloudModel, s *Session, spec cloudprov.CloudSpec) *cloudprov.Allocation {
	started := s.Started
	if started.IsZero() {
		started = p.c.now()
	}
	return &cloudprov.Allocation{
		Device: cloudprov.DeviceDesc{
			Model:         m.Name,
			Platform:      platform,
			OSVersion:     m.OSVersion,
			FormFactor:    m.FormFactor,
			Manufacturer:  m.Manufacturer,
			DashboardURL:  DashboardURL(s.ARN),
			AppID:         spec.AppID,
			AppiumVersion: appiumVersionOf(spec, platform, m.OSVersion),
		},
		Endpoint: s.Endpoint,
		Caps:     Capabilities(platform, spec.Caps),
		Ref:      s.ARN,
		Started:  started,
		Expires:  started.Add(SessionLimit),
	}
}

// appiumVersionOf is the Appium version a session runs: the one asked for,
// else the platform's (Create picks it the same way).
func appiumVersionOf(spec cloudprov.CloudSpec, platform, osVersion string) string {
	if spec.AppiumVersion != "" {
		return spec.AppiumVersion
	}
	return AppiumVersion(platform, osVersion)
}

// DashboardURL is the AWS console page of a session, built from its ARN
// (arn:aws:devicefarm:us-west-2:<account>:session:<project>/<session>/<n>).
// It holds no secret. An ARN of another shape gives "".
func DashboardURL(sessionARN string) string {
	i := strings.Index(sessionARN, ":session:")
	if i < 0 {
		return ""
	}
	parts := strings.Split(sessionARN[i+len(":session:"):], "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return fmt.Sprintf("https://%s.console.aws.amazon.com/devicefarm/home?region=%s#/mobile/projects/%s/sessions/%s",
		Region, Region, parts[0], parts[1])
}

// stopRef stops a session. One already ended (COMPLETED, STOPPING) or gone
// (NotFound) counts as stopped.
func (p *Provider) stopRef(ctx context.Context, arn string) (*cloudprov.DeviceMinutes, error) {
	m, err := p.c.Stop(ctx, arn)
	if err == nil {
		return p.finalMinutes(ctx, arn, m), nil
	}
	if isCode(err, "NotFoundException") {
		return nil, nil
	}
	s, gerr := p.c.Get(ctx, arn)
	if gerr == nil && (s.Status == StateCompleted || s.Status == StateStopping) {
		return s.Minutes, nil
	}
	if gerr != nil && isCode(gerr, "NotFoundException") {
		return nil, nil
	}
	return nil, err
}

// finalMinutes waits for a stopped session's device minutes: Device Farm
// fills them in only once the session has COMPLETED, a few seconds after
// the stop. It polls for up to finalMinutesWait and returns the last
// minutes it read (m when it read none).
func (p *Provider) finalMinutes(ctx context.Context, arn string, m *cloudprov.DeviceMinutes) *cloudprov.DeviceMinutes {
	deadline := time.Now().Add(finalMinutesWait)
	for (m == nil || m.Total == 0) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return m
		case <-time.After(finalMinutesPoll):
		}
		s, err := p.c.Get(ctx, arn)
		if err != nil {
			continue
		}
		if s.Minutes != nil {
			m = s.Minutes
		}
		if s.Status == StateCompleted && m != nil && m.Total > 0 {
			break
		}
	}
	return m
}

// Install installs an uploaded app (from Upload) into the session. It makes
// no upload lookup.
func (p *Provider) Install(ctx context.Context, a *cloudprov.Allocation, app cloudprov.AppRef) (*cloudprov.InstallResult, error) {
	if a == nil || a.Ref == "" || app.Ref == "" {
		return nil, cloudprov.Errorf(cloudprov.Other, "install needs a session and an uploaded app")
	}
	ref, err := p.c.Install(ctx, a.Ref, app.Ref)
	if err != nil {
		return nil, p.fail(err)
	}
	if ref.Name == "" {
		ref.Name = app.Name
	}
	return &cloudprov.InstallResult{AppRef: *ref}, nil
}

// Refresh reads a session's state; reason explains an ended session.
func (p *Provider) Refresh(ctx context.Context, a *cloudprov.Allocation) (string, string, error) {
	if a == nil || a.Ref == "" {
		return "", "", cloudprov.Errorf(cloudprov.Other, "refresh needs a session")
	}
	s, err := p.c.Get(ctx, a.Ref)
	if err != nil {
		return "", "", p.fail(err)
	}
	var reason string
	if s.Status == StateCompleted || s.Status == StateStopping {
		reason = s.endedReason()
	}
	return s.Status, p.Redact(reason), nil
}

// Release stops a session and reports its device minutes.
func (p *Provider) Release(ctx context.Context, a *cloudprov.Allocation) (*cloudprov.ReleaseResult, error) {
	if a == nil || a.Ref == "" {
		return nil, cloudprov.Errorf(cloudprov.Other, "release needs a session")
	}
	m, err := p.stopRef(ctx, a.Ref)
	if err != nil {
		return nil, p.fail(err)
	}
	if m == nil || m.Total == 0 {
		m = p.estimate(ctx, a.Ref, a.Started)
	}
	return &cloudprov.ReleaseResult{Minutes: m}, nil
}

// estimateMinutes counts the minutes from start to end, rounded down to a
// tenth, marked as an estimate. Device Farm's billed figure adds the
// cleanup after the release (minutes for an iPhone).
func estimateMinutes(start, end time.Time) *cloudprov.DeviceMinutes {
	n := math.Floor(end.Sub(start).Minutes()*10) / 10
	if n < 0 {
		n = 0
	}
	return &cloudprov.DeviceMinutes{Metered: n, Total: n, Estimated: true}
}

// ReleaseRef stops a session by its ARN; one already ended or gone counts as
// released. Its minutes are Device Farm's once settled, else estimated as
// Release does; nil when Device Farm no longer knows the session.
func (p *Provider) ReleaseRef(ctx context.Context, ref string) (*cloudprov.DeviceMinutes, error) {
	if ref == "" {
		return nil, cloudprov.Errorf(cloudprov.Other, "release needs a session ARN")
	}
	m, err := p.stopRef(ctx, ref)
	if err != nil {
		return nil, p.fail(err)
	}
	if m == nil || m.Total == 0 {
		m = p.estimate(ctx, ref, time.Time{})
	}
	return m, nil
}

// estimate counts a released session's minutes from its creation (the
// setup is billed too) to now (estimateMinutes), until Device Farm settles
// the figure. started is the fallback when the session cannot be read; nil
// when neither is known.
func (p *Provider) estimate(ctx context.Context, arn string, started time.Time) *cloudprov.DeviceMinutes {
	from := started
	if s, err := p.c.Get(ctx, arn); err == nil && !s.Created.IsZero() {
		from = s.Created
	}
	if from.IsZero() {
		return nil
	}
	return estimateMinutes(from, time.Now())
}

// Upload uploads a local .apk or .ipa, reusing an identical upload.
func (p *Provider) Upload(ctx context.Context, localPath string) (*cloudprov.AppRef, error) {
	ref, err := p.c.Upload(ctx, localPath)
	if err != nil {
		return nil, p.fail(err)
	}
	return ref, nil
}

// Apps lists the project's app uploads.
func (p *Provider) Apps(ctx context.Context) ([]cloudprov.AppRef, error) {
	apps, err := p.c.Apps(ctx)
	if err != nil {
		return nil, p.fail(err)
	}
	return apps, nil
}

// Account describes the account (see Client.Account).
func (p *Provider) Account(ctx context.Context) (map[string]any, error) {
	acct, err := p.c.Account(ctx)
	if err != nil {
		return nil, p.fail(err)
	}
	return acct, nil
}

// Sessions lists the project's remote access sessions.
func (p *Provider) Sessions(ctx context.Context) ([]cloudprov.SessionSummary, error) {
	list, err := p.c.List(ctx)
	if err != nil {
		return nil, p.fail(err)
	}
	out := make([]cloudprov.SessionSummary, 0, len(list))
	for _, s := range list {
		sum := cloudprov.SessionSummary{
			Name:          s.Name,
			Ref:           s.ARN,
			Status:        s.Status,
			Result:        s.Result,
			Model:         s.DeviceName,
			DeviceMinutes: s.Minutes,
		}
		if !s.Created.IsZero() {
			sum.Created = s.Created.UTC().Format(time.RFC3339)
		}
		out = append(out, sum)
	}
	return out, nil
}

// availabilityRank orders candidates: HIGHLY_AVAILABLE, AVAILABLE, BUSY,
// then anything else.
func availabilityRank(a string) int {
	switch a {
	case "HIGHLY_AVAILABLE":
		return 0
	case "AVAILABLE":
		return 1
	case "BUSY":
		return 2
	}
	return 3
}

// Candidates filters models by spec (name, exact OS, OS range), drops
// TEMPORARY_NOT_AVAILABLE ones and orders the rest: availability, newest OS,
// name. A name matches the exact model name (any case), the catalog id, or
// a case-insensitive regexp on either; an invalid regexp matches only
// exactly. None is a NotFound error naming up to five of the closest models.
func Candidates(models []cloudprov.CloudModel, spec cloudprov.CloudSpec) ([]cloudprov.CloudModel, error) {
	platform := strings.ToLower(spec.Platform)
	var re *regexp.Regexp
	if spec.Name != "" {
		re, _ = regexp.Compile("(?i)" + spec.Name)
	}
	var pool, out []cloudprov.CloudModel
	for _, m := range models {
		if platform != "" && m.Platform != platform {
			continue
		}
		pool = append(pool, m)
		if m.Availability == "TEMPORARY_NOT_AVAILABLE" {
			continue
		}
		if spec.Name != "" && !strings.EqualFold(m.Name, spec.Name) && m.ID != spec.Name &&
			(re == nil || (!re.MatchString(m.Name) && !re.MatchString(m.ID))) {
			continue
		}
		if !cloudprov.OSVersionMatches(m.OSVersion, spec.OSVersion) {
			continue
		}
		if !cloudprov.VersionInRange(m.OSVersion, spec.OSVersionMin, spec.OSVersionMax) {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, notFound(pool, spec)
	}
	// A name that names models exactly ("Google Pixel 8") picks only those,
	// not the others its pattern also matches ("Google Pixel 8a").
	var exact []cloudprov.CloudModel
	for _, m := range out {
		if strings.EqualFold(m.Name, spec.Name) || m.ID == spec.Name {
			exact = append(exact, m)
		}
	}
	if len(exact) > 0 {
		out = exact
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := availabilityRank(a.Availability), availabilityRank(b.Availability); ra != rb {
			return ra < rb
		}
		if c := compareOS(a.OSVersion, b.OSVersion); c != 0 {
			return c > 0
		}
		return a.Name < b.Name
	})
	return out, nil
}

// compareOS orders two OS versions; one that does not parse sorts first.
func compareOS(a, b string) int {
	x, _ := cloudprov.ParseVersion(a)
	y, _ := cloudprov.ParseVersion(b)
	return cloudprov.CompareVersions(x, y)
}

// notFound describes a candidate search that matched nothing, with up to
// five of the closest models by name.
func notFound(pool []cloudprov.CloudModel, spec cloudprov.CloudSpec) error {
	var want []string
	if spec.Name != "" {
		want = append(want, fmt.Sprintf("name %q", spec.Name))
	}
	if spec.OSVersion != "" {
		want = append(want, "OS "+spec.OSVersion)
	}
	if spec.OSVersionMin != "" {
		want = append(want, "OS >= "+spec.OSVersionMin)
	}
	if spec.OSVersionMax != "" {
		want = append(want, "OS <= "+spec.OSVersionMax)
	}
	what := "any model"
	if len(want) > 0 {
		what = strings.Join(want, ", ")
	}
	if len(pool) == 0 {
		return cloudprov.Errorf(cloudprov.NotFound, "Device Farm lists no remote-access %s devices", spec.Platform)
	}
	target := strings.ToLower(spec.Name)
	type scored struct {
		label string
		d     int
	}
	seen := map[string]bool{}
	var all []scored
	for _, m := range pool {
		label := m.Name + " " + m.OSVersion
		if seen[label] {
			continue
		}
		seen[label] = true
		all = append(all, scored{label, levenshtein(target, strings.ToLower(m.Name))})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].d != all[j].d {
			return all[i].d < all[j].d
		}
		return all[i].label < all[j].label
	})
	if len(all) > 5 {
		all = all[:5]
	}
	names := make([]string, len(all))
	for i, s := range all {
		names[i] = s.label
	}
	return cloudprov.Errorf(cloudprov.NotFound, "no available Device Farm %s model matches %s; closest: %s",
		spec.Platform, what, strings.Join(names, ", "))
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
