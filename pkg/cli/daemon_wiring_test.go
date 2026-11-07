package cli

import (
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func envOf(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func TestTeamIDFromEnv(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want string
	}{
		{"MAESTRO_TEAM_ID", map[string]string{"MAESTRO_TEAM_ID": "TEAM000001"}, "TEAM000001"},
		{"DEVELOPMENT_TEAM only", map[string]string{"DEVELOPMENT_TEAM": "TEAM000002"}, "TEAM000002"},
		{"both prefer MAESTRO_TEAM_ID", map[string]string{"MAESTRO_TEAM_ID": "TEAM000001", "DEVELOPMENT_TEAM": "TEAM000002"}, "TEAM000001"},
		{"empty MAESTRO_TEAM_ID skipped", map[string]string{"MAESTRO_TEAM_ID": " ", "DEVELOPMENT_TEAM": "TEAM000002"}, "TEAM000002"},
		{"trimmed", map[string]string{"MAESTRO_TEAM_ID": " TEAM000003\n"}, "TEAM000003"},
		{"neither", map[string]string{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := teamIDFromEnv(envOf(tt.vars)); got != tt.want {
				t.Errorf("teamIDFromEnv = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAttachToRunConfigTeamID(t *testing.T) {
	t.Setenv("MAESTRO_TEAM_ID", "TEAMENV001")
	t.Setenv("DEVELOPMENT_TEAM", "")

	// A REST or JavaScript attach names no team: the daemon's environment
	// supplies it.
	rc := attachToRunConfig("00008120-001631DA0C09A01E", daemon.AttachConfig{Platform: "ios"})
	if rc.TeamID != "TEAMENV001" {
		t.Errorf("TeamID = %q, want the daemon's MAESTRO_TEAM_ID", rc.TeamID)
	}

	// A team in the request wins.
	rc = attachToRunConfig("00008120-001631DA0C09A01E", daemon.AttachConfig{Platform: "ios", TeamID: "TEAMREQ001"})
	if rc.TeamID != "TEAMREQ001" {
		t.Errorf("TeamID = %q, want the request's team", rc.TeamID)
	}

	// No team anywhere stays empty.
	t.Setenv("MAESTRO_TEAM_ID", "")
	rc = attachToRunConfig("00008120-001631DA0C09A01E", daemon.AttachConfig{Platform: "ios"})
	if rc.TeamID != "" {
		t.Errorf("TeamID = %q, want empty", rc.TeamID)
	}
}

func TestAttachToRunConfigOptions(t *testing.T) {
	idle := 750
	rc := attachToRunConfig("emulator-5554", daemon.AttachConfig{
		Platform:           "Android",
		WaitForIdleTimeout: &idle,
		Extra: map[string]any{
			"headed":            true,
			"browser":           "chromium",
			"userDataDir":       "/tmp/profile",
			"windowSize":        "390x844",
			"androidTcpForward": true,
			"capsFile":          "/tmp/caps.json",
			"newCommandTimeout": float64(90),
		},
	})
	if rc.Platform != "android" || rc.Driver != "uiautomator2" || rc.AppiumURL != "http://127.0.0.1:4723" {
		t.Errorf("defaults: platform %q driver %q appium %q", rc.Platform, rc.Driver, rc.AppiumURL)
	}
	if rc.WaitForIdleTimeout != 750 {
		t.Errorf("WaitForIdleTimeout = %d, want 750", rc.WaitForIdleTimeout)
	}
	if !rc.Headed || rc.Browser != "chromium" || rc.UserDataDir != "/tmp/profile" || rc.WindowSize != "390x844" ||
		!rc.AndroidTCPForward || rc.CapsFile != "/tmp/caps.json" || rc.NewCommandTimeout != 90 {
		t.Errorf("extra options not applied: %+v", rc)
	}
}
