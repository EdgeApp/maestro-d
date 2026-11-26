package aicheck_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/aicheck"
	"github.com/devicelab-dev/maestro-runner/pkg/aicheck/aichecktest"
)

const (
	fakeJevKey       = "jev-FAKE-KEY-0001"
	fakeAnthropicKey = "sk-ant-FAKE-KEY-0002"
)

type A = aichecktest.Answer

func pngOf(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

var screen = pngOf(400, 800)

func checker(fake *aichecktest.Server, keys aicheck.Keys) *aicheck.Checker {
	return &aicheck.Checker{
		Keys:         func() (aicheck.Keys, error) { return keys, nil },
		JevURL:       fake.JevURL(),
		AnthropicURL: fake.AnthropicURL(),
	}
}

var both = aicheck.Keys{Jev: fakeJevKey, Anthropic: fakeAnthropicKey}

func check(t *testing.T, c *aicheck.Checker, req aicheck.Request) *aicheck.Result {
	t.Helper()
	if req.Assertion == "" {
		req.Assertion = "A sign-in form is on screen"
	}
	res, err := c.Check(context.Background(), screen, req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func models(res *aicheck.Result) []string {
	var out []string
	for _, e := range res.Escalations {
		out = append(out, e.Model)
	}
	return out
}

func TestJevStops(t *testing.T) {
	for _, tc := range []struct {
		noul    float64
		verdict string
		model   string
		asked   []string
	}{
		{0.7, aicheck.Pass, aicheck.ModelJev, []string{aicheck.ModelJev}},
		{0.3, aicheck.Fail, aicheck.ModelJev, []string{aicheck.ModelJev}},
		{0.5, aicheck.Pass, aicheck.ModelHaiku, []string{aicheck.ModelJev, aicheck.ModelHaiku}},
	} {
		fake := aichecktest.New(t, map[string]A{
			aicheck.ModelJev:   {Noul: aichecktest.Noul(tc.noul), InputTokens: 1000},
			aicheck.ModelHaiku: {Verdict: aicheck.Pass, Confidence: 0.9, Observed: "a form", InputTokens: 1000, OutputTokens: 100},
		})
		res := check(t, checker(fake, both), aicheck.Request{})
		if res.Verdict != tc.verdict || res.Model != tc.model || !reflect.DeepEqual(fake.Models(), tc.asked) || !reflect.DeepEqual(models(res), tc.asked) {
			t.Fatalf("noul %v: %+v asked %v", tc.noul, res, fake.Models())
		}
		if res.Escalations[0].Confidence == nil || *res.Escalations[0].Confidence != tc.noul || res.Escalations[0].CostUSD != 1000*aicheck.JevPricePerToken {
			t.Fatalf("jev stage %+v", res.Escalations[0])
		}
		if tc.model == aicheck.ModelJev && res.Confidence != tc.noul {
			t.Fatalf("confidence %v", res.Confidence)
		}
		if tc.noul == 0.5 && res.Escalations[0].Reason != aicheck.ReasonUncertain {
			t.Fatalf("reason %+v", res.Escalations[0])
		}
	}
}

func TestJevErrorsEscalate(t *testing.T) {
	for _, jev := range []A{
		{Status: http.StatusBadRequest, Body: `{"error":"image rejected"}`},
		{Body: `not json`},
		{Noul: nil},
	} {
		fake := aichecktest.New(t, map[string]A{
			aicheck.ModelJev:   jev,
			aicheck.ModelHaiku: {Verdict: aicheck.Fail, Confidence: 0.8},
		})
		res := check(t, checker(fake, both), aicheck.Request{})
		if res.Verdict != aicheck.Fail || res.Model != aicheck.ModelHaiku || res.Escalations[0].Reason != aicheck.ReasonError || res.Escalations[0].Error == "" {
			t.Fatalf("%+v", res)
		}
	}
	// An unreachable Jev escalates too.
	fake := aichecktest.New(t, map[string]A{aicheck.ModelHaiku: {Verdict: aicheck.Pass, Confidence: 0.8}})
	c := checker(fake, both)
	c.JevURL = "http://127.0.0.1:1/v1/systemone"
	if res := check(t, c, aicheck.Request{}); res.Model != aicheck.ModelHaiku || !strings.Contains(res.Escalations[0].Error, "jev:") {
		t.Fatalf("%+v", res)
	}
}

func TestClaudeStagesStopAtTheirBar(t *testing.T) {
	low := A{Verdict: aicheck.Pass, Confidence: 0.69}
	hit := A{Verdict: aicheck.Pass, Confidence: 0.7, Observed: "seen"}
	for _, tc := range []struct {
		haiku, sonnet, opus A
		verdict, model      string
		n                   int
	}{
		{hit, low, low, aicheck.Pass, aicheck.ModelHaiku, 2},
		{low, hit, low, aicheck.Pass, aicheck.ModelSonnet, 3},
		{low, low, hit, aicheck.Pass, aicheck.ModelOpus, 4},
		{low, low, A{Verdict: aicheck.Fail, Confidence: 0.5, Observed: "hm"}, aicheck.Unsure, aicheck.ModelOpus, 4},
		{low, low, A{Verdict: aicheck.Unsure, Confidence: 0.95}, aicheck.Unsure, aicheck.ModelOpus, 4},
	} {
		fake := aichecktest.New(t, map[string]A{
			aicheck.ModelJev: {Noul: aichecktest.Noul(0.5)}, aicheck.ModelHaiku: tc.haiku, aicheck.ModelSonnet: tc.sonnet, aicheck.ModelOpus: tc.opus,
		})
		res := check(t, checker(fake, both), aicheck.Request{})
		if res.Verdict != tc.verdict || res.Model != tc.model || len(res.Escalations) != tc.n {
			t.Fatalf("%+v", res)
		}
	}
}

func TestThresholdsOverrideEachDefault(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	fake := aichecktest.New(t, map[string]A{
		aicheck.ModelJev:    {Noul: aichecktest.Noul(0.99)},
		aicheck.ModelHaiku:  {Verdict: aicheck.Pass, Confidence: 1},
		aicheck.ModelSonnet: {Verdict: aicheck.Pass, Confidence: 1},
		aicheck.ModelOpus:   {Verdict: aicheck.Pass, Confidence: 0.75},
	})
	res := check(t, checker(fake, both), aicheck.Request{Thresholds: aicheck.Thresholds{JevPass: f(1.0), JevFail: f(0.0), Haiku: f(1.01), Sonnet: f(1.01)}})
	want := []string{aicheck.ModelJev, aicheck.ModelHaiku, aicheck.ModelSonnet, aicheck.ModelOpus}
	if res.Verdict != aicheck.Pass || res.Model != aicheck.ModelOpus || !reflect.DeepEqual(models(res), want) {
		t.Fatalf("%+v", res)
	}
	// Opus's own bar too.
	res = check(t, checker(fake, both), aicheck.Request{Thresholds: aicheck.Thresholds{JevPass: f(1.0), JevFail: f(0.0), Haiku: f(1.01), Sonnet: f(1.01), Opus: f(0.8)}})
	if res.Verdict != aicheck.Unsure {
		t.Fatalf("%+v", res)
	}
	// A lower Jev pass bar stops at a noul the default would escalate.
	fake.Set(aicheck.ModelJev, A{Noul: aichecktest.Noul(0.6)})
	if res = check(t, checker(fake, both), aicheck.Request{Thresholds: aicheck.Thresholds{JevPass: f(0.6)}}); res.Model != aicheck.ModelJev || res.Verdict != aicheck.Pass {
		t.Fatalf("%+v", res)
	}
	fake.Set(aicheck.ModelJev, A{Noul: aichecktest.Noul(0.4)})
	if res = check(t, checker(fake, both), aicheck.Request{Thresholds: aicheck.Thresholds{JevFail: f(0.4)}}); res.Model != aicheck.ModelJev || res.Verdict != aicheck.Fail {
		t.Fatalf("%+v", res)
	}
}

func TestClaudeErrorsEscalate(t *testing.T) {
	fake := aichecktest.New(t, map[string]A{
		aicheck.ModelJev:    {Status: 529, Body: "overloaded"},
		aicheck.ModelHaiku:  {Refusal: true, Text: "no"},
		aicheck.ModelSonnet: {Text: "I think it passes"},
		aicheck.ModelOpus:   {Text: `{"verdict": "maybe", "confidence": 1}`},
	})
	res := check(t, checker(fake, both), aicheck.Request{})
	if res.Verdict != aicheck.Unsure || res.Model != "" || len(res.Escalations) != 4 {
		t.Fatalf("%+v", res)
	}
	for _, e := range res.Escalations {
		if e.Reason != aicheck.ReasonError {
			t.Fatalf("%+v", e)
		}
	}
	for _, text := range []string{`{"verdict": `, `x`} {
		fake.Set(aicheck.ModelOpus, A{Text: text})
		if res := check(t, checker(fake, both), aicheck.Request{}); res.Escalations[3].Reason != aicheck.ReasonError {
			t.Fatalf("%+v", res)
		}
	}
	fake.Set(aicheck.ModelOpus, A{Status: 401, Body: `{"error":"bad key"}`})
	fake.Set(aicheck.ModelSonnet, A{Body: "not json"})
	if res := check(t, checker(fake, both), aicheck.Request{}); !strings.Contains(res.Escalations[3].Error, "HTTP 401") || !strings.Contains(res.Escalations[2].Error, "unreadable") {
		t.Fatalf("%+v", res)
	}
	c := checker(fake, both)
	c.AnthropicURL = "http://127.0.0.1:1/v1/messages"
	if res := check(t, c, aicheck.Request{}); res.Escalations[1].Reason != aicheck.ReasonError {
		t.Fatalf("%+v", res)
	}
}

func TestMissingKeysAreSkipped(t *testing.T) {
	fake := aichecktest.New(t, map[string]A{
		aicheck.ModelJev:   {Noul: aichecktest.Noul(0.9)},
		aicheck.ModelHaiku: {Verdict: aicheck.Pass, Confidence: 0.9},
	})
	res := check(t, checker(fake, aicheck.Keys{Anthropic: fakeAnthropicKey}), aicheck.Request{})
	if res.Model != aicheck.ModelHaiku || res.Escalations[0].Reason != aicheck.ReasonNoKey || res.Escalations[0].Model != aicheck.ModelJev {
		t.Fatalf("%+v", res)
	}
	res = check(t, checker(fake, aicheck.Keys{}), aicheck.Request{})
	if res.Verdict != aicheck.Unsure || res.Model != "" || len(res.Escalations) != 4 || len(fake.Requests()) != 1 {
		t.Fatalf("%+v %d", res, len(fake.Requests()))
	}
	for _, e := range res.Escalations {
		if e.Reason != aicheck.ReasonNoKey {
			t.Fatalf("%+v", e)
		}
	}
	// A nil key source is no keys.
	c := checker(fake, both)
	c.Keys = nil
	if res := check(t, c, aicheck.Request{}); res.Verdict != aicheck.Unsure {
		t.Fatalf("%+v", res)
	}
	c.Keys = func() (aicheck.Keys, error) { return aicheck.Keys{}, errors.New("key file unreadable") }
	if _, err := c.Check(context.Background(), screen, aicheck.Request{Assertion: "x"}); err == nil {
		t.Fatal("key error not returned")
	}
}

func TestBudgetStops(t *testing.T) {
	fake := aichecktest.New(t, map[string]A{
		aicheck.ModelJev:    {Noul: aichecktest.Noul(0.5)},
		aicheck.ModelHaiku:  {Verdict: aicheck.Pass, Confidence: 0.5, InputTokens: 1000, OutputTokens: 100},
		aicheck.ModelSonnet: {Verdict: aicheck.Pass, Confidence: 0.9},
	})
	res := check(t, checker(fake, both), aicheck.Request{BudgetUSD: 0.005})
	last := res.Escalations[len(res.Escalations)-1]
	if res.Verdict != aicheck.Unsure || last.Model != aicheck.ModelSonnet || last.Reason != aicheck.ReasonBudget || !reflect.DeepEqual(fake.Models(), []string{aicheck.ModelJev, aicheck.ModelHaiku}) {
		t.Fatalf("%+v %v", res, fake.Models())
	}
	if want := 1000*0.10/1e6 + 100*0.50/1e6; res.CostUSD < want || res.CostUSD > want+1e-6 {
		t.Fatalf("cost %v", res.CostUSD)
	}
	// A budget too small for Jev stops before any call.
	res = check(t, checker(fake, both), aicheck.Request{BudgetUSD: 1e-9})
	if res.Verdict != aicheck.Unsure || len(res.Escalations) != 1 || res.Escalations[0].Reason != aicheck.ReasonBudget {
		t.Fatalf("%+v", res)
	}
}

func TestBudgetCapOnOwnKeys(t *testing.T) {
	k := aicheck.Keys{Own: true, MaxBudgetUSD: 0.5}
	for in, want := range map[float64]float64{0: 0.5, 2: 0.5, 0.1: 0.1} {
		if got := k.Budget(in); got != want {
			t.Fatalf("Budget(%v) = %v", in, got)
		}
	}
	// Attach keys are not capped.
	if got := (aicheck.Keys{MaxBudgetUSD: 0.5}).Budget(2); got != 2 {
		t.Fatal(got)
	}
	// The cap stops a larger client budget's spend.
	fake := aichecktest.New(t, map[string]A{
		aicheck.ModelJev:    {Noul: aichecktest.Noul(0.5)},
		aicheck.ModelHaiku:  {Verdict: aicheck.Pass, Confidence: 0.5},
		aicheck.ModelSonnet: {Verdict: aicheck.Pass, Confidence: 0.9},
	})
	ownKeys := both
	ownKeys.Own, ownKeys.MaxBudgetUSD = true, 0.005
	own := checker(fake, ownKeys)
	res := check(t, own, aicheck.Request{BudgetUSD: 10})
	if res.Escalations[len(res.Escalations)-1].Reason != aicheck.ReasonBudget {
		t.Fatalf("%+v", res)
	}
}

func TestBadRequests(t *testing.T) {
	c := &aicheck.Checker{}
	if _, err := c.Check(context.Background(), screen, aicheck.Request{Assertion: "  "}); !errors.Is(err, aicheck.ErrNoAssertion) {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background(), []byte("not an image"), aicheck.Request{Assertion: "x"}); err == nil {
		t.Fatal("bad image accepted")
	}
}

func decodeAsset(t *testing.T, r aichecktest.Request) image.Image {
	t.Helper()
	assets := r.Body["assets"].(map[string]any)
	for _, a := range assets {
		data, _ := base64.StdEncoding.DecodeString(a.(map[string]any)["data_base64"].(string))
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	t.Fatal("no asset")
	return nil
}

func TestJevRequestShape(t *testing.T) {
	for _, tc := range []struct {
		w, h       int
		wantW, wan int
	}{
		{1, 1, 280, 280},
		{4000, 2000, 1568, 784},
		{100, 300, 280, 840},
		{3000, 100, 1568, 280}, // padded: 1568×52 then 280 high
		{1170, 2532, 725, 1568},
		{500, 600, 500, 600},
	} {
		fake := aichecktest.New(t, map[string]A{aicheck.ModelJev: {Noul: aichecktest.Noul(0.9)}})
		if _, err := checker(fake, both).Check(context.Background(), pngOf(tc.w, tc.h), aicheck.Request{Assertion: "x"}); err != nil {
			t.Fatal(err)
		}
		r := fake.Requests()[0]
		b := decodeAsset(t, r).Bounds()
		if b.Dx() != tc.wantW || b.Dy() != tc.wan {
			t.Fatalf("%dx%d -> %dx%d", tc.w, tc.h, b.Dx(), b.Dy())
		}
		if b.Dx() < aicheck.JevMinSide || b.Dx() > aicheck.JevMaxSide || b.Dy() < aicheck.JevMinSide || b.Dy() > aicheck.JevMaxSide {
			t.Fatalf("out of bounds %v", b)
		}
	}
	fake := aichecktest.New(t, map[string]A{aicheck.ModelJev: {Noul: aichecktest.Noul(0.5)}, aicheck.ModelHaiku: {Verdict: aicheck.Pass, Confidence: 0.5},
		aicheck.ModelSonnet: {Verdict: aicheck.Pass, Confidence: 0.5}, aicheck.ModelOpus: {Verdict: aicheck.Pass, Confidence: 0.5}})
	for i := 0; i < 3; i++ {
		check(t, checker(fake, both), aicheck.Request{})
	}
	for _, r := range fake.Requests() {
		switch r.Path {
		case "/v1/systemone":
			if r.Header.Get(aicheck.JevImageHeader) != "true" || r.Header.Get("Authorization") != "Bearer "+fakeJevKey || r.Model != aicheck.ModelJev {
				t.Fatalf("jev request %v", r.Header)
			}
			q := r.Body["questions"].(map[string]any)["assertion"].(map[string]any)
			if q["type"] != "noul" || !strings.Contains(q["instructions"].(string), "A sign-in form is on screen") {
				t.Fatalf("%v", q)
			}
		case "/v1/messages":
			if r.Header.Get("x-api-key") != fakeAnthropicKey || r.Header.Get("anthropic-version") != aicheck.AnthropicVersion || r.Header.Get(aicheck.JevImageHeader) != "" {
				t.Fatalf("claude request %v", r.Header)
			}
		}
	}
}

func TestClaudeImageIsDownscaled(t *testing.T) {
	fake := aichecktest.New(t, map[string]A{aicheck.ModelHaiku: {Verdict: aicheck.Pass, Confidence: 0.9}})
	big := image.NewRGBA(image.Rect(0, 0, 2000, 3000))
	var jb bytes.Buffer
	_ = jpeg.Encode(&jb, big, nil) // a JPEG screenshot is read too
	if _, err := checker(fake, aicheck.Keys{Anthropic: fakeAnthropicKey}).Check(context.Background(), jb.Bytes(), aicheck.Request{Assertion: "x"}); err != nil {
		t.Fatal(err)
	}
	msgs := fake.Requests()[0].Body["messages"].([]any)
	src := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	data, _ := base64.StdEncoding.DecodeString(src["data"].(string))
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Height > 1568 || cfg.Width*cfg.Height > 1_150_000 {
		t.Fatalf("%+v %v", cfg, err)
	}
	// A small one is sent as it is.
	fake2 := aichecktest.New(t, map[string]A{aicheck.ModelHaiku: {Verdict: aicheck.Pass, Confidence: 0.9}})
	if _, err := checker(fake2, aicheck.Keys{Anthropic: fakeAnthropicKey}).Check(context.Background(), pngOf(300, 200), aicheck.Request{Assertion: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestNoKeyInResultOrErrors(t *testing.T) {
	// A server that echoes the key in its error body.
	fake := aichecktest.New(t, map[string]A{
		aicheck.ModelJev:    {Status: 401, Body: "bad key " + fakeJevKey},
		aicheck.ModelHaiku:  {Status: 401, Body: "bad key " + fakeAnthropicKey},
		aicheck.ModelSonnet: {Verdict: aicheck.Unsure, Confidence: 0.1, Observed: "the screen shows " + fakeAnthropicKey},
		aicheck.ModelOpus:   {Verdict: aicheck.Pass, Confidence: 0.9, Observed: "shown: " + fakeAnthropicKey},
	})
	res := check(t, checker(fake, both), aicheck.Request{})
	out, _ := json.Marshal(res)
	if strings.Contains(string(out), fakeJevKey) || strings.Contains(string(out), fakeAnthropicKey) || !strings.Contains(string(out), "[redacted]") {
		t.Fatalf("%s", out)
	}
}

func TestDefaultEndpointsAndClient(t *testing.T) {
	// The default endpoints are the real APIs: with no key nothing is sent.
	c := &aicheck.Checker{Keys: func() (aicheck.Keys, error) { return aicheck.Keys{}, nil }}
	if res, err := c.Check(context.Background(), screen, aicheck.Request{Assertion: "x"}); err != nil || res.Verdict != aicheck.Unsure {
		t.Fatal(res, err)
	}
	// A cancelled context fails the calls, which escalate.
	fake := aichecktest.New(t, map[string]A{})
	cc := checker(fake, both)
	cc.HTTP = &http.Client{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := cc.Check(ctx, screen, aicheck.Request{Assertion: "x"})
	if err != nil || res.Verdict != aicheck.Unsure || res.Escalations[0].Reason != aicheck.ReasonError {
		t.Fatalf("%+v %v", res, err)
	}
}
