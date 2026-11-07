package wda

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/config"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// mediaFile writes a small file into a temp dir and returns its path.
func mediaFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// addMediaServer is a WebDriverAgent that records /wda/addMedia bodies and
// answers each with reply.
func addMediaServer(t *testing.T, reply map[string]interface{}) (*Client, *[]map[string]string) {
	t.Helper()
	var bodies []map[string]string
	server := mockWDAServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/wda/addMedia") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		bodies = append(bodies, body)
		jsonResponse(w, reply)
	})
	t.Cleanup(server.Close)
	return NewClientURL(server.URL), &bodies
}

func deviceDriver(client *Client) *Driver {
	return &Driver{client: client, info: &core.PlatformInfo{Platform: "ios", IsSimulator: false}, udid: "00000000-0000000000000000"}
}

func TestAddMediaClientPosts(t *testing.T) {
	client, bodies := addMediaServer(t, map[string]interface{}{"value": map[string]interface{}{"photosAccess": "add-only"}})
	access, err := client.AddMedia("red.png", "image/png", []byte("PNGDATA"))
	if err != nil {
		t.Fatalf("AddMedia: %v", err)
	}
	if access != "add-only" {
		t.Errorf("access = %q, want add-only", access)
	}
	want := []map[string]string{{
		"name": "red.png", "mimeType": "image/png",
		"data": base64.StdEncoding.EncodeToString([]byte("PNGDATA")),
	}}
	if !reflect.DeepEqual(*bodies, want) {
		t.Errorf("bodies = %v, want %v", *bodies, want)
	}
}

func TestAddMediaClientError(t *testing.T) {
	client, _ := addMediaServer(t, map[string]interface{}{
		"value": map[string]interface{}{"error": "unknown error", "message": "PhotoKit could not add the media"},
	})
	_, err := client.AddMedia("red.png", "image/png", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "PhotoKit could not add the media") {
		t.Errorf("AddMedia error = %v, want the WDA message", err)
	}
}

func TestAddMediaDevicePostsEachFile(t *testing.T) {
	var simctl [][]string
	withFakeExec(t, &simctl, false)
	client, bodies := addMediaServer(t, map[string]interface{}{"value": nil})
	png := mediaFile(t, "red.png", []byte("PNGDATA"))
	mp4 := mediaFile(t, "clip.mp4", []byte("MP4DATA"))

	result := deviceDriver(client).addMedia(&flow.AddMediaStep{Files: []string{png, mp4}})
	if !result.Success {
		t.Fatalf("addMedia failed: %v (%s)", result.Error, result.Message)
	}
	if len(simctl) != 0 {
		t.Errorf("a real device must not run simctl: %v", simctl)
	}
	want := []map[string]string{
		{"name": "red.png", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("PNGDATA"))},
		{"name": "clip.mp4", "mimeType": "video/mp4", "data": base64.StdEncoding.EncodeToString([]byte("MP4DATA"))},
	}
	if !reflect.DeepEqual(*bodies, want) {
		t.Errorf("bodies = %v, want %v", *bodies, want)
	}
	if !strings.Contains(result.Message, "Added 2 media file(s) to the device") {
		t.Errorf("message = %q", result.Message)
	}
	for _, b := range *bodies {
		t.Logf("POST /wda/addMedia name=%s mimeType=%s data=%d base64 chars", b["name"], b["mimeType"], len(b["data"]))
	}
	t.Logf("result: %s", result.Message)
}

func TestAddMediaDeviceStaleWebDriverAgent(t *testing.T) {
	// A WebDriverAgent built before /wda/addMedia existed answers like this.
	client, _ := addMediaServer(t, map[string]interface{}{
		"value": map[string]interface{}{
			"error":   "unknown command",
			"message": "Unhandled endpoint: /wda/addMedia -- http://127.0.0.1:8100/ with parameters {}",
		},
	})
	png := mediaFile(t, "red.png", []byte("PNGDATA"))

	result := deviceDriver(client).addMedia(&flow.AddMediaStep{Files: []string{png}})
	if result.Success {
		t.Fatal("expected failure from a WebDriverAgent without /wda/addMedia")
	}
	builds := filepath.Join(config.GetCacheDir(), "wda-builds")
	for _, want := range []string{builds, "rebuilds it", "maestro-d wda update"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message lacks %q: %s", want, result.Message)
		}
	}
	if strings.Contains(result.Message, "devicelab") {
		t.Errorf("message must not point at the devicelab driver: %s", result.Message)
	}
	t.Logf("result: %s", result.Message)

	// With the runner's build directory known, the message names it.
	d := deviceDriver(client)
	d.SetWDABuildDir("/cache/wda-builds/device-ios26.6.1-teamABCDE12345")
	result = d.addMedia(&flow.AddMediaStep{Files: []string{png}})
	if !strings.Contains(result.Message, "Delete /cache/wda-builds/device-ios26.6.1-teamABCDE12345 so the next attach rebuilds it") {
		t.Errorf("message should name the exact build: %s", result.Message)
	}
	t.Logf("result with the build known: %s", result.Message)
}

func TestAddMediaDeviceErrorNamesTheFile(t *testing.T) {
	client, _ := addMediaServer(t, map[string]interface{}{
		"value": map[string]interface{}{"error": "unknown error", "message": "Photos access was refused"},
	})
	png := mediaFile(t, "red.png", []byte("PNGDATA"))

	result := deviceDriver(client).addMedia(&flow.AddMediaStep{Files: []string{png}})
	if result.Success {
		t.Fatal("expected failure")
	}
	if !strings.Contains(result.Message, "red.png") || !strings.Contains(result.Message, "Photos access was refused") {
		t.Errorf("message = %q", result.Message)
	}
}

func TestAddMediaDeviceMissingFileSendsNothing(t *testing.T) {
	client, bodies := addMediaServer(t, map[string]interface{}{"value": nil})
	png := mediaFile(t, "red.png", []byte("PNGDATA"))
	missing := filepath.Join(t.TempDir(), "gone.png")

	result := deviceDriver(client).addMedia(&flow.AddMediaStep{Files: []string{png, missing}})
	if result.Success || !strings.Contains(result.Message, "Media file not found") {
		t.Fatalf("result = %+v, want a missing-file failure", result)
	}
	if len(*bodies) != 0 {
		t.Errorf("sent %d file(s) before checking them all", len(*bodies))
	}
}

func TestAddMediaRejectsUnsupportedFiles(t *testing.T) {
	result := deviceDriver(nil).addMedia(&flow.AddMediaStep{Files: []string{"notes.txt"}})
	if result.Success {
		t.Fatal("expected an unsupported file type to fail")
	}
}

func TestAddMediaSimulatorRunsSimctl(t *testing.T) {
	var got [][]string
	withFakeExec(t, &got, false)
	png := mediaFile(t, "red.png", []byte("PNGDATA"))
	d := &Driver{info: &core.PlatformInfo{Platform: "ios", IsSimulator: true}, udid: "SIM-UDID"}

	result := d.addMedia(&flow.AddMediaStep{Files: []string{png}})
	if !result.Success {
		t.Fatalf("addMedia failed: %v", result.Error)
	}
	if want := [][]string{{"xcrun", "simctl", "addmedia", "SIM-UDID", png}}; !reflect.DeepEqual(got, want) {
		t.Errorf("commands = %v, want %v", got, want)
	}
}

func TestAddMediaSimulatorFailure(t *testing.T) {
	var got [][]string
	withFakeExec(t, &got, true)
	png := mediaFile(t, "red.png", []byte("PNGDATA"))
	d := &Driver{info: &core.PlatformInfo{Platform: "ios", IsSimulator: true}, udid: "SIM-UDID"}

	result := d.addMedia(&flow.AddMediaStep{Files: []string{png}})
	if result.Success || !strings.Contains(result.Message, "Failed to add media") {
		t.Errorf("result = %+v, want a simctl failure", result)
	}
}

func TestAddMediaDeviceReportsFullPhotosAccess(t *testing.T) {
	// A runner built with Xcode 26 or earlier has no add-only usage
	// description, so iOS asks for the whole library; the result says so.
	client, _ := addMediaServer(t, map[string]interface{}{"value": map[string]interface{}{"photosAccess": "full"}})
	png := mediaFile(t, "red.png", []byte("PNGDATA"))

	result := deviceDriver(client).addMedia(&flow.AddMediaStep{Files: []string{png}})
	if !result.Success || !strings.Contains(result.Message, "full Photos access") || !strings.Contains(result.Message, "Xcode 26 or earlier") {
		t.Errorf("result = %+v, want success naming full Photos access", result)
	}
	t.Logf("result: %s", result.Message)

	client, _ = addMediaServer(t, map[string]interface{}{"value": map[string]interface{}{"photosAccess": "add-only"}})
	if result := deviceDriver(client).addMedia(&flow.AddMediaStep{Files: []string{png}}); strings.Contains(result.Message, "full Photos access") {
		t.Errorf("add-only access must not be reported as full: %s", result.Message)
	}
}

func TestAddMediaClientOwnTimeout(t *testing.T) {
	if addMediaTimeout <= 45*time.Second {
		t.Errorf("addMediaTimeout = %v; it must exceed the handler's 15 s alert + 30 s PhotoKit waits", addMediaTimeout)
	}
}
