package aicheck

// claude.go: the later stages, Claude Haiku, Sonnet and Opus through the
// Messages API (raw HTTP, as every other HTTP client in maestro-d is).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
)

// AnthropicVersion is the Messages API version sent.
const AnthropicVersion = "2026-10-21"

// claudeMaxTokens bounds an answer, thinking included.
const claudeMaxTokens = 2048

// Price is a model's price per million tokens.
type Price struct{ InputPerMTok, OutputPerMTok float64 }

// Prices are the first-party API rates of the waterfall's Claude models.
var Prices = map[string]Price{
	ModelHaiku:  {0.10, 0.50},
	ModelSonnet: {2.00, 10.00},
	ModelOpus:   {4.00, 20.00},
}

func (p Price) cost(in, out int) float64 {
	return (float64(in)*p.InputPerMTok + float64(out)*p.OutputPerMTok) / 1e6
}

const claudeSystem = `You judge whether a statement is true of a screenshot of a phone, tablet, TV or browser screen.
Look only at what the screenshot shows. Answer with JSON only: {"verdict": "pass" | "fail" | "unsure", "confidence": 0 to 1, "observed": "one or two sentences on what the screen shows that decides it"}.
"pass" means the statement is true of the screen, "fail" that it is false, "unsure" that the screenshot cannot settle it. confidence is how sure you are of the verdict.`

var claudeSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"verdict":    map[string]any{"type": "string", "enum": []string{Pass, Fail, Unsure}},
		"confidence": map[string]any{"type": "number"},
		"observed":   map[string]any{"type": "string"},
	},
	"required":             []string{"verdict", "confidence", "observed"},
	"additionalProperties": false,
}

// claude asks one Claude model and reports whether its answer ends the
// waterfall.
func (r *run) claude(ctx context.Context, img image.Image, assertion, model string, bar float64) bool {
	if r.keys.Anthropic == "" {
		r.res.Escalations = append(r.res.Escalations, Escalation{Model: model, Reason: ReasonNoKey})
		return false
	}
	fitted := fitClaude(img)
	b := fitted.Bounds()
	price := Prices[model]
	if !r.affordable(model, price.cost(b.Dx()*b.Dy()/750+800, claudeMaxTokens)) {
		return false
	}
	ans, cost, err := r.askClaude(ctx, fitted, assertion, model)
	r.spend(cost)
	if err != nil {
		r.res.Escalations = append(r.res.Escalations, Escalation{Model: model, CostUSD: cost, Reason: ReasonError, Error: r.redact(errText(err))})
		return false
	}
	e := Escalation{Model: model, Verdict: ans.Verdict, Confidence: ptr(ans.Confidence), CostUSD: cost}
	observed := r.redact(ans.Observed)
	if (ans.Verdict == Pass || ans.Verdict == Fail) && ans.Confidence >= bar {
		r.res.Escalations = append(r.res.Escalations, e)
		return r.answer(model, ans.Verdict, ans.Confidence, observed)
	}
	e.Reason = ReasonUncertain
	r.res.Escalations = append(r.res.Escalations, e)
	r.unsureFrom(model, ans.Confidence, observed)
	return false
}

type claudeAnswer struct {
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
	Observed   string  `json:"observed"`
}

type claudeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (r *run) askClaude(ctx context.Context, img image.Image, assertion, model string) (claudeAnswer, float64, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": claudeMaxTokens,
		"system":     claudeSystem,
		"output_config": map[string]any{
			"effort": "low",
			"format": map[string]any{"type": "json_schema", "schema": claudeSchema},
		},
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(encodePNG(img)),
				}},
				map[string]any{"type": "text", "text": "Statement: " + assertion},
			},
		}},
	}
	data, _ := json.Marshal(body)
	url := r.c.AnthropicURL
	if url == "" {
		url = DefaultAnthropicURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return claudeAnswer{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", r.keys.Anthropic)
	req.Header.Set("anthropic-version", AnthropicVersion)
	resp, err := r.client().Do(req)
	if err != nil {
		return claudeAnswer{}, 0, fmt.Errorf("%s: %w", model, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return claudeAnswer{}, 0, fmt.Errorf("%s: HTTP %d: %s", model, resp.StatusCode, snippet(raw))
	}
	var out claudeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return claudeAnswer{}, 0, fmt.Errorf("%s: unreadable answer: %w", model, err)
	}
	cost := Prices[model].cost(out.Usage.InputTokens, out.Usage.OutputTokens)
	if out.StopReason == "refusal" {
		return claudeAnswer{}, cost, fmt.Errorf("%s declined the request", model)
	}
	var text strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	ans, err := parseAnswer(text.String())
	if err != nil {
		return claudeAnswer{}, cost, fmt.Errorf("%s: %w", model, err)
	}
	return ans, cost, nil
}

// parseAnswer reads the JSON object in a model's text.
func parseAnswer(text string) (claudeAnswer, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return claudeAnswer{}, fmt.Errorf("no JSON answer in %q", snippet([]byte(text)))
	}
	var a claudeAnswer
	if err := json.Unmarshal([]byte(text[start:end+1]), &a); err != nil {
		return claudeAnswer{}, fmt.Errorf("unreadable JSON answer: %w", err)
	}
	switch a.Verdict {
	case Pass, Fail, Unsure:
	default:
		return claudeAnswer{}, fmt.Errorf("verdict %q is not pass, fail or unsure", a.Verdict)
	}
	return a, nil
}
