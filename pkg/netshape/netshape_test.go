package netshape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a shaper stand-in that keeps every request it is sent.
type recorder struct {
	mu     sync.Mutex
	reqs   []recorded
	status int
	answer string
	delay  time.Duration
}

type recorded struct {
	Method, Path, Query, ContentType, Auth string
	Body                                   string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.reqs = append(r.reqs, recorded{req.Method, req.URL.Path, req.URL.RawQuery, req.Header.Get("Content-Type"), req.Header.Get("Authorization"), string(body)})
	status, answer, delay := r.status, r.answer, r.delay
	r.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-req.Context().Done():
			return
		}
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, answer)
}

func (r *recorder) last(t *testing.T) recorded {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		t.Fatal("the shaper received no request")
	}
	return r.reqs[len(r.reqs)-1]
}

func writeConfig(t *testing.T, dir string, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func jsonConfig(t *testing.T, devices map[string]Device) string {
	t.Helper()
	data, err := json.Marshal(Config{Devices: devices})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLoad_MissingFileIsEmpty(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Lookup("any"); ok {
		t.Fatal("a missing file configured a device")
	}
}

func TestLoad_RefusesAFileOthersCanRead(t *testing.T) {
	path := writeConfig(t, t.TempDir(), `{"devices":{}}`, 0o644)
	_, err := Load(path)
	var ce *ConfigError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "chmod 600") || ce.Path != path {
		t.Fatalf("err = %v", err)
	}
}

func TestLoad_StatFailureIsAConfigError(t *testing.T) {
	// A path through a regular file cannot be stat'ed as anything but an error.
	file := writeConfig(t, t.TempDir(), `{}`, 0o600)
	_, err := Load(filepath.Join(file, "network.json"))
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoad_UnreadableIsAConfigError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	path := writeConfig(t, t.TempDir(), `{}`, 0o200)
	_, err := Load(path)
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoad_ErrorsNameTheFileAndDevice(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `{"devices":{"phone":{"setTrafficShape":{"method":"POST","url":"http://x/"}}}}`, 0o600)
	_, err := Load(path)
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Path != path || ce.Device != "phone" {
		t.Fatalf("err = %#v", err)
	}
	want := fmt.Sprintf("config %s: device phone: has setTrafficShape but no resetTrafficShape", path)
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}

	path = writeConfig(t, dir, `{"devices":`, 0o600)
	_, err = Load(path)
	if !errors.As(err, &ce) || ce.Device != "" || !strings.HasPrefix(err.Error(), "config "+path+": ") {
		t.Fatalf("err = %v", err)
	}
}

func TestParse_Validation(t *testing.T) {
	good := `"resetTrafficShape":{"method":"DELETE","url":"https://s.example/shape"}`
	cases := []struct {
		name, json, want string
	}{
		{"unknown top-level key", `{"devices":{},"extra":1}`, `unknown field "extra"`},
		{"unknown device key", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"https://s.example/"},` + good + `,"airplane":{}}}}`, `unknown field "airplane"`},
		{"unknown call key", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"https://s.example/","timeout":3},` + good + `}}}`, `unknown field "timeout"`},
		{"trailing data", `{"devices":{}} {}`, "trailing data"},
		{"empty device", `{"devices":{"d":{}}}`, "neither setTrafficShape nor resetTrafficShape"},
		{"description is config.json's", `{"devices":{"d":{"description":"x",` + good + `}}}`, `unknown field "description"`},
		{"reset only", `{"devices":{"d":{` + good + `}}}`, "has resetTrafficShape but no setTrafficShape"},
		{"bad method", `{"devices":{"d":{"setTrafficShape":{"method":"FETCH","url":"https://s.example/"},` + good + `}}}`, `setTrafficShape: method "FETCH"`},
		{"lower-case method", `{"devices":{"d":{"setTrafficShape":{"method":"post","url":"https://s.example/"},` + good + `}}}`, `method "post"`},
		{"body on GET", `{"devices":{"d":{"setTrafficShape":{"method":"GET","url":"https://s.example/","body":{}},` + good + `}}}`, "a GET call has no body"},
		{"body on DELETE", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"https://s.example/"},"resetTrafficShape":{"method":"DELETE","url":"https://s.example/","body":{"a":1}}}}}`, "resetTrafficShape: a DELETE call has no body"},
		{"relative URL", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"/v1/shape"},` + good + `}}}`, "not an absolute http or https URL"},
		{"other scheme", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"ftp://s.example/"},` + good + `}}}`, "not an absolute http or https URL"},
		{"unparsable URL", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"http://[::1"},` + good + `}}}`, "not an absolute http or https URL"},
		{"unknown placeholder in URL", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"https://s.example/?bw=${bandwidth}"},` + good + `}}}`, "url: unknown placeholder ${bandwidth}"},
		{"unknown placeholder in header", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"https://s.example/","headers":{"X-Id":"${serial}"}},` + good + `}}}`, "header X-Id: unknown placeholder ${serial}"},
		{"unknown placeholder in body", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"https://s.example/","body":{"shapes":[{"delay":"${latency}"}]}},` + good + `}}}`, "body: unknown placeholder ${latency}"},
		{"unknown placeholder in body list", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"https://s.example/","body":["${x}"]},` + good + `}}}`, "body: unknown placeholder ${x}"},
		{"directionValues on reset", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"https://s.example/"},"resetTrafficShape":{"method":"POST","url":"https://s.example/","directionValues":{"both":"all"}}}}}`, "directionValues belongs on setTrafficShape only"},
		{"bad directionValues key", `{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"https://s.example/","directionValues":{"down":"dn"}},` + good + `}}}`, `directionValues key "down"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.json))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestParse_AcceptsAFullConfig(t *testing.T) {
	cfg, err := Parse([]byte(`{"devices":{
		"a":{"setTrafficShape":{"method":"POST","url":"https://s.example/v1/shape","headers":{"Authorization":"Bearer t"},
			"body":{"ip":"192.0.2.15","mbit":"${mbit}","direction":"${direction}","tags":["${deviceId}",3,true,null]},
			"directionValues":{"download":"down","upload":"up","both":"both"},"insecureSkipVerify":true},
		     "resetTrafficShape":{"method":"POST","url":"https://s.example/v1/shape","body":{"ip":"192.0.2.15"}}},
		"b":{"setTrafficShape":{"method":"PUT","url":"http://s.example/d/7?mbit=${mbit}&dir=${direction}"},
		     "resetTrafficShape":{"method":"DELETE","url":"http://s.example/d/7"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		d, ok := cfg.Lookup(id)
		if !ok || d.Set == nil || d.Reset == nil {
			t.Fatalf("%s: ok=%v %+v", id, ok, d)
		}
	}
}

func TestValues_Validate(t *testing.T) {
	good := []Values{{}, {Mbit: 1.5, LossPct: 100, LatencyMs: 250, JitterMs: 30, Direction: DirectionDownload}, {Direction: DirectionUpload}, {Direction: DirectionBoth}}
	for _, v := range good {
		if err := v.Validate(); err != nil {
			t.Errorf("%+v: %v", v, err)
		}
	}
	bad := map[string]Values{
		"mbit must be a number >= 0":                                  {Mbit: -1},
		"lossPct must be at most 100":                                 {LossPct: 101},
		"lossPct must be a number >= 0":                               {LossPct: -0.5},
		"latencyMs must be a number >= 0":                             {LatencyMs: math.NaN()},
		"jitterMs must be a number >= 0":                              {JitterMs: math.Inf(1)},
		`direction must be one of download, upload, both, got "down"`: {Direction: "down"},
	}
	for want, v := range bad {
		if err := v.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err = %v, want %q", v, err, want)
		}
	}
}

func TestFill_TypedTextAndEscaped(t *testing.T) {
	c := &Call{
		Method:  "POST",
		URL:     "https://s.example/d/${deviceId}?mbit=${mbit}&loss=${lossPct}&delay=${latencyMs}&jitter=${jitterMs}&dir=${direction}",
		Headers: map[string]string{"X-Shape": "${mbit}mbit ${direction}", "Authorization": "Bearer t"},
		Body: map[string]any{
			"ip": "192.0.2.15", "mbit": "${mbit}", "lossPct": "${lossPct}", "latencyMs": "${latencyMs}",
			"jitterMs": "${jitterMs}", "direction": "${direction}", "device": "${deviceId}",
			"label": "${mbit} Mbit for ${deviceId}", "list": []any{"${latencyMs}", 2.0, true, nil},
		},
		DirectionValues: map[string]string{DirectionDownload: "down", DirectionUpload: "up"},
	}
	req, err := Fill(c, Values{Mbit: 1.5, LossPct: 2, LatencyMs: 100, JitterMs: 0.25, Direction: DirectionDownload}, "avd:Pixel 8")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://s.example/d/avd:Pixel%208?mbit=1.5&loss=2&delay=100&jitter=0.25&dir=down"; req.URL != want {
		t.Errorf("url = %s, want %s", req.URL, want)
	}
	if req.Headers["X-Shape"] != "1.5mbit down" || req.Headers["Authorization"] != "Bearer t" {
		t.Errorf("headers = %v", req.Headers)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ip": "192.0.2.15", "mbit": 1.5, "lossPct": 2.0, "latencyMs": 100.0, "jitterMs": 0.25, "direction": "down",
		"device": "avd:Pixel 8", "label": "1.5 Mbit for avd:Pixel 8", "list": []any{100.0, 2.0, true, nil},
	}
	if fmt.Sprint(body) != fmt.Sprint(want) {
		t.Errorf("body = %v\nwant   %v", body, want)
	}
	if !strings.Contains(string(req.Body), `"mbit":1.5`) {
		t.Errorf("mbit was not sent as a number: %s", req.Body)
	}
}

func TestFill_DefaultsAndPlainDirectionNames(t *testing.T) {
	c := &Call{Method: "PUT", URL: "http://s.example/?m=${mbit}&l=${lossPct}&d=${latencyMs}&j=${jitterMs}&dir=${direction}"}
	req, err := Fill(c, Values{}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://s.example/?m=0&l=0&d=0&j=0&dir=both"; req.URL != want {
		t.Errorf("url = %s, want %s", req.URL, want)
	}
	if req.Body != nil || req.Headers != nil {
		t.Errorf("a call without body or headers got %q / %v", req.Body, req.Headers)
	}
	req, _ = Fill(c, Values{Direction: DirectionUpload}, "x")
	if !strings.HasSuffix(req.URL, "dir=upload") {
		t.Errorf("url = %s", req.URL)
	}
	c.DirectionValues = map[string]string{DirectionDownload: "dn"}
	req, _ = Fill(c, Values{Direction: DirectionUpload}, "x")
	if !strings.HasSuffix(req.URL, "dir=upload") {
		t.Errorf("an unmapped direction must keep its name: %s", req.URL)
	}
}

func TestFill_BodyThatCannotBeEncoded(t *testing.T) {
	c := &Call{Method: "POST", URL: "http://s.example/", Body: map[string]any{"bad": math.Inf(1)}}
	if _, err := Fill(c, Values{}, "x"); err == nil {
		t.Fatal("expected an encoding error")
	}
}

func newShaper(t *testing.T, rec http.Handler, devices func(base string) map[string]Device) (*Shaper, *[]string) {
	t.Helper()
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	path := writeConfig(t, t.TempDir(), jsonConfig(t, devices(srv.URL)), 0o600)
	var lines []string
	var mu sync.Mutex
	return &Shaper{Source: Source{Legacy: path}, DeviceID: "phone", Log: func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}}, &lines
}

func calls(base string) map[string]Device {
	return map[string]Device{"phone": {
		Set: &Call{Method: "POST", URL: base + "/v1/shape", Headers: map[string]string{"Authorization": "Bearer secret"},
			Body:            map[string]any{"ip": "192.0.2.15", "mbit": "${mbit}", "lossPct": "${lossPct}", "latencyMs": "${latencyMs}", "jitterMs": "${jitterMs}", "direction": "${direction}"},
			DirectionValues: map[string]string{DirectionDownload: "down", DirectionUpload: "up"}},
		Reset: &Call{Method: "DELETE", URL: base + "/v1/shape/192.0.2.15"},
	}}
}

func TestShaper_SetSendsTheWholeShape(t *testing.T) {
	rec := &recorder{}
	s, lines := newShaper(t, rec, calls)
	res, err := s.Set(context.Background(), Values{Mbit: 1.5, LatencyMs: 100, Direction: DirectionUpload})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 {
		t.Errorf("status = %d", res.Status)
	}
	got := rec.last(t)
	if got.Method != "POST" || got.Path != "/v1/shape" || got.ContentType != "application/json" || got.Auth != "Bearer secret" {
		t.Errorf("request = %+v", got)
	}
	if want := `{"direction":"up","ip":"192.0.2.15","jitterMs":0,"latencyMs":100,"lossPct":0,"mbit":1.5}`; got.Body != want {
		t.Errorf("body = %s, want %s", got.Body, want)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "traffic shape phone setTrafficShape: POST http://") ||
		!strings.Contains((*lines)[0], "(headers Authorization=[redacted]) -> 200 in ") || strings.Contains((*lines)[0], "secret") {
		t.Errorf("log = %q", *lines)
	}
}

func TestShaper_Reset(t *testing.T) {
	rec := &recorder{status: http.StatusNoContent}
	s, _ := newShaper(t, rec, calls)
	if _, err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t); got.Method != "DELETE" || got.Path != "/v1/shape/192.0.2.15" || got.Body != "" || got.ContentType != "" {
		t.Errorf("request = %+v", got)
	}
}

func TestShaper_NotConfigured(t *testing.T) {
	rec := &recorder{}
	s, _ := newShaper(t, rec, calls)
	s.DeviceID = "other"
	if _, err := s.Device(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("an unlisted device: %v", err)
	}
	for _, err := range []error{
		func() error { _, err := s.Set(context.Background(), Values{}); return err }(),
		func() error { _, err := s.Reset(context.Background()); return err }(),
	} {
		if !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "no traffic shaper is configured for other on this host") {
			t.Errorf("err = %v", err)
		}
	}
	missing := &Shaper{Source: Source{Legacy: filepath.Join(t.TempDir(), FileName)}, DeviceID: "phone"}
	if _, err := missing.Set(context.Background(), Values{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("missing file: err = %v", err)
	}
	if len(rec.reqs) != 0 {
		t.Errorf("an unconfigured device sent %d request(s)", len(rec.reqs))
	}
}

func TestShaper_ConfigErrors(t *testing.T) {
	rec := &recorder{}
	s, _ := newShaper(t, rec, calls)
	if _, err := s.Device(); err != nil {
		t.Fatalf("the configured device: %v", err)
	}
	if err := os.WriteFile(s.Source.Legacy, []byte(`{"devices":`), 0o600); err != nil {
		t.Fatal(err)
	}
	var ce *ConfigError
	if _, err := s.Device(); !errors.As(err, &ce) {
		t.Errorf("a malformed file: %v", err)
	}
	if _, err := s.Set(context.Background(), Values{}); !errors.As(err, &ce) {
		t.Errorf("Set err = %v", err)
	}
	if _, err := s.Reset(context.Background()); !errors.As(err, &ce) {
		t.Errorf("Reset err = %v", err)
	}
}

func TestShaper_ReadsTheFileOnEveryCall(t *testing.T) {
	rec := &recorder{}
	s, _ := newShaper(t, rec, calls)
	srvBase := strings.TrimSuffix(setURL(t, s), "/v1/shape")
	edited := calls(srvBase)
	edited["phone"].Set.URL = srvBase + "/v2/shape"
	if err := os.WriteFile(s.Source.Legacy, []byte(jsonConfig(t, edited)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(context.Background(), Values{}); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t); got.Path != "/v2/shape" {
		t.Errorf("path = %s; the edit was not picked up", got.Path)
	}
}

// setURL returns the device's configured setTrafficShape URL.
func setURL(t *testing.T, s *Shaper) string {
	t.Helper()
	d, err := s.Device()
	if err != nil {
		t.Fatal(err)
	}
	return d.Set.URL
}

func TestShaper_InvalidValuesSendNothing(t *testing.T) {
	rec := &recorder{}
	s, _ := newShaper(t, rec, calls)
	if _, err := s.Set(context.Background(), Values{LossPct: 150}); err == nil {
		t.Fatal("expected a validation error")
	}
	if len(rec.reqs) != 0 {
		t.Errorf("invalid values sent %d request(s)", len(rec.reqs))
	}
}

func TestShaper_Non2xxQuotesTheAnswer(t *testing.T) {
	rec := &recorder{status: http.StatusConflict, answer: "  shaper busy: " + strings.Repeat("x", 3000)}
	s, _ := newShaper(t, rec, calls)
	_, err := s.Set(context.Background(), Values{})
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Status != 409 || fe.Command != CommandSet {
		t.Fatalf("err = %v", err)
	}
	if len(fe.Excerpt) != excerptMax-2 || !strings.HasPrefix(fe.Excerpt, "shaper busy: ") {
		t.Errorf("excerpt is %d bytes: %.40q", len(fe.Excerpt), fe.Excerpt)
	}
	if !strings.Contains(err.Error(), "setTrafficShape: POST http://") || !strings.Contains(err.Error(), "answered 409: shaper busy") {
		t.Errorf("message = %.120s", err)
	}
	rec.answer = ""
	_, err = s.Set(context.Background(), Values{})
	if !strings.HasSuffix(err.Error(), "answered 409") {
		t.Errorf("message = %s", err)
	}
}

func TestShaper_Timeout(t *testing.T) {
	rec := &recorder{delay: 5 * time.Second}
	s, lines := newShaper(t, rec, calls)
	s.Timeout = 50 * time.Millisecond
	started := time.Now()
	_, err := s.Reset(context.Background())
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Status != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Errorf("the timeout took %s", time.Since(started))
	}
	if !strings.Contains(err.Error(), "resetTrafficShape: DELETE http://") || !strings.Contains((*lines)[0], "failed after") {
		t.Errorf("err = %v, log = %q", err, *lines)
	}
}

func TestShaper_DefaultTimeoutApplies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := &recorder{}
	s, _ := newShaper(t, rec, calls)
	if _, err := s.Set(ctx, Values{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestShaper_InsecureSkipVerify(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewTLSServer(rec)
	t.Cleanup(srv.Close)
	devices := calls(srv.URL)
	path := writeConfig(t, t.TempDir(), jsonConfig(t, devices), 0o600)
	s := &Shaper{Source: Source{Legacy: path}, DeviceID: "phone"}
	if _, err := s.Set(context.Background(), Values{}); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a self-signed shaper was trusted without insecureSkipVerify: %v", err)
	}
	devices["phone"].Set.InsecureSkipVerify = true
	if err := os.WriteFile(path, []byte(jsonConfig(t, devices)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(context.Background(), Values{}); err != nil {
		t.Fatal(err)
	}
}

func TestShaper_BadURLAfterFilling(t *testing.T) {
	s := &Shaper{Source: Source{Legacy: writeConfig(t, t.TempDir(), jsonConfig(t, map[string]Device{"phone": {
		Set:   &Call{Method: "POST", URL: "http://s.example/\x7f"},
		Reset: &Call{Method: "POST", URL: "http://s.example/"},
	}}), 0o600)}, DeviceID: "phone"}
	// The control character fails validation before any request is built.
	if _, err := s.Set(context.Background(), Values{}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRedaction(t *testing.T) {
	for raw, want := range map[string]string{
		"https://user:pw@s.example/x?y=1":  "https://[redacted]@s.example/x?[redacted]",
		"https://s.example/x":              "https://s.example/x",
		"https://s.example/a%20b?token=t1": "https://s.example/a%20b?[redacted]",
		"http://[::1":                      "[unparsable URL]",
	} {
		if got := redactURL(raw); got != want {
			t.Errorf("redactURL(%s) = %s, want %s", raw, got, want)
		}
	}
	if got := redactedHeaders(map[string]string{"X-B": "2", "Authorization": "Bearer t"}); got != " (headers Authorization=[redacted], X-B=[redacted])" {
		t.Errorf("redactedHeaders = %s", got)
	}
	if got := redactedHeaders(nil); got != "" {
		t.Errorf("redactedHeaders(nil) = %q", got)
	}
}

func TestShaper_LogsRedactedUserInfo(t *testing.T) {
	rec := &recorder{}
	s, lines := newShaper(t, rec, func(base string) map[string]Device {
		d := calls(strings.Replace(base, "http://", "http://user:pw@", 1))
		return d
	})
	if _, err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*lines)[0], "pw") || !strings.Contains((*lines)[0], "[redacted]@") {
		t.Errorf("log = %q", *lines)
	}
}

func TestErrorMessages(t *testing.T) {
	ce := &ConfigError{Path: "/h/network.json", Err: errors.New("boom")}
	if ce.Error() != "config /h/network.json: boom" || ce.Unwrap().Error() != "boom" {
		t.Errorf("ConfigError = %q", ce)
	}
	fe := &FetchError{Command: CommandSet, Method: "POST", URL: "http://s/", Err: errors.New("refused")}
	if fe.Error() != "setTrafficShape: POST http://s/: refused" || fe.Unwrap().Error() != "refused" {
		t.Errorf("FetchError = %q", fe)
	}
}

// A call that gets no answer never repeats the configured URL's user,
// password or query string, in the error a caller sees or in the log.
func TestShaper_NoAnswerHidesTheURLsSecrets(t *testing.T) {
	var lines []string
	path := writeConfig(t, t.TempDir(), jsonConfig(t, map[string]Device{"phone": {
		Set:   &Call{Method: "POST", URL: "http://shapeuser:S3cret@127.0.0.1:1/v1/shape?token=QTOKEN"},
		Reset: &Call{Method: "POST", URL: "http://127.0.0.1:1/"},
	}}), 0o600)
	s := &Shaper{Source: Source{Legacy: path}, DeviceID: "phone", Log: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}
	_, err := s.Set(context.Background(), Values{})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	for _, text := range append([]string{err.Error()}, lines...) {
		for _, secret := range []string{"shapeuser", "S3cret", "QTOKEN"} {
			if strings.Contains(text, secret) {
				t.Errorf("%q leaks %s", text, secret)
			}
		}
	}
	if !strings.Contains(err.Error(), "setTrafficShape: POST http://[redacted]@127.0.0.1:1/v1/shape?[redacted]: ") {
		t.Errorf("err = %v", err)
	}
}

// A value in the path is path-escaped, one in the query query-escaped.
func TestFill_PathAndQueryEscaping(t *testing.T) {
	c := &Call{Method: "PUT", URL: "https://s.example/dir/${direction}/${deviceId}?d=${direction}&id=${deviceId}",
		DirectionValues: map[string]string{DirectionBoth: "in out"}}
	req, err := Fill(c, Values{}, "avd:a/b")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://s.example/dir/in%20out/avd:a%2Fb?d=in+out&id=avd%3Aa%2Fb"; req.URL != want {
		t.Errorf("url = %s, want %s", req.URL, want)
	}
}

// Numbers written in a body reach the shaper exactly as written.
func TestParse_BodyNumbersPassThrough(t *testing.T) {
	cfg, err := Parse([]byte(`{"devices":{"d":{
		"setTrafficShape":{"method":"POST","url":"https://s.example/","body":{"pipe":9007199254740993,"big":1e21,"rate":"${mbit}"}},
		"resetTrafficShape":{"method":"POST","url":"https://s.example/","body":{"pipe":9007199254740993}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := cfg.Lookup("d")
	req, err := Fill(d.Set, Values{Mbit: 2}, "d")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"big":1e21,"pipe":9007199254740993,"rate":2}`; string(req.Body) != want {
		t.Errorf("body = %s, want %s", req.Body, want)
	}
}

func TestValueErrorAndDefaults(t *testing.T) {
	var ve *ValueError
	if err := (Values{LossPct: 101}).Validate(); !errors.As(err, &ve) {
		t.Fatalf("err = %#v", err)
	}
	if err := ValidateDirection("sideways"); !errors.As(err, &ve) || ve.Error() != `direction must be one of download, upload, both, got "sideways"` {
		t.Fatalf("err = %v", err)
	}
	if (Values{}).WithDefaults().Direction != DirectionBoth || (Values{Direction: DirectionUpload}).WithDefaults().Direction != DirectionUpload {
		t.Fatal("WithDefaults")
	}
}

// A bad URL is named in the error with its credentials redacted: the
// message reaches callers of any device on the host, and the log.
func TestParse_BadURLIsRedacted(t *testing.T) {
	_, err := Parse([]byte(`{"devices":{"d":{
		"setTrafficShape":{"method":"POST","url":"htps://user:secret@shaper.example/v1?token=abc"},
		"resetTrafficShape":{"method":"POST","url":"https://shaper.example/"}}}}`))
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "abc") || strings.Contains(err.Error(), "user") ||
		!strings.Contains(err.Error(), "url htps://[redacted]@shaper.example/v1?[redacted] is not an absolute http or https URL") {
		t.Fatalf("err = %v", err)
	}
}

// Windows has no owner-only mode bits; there the file's ACL governs.
func TestLoad_ModeCheckIsSkippedOnWindows(t *testing.T) {
	old := goos
	goos = "windows"
	t.Cleanup(func() { goos = old })
	path := writeConfig(t, t.TempDir(), `{"devices":{}}`, 0o644)
	if _, err := Load(path); err != nil {
		t.Fatalf("err = %v", err)
	}
}

// A redirect is the answer, not something to follow: a shaper's login
// redirect must fail the step, not read as success.
func TestShaper_ARedirectFails(t *testing.T) {
	target := &recorder{}
	tsrv := httptest.NewServer(target)
	t.Cleanup(tsrv.Close)
	rec := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tsrv.URL+"/login", http.StatusFound)
	})
	s, _ := newShaper(t, rec, calls)
	_, err := s.Set(context.Background(), Values{})
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Status != http.StatusFound {
		t.Fatalf("err = %v", err)
	}
	if len(target.reqs) != 0 {
		t.Fatalf("the redirect was followed: %+v", target.reqs)
	}
}

func TestParse_PlaceholderInTheHostIsRefused(t *testing.T) {
	for _, u := range []string{"http://shaper-${deviceId}.lab/set?token=SECRET", "http://${deviceId}/x", "https://u:p@h${mbit}:80/"} {
		_, err := Parse([]byte(`{"devices":{"d":{"setTrafficShape":{"method":"POST","url":"` + u + `"},"resetTrafficShape":{"method":"POST","url":"https://s.example/"}}}}`))
		if err == nil || !strings.Contains(err.Error(), "placeholders belong in the path or the query string, not the host") ||
			strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: err = %v", u, err)
		}
	}
}

// A URL Go cannot build a request from is named redacted, with the cause.
func TestShaper_AnUnbuildableURLIsRedacted(t *testing.T) {
	s := &Shaper{DeviceID: "d"}
	_, err := s.send(context.Background(), CommandSet, &Call{Method: "POST", URL: "http://h:bad port/x?token=SECRET"}, Values{})
	var fe *FetchError
	if !errors.As(err, &fe) || strings.Contains(err.Error(), "SECRET") || err.Error() != `setTrafficShape: POST [unparsable URL]: invalid port ":bad port" after host` {
		t.Fatalf("err = %v", err)
	}
}

// config.json lists hub devices: an id, and optionally a description and
// a trafficShaping pair. Every entry is shared, in file order; only one
// with both calls is shapeable.
func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"hubDevices":[
		{"id":"described","description":"  Spare Pixel on the left rack "},
		{"id":"both","description":"Lab iPhone","trafficShaping":{
			"setTrafficShape":{"method":"POST","url":"https://s.example/"},
			"resetTrafficShape":{"method":"DELETE","url":"https://s.example/"}}},
		{"id":"bare"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Shared, ",") != "described,both,bare" {
		t.Errorf("Shared = %v", cfg.Shared)
	}
	for _, c := range []struct {
		id, description string
		shapeable       bool
	}{
		{"described", "Spare Pixel on the left rack", false},
		{"both", "Lab iPhone", true},
		{"bare", "", false},
	} {
		d, ok := cfg.Lookup(c.id)
		if !ok || d.Description != c.description || d.Shapeable() != c.shapeable {
			t.Errorf("%s: ok=%v description=%q shapeable=%v", c.id, ok, d.Description, d.Shapeable())
		}
	}
	if empty, err := ParseConfig([]byte(`{}`)); err != nil || len(empty.Shared) != 0 {
		t.Errorf("an empty config: %+v %v", empty, err)
	}
}

func TestParseConfig_Validation(t *testing.T) {
	set := `"setTrafficShape":{"method":"POST","url":"https://s.example/"}`
	reset := `"resetTrafficShape":{"method":"DELETE","url":"https://s.example/"}`
	for _, c := range []struct{ name, json, want string }{
		{"no id", `{"hubDevices":[{"description":"x"}]}`, "hubDevices[0] has no id"},
		{"blank id", `{"hubDevices":[{"id":"  "}]}`, "hubDevices[0] has no id"},
		{"listed twice", `{"hubDevices":[{"id":"a"},{"id":"a"}]}`, "device a: is listed twice in hubDevices"},
		{"unknown entry key", `{"hubDevices":[{"id":"a","shared":true}]}`, `unknown field "shared"`},
		{"unknown top-level key", `{"hubDevices":[],"devices":{}}`, `unknown field "devices"`},
		{"set only", `{"hubDevices":[{"id":"a","trafficShaping":{` + set + `}}]}`, "device a: trafficShaping: has setTrafficShape but no resetTrafficShape"},
		{"empty trafficShaping", `{"hubDevices":[{"id":"a","trafficShaping":{}}]}`, "trafficShaping: has neither setTrafficShape nor resetTrafficShape"},
		{"bad call", `{"hubDevices":[{"id":"a","trafficShaping":{"setTrafficShape":{"method":"FETCH","url":"https://s.example/"},` + reset + `}}]}`, `setTrafficShape: method "FETCH"`},
		{"trailing data", `{"hubDevices":[]} {}`, "trailing data"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(c.json)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// Source reads config.json when it exists and network.json only when it
// does not; an explicit path is read as is, with no fallback.
func TestSource_ResolveAndLoad(t *testing.T) {
	home := t.TempDir()
	src := Source{Home: home}
	if p, legacy := src.Resolve(); p != filepath.Join(home, FileName) || !legacy {
		t.Errorf("neither file: %s %v", p, legacy)
	}
	if cfg, _, err := src.Load(); err != nil || len(cfg.Devices) != 0 {
		t.Errorf("neither file loads empty: %+v %v", cfg, err)
	}
	writeConfig(t, home, `{"devices":{"old":{"setTrafficShape":{"method":"POST","url":"https://s.example/"},"resetTrafficShape":{"method":"DELETE","url":"https://s.example/"}}}}`, 0o600)
	if cfg, p, err := src.Load(); err != nil || p != filepath.Join(home, FileName) || !cfg.Devices["old"].Shapeable() || len(cfg.Shared) != 0 {
		t.Errorf("network.json only: %+v %s %v", cfg, p, err)
	}
	cfgPath := filepath.Join(home, ConfigFileName)
	if err := os.WriteFile(cfgPath, []byte(`{"hubDevices":[{"id":"new","description":"From config.json"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, p, err := src.Load()
	if err != nil || p != cfgPath || cfg.Devices["new"].Description != "From config.json" {
		t.Fatalf("both files: %+v %s %v", cfg, p, err)
	}
	if _, ok := cfg.Lookup("old"); ok {
		t.Error("network.json was read beside config.json")
	}
	// A config.json that cannot be used is an error, not a fallback.
	if err := os.WriteFile(cfgPath, []byte(`{"hubDevices":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, p, err := src.Load(); err == nil || p != cfgPath {
		t.Errorf("a broken config.json: %s %v", p, err)
	}
	// Explicit paths, each in its own format, with no fallback.
	missing := filepath.Join(t.TempDir(), "absent.json")
	if p, legacy := (Source{Config: missing, Home: home}).Resolve(); p != missing || legacy {
		t.Errorf("--config: %s %v", p, legacy)
	}
	if cfg, _, err := (Source{Config: missing, Home: home}).Load(); err != nil || len(cfg.Devices) != 0 {
		t.Errorf("a missing --config file is empty: %+v %v", cfg, err)
	}
	if p, legacy := (Source{Legacy: missing, Home: home}).Resolve(); p != missing || !legacy {
		t.Errorf("--network-config: %s %v", p, legacy)
	}
}

// A device in config.json without trafficShaping cannot be shaped: both
// commands answer ErrNotConfigured and send nothing.
func TestShaper_ConfigDeviceWithoutShapingIsNotConfigured(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(path, []byte(`{"hubDevices":[{"id":"phone","description":"Lab phone"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Shaper{Source: Source{Config: path}, DeviceID: "phone"}
	if _, err := s.Device(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Device: %v", err)
	}
	if _, err := s.Set(context.Background(), Values{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Set: %v", err)
	}
	if _, err := s.Reset(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Reset: %v", err)
	}
	if len(rec.reqs) != 0 {
		t.Errorf("sent %d request(s)", len(rec.reqs))
	}
}

// A config.json device's calls are sent like network.json's.
func TestShaper_ConfigDeviceShapes(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), ConfigFileName)
	body := `{"hubDevices":[{"id":"phone","trafficShaping":{
		"setTrafficShape":{"method":"PUT","url":"` + srv.URL + `/shape/${deviceId}?mbit=${mbit}"},
		"resetTrafficShape":{"method":"DELETE","url":"` + srv.URL + `/shape/${deviceId}"}}}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Shaper{Source: Source{Home: filepath.Dir(path)}, DeviceID: "phone"}
	if _, err := s.Set(context.Background(), Values{Mbit: 2}); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t); got.Method != "PUT" || got.Path != "/shape/phone" || got.Query != "mbit=2" {
		t.Errorf("set = %+v", got)
	}
}

// A device with no calls of its own uses its Alias's.
func TestShaper_Alias(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	body := `{"hubDevices":[{"id":"serial","description":"no calls"},{"id":"avd:pix","trafficShaping":{
		"setTrafficShape":{"method":"PUT","url":"https://s.example/shape"},
		"resetTrafficShape":{"method":"DELETE","url":"https://s.example/shape"}}}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Shaper{Source: Source{Config: path}, DeviceID: "serial", Alias: "avd:pix"}
	if d, err := s.Device(); err != nil || d.Set.Method != "PUT" {
		t.Fatalf("serial through its alias: %+v %v", d, err)
	}
	s = &Shaper{Source: Source{Config: path}, DeviceID: "avd:pix"}
	if _, err := s.Device(); err != nil {
		t.Fatalf("own entry: %v", err)
	}
	s = &Shaper{Source: Source{Config: path}, DeviceID: "other", Alias: "avd:none"}
	if _, err := s.Device(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no entry, no alias: %v", err)
	}
}

// Unread names the <home> files that exist but are not read.
func TestSource_Unread(t *testing.T) {
	home := t.TempDir()
	cfg, legacy := filepath.Join(home, ConfigFileName), filepath.Join(home, FileName)
	other := filepath.Join(t.TempDir(), "lab.json")
	for _, p := range []string{legacy, other} {
		if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := (Source{Home: home}).Unread(); len(got) != 0 {
		t.Errorf("network.json alone: %v", got)
	}
	if got := (Source{Home: home, Config: other}).Unread(); strings.Join(got, ",") != legacy {
		t.Errorf("--config: %v", got)
	}
	if err := os.WriteFile(cfg, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (Source{Home: home}).Unread(); strings.Join(got, ",") != legacy {
		t.Errorf("config.json beside network.json: %v", got)
	}
	if got := (Source{Home: home, Legacy: other}).Unread(); strings.Join(got, ",") != cfg+","+legacy {
		t.Errorf("--network-config: %v", got)
	}
}

// Shapeable needs both calls, however the Device was made.
func TestDevice_ShapeableNeedsBothCalls(t *testing.T) {
	call := &Call{Method: "POST", URL: "https://s.example/"}
	for _, c := range []struct {
		d    Device
		want bool
	}{
		{Device{Set: call, Reset: call}, true},
		{Device{Set: call}, false},
		{Device{Reset: call}, false},
		{Device{Description: "x"}, false},
	} {
		if got := c.d.Shapeable(); got != c.want {
			t.Errorf("%+v: Shapeable() = %v", c.d, got)
		}
	}
}

// ParseConfig keeps a device's capabilities exactly as written, any JSON
// value, apart from the description; a null one is none, and it takes no
// part in validation.
func TestParseConfig_CapabilitiesKeptAsWritten(t *testing.T) {
	const caps = `{"teamstation": {"class": "ipad", "features": ["camera"]}, "anything": [1, 2], "big": 123456789012345678901234567890, "tiny": 1e400}`
	cfg, err := ParseConfig([]byte(`{"hubDevices": [
		{"id": "a", "description": "Lab iPad", "capabilities": ` + caps + `},
		{"id": "b", "capabilities": 7},
		{"id": "c", "capabilities": null},
		{"id": "d"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(cfg.Devices["a"].Capabilities); got != caps || cfg.Devices["a"].Description != "Lab iPad" {
		t.Fatalf("a: %q", got)
	}
	if got := string(cfg.Devices["b"].Capabilities); got != "7" {
		t.Fatalf("b: %q", got)
	}
	if cfg.Devices["c"].Capabilities != nil || cfg.Devices["d"].Capabilities != nil {
		t.Fatalf("c, d: %+v", cfg.Devices)
	}
	if _, err := ParseConfig([]byte(`{"hubDevices": [{"id": "a", "capabilities": {"x": }}]}`)); err == nil {
		t.Fatal("malformed capabilities parsed")
	}
}
