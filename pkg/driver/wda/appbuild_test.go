package wda

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func physicalTestDriver(t *testing.T) *Driver {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]interface{}{"status": 0})
	}))
	t.Cleanup(server.Close)
	d := createTestDriver(server)
	d.udid = "00008120-PHONE"
	return d
}

// clearState on a physical device reinstalls from the build the daemon
// keeps, when there is no app file; an app file wins over it.
func TestClearState_PhysicalReinstallsKeptBuild(t *testing.T) {
	var _ core.AppBuildUser = (*Driver)(nil)
	d := physicalTestDriver(t)
	asked := ""
	d.SetAppBuildSource(func(appID string) string { asked = appID; return "/builds/TeamStation.ipa" })
	var got [][]string
	withFakeExec(t, &got, false)
	res := d.clearState(&flow.ClearStateStep{AppID: "app.teamstation.develop"})
	if !res.Success || asked != "app.teamstation.develop" {
		t.Fatalf("clearState: %+v asked %q", res, asked)
	}
	last := got[len(got)-1]
	if strings.Join(last, " ") != "xcrun devicectl device install app --device 00008120-PHONE /builds/TeamStation.ipa" {
		t.Fatalf("install: %v", last)
	}
	if strings.Join(got[len(got)-2], " ") != "xcrun devicectl device uninstall app --device 00008120-PHONE app.teamstation.develop" {
		t.Fatalf("uninstall: %v", got)
	}

	d.SetAppFile("/flows/App.ipa")
	got = nil
	if res := d.clearState(&flow.ClearStateStep{AppID: "a"}); !res.Success || !strings.HasSuffix(strings.Join(got[len(got)-1], " "), "/flows/App.ipa") {
		t.Fatalf("app file: %+v %v", res, got)
	}
}

// Without a kept build the daemon's answer names installApp; without a
// build source (the CLI) it still names --app-file.
func TestClearState_PhysicalNoKeptBuild(t *testing.T) {
	d := physicalTestDriver(t)
	d.SetAppBuildSource(func(string) string { return "" })
	var got [][]string
	withFakeExec(t, &got, false)
	res := d.clearState(&flow.ClearStateStep{AppID: "app.teamstation.develop"})
	if res.Success || !strings.Contains(res.Message, "installApp") || !strings.Contains(res.Error.Error(), "installApp") {
		t.Fatalf("no build: %+v", res)
	}
	for _, c := range got {
		if strings.Contains(strings.Join(c, " "), "uninstall") {
			t.Fatalf("uninstalled with nothing to reinstall: %v", got)
		}
	}
	d.SetAppBuildSource(nil)
	if res := d.clearState(&flow.ClearStateStep{AppID: "a"}); res.Success || !strings.Contains(res.Message, "--app-file") {
		t.Fatalf("CLI: %+v", res)
	}
}
