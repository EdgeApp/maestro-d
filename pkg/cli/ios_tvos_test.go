package cli

import (
	"context"
	"encoding/json"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/wda"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/flutter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestConfiguredIOSDriverRuntimeAndCleanup(t *testing.T) {
	for _, tv := range []bool{false, true} {
		var button string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			button = body.Name
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":{"sessionId":"fixture","capabilities":{}},"sessionId":"fixture","status":0}`))
		}))
		count := 0
		info := &core.PlatformInfo{Platform: "ios", DeviceID: "fixture", IsSimulator: true}
		driver, close := configuredIOSDriver(wda.NewClientURL(server.URL), info, &RunConfig{}, tv, func() { count++ })
		_, wrapped := driver.(*flutter.FlutterDriver)
		if wrapped == tv {
			t.Fatal("incorrect Flutter wrapper for runtime")
		}
		if tv {
			result := driver.Execute(&flow.PressKeyStep{Key: "DPAD_LEFT"})
			if !result.Success || button != "left" {
				t.Fatalf("TV key lost: %+v %q", result, button)
			}
		}
		close()
		if count != 1 {
			t.Fatal("underlying runner cleanup lost")
		}
		server.Close()
	}
}

type fixtureTVRunner struct {
	url     string
	cleaned bool
}

func (r *fixtureTVRunner) Build(context.Context) error     { return nil }
func (r *fixtureTVRunner) Start(context.Context) error     { return nil }
func (r *fixtureTVRunner) Cleanup()                        { r.cleaned = true }
func (r *fixtureTVRunner) Port() uint16                    { return 0 }
func (r *fixtureTVRunner) BaseURL() string                 { return r.url }
func (r *fixtureTVRunner) Devicectl() *wda.DevicectlLookup { return nil }
func (r *fixtureTVRunner) IsTV() bool                      { return true }

func TestCreateTVDriverPropagatesRuntime(t *testing.T) {
	old := newIOSWDARunner
	if _, ok := old("fixture", "", "").(*wda.Runner); !ok {
		t.Fatal("production runner factory")
	}
	defer func() { newIOSWDARunner = old }()
	dir := t.TempDir()
	data := `{"devices":{"com.apple.CoreSimulator.SimRuntime.tvOS-27-2":[{"udid":"fixture-tv","name":"Fixture TV","state":"Booted","isAvailable":true}]}}`
	if err := os.WriteFile(filepath.Join(dir, "xcrun"), []byte("#!/bin/sh\ncat <<'JSON'\n"+data+"\nJSON\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var button string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		button = body.Name
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":{"sessionId":"fixture","capabilities":{},"width":1920,"height":1080},"sessionId":"fixture","status":0}`))
	}))
	defer server.Close()
	runner := &fixtureTVRunner{url: server.URL}
	newIOSWDARunner = func(udid, team, bundle string) iosWDARunner {
		if udid != "fixture-tv" {
			t.Fatal(udid)
		}
		return runner
	}
	driver, close, err := CreateIOSDriver(&RunConfig{Devices: []string{"fixture-tv"}, NoDriverInstall: true, NoAppInstall: true})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	result := driver.Execute(&flow.PressKeyStep{Key: "DPAD_UP"})
	if !result.Success || button != "up" {
		t.Fatalf("TV runtime not propagated: %+v %s", result, button)
	}
	close()
	if !runner.cleaned {
		t.Fatal("runner cleanup")
	}
}
