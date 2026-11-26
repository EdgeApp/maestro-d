package aicheck

// jev.go: the first stage, TypeSafe's Jev (System One), one noul question.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
)

// JevPricePerToken is Jev's price per input token ($42 per billion); output
// is free.
const JevPricePerToken = 42e-9

// JevImageHeader turns on image input, which TypeSafe marks pre-production.
const JevImageHeader = "TypeSafe-Danger-Zone-Images"

// jevScreenRef names the screenshot inside the request; Jev matches
// references as plain text, so it is one no sentence contains.
const jevScreenRef = "maestro-d-screen-5f0c2a9e"

// jevEstimateTokens is a generous bound on a screenshot check's input.
const jevEstimateTokens = 4000

// jev asks Jev and reports whether its answer ends the waterfall.
func (r *run) jev(ctx context.Context, img image.Image, assertion string, passAt, failAt float64) bool {
	if r.keys.Jev == "" {
		r.res.Escalations = append(r.res.Escalations, Escalation{Model: ModelJev, Reason: ReasonNoKey})
		return false
	}
	if !r.affordable(ModelJev, jevEstimateTokens*JevPricePerToken) {
		return false
	}
	noul, cost, err := r.askJev(ctx, img, assertion)
	r.spend(cost)
	if err != nil {
		r.res.Escalations = append(r.res.Escalations, Escalation{Model: ModelJev, CostUSD: cost, Reason: ReasonError, Error: r.redact(errText(err))})
		return false
	}
	e := Escalation{Model: ModelJev, Confidence: ptr(noul), CostUSD: cost}
	observed := fmt.Sprintf("Jev noul %.3f that the assertion holds", noul)
	switch {
	case noul >= passAt:
		e.Verdict = Pass
		r.res.Escalations = append(r.res.Escalations, e)
		return r.answer(ModelJev, Pass, noul, observed)
	case noul <= failAt:
		e.Verdict = Fail
		r.res.Escalations = append(r.res.Escalations, e)
		return r.answer(ModelJev, Fail, noul, observed)
	}
	e.Verdict, e.Reason = Unsure, ReasonUncertain
	r.res.Escalations = append(r.res.Escalations, e)
	r.unsureFrom(ModelJev, noul, observed)
	return false
}

type jevAsset struct {
	Type       string `json:"type"`
	Format     string `json:"format"`
	DataBase64 string `json:"data_base64"`
}

type jevQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

type jevRequest struct {
	Model     string                 `json:"model"`
	State     map[string]string      `json:"state"`
	Assets    map[string]jevAsset    `json:"assets"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevResponse struct {
	Answers map[string]struct {
		Type string   `json:"type"`
		Noul *float64 `json:"noul"`
	} `json:"answers"`
	Usage struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

func (r *run) askJev(ctx context.Context, img image.Image, assertion string) (float64, float64, error) {
	body := jevRequest{
		Model: ModelJev,
		State: map[string]string{"screen": jevScreenRef},
		Assets: map[string]jevAsset{jevScreenRef: {
			Type: "image", Format: "png", DataBase64: base64.StdEncoding.EncodeToString(encodePNG(fitJev(img))),
		}},
		Questions: map[string]jevQuestion{"assertion": {
			Type:         "noul",
			Instructions: "`screen` is a screenshot of a phone, tablet, TV or browser. Is this statement true of what `screen` shows: " + assertion,
		}},
	}
	data, _ := json.Marshal(body)
	url := r.c.JevURL
	if url == "" {
		url = DefaultJevURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.keys.Jev)
	req.Header.Set(JevImageHeader, "true")
	resp, err := r.client().Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("jev: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("jev: HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var out jevResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, 0, fmt.Errorf("jev: unreadable answer: %w", err)
	}
	cost := float64(out.Usage.InputTokens) * JevPricePerToken
	a, ok := out.Answers["assertion"]
	if !ok || a.Noul == nil {
		return 0, cost, fmt.Errorf("jev: no noul in the answer: %s", snippet(raw))
	}
	return *a.Noul, cost, nil
}

// snippet is the start of a response body, for an error message.
func snippet(b []byte) string {
	const n = 300
	b = bytes.TrimSpace(b)
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
