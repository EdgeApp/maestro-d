package netshape

import (
	"strings"
	"testing"
)

func TestParseConfig_InstallAppBlock(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"installApp": {"allowHosts": [" Mason.TeamStation.app ", "127.0.0.1", "[::1]"]}}`))
	if err != nil || cfg.InstallApp == nil {
		t.Fatalf("%+v %v", cfg, err)
	}
	if got := strings.Join(cfg.InstallApp.AllowHosts, ","); got != "mason.teamstation.app,127.0.0.1,::1" {
		t.Fatalf("hosts: %s", got)
	}
	if cfg, err := ParseConfig([]byte(`{"hubDevices": []}`)); err != nil || cfg.InstallApp != nil {
		t.Fatalf("%+v %v", cfg, err)
	}
	for body, want := range map[string]string{
		`{"installApp": {"allowHosts": ["a"], "hosts": []}}`: `unknown field "hosts"`,
		`{"installApp": {}}`:                                           "lists no host",
		`{"installApp": {"allowHosts": []}}`:                           "lists no host",
		`{"installApp": {"allowHosts": [" "]}}`:                        "is not a host name",
		`{"installApp": {"allowHosts": ["https://mason.example"]}}`:    "is not a host name",
		`{"installApp": {"allowHosts": ["mason.example:9300"]}}`:       "is not a host name",
		`{"installApp": {"allowHosts": ["*.digitaloceanspaces.com"]}}`: "is not a host name",
		`{"installApp": {"allowHosts": ["mason.example/api"]}}`:        "is not a host name",
	} {
		if _, err := ParseConfig([]byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", body, err)
		}
	}
	if _, err := ParseConfig([]byte(`{"installApp": {}}`)); err == nil || !strings.HasPrefix(err.Error(), "installApp: ") {
		t.Fatalf("prefix: %v", err)
	}
	if _, err := Parse([]byte(`{"devices": {}, "installApp": {"allowHosts": ["a"]}}`)); err == nil {
		t.Fatal("network.json took an installApp block")
	}
}

func TestInstallApp_Allows(t *testing.T) {
	a := &InstallApp{AllowHosts: []string{"mason.teamstation.app", "::1"}}
	for _, c := range []struct {
		scheme, host    string
		first, redirect bool
	}{
		{"https", "mason.teamstation.app", true, true},
		{"http", "MASON.teamstation.app", true, true},
		{"https", "[::1]", true, true},
		{"https", "evil.example", false, false},
		{"https", "teamstation.app", false, false},
		{"https", "edge.nyc3.digitaloceanspaces.com", false, true},
		{"https", "NYC3.DigitalOceanSpaces.com", false, true},
		{"http", "edge.nyc3.digitaloceanspaces.com", false, false},
		{"https", ".digitaloceanspaces.com", false, false},
		{"https", "digitaloceanspaces.com.evil.example", false, false},
	} {
		if got := a.Allows(c.host); got != c.first {
			t.Errorf("Allows(%s) = %v", c.host, got)
		}
		if got := a.AllowsRedirect(c.scheme, c.host); got != c.redirect {
			t.Errorf("AllowsRedirect(%s, %s) = %v", c.scheme, c.host, got)
		}
	}
}
