package wda

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// classServer answers /element/{id}/name with WebDriverAgent's long type
// name and leaves every other route to mockWDAServerForDriver.
func classServer(t *testing.T) *httptest.Server {
	t.Helper()
	base := mockWDAServerForDriver()
	t.Cleanup(base.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/name") {
			jsonResponse(w, map[string]interface{}{"value": "XCUIElementTypeButton"})
			return
		}
		r.URL.Host = strings.TrimPrefix(base.URL, "http://")
		r.URL.Scheme = "http"
		r.RequestURI = ""
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Errorf("proxy: %v", err)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestShortElementType(t *testing.T) {
	cases := map[string]string{
		"XCUIElementTypeButton":     "Button",
		"XCUIElementTypeStaticText": "StaticText",
		"Button":                    "Button",
		"":                          "",
		"AppiumAUT":                 "AppiumAUT",
	}
	for in, want := range cases {
		if got := shortElementType(in); got != want {
			t.Errorf("shortElementType(%q) = %q, want %q", in, got, want)
		}
	}
}

// A WDA element lookup reports the class the simulator's devicelab runner
// would: "Button", not "XCUIElementTypeButton".
func TestGetElementInfo_ClassDropsXCUIPrefix(t *testing.T) {
	d := createTestDriver(classServer(t))
	info, err := d.getElementInfo("elem-123")
	if err != nil {
		t.Fatalf("getElementInfo: %v", err)
	}
	if info.Class != "Button" {
		t.Errorf("Class = %q, want %q", info.Class, "Button")
	}
}

// The page-source fallback carries the same short class.
func TestFindElementByPageSource_ClassDropsXCUIPrefix(t *testing.T) {
	server := mockWDAServerForDriver()
	defer server.Close()
	d := createTestDriver(server)

	info, err := d.findElementByPageSourceOnce(flow.Selector{ID: "emailField"})
	if err != nil {
		t.Fatalf("findElementByPageSourceOnce: %v", err)
	}
	if info.Class != "TextField" {
		t.Errorf("Class = %q, want %q", info.Class, "TextField")
	}

	info, err = d.findElementRelativeOnce(flow.Selector{ID: "loginBtn", Above: &flow.Selector{ID: "emailField"}})
	if err != nil {
		t.Fatalf("findElementRelativeOnce: %v", err)
	}
	if info.Class != "Button" {
		t.Errorf("relative Class = %q, want %q", info.Class, "Button")
	}
}
