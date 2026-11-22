// Package cloudprov is the contract between the daemon and a cloud device
// provider (AWS Device Farm today, BrowserStack later).
//
// A provider creates, inspects and ends the provider-side session for a real
// device in its cloud. The daemon holds the Appium driver for that session and
// owns everything every provider shares: keepalive, idle release, attach,
// records, lease binding and release.
//
// The package imports only the standard library, so both pkg/daemon and a
// provider package import it without importing each other.
package cloudprov

import (
	"context"
	"time"
)

// RemoteProvider is a cloud device source. The daemon holds the Appium driver;
// the provider creates, inspects and ends the provider-side session.
//
// Every method returns its failures as *Error, already classified and
// redacted.
type RemoteProvider interface {
	// Name is the provider's name, as clients write it ("devicefarm").
	Name() string
	// IDPrefix starts the ids of the devices it launches ("df").
	IDPrefix() string
	// Catalog lists the models the provider can launch; platform is
	// "android", "ios" or "" for both.
	Catalog(ctx context.Context, platform string) ([]CloudModel, error)
	// Allocate resolves the apps, picks a model, creates the provider
	// session and waits until it runs. Every session it creates is reported
	// through h.Created before its first wait, and every session it stops
	// through h.Abandoned.
	Allocate(ctx context.Context, spec CloudSpec, h AllocateHooks) (*Allocation, error)
	// Install installs an uploaded app into a running session.
	Install(ctx context.Context, a *Allocation, app AppRef) (*InstallResult, error)
	// Refresh reads the provider's state of a session.
	Refresh(ctx context.Context, a *Allocation) (state, reason string, err error)
	// Release ends a session and reports the minutes it used.
	Release(ctx context.Context, a *Allocation) (*ReleaseResult, error)
	// ReleaseRef ends a session by its reference alone (orphans and
	// start-up self-release) and reports its minutes as Release does (nil
	// when the provider no longer knows the session). A session that
	// already ended is released.
	ReleaseRef(ctx context.Context, ref string) (*DeviceMinutes, error)
	// Upload uploads an app from the daemon's disk, reusing an identical
	// earlier upload.
	Upload(ctx context.Context, localPath string) (*AppRef, error)
	// Apps lists the uploaded apps.
	Apps(ctx context.Context) ([]AppRef, error)
	// Account describes the account: trial minutes and the like.
	Account(ctx context.Context) (map[string]any, error)
	// Sessions lists the provider sessions.
	Sessions(ctx context.Context) ([]SessionSummary, error)
	// Redact removes secrets (endpoint paths, presigned queries, key ids)
	// from s. Every daemon log line that may hold a URL, caps or a provider
	// error goes through it.
	Redact(s string) string
}

// AllocateHooks lets the daemon record every provider session the moment it
// exists, before any wait.
type AllocateHooks struct {
	// DeviceID is the id the daemon gives the device; the provider names
	// the session with it.
	DeviceID string
	// Progress reports session states (PENDING_DEVICE, PENDING_CONCURRENCY,
	// RUNNING …).
	Progress func(state string)
	// Created is called after each session is created and before its first
	// status poll. An error makes Allocate stop that session and fail.
	Created func(ref string) error
	// Abandoned is called after Allocate stops a session it created: a
	// candidate given up, a failure or a cancellation.
	Abandoned func(ref string)
}

// CloudSpec is what a launch asks for, from `device create` or a cloud lease.
type CloudSpec struct {
	Provider       string         `json:"provider"`                // "devicefarm"
	Platform       string         `json:"platform,omitempty"`      // android | ios
	Name           string         `json:"name,omitempty"`          // model: exact name, catalog id or regexp
	OSVersion      string         `json:"osVersion,omitempty"`     // exact
	OSVersionMin   string         `json:"osVersionMin,omitempty"`  // inclusive
	OSVersionMax   string         `json:"osVersionMax,omitempty"`  // inclusive, at its own precision
	App            string         `json:"app,omitempty"`           // host path or upload ARN
	AuxiliaryApps  []string       `json:"auxiliaryApps,omitempty"` // at most 3
	AppiumVersion  string         `json:"appiumVersion,omitempty"` // "2" | "3"
	Caps           map[string]any `json:"caps,omitempty"`          // merged last into the session caps
	AppID          string         `json:"appId,omitempty"`
	IdleReleaseSec int            `json:"idleReleaseSec,omitempty"` // default 600, 60..9000
	BootTimeoutSec int            `json:"bootTimeoutSec,omitempty"` // default 900
}

// CloudModel is one model a provider can launch.
type CloudModel struct {
	ID           string `json:"id"`  // df:<platform>:<slug>:<os>
	ARN          string `json:"arn"` // the provider's model reference
	Platform     string `json:"platform"`
	Name         string `json:"name"`
	OSVersion    string `json:"osVersion"`
	FormFactor   string `json:"formFactor,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Availability string `json:"availability,omitempty"`
	Available    bool   `json:"available"`
}

// DeviceDesc describes the device behind an allocation; the daemon builds its
// DeviceInfo from it.
type DeviceDesc struct {
	Model        string
	Platform     string
	OSVersion    string
	FormFactor   string
	Manufacturer string
	DashboardURL string // console page for the session; no secret
	AppID        string // the app the session was created with, when known
	// AppiumVersion is the Appium major version the provider runs for the
	// session ("2" or "3").
	AppiumVersion string
}

// Allocation is a provider session the daemon holds. Endpoint and Caps carry
// secrets and are never serialised outside the process.
type Allocation struct {
	Device   DeviceDesc
	Endpoint string         // Appium base URL
	Caps     map[string]any // W3C alwaysMatch for the WebDriver session
	Ref      string         // provider session reference (Device Farm session ARN)
	Started  time.Time
	Expires  time.Time
}

// DeviceMinutes is the device time a session used.
type DeviceMinutes struct {
	Metered float64 `json:"metered"`
	Total   float64 `json:"total"`
	// Estimated marks minutes counted from the session's creation to its
	// release, rounded down, because the provider had not settled its own
	// figure yet. The provider also bills the cleanup after the release
	// (minutes for an iPhone), so its billed figure is higher.
	Estimated bool `json:"estimated,omitempty"`
}

// ReleaseResult is what ending a session reports.
type ReleaseResult struct {
	Minutes *DeviceMinutes
}

// InstallResult is what an install into a session reports.
type InstallResult struct {
	AppRef AppRef
}

// AppRef is an uploaded app.
type AppRef struct {
	Ref      string `json:"appRef"` // the provider's upload reference (ARN)
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Status   string `json:"status,omitempty"`
	Created  string `json:"created,omitempty"`
	Reused   bool   `json:"reused,omitempty"`
	Platform string `json:"platform,omitempty"`
}

// SessionSummary is one provider session in a listing.
type SessionSummary struct {
	Name          string         `json:"name"` // the device id the daemon gave it
	Ref           string         `json:"ref"`
	Status        string         `json:"status"`
	Result        string         `json:"result,omitempty"`
	Model         string         `json:"model,omitempty"` // the device model, e.g. "Google Pixel 8"
	Created       string         `json:"created,omitempty"`
	DeviceMinutes *DeviceMinutes `json:"deviceMinutes,omitempty"`
}
