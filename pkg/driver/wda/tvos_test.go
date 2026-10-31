package wda

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

func TestTVSimulatorDestination(t *testing.T) {
	dir := t.TempDir()
	data := `{"devices":{"com.apple.CoreSimulator.SimRuntime.tvOS-27-2":[{"udid":"fixture-tv","name":"Fixture TV","state":"Booted","isAvailable":true}]}}`
	if err := os.WriteFile(filepath.Join(dir, "xcrun"), []byte("#!/bin/sh\ncat <<'JSON'\n"+data+"\nJSON\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := NewRunner("fixture-tv", "", "")
	want := "platform=tvOS Simulator,arch=" + simulator.XcodebuildArch(runtime.GOARCH) + ",id=fixture-tv"
	if got := r.destination(); got != want {
		t.Fatalf("TV simulator destination: got %s; want %s", got, want)
	}
	cache, err := r.getBuildCacheDir()
	if err != nil || !strings.HasSuffix(cache, "sim-tvos27.2-tv") {
		t.Fatalf("cache %s: %v", cache, err)
	}
	if !strings.Contains(r.destination(), "tvOS") {
		t.Fatal("lost TV runtime")
	}
}

func TestTVRuntimeSelection(t *testing.T) {
	for _, platform := range []string{"iOS", "tvOS", "watchOS", "xrOS"} {
		t.Run(platform, func(t *testing.T) {
			r := NewRunner("fixture", "", "fixture.bundle")
			r.listSimulators = func() ([]simulator.SimulatorDevice, error) {
				return []simulator.SimulatorDevice{{UDID: "fixture", Platform: platform, OSVersion: "27.2", IsAvailable: true}}, nil
			}
			err := r.configureRuntime()
			supported := platform == "iOS" || platform == "tvOS"
			if (err == nil) != supported {
				t.Fatalf("%s: %v", platform, err)
			}
			if !supported {
				if r.Build(context.Background()) == nil {
					t.Fatal("unsupported build accepted")
				}
				if r.configureRuntime() == nil {
					t.Fatal("lost runtime failure")
				}
				return
			}
			if r.IsTV() != (platform == "tvOS") {
				t.Fatal("wrong runtime")
			}
			want := "WebDriverAgentRunner"
			if platform == "tvOS" {
				want += "_tvOS"
			}
			args := strings.Join(r.buildArguments("fixture.xcodeproj"), " ")
			if !strings.Contains(args, "PRODUCT_BUNDLE_IDENTIFIER=fixture.bundle") {
				t.Fatal(args)
			}
			if !strings.Contains(args, "-scheme "+want) {
				t.Fatal(args)
			}
			if strings.Contains(args, "TVOS_DEPLOYMENT_TARGET=15.1") != r.IsTV() {
				t.Fatal(args)
			}
			if r.runnerScheme() != want {
				t.Fatalf("scheme %s", r.runnerScheme())
			}
			if version, _ := r.getIOSVersion(); version != "27.2" {
				t.Fatal(version)
			}
		})
	}
	for _, devices := range [][]simulator.SimulatorDevice{nil, {{UDID: "different", Platform: "tvOS", IsAvailable: true}}, {{UDID: "fixture", Platform: "tvOS", IsAvailable: false}}} {
		r := NewRunner("fixture", "", "")
		r.listSimulators = func() ([]simulator.SimulatorDevice, error) { return devices, nil }
		if err := r.configureRuntime(); err != nil || r.IsTV() {
			t.Fatalf("no-match fallback: %v", err)
		}
	}
	r := NewRunner("fixture", "", "")
	r.listSimulators = func() ([]simulator.SimulatorDevice, error) { return nil, fmt.Errorf("simctl unavailable") }
	if err := r.configureRuntime(); err != nil || r.IsTV() {
		t.Fatal("discovery failure changed fallback")
	}
}

func TestTVRemoteKeys(t *testing.T) {
	var got string
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		got = payload.Name
		if fail {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"value":{"error":"unknown error","message":"remote unavailable"}}`))
			return
		}
		jsonResponse(w, map[string]interface{}{"status": 0})
	}))
	defer server.Close()
	d := createTestDriver(server)
	d.SetTVMode(true)
	cases := map[string]string{"up": "up", "DPAD_UP": "up", "down": "down", "DPAD_DOWN": "down", "left": "left", "DPAD_LEFT": "left", "right": "right", "DPAD_RIGHT": "right", "select": "select", "center": "select", "DPAD_CENTER": "select", "enter": "select", "back": "menu", "menu": "menu", "playPause": "playPause", "play_pause": "playPause", "home": "home"}
	for key, want := range cases {
		if result := d.pressKey(&flow.PressKeyStep{Key: key}); !result.Success || got != want {
			t.Fatalf("%s: got %s result %+v", key, got, result)
		}
	}
	fail = true
	if d.pressKey(&flow.PressKeyStep{Key: "up"}).Success {
		t.Fatal("remote failure hidden")
	}
	if d.pressKey(&flow.PressKeyStep{Key: "unknown"}).Success {
		t.Fatal("unknown key accepted")
	}
	d.SetTVMode(false)
	if d.pressKey(&flow.PressKeyStep{Key: "up"}).Success {
		t.Fatal("TV key leaked into iOS")
	}
}
