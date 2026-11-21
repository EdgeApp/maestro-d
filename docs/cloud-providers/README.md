# Cloud Provider Integration

maestro-runner automatically detects cloud Appium providers from the `--appium-url` and reports test pass/fail after the run completes.

## Supported providers

- [TestingBot](testingbot.md)
- [Sauce Labs](saucelabs.md)

## Device providers

maestro-d also launches and holds real phones in a device cloud, and drives
them like local devices: see [docs/daemon/cloud.md](../daemon/cloud.md).

- [AWS Device Farm](devicefarm.md)

## How it works

1. **Detect** — after `--appium-url` is parsed, each registered provider checks if the URL matches (e.g., contains "saucelabs")
2. **Extract metadata** — after the Appium session is created, the provider reads session capabilities and stores provider-specific data (job IDs, session type, etc.) in a `map[string]string`
3. **Report result** — after all flows and reports complete, the provider receives the full test result and reports pass/fail to the cloud API

No extra flags needed — detection and reporting happen automatically.

## Adding a new provider

All provider code lives in `pkg/cloud/`. To add a new provider:

### 1. Create the file

Copy `pkg/cloud/example_provider.go` to `pkg/cloud/<yourprovider>.go`.

### 2. Implement the Provider interface

```go
package cloud

type Provider interface {
    // Name returns the human-readable provider name.
    Name() string

    // ExtractMeta is called once after the Appium session is created.
    // Read what you need from sessionID and caps, write to meta.
    ExtractMeta(sessionID string, caps map[string]interface{}, meta map[string]string)

    // ReportResult is called once after all flows and reports complete.
    // Use meta for provider-specific data, result for test outcome.
    ReportResult(appiumURL string, meta map[string]string, result *TestResult) error
}
```

### 3. Register via init()

The factory function checks the URL and returns a provider or `nil`:

```go
func init() {
    Register(func(appiumURL string) Provider {
        if !strings.Contains(strings.ToLower(appiumURL), "yourprovider") {
            return nil
        }
        return &yourProvider{}
    })
}
```

### 4. Example skeleton

A complete skeleton is available at `pkg/cloud/example_provider.go`. Copy it, rename, and implement the TODOs.

### 5. Add tests

Create `pkg/cloud/<yourprovider>_test.go` with tests for:
- URL detection (matches your provider, rejects others)
- ExtractMeta (correct meta keys)
- ReportResult (use `httptest.NewServer` to verify endpoint, auth, body)

### 6. Add documentation

Create `docs/cloud-providers/<yourprovider>.md` with:
- Run command example
- Example capabilities JSON
- Any provider-specific notes

Add a link in the main `README.md` under **Cloud Providers** and in this file under **Supported providers**.

## TestResult fields

`ReportResult` receives the full test outcome. Use what your provider's API supports:

```go
type TestResult struct {
    Passed      bool           // overall pass/fail
    Total       int            // total flow count
    PassedCount int            // flows that passed
    FailedCount int            // flows that failed
    Duration    int64          // total duration in milliseconds
    OutputDir   string         // path to log, reports, screenshots
    Flows       []FlowResult   // per-flow details
}

type FlowResult struct {
    Name     string  // flow name
    File     string  // source YAML file path
    Passed   bool    // this flow passed
    Duration int64   // milliseconds
    Error    string  // error message (empty if passed)
}
```

- Most providers only need `result.Passed` for a simple pass/fail update
- `result.Flows` is available for providers that support per-test annotations
- `result.OutputDir` contains `maestro-runner.log`, `report.html`, `report.json`, `junit-report.xml`, and screenshots — providers can upload these if their API supports artifacts

## Meta map

The `meta map[string]string` is owned by the caller and passed through `ExtractMeta` → `ReportResult`. Each provider writes its own keys. Examples:

| Provider | Keys | Description |
|----------|------|-------------|
| Sauce Labs | `jobID`, `type` | `type` is "rdc" (real device) or "vms" (emulator/simulator) |
| (new provider) | `jobID` | Typically the WebDriver session ID |

No naming conflicts since only one provider is active per session.

## Credentials

Each provider handles credentials internally in `ReportResult`. The common pattern is:

1. Extract from `--appium-url` userinfo (e.g., `https://USER:KEY@hub.example.com`)
2. Fall back to provider-specific environment variables

This keeps credential logic out of the shared interface.

## Error handling

`ReportResult` errors are logged as warnings — they never fail the test run. Local test results and reports are always generated regardless of cloud reporting success.
