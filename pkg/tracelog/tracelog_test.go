package tracelog

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathNeedsTheEnvAndAnOutputDir(t *testing.T) {
	t.Setenv(EnvVar, "")
	if p := Path("/out"); p != "" {
		t.Errorf("trace off: Path = %q, want empty", p)
	}
	t.Setenv(EnvVar, "1")
	if p := Path("/out"); p != filepath.Join("/out", "client.log") {
		t.Errorf("trace on: Path = %q", p)
	}
	if p := Path(""); p != "" {
		t.Errorf("no output dir: Path = %q, want empty", p)
	}
}

func TestRedact(t *testing.T) {
	clip := base64.StdEncoding.EncodeToString([]byte("sécret"))
	cases := []struct{ body, mustNot, must string }{
		{`{"text":"1234"}`, "1234", "<redacted 4 chars>"},
		{`{"actions":[{"type":"key","actions":[{"type":"keyDown","value":"p"},{"type":"keyUp","value":"p"},{"type":"keyDown","value":"i"}]}]}`, `"value":"p"`, `"type":"keyDown"`},
		{`{"content":"` + clip + `","contentType":"plaintext"}`, clip, `"contentType":"plaintext"`},
		{`{"value":["a","b"],"strategy":"id"}`, `"a"`, `"strategy":"id"`},
		{`{"strategy":"id","selector":"Search settings"}`, "<redacted", "Search settings"},
	}
	for _, c := range cases {
		got := Redact([]byte(c.body))
		if strings.Contains(got, c.mustNot) || !strings.Contains(got, c.must) {
			t.Errorf("Redact(%s) = %s; must not contain %q, must contain %q", c.body, got, c.mustNot, c.must)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("Redact(%s) is not JSON: %s", c.body, got)
		}
	}
	if got := Redact([]byte("not json secret")); strings.Contains(got, "secret") {
		t.Errorf("a non-JSON body was echoed: %s", got)
	}
}

func TestOpenCapsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.log")
	l, f := Open(path, 1024)
	if l == nil || f == nil {
		t.Fatal("Open returned nil")
	}
	defer f.Close()
	for i := 0; i < 200; i++ {
		l.Printf("GET /session/x/element [1ms] OK body=%s", strings.Repeat("x", 40))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 1024 || fi.Size() == 0 {
		t.Errorf("trace size %d, want 1..1024", fi.Size())
	}
	again, f2 := Open(path, 1024)
	if again == nil {
		t.Fatal("reopening an existing trace failed")
	}
	_ = f2.Close()
	if l, c := Open(filepath.Join(path, "not-a-dir", "x.log"), 1024); l != nil || c != nil {
		t.Error("an unopenable path should give nil, nil")
	}
	Discard().Printf("nothing")
}
