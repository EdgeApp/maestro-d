package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

const (
	defaultFindTimeoutMs       = 17000
	defaultActionableTimeoutMs = 2000 // brief window to wait for the actionable gate to pass post-find
	optionalFindTimeoutMs      = 7000
	defaultViewportW           = 1280
	defaultViewportH           = 800
)

// How long a tap's native click may wait for its element to be interactable
// before the tap falls back to a JS click.
const defaultClickTimeout = 5 * time.Second

// finePointerSettings are the Blink settings of a mouse: a fine primary
// pointer and hover (pointer type 4 is fine, hover type 2 is hover).
const finePointerSettings = "primaryPointerType=4,availablePointerTypes=4,primaryHoverType=2,availableHoverTypes=2"

// Config holds browser driver configuration.
type Config struct {
	Headless    bool
	URL         string // Initial URL to navigate to
	ChromeBin   string // Path to Chrome binary (empty = auto-download)
	Browser     string // "chrome", "chromium", or path to binary (default: chromium)
	UserDataDir string // Persistent profile directory (empty = ephemeral profile)
	ViewportW   int
	ViewportH   int
}

// Driver implements core.Driver for desktop browser testing using Rod + CDP.
type Driver struct {
	browser *rod.Browser
	page    *rod.Page
	config  Config

	// launcher started Chrome. When ownsProfile is set, go-rod made the
	// profile under $TMPDIR/rod/user-data, and Close kills Chrome and
	// removes it; a caller's UserDataDir is never touched.
	launcher    profileLauncher
	ownsProfile bool

	findTimeoutMs int
	clipboard     string
	viewportW     int
	viewportH     int

	// Dialog handling
	dialogCh chan *proto.PageJavascriptDialogOpening
	stopCh   chan struct{}

	// Tab management
	tabLabels map[string]*rod.Page // label → page mapping

	// Network interception
	networkMocks  []networkMock // active mock rules
	networkBlocks []string      // blocked URL patterns
	fetchEnabled  bool          // whether Fetch domain is enabled
	networkMu     sync.Mutex    // protects mocks, observation scopes and emulation

	networkScopes    map[proto.TargetSessionID]*networkScope
	networkEmulation map[proto.TargetSessionID]*networkEmulationHold

	// Console capture
	consoleLogs []ConsoleEntry
	consoleMu   sync.Mutex

	// Selector validation dedup
	warnedFields map[string]bool

	closeOnce sync.Once
	closeErr  error
}

// New creates a new browser Driver.
func New(cfg Config) (*Driver, error) {
	if cfg.ViewportW == 0 {
		cfg.ViewportW = defaultViewportW
	}
	if cfg.ViewportH == 0 {
		cfg.ViewportH = defaultViewportH
	}

	// Cache downloaded browsers in ~/.maestro-d/browsers/
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		launcher.DefaultBrowserDir = filepath.Join(homeDir, ".maestro-d", "browsers")
	}

	// Resolve which browser binary to use.
	// Priority: ChromeBin (direct path) > Browser flag > default (chromium/download)
	chromeBin := resolveBrowserBin(cfg)

	// Use a clean profile with password manager disabled
	l := launcher.New().Headless(cfg.Headless).
		Set("no-first-run").
		Set("disable-default-apps").
		Set("disable-popup-blocking").
		Set("disable-translate").
		Set("disable-background-timer-throttling").
		Set("disable-component-update").
		Set("password-store", "basic")
	// Chrome's setuid sandbox relies on user namespaces, which CI runners
	// (e.g. GitHub Actions) restrict — causing the zygote to abort on launch.
	// Disable the sandbox only under CI or when explicitly requested.
	if os.Getenv("CI") != "" || os.Getenv("MAESTRO_NO_SANDBOX") != "" {
		l = l.Set("no-sandbox")
	}
	// Headless Chrome on Linux, with no input device, reports no fine
	// pointer and no hover, so a page takes it for a touch screen: Enter in
	// a composer stops sending and menus open their touch forms. A Mac's
	// headless Chrome reports a mouse; this makes Linux's do the same.
	if cfg.Headless && runtime.GOOS == "linux" {
		l = l.Set("blink-settings", finePointerSettings)
	}
	if cfg.UserDataDir != "" {
		// Persistent profile: cookies / localStorage / extensions survive
		// across runs. Caller is responsible for the directory's lifecycle.
		l = l.Set("user-data-dir", cfg.UserDataDir)
	}
	if chromeBin != "" {
		l = l.Bin(chromeBin)
	} else {
		// Using Rod's bundled Chromium — check if download is needed
		browserDir := launcher.DefaultBrowserDir
		if needsDownload(browserDir) {
			log.Printf("[browser] Downloading Chromium (first time only, subsequent runs will be faster)...")
		}
	}

	// Write Chrome preferences to disable password manager and breach detection
	if dataDir := l.Get("user-data-dir"); dataDir != "" {
		writeChromePref(dataDir)
	}

	ownsProfile := cfg.UserDataDir == ""
	controlURL, err := l.Launch()
	if err != nil {
		// Chrome never started, so there is nothing to kill or wait for.
		if ownsProfile {
			removeProfileDir(l)
		}
		return nil, fmt.Errorf("failed to launch browser: %w", err)
	}

	// Connect with NoDefaultDevice so we control the viewport
	browser := rod.New().ControlURL(controlURL).NoDefaultDevice()
	if err := browser.Connect(); err != nil {
		removeProfile(l, ownsProfile)
		return nil, fmt.Errorf("failed to connect to browser: %w", err)
	}

	// Create a new page
	page, err := browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		_ = browser.Close()
		removeProfile(l, ownsProfile)
		return nil, fmt.Errorf("failed to create page: %w", err)
	}

	// Set viewport
	err = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:  cfg.ViewportW,
		Height: cfg.ViewportH,
	})
	if err != nil {
		_ = browser.Close()
		removeProfile(l, ownsProfile)
		return nil, fmt.Errorf("failed to set viewport: %w", err)
	}

	// Inject JS helper (persists across navigations)
	_, err = page.EvalOnNewDocument(jsHelperCode)
	if err != nil {
		_ = browser.Close()
		removeProfile(l, ownsProfile)
		return nil, fmt.Errorf("failed to inject JS helper: %w", err)
	}

	d := &Driver{
		browser:       browser,
		page:          page,
		config:        cfg,
		launcher:      l,
		ownsProfile:   ownsProfile,
		findTimeoutMs: defaultFindTimeoutMs,
		viewportW:     cfg.ViewportW,
		viewportH:     cfg.ViewportH,
		dialogCh:      make(chan *proto.PageJavascriptDialogOpening, 10),
		stopCh:        make(chan struct{}),
		tabLabels:     make(map[string]*rod.Page),
		warnedFields:  make(map[string]bool),
	}

	// Start background handlers
	d.startDialogHandler()
	d.startConsoleHandler()

	// Navigate to initial URL if provided
	if cfg.URL != "" {
		if err := d.navigateAndWait(page, func() error { return page.Navigate(cfg.URL) }); err != nil {
			_ = d.Close()
			return nil, fmt.Errorf("failed to navigate to %s: %w", cfg.URL, err)
		}
	}

	return d, nil
}

// startDialogHandler starts a background goroutine to capture and auto-dismiss dialog events.
// Native JS dialogs (alert/confirm/prompt) block all CDP communication until handled.
// Uses Rod's EachEvent pattern — the goroutine blocks until the browser closes.
func (d *Driver) startDialogHandler() {
	go d.page.EachEvent(func(e *proto.PageJavascriptDialogOpening) bool {
		// Capture event for explicit acceptAlert/dismissAlert steps
		select {
		case d.dialogCh <- e:
		default:
			// Channel full — drop oldest
			select {
			case <-d.dialogCh:
			default:
			}
			d.dialogCh <- e
		}

		// Auto-accept the dialog to unblock CDP communication.
		// Tests can still use acceptAlert/dismissAlert for explicit handling.
		err := proto.PageHandleJavaScriptDialog{Accept: true, PromptText: ""}.Call(d.page)
		if err != nil {
			log.Printf("[browser] failed to auto-dismiss dialog (%s): %v", e.Type, err)
		}

		return false // keep listening
	})()
}

// writeChromePref writes a Chrome Preferences file that disables password manager
// and breach detection popups. Must be called before Launch().
func writeChromePref(userDataDir string) {
	defaultDir := filepath.Join(userDataDir, "Default")
	if err := os.MkdirAll(defaultDir, 0o755); err != nil {
		return
	}
	prefs := `{
  "credentials_enable_service": false,
  "profile": {
    "password_manager_enabled": false,
    "password_manager_leak_detection": false
  },
  "password_manager": {
    "leak_detection": false
  }
}`
	_ = os.WriteFile(filepath.Join(defaultDir, "Preferences"), []byte(prefs), 0o644)
}

// detectChrome returns the path to an installed Chrome/Chromium binary, or empty string.
func detectChrome() string {
	var paths []string
	switch runtime.GOOS {
	case "darwin":
		paths = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "linux":
		paths = []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
		}
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// resolveBrowserBin determines which browser binary to use based on config.
// Returns empty string to let Rod download/use its bundled Chromium.
func resolveBrowserBin(cfg Config) string {
	// Explicit ChromeBin takes highest priority (programmatic use)
	if cfg.ChromeBin != "" {
		return cfg.ChromeBin
	}

	browser := strings.TrimSpace(cfg.Browser)
	switch strings.ToLower(browser) {
	case "chrome":
		// User explicitly requested installed Chrome
		if path := detectChrome(); path != "" {
			log.Printf("[browser] Using installed Chrome: %s", path)
			return path
		}
		log.Printf("[browser] Chrome not found, falling back to Chromium")
		return "" // fall back to Rod download
	case "", "chromium":
		// Default: use Rod's bundled Chromium (download if needed)
		return ""
	default:
		// Treat as a custom path to a browser binary
		if _, err := os.Stat(browser); err == nil {
			log.Printf("[browser] Using custom browser: %s", browser)
			return browser
		}
		log.Printf("[browser] Browser binary not found at %s, falling back to Chromium", browser)
		return ""
	}
}

// EnsureBrowser ensures the browser binary is available, downloading Chromium
// if needed. Call this once before creating multiple parallel drivers to avoid
// N simultaneous downloads on first run.
func EnsureBrowser(cfg Config) error {
	if resolveBrowserBin(cfg) != "" {
		return nil // using a local binary, no download needed
	}

	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		launcher.DefaultBrowserDir = filepath.Join(homeDir, ".maestro-d", "browsers")
	}

	b := launcher.NewBrowser()
	b.RootDir = launcher.DefaultBrowserDir

	if needsDownload(b.RootDir) {
		log.Printf("[browser] Downloading Chromium (first time only, subsequent runs will be faster)...")
	}

	_, err := b.Get()
	return err
}

// needsDownload checks if the browser cache directory is empty or missing.
func needsDownload(browserDir string) bool {
	entries, err := os.ReadDir(browserDir)
	if err != nil {
		return true // directory doesn't exist
	}
	return len(entries) == 0
}

// Close shuts down the browser and, when go-rod made the profile, removes
// it. Safe to call multiple times.
func (d *Driver) Close() error {
	d.closeOnce.Do(func() {
		close(d.stopCh)
		if d.browser != nil {
			d.closeErr = d.browser.Close()
		}
		removeProfile(d.launcher, d.ownsProfile)
	})
	return d.closeErr
}

// profileLauncher is the part of go-rod's *launcher.Launcher that ends a
// browser and removes its temporary profile.
type profileLauncher interface {
	Kill()
	Cleanup()
	Get(name flags.Flag) string
}

// cleanupWait bounds how long removeProfile waits for go-rod's Cleanup,
// which blocks until Chrome has exited.
var cleanupWait = 10 * time.Second

// removeProfile kills the browser l started and removes the temporary
// profile go-rod made for it. A caller-supplied profile (owns false) is
// left alone.
func removeProfile(l profileLauncher, owns bool) {
	if l == nil || !owns {
		return
	}
	l.Kill()
	done := make(chan struct{})
	go func() {
		l.Cleanup()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(cleanupWait):
	}
	removeProfileDir(l)
}

// removeProfileDir deletes the profile directory go-rod chose for l, if it
// is still there.
func removeProfileDir(l profileLauncher) {
	if dir := l.Get(flags.UserDataDir); dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// networkTracker tracks in-flight network requests via CDP Network domain events.
// Used to detect network idle state after navigations (SPAs with AJAX/fetch).
type networkTracker struct {
	mu       sync.Mutex
	inflight map[proto.NetworkRequestID]struct{}
	lastIdle time.Time
}

func newNetworkTracker() *networkTracker {
	return &networkTracker{
		inflight: make(map[proto.NetworkRequestID]struct{}),
		lastIdle: time.Now(),
	}
}

func (t *networkTracker) onRequest(id proto.NetworkRequestID, url string, resourceType proto.NetworkResourceType) {
	// Skip WebSocket connections (never fire loadingFinished) and data: URLs
	if resourceType == proto.NetworkResourceTypeWebSocket ||
		resourceType == proto.NetworkResourceTypeEventSource ||
		strings.HasPrefix(url, "data:") {
		return
	}
	t.mu.Lock()
	t.inflight[id] = struct{}{}
	t.mu.Unlock()
}

func (t *networkTracker) onComplete(id proto.NetworkRequestID) {
	t.mu.Lock()
	if _, ok := t.inflight[id]; ok {
		delete(t.inflight, id)
		if len(t.inflight) == 0 {
			t.lastIdle = time.Now()
		}
	}
	t.mu.Unlock()
}

// waitForIdle waits until no network requests are in-flight for quietPeriod.
// Returns true if idle was reached, false if timeout expired (proceeds anyway).
func (t *networkTracker) waitForIdle(timeout, quietPeriod time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		idle := len(t.inflight) == 0
		idleSince := t.lastIdle
		t.mu.Unlock()
		if idle && time.Since(idleSince) >= quietPeriod {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// navigateAndWait observes only this navigation, including asynchronous loads
// that keep running after the page's load event and DOM stabilization.
func (d *Driver) navigateAndWait(page *rod.Page, navigate func() error) (err error) {
	tracker := newNetworkTracker()
	observation, err := d.observeNetwork(page, page.GetContext(),
		func(e *proto.NetworkRequestWillBeSent) {
			url := ""
			if e.Request != nil {
				url = e.Request.URL
			}
			tracker.onRequest(e.RequestID, url, e.Type)
		},
		func(e *proto.NetworkLoadingFinished) { tracker.onComplete(e.RequestID) },
		func(e *proto.NetworkLoadingFailed) { tracker.onComplete(e.RequestID) },
	)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, observation.close()) }()
	if err = navigate(); err != nil {
		return err
	}
	d.waitForPageReady(page, tracker)
	return nil
}

// Execute runs a single step and returns the result.
func (d *Driver) Execute(step flow.Step) *core.CommandResult {
	start := time.Now()

	var result *core.CommandResult
	switch s := step.(type) {
	// Tap commands
	case *flow.TapOnStep:
		result = d.tapOn(s)
	case *flow.DoubleTapOnStep:
		result = d.doubleTapOn(s)
	case *flow.LongPressOnStep:
		result = d.longPressOn(s)
	case *flow.TapOnPointStep:
		result = d.tapOnPoint(s)

	// Assert commands
	case *flow.AssertVisibleStep:
		result = d.assertVisible(s)
	case *flow.AssertNotVisibleStep:
		result = d.assertNotVisible(s)

	// Input commands
	case *flow.InputTextStep:
		result = d.inputText(s)
	case *flow.EraseTextStep:
		result = d.eraseText(s)
	case *flow.HideKeyboardStep:
		result = d.hideKeyboard(s)
	case *flow.InputRandomStep:
		result = d.inputRandom(s)

	// Scroll/Swipe commands
	case *flow.ScrollStep:
		result = d.scroll(s)
	case *flow.ScrollUntilVisibleStep:
		result = d.scrollUntilVisible(s)
	case *flow.SwipeStep:
		result = d.swipe(s)
	case *flow.DragAndDropStep:
		result = d.dragAndDrop(s)

	// Navigation commands
	case *flow.BackStep:
		result = d.back(s)
	case *flow.PressKeyStep:
		result = d.pressKey(s)

	// App lifecycle (URL-based for browser)
	case *flow.LaunchAppStep:
		result = d.launchApp(s)
	case *flow.StopAppStep:
		result = d.stopApp(s)
	case *flow.KillAppStep:
		result = d.killApp(s)
	case *flow.ClearStateStep:
		result = d.clearState(s)

	// Clipboard
	case *flow.CopyTextFromStep:
		result = d.copyTextFrom(s)
	case *flow.PasteTextStep:
		result = d.pasteText(s)
	case *flow.SetClipboardStep:
		result = d.setClipboard(s)

	// Device control
	case *flow.SetOrientationStep:
		result = d.setOrientation(s)
	case *flow.OpenLinkStep:
		result = d.openLink(s)
	case *flow.OpenBrowserStep:
		result = d.openBrowser(s)
	case *flow.SetLocationStep:
		result = d.setLocation(s)

	// Wait commands
	case *flow.WaitUntilStep:
		result = d.waitUntil(s)
	case *flow.WaitForAnimationToEndStep:
		result = d.waitForAnimationToEnd(s)

	// Media
	case *flow.TakeScreenshotStep:
		result = d.takeScreenshot(s)
	case *flow.AssertScreenshotStep:
		result = d.takeScreenshot(&flow.TakeScreenshotStep{CropOn: s.CropOn})

	// Alert handling
	case *flow.AcceptAlertStep:
		result = d.acceptAlert(s)
	case *flow.DismissAlertStep:
		result = d.dismissAlert(s)

	// Browser scripting
	case *flow.EvalBrowserScriptStep:
		result = d.evalBrowserScript(s)
	case *flow.RunBrowserScriptStep:
		result = d.runBrowserScript(s)

	// Console capture
	case *flow.GetConsoleLogsStep:
		result = d.getConsoleLogs()
	case *flow.ClearConsoleLogsStep:
		result = d.clearConsoleLogs()
	case *flow.AssertNoJSErrorsStep:
		result = d.assertNoJSErrors()

	// Browser state (cookies, auth)
	case *flow.SetCookiesStep:
		result = d.setCookies(s)
	case *flow.GetCookiesStep:
		result = d.getCookies(s)
	case *flow.SaveAuthStateStep:
		result = d.saveAuthState(s)
	case *flow.LoadAuthStateStep:
		result = d.loadAuthState(s)

	// File & permissions
	case *flow.UploadFileStep:
		result = d.uploadFile(s)
	case *flow.WaitForDownloadStep:
		result = d.waitForDownload(s)
	case *flow.GrantPermissionsStep:
		result = d.grantPermissions(s)
	case *flow.ResetPermissionsStep:
		result = d.resetPermissions()

	// Tab management
	case *flow.OpenTabStep:
		result = d.openTab(s)
	case *flow.SwitchTabStep:
		result = d.switchTab(s)
	case *flow.CloseTabStep:
		result = d.closeTab()

	// Network interception
	case *flow.MockNetworkStep:
		result = d.mockNetwork(s)
	case *flow.BlockNetworkStep:
		result = d.blockNetwork(s)
	case *flow.SetNetworkConditionsStep:
		result = d.setNetworkConditions(s)
	case *flow.WaitForRequestStep:
		result = d.waitForRequest(s)
	case *flow.ClearNetworkMocksStep:
		result = d.clearNetworkMocks()

	// Dark mode is the page's prefers-color-scheme rather than an OS switch —
	// see applyDarkMode.
	case *flow.SetDarkModeStep:
		result = d.applyDarkMode(s.Enabled)
	case *flow.ToggleDarkModeStep:
		result = d.toggleDarkMode()
	case *flow.AssertDarkModeStep:
		result = d.assertDarkModeIs(true)
	case *flow.AssertLightModeStep:
		result = d.assertDarkModeIs(false)

	// Unsupported — mobile-only or not applicable to web
	case *flow.SetAirplaneModeStep, *flow.ToggleAirplaneModeStep:
		result = unsupportedResult("airplane mode is not supported on web platform")
	case *flow.TravelStep:
		result = unsupportedResult("travel is not supported on web platform")
	case *flow.AddMediaStep:
		result = unsupportedResult("addMedia is not supported on web platform")
	case *flow.StartRecordingStep:
		result = unsupportedResult("startRecording is not supported on web platform")
	case *flow.StopRecordingStep:
		result = unsupportedResult("stopRecording is not supported on web platform")
	case *flow.ClearKeychainStep:
		result = unsupportedResult("clearKeychain is not supported on web platform")
	case *flow.SetPermissionsStep:
		result = unsupportedResult("setPermissions is not supported on web platform")
	// assertWithAI and assertNoDefectsWithAI never reach a driver: the
	// executor runs them on the driver's screenshot (maestro-d, aicheck.go).
	case *flow.ExtractTextWithAIStep:
		result = unsupportedResult("extractTextWithAI is not supported on web platform")

	default:
		result = &core.CommandResult{
			Success: false,
			Error:   fmt.Errorf("unknown step type: %T", step),
			Message: fmt.Sprintf("Step type '%T' is not supported on web platform", step),
		}
	}

	result.Duration = time.Since(start)
	return result
}

// Screenshot captures the current page as PNG.
func (d *Driver) Screenshot() ([]byte, error) {
	return d.page.Screenshot(true, nil)
}

// LocateElement finds sel's element with its bounds where a screenshot shows
// it, which mask needs: the visibility check's fast path reports no element.
// A web screenshot is the whole page, laid out at its content size, so the
// bounds are measured in that layout, in CSS pixels from the page's top-left,
// the space ScreenshotSize reports.
func (d *Driver) LocateElement(sel flow.Selector) (*core.ElementInfo, error) {
	elem, info, err := d.findElement(sel, isOptional(sel.Optional), 0)
	if err != nil {
		return nil, err
	}
	bounds, err := d.screenshotBounds(elem)
	if err != nil {
		return nil, err
	}
	info.Bounds = bounds
	return info, nil
}

// ScreenshotSize is the size, in CSS pixels, of the page a screenshot covers:
// its content size, the size Rod's full-page capture lays it out at.
func (d *Driver) ScreenshotSize() (int, int, error) {
	metrics, err := proto.PageGetLayoutMetrics{}.Call(d.page)
	if err != nil {
		return 0, 0, err
	}
	if metrics.CSSContentSize == nil {
		return 0, 0, errors.New("failed to get css content size")
	}
	return int(metrics.CSSContentSize.Width), int(metrics.CSSContentSize.Height), nil
}

// screenshotBounds measures elem in the layout a screenshot is taken in: the
// viewport set to the page's content size, as Rod's full-page capture sets it,
// so the page is not scrolled and fixed elements sit where the capture shows
// them. An element in an iframe is placed in the top frame. The viewport is
// restored afterwards, as the capture restores it.
func (d *Driver) screenshotBounds(elem *rod.Element) (core.Bounds, error) {
	width, height, err := d.ScreenshotSize()
	if err != nil {
		return core.Bounds{}, err
	}
	saved := proto.EmulationSetDeviceMetricsOverride{}
	hasSaved := d.page.LoadState(&saved)
	whole := saved
	whole.Width, whole.Height = width, height
	if err := d.page.SetViewport(&whole); err != nil {
		return core.Bounds{}, err
	}
	defer func() {
		if !hasSaved {
			_ = proto.EmulationClearDeviceMetricsOverride{}.Call(d.page)
			return
		}
		_ = d.page.SetViewport(&saved)
	}()

	res, err := elem.Eval(`() => {
		var r = this.getBoundingClientRect();
		var c = window.__maestro.topFrameClickPoint(this);
		return [c.x - r.width / 2, c.y - r.height / 2, r.width, r.height];
	}`)
	if err != nil {
		return core.Bounds{}, err
	}
	box := res.Value.Arr()
	if len(box) != 4 {
		return core.Bounds{}, fmt.Errorf("element bounds: got %d values", len(box))
	}
	// Whole pixels that cover the element, so a fractional edge is masked too.
	left, top := math.Floor(box[0].Num()), math.Floor(box[1].Num())
	right, bottom := math.Ceil(box[0].Num()+box[2].Num()), math.Ceil(box[1].Num()+box[3].Num())
	return core.Bounds{X: int(left), Y: int(top), Width: int(right - left), Height: int(bottom - top)}, nil
}

// Hierarchy returns the full accessibility tree as JSON.
func (d *Driver) Hierarchy() ([]byte, error) {
	result, err := proto.AccessibilityGetFullAXTree{}.Call(d.page)
	if err != nil {
		return nil, fmt.Errorf("failed to get AX tree: %w", err)
	}
	return json.Marshal(result.Nodes)
}

// GetState returns the current browser state.
func (d *Driver) GetState() *core.StateSnapshot {
	orientation := "portrait"
	if d.viewportW > d.viewportH {
		orientation = "landscape"
	}
	return &core.StateSnapshot{
		Orientation:   orientation,
		ClipboardText: d.clipboard,
	}
}

// GetPlatformInfo returns platform information.
func (d *Driver) GetPlatformInfo() *core.PlatformInfo {
	return &core.PlatformInfo{
		Platform:     "web",
		DeviceName:   "Chrome",
		IsSimulator:  true,
		ScreenWidth:  d.viewportW,
		ScreenHeight: d.viewportH,
	}
}

// SetFindTimeout sets the default timeout for finding elements.
func (d *Driver) SetFindTimeout(ms int) {
	if ms > 0 {
		d.findTimeoutMs = ms
	}
}

// FindTimeout returns the effective find timeout in milliseconds.
func (d *Driver) FindTimeout() int {
	return d.findTimeoutMs
}

// SetWaitForIdleTimeout is a no-op for browser since Rod handles waits differently.
func (d *Driver) SetWaitForIdleTimeout(ms int) error {
	return nil
}

// SetContext is a no-op for browser — Rod waits use their own timeouts.
func (d *Driver) SetContext(ctx context.Context) {}

// successResult creates a success result.
func successResult(msg string, elem *core.ElementInfo) *core.CommandResult {
	return &core.CommandResult{
		Success: true,
		Message: msg,
		Element: elem,
	}
}

// errorResult creates an error result.
func errorResult(err error, msg string) *core.CommandResult {
	return &core.CommandResult{
		Success: false,
		Error:   err,
		Message: msg,
	}
}

// unsupportedResult creates an error result for unsupported commands.
func unsupportedResult(msg string) *core.CommandResult {
	return &core.CommandResult{
		Success: false,
		Error:   fmt.Errorf("%s", msg),
		Message: msg,
	}
}

// ConsoleEntry represents a captured browser console message or JS exception.
type ConsoleEntry struct {
	Level   string `json:"level"`   // "log", "warn", "error", "info", "exception"
	Message string `json:"message"` // Console message text
}

// ConsoleLogs returns all captured console entries.
func (d *Driver) ConsoleLogs() []ConsoleEntry {
	d.consoleMu.Lock()
	defer d.consoleMu.Unlock()
	out := make([]ConsoleEntry, len(d.consoleLogs))
	copy(out, d.consoleLogs)
	return out
}

// ConsoleLogReport returns the captured console entries shaped for the
// per-flow report. Implements the executor's consoleLogReporter interface so
// the flow runner can auto-surface JS errors without requiring users to
// invoke `getConsoleLogs` explicitly. Web flows only — other drivers don't
// implement this method, and the executor's type assertion returns nil for
// them.
func (d *Driver) ConsoleLogReport() []report.ConsoleLog {
	d.consoleMu.Lock()
	defer d.consoleMu.Unlock()
	if len(d.consoleLogs) == 0 {
		return nil
	}
	out := make([]report.ConsoleLog, len(d.consoleLogs))
	for i, e := range d.consoleLogs {
		out[i] = report.ConsoleLog{Level: e.Level, Message: e.Message}
	}
	return out
}

// ClearConsoleLogReport resets the captured-console buffer. Called by the
// flow runner at the start of each top-level flow so pre-flow noise (events
// captured during driver construction's initial navigation, before the
// user's first step ran) doesn't pollute the per-flow report — and so
// per-flow auto-surface counts match what the user's flow actually
// triggered. Same buffer the existing `clearConsoleLogs` flow step writes
// to; this method exists separately so the executor can call it via the
// consoleLogReporter interface without needing to dispatch a flow command.
func (d *Driver) ClearConsoleLogReport() {
	d.consoleMu.Lock()
	defer d.consoleMu.Unlock()
	d.consoleLogs = nil
}

// startConsoleHandler starts a background goroutine to capture console.log/error/warn
// and uncaught JS exceptions via the CDP Runtime domain.
func (d *Driver) startConsoleHandler() {
	if err := (proto.RuntimeEnable{}).Call(d.page); err != nil {
		log.Printf("[browser] failed to enable Runtime domain: %v", err)
		return
	}

	go d.page.EachEvent(func(e *proto.RuntimeConsoleAPICalled) bool {
		level := string(e.Type)
		msg := formatConsoleArgs(e.Args)

		d.consoleMu.Lock()
		d.consoleLogs = append(d.consoleLogs, ConsoleEntry{Level: level, Message: msg})
		d.consoleMu.Unlock()

		select {
		case <-d.stopCh:
			return true
		default:
			return false
		}
	}, func(e *proto.RuntimeExceptionThrown) bool {
		msg := e.ExceptionDetails.Text
		if e.ExceptionDetails.Exception != nil {
			if desc := e.ExceptionDetails.Exception.Description; desc != "" {
				msg = desc
			}
		}

		d.consoleMu.Lock()
		d.consoleLogs = append(d.consoleLogs, ConsoleEntry{Level: "exception", Message: msg})
		d.consoleMu.Unlock()

		select {
		case <-d.stopCh:
			return true
		default:
			return false
		}
	})()
}

// formatConsoleArgs converts RuntimeRemoteObject args to a readable string.
func formatConsoleArgs(args []*proto.RuntimeRemoteObject) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg.Value.Str() != "":
			parts = append(parts, arg.Value.Str())
		case arg.Description != "":
			parts = append(parts, arg.Description)
		default:
			parts = append(parts, arg.Value.String())
		}
	}
	return strings.Join(parts, " ")
}
