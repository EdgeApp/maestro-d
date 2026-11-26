// Package aicheck judges a screenshot against a plain-words assertion,
// escalating from the cheapest model to the most expensive until one
// answers with confidence: TypeSafe's Jev, then Claude Haiku, Sonnet and
// Opus. maestro-d's executor runs it for assertWithAI and
// assertNoDefectsWithAI on every driver.
//
// Keys never leave this package except in the request headers to their own
// API: results, errors and logs carry no key.
package aicheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Verdicts.
const (
	Pass   = "pass"
	Fail   = "fail"
	Unsure = "unsure"
)

// Models, in waterfall order.
const (
	ModelJev    = "jev-1.14.0"
	ModelHaiku  = "claude-haiku-5-5"
	ModelSonnet = "claude-sonnet-5-5"
	ModelOpus   = "claude-opus-5-5"
)

// Escalation reasons for a stage that gave no confident answer.
const (
	ReasonNoKey     = "no-key"
	ReasonBudget    = "budget"
	ReasonError     = "error"
	ReasonUncertain = "below-threshold"
)

// NoDefectsAssertion is assertNoDefectsWithAI's built-in assertion.
const NoDefectsAssertion = "The screen shows no visual defect: nothing blank, clipped, overlapping, garbled or showing an error"

// Default endpoints.
const (
	DefaultJevURL       = "https://api.typesafe.ai/v1/systemone"
	DefaultAnthropicURL = "https://api.anthropic.com/v1/messages"
)

// Keys are the API keys a check spends. Own marks the daemon's own keys
// (its config's ai block): a request's budget is capped at MaxBudgetUSD,
// which is also the budget when the request names none.
type Keys struct {
	Jev       string
	Anthropic string
	Own       bool
	// MaxBudgetUSD is the cap on Own keys (0 = none).
	MaxBudgetUSD float64
}

// Thresholds are the waterfall's stopping points; nil takes the default.
type Thresholds struct {
	JevPass, JevFail, Haiku, Sonnet, Opus *float64
}

// Defaults.
const (
	DefaultJevPass    = 0.7
	DefaultJevFail    = 0.3
	DefaultConfidence = 0.7
)

func pick(v *float64, def float64) float64 {
	if v != nil {
		return *v
	}
	return def
}

// Request is one check.
type Request struct {
	Assertion string
	// BudgetUSD is the caller's cap on what the check spends (0 = none).
	BudgetUSD  float64
	Thresholds Thresholds
}

// Escalation is one stage the waterfall reached: the model asked (or
// skipped), its answer, what it cost, and why the waterfall went on (or
// stopped, for budget).
type Escalation struct {
	Model      string   `json:"model"`
	Verdict    string   `json:"verdict,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	CostUSD    float64  `json:"costUsd"`
	Reason     string   `json:"reason,omitempty"`
	// Error describes a failed call (no key ever appears in it).
	Error string `json:"error,omitempty"`
}

// Result is a check's answer. Confidence is Jev's noul or the model's own
// confidence; Model is the one that answered ("" when none did).
type Result struct {
	Verdict     string       `json:"verdict"`
	Confidence  float64      `json:"confidence"`
	Model       string       `json:"model"`
	Escalations []Escalation `json:"escalations"`
	Observed    string       `json:"observed"`
	CostUSD     float64      `json:"costUsd"`
}

// Checker runs checks. The zero value is not usable; set Keys.
type Checker struct {
	// Keys returns the keys at check time (a key file is read then).
	Keys func() (Keys, error)
	// Endpoints; empty takes the defaults.
	JevURL, AnthropicURL string
	HTTP                 *http.Client
}

// ErrNoAssertion is a check with nothing to check.
var ErrNoAssertion = errors.New("assertion is required")

// Budget is the budget a request runs under: the caller's, capped on the
// daemon's own keys. 0 is no budget.
func (k Keys) Budget(requested float64) float64 {
	if !k.Own || k.MaxBudgetUSD <= 0 {
		return requested
	}
	if requested <= 0 || requested > k.MaxBudgetUSD {
		return k.MaxBudgetUSD
	}
	return requested
}

// Check judges screenshot (PNG or JPEG) against req.Assertion. Only an
// unusable request or screenshot is an error: every verdict, unsure
// included, is a Result.
func (c *Checker) Check(ctx context.Context, screenshot []byte, req Request) (*Result, error) {
	assertion := strings.TrimSpace(req.Assertion)
	if assertion == "" {
		return nil, ErrNoAssertion
	}
	img, err := decodeImage(screenshot)
	if err != nil {
		return nil, err
	}
	var keys Keys
	if c.Keys != nil {
		if keys, err = c.Keys(); err != nil {
			return nil, err
		}
	}
	r := &run{c: c, keys: keys, budget: keys.Budget(req.BudgetUSD), res: &Result{Verdict: Unsure, Escalations: []Escalation{}}}
	t := req.Thresholds
	if r.jev(ctx, img, assertion, pick(t.JevPass, DefaultJevPass), pick(t.JevFail, DefaultJevFail)) {
		return r.res, nil
	}
	for _, stage := range []struct {
		model string
		bar   float64
	}{
		{ModelHaiku, pick(t.Haiku, DefaultConfidence)},
		{ModelSonnet, pick(t.Sonnet, DefaultConfidence)},
		{ModelOpus, pick(t.Opus, DefaultConfidence)},
	} {
		if r.claude(ctx, img, assertion, stage.model, stage.bar) {
			return r.res, nil
		}
	}
	return r.res, nil
}

type run struct {
	c       *Checker
	keys    Keys
	budget  float64
	res     *Result
	stopped bool // the budget ran out: no later stage is asked
}

func (r *run) client() *http.Client {
	if r.c.HTTP != nil {
		return r.c.HTTP
	}
	return &http.Client{Timeout: 90 * time.Second}
}

// affordable reports whether a call estimated at est fits the budget, and
// records the stop when it does not.
func (r *run) affordable(model string, est float64) bool {
	if r.stopped {
		return false
	}
	if r.budget > 0 && r.res.CostUSD+est > r.budget {
		r.res.Escalations = append(r.res.Escalations, Escalation{Model: model, Reason: ReasonBudget})
		r.stopped = true
		return false
	}
	return true
}

// spend adds a call's cost.
func (r *run) spend(cost float64) { r.res.CostUSD += cost }

// answer records the answering stage and stops the waterfall.
func (r *run) answer(model, verdict string, confidence float64, observed string) bool {
	r.res.Verdict, r.res.Confidence, r.res.Model, r.res.Observed = verdict, confidence, model, observed
	return true
}

// unsureFrom keeps the last uncertain answer as the result when nothing
// more confident follows.
func (r *run) unsureFrom(model string, confidence float64, observed string) {
	r.res.Verdict, r.res.Confidence, r.res.Model, r.res.Observed = Unsure, confidence, model, observed
}

// redact removes the keys from a message bound for a result or a log.
func (r *run) redact(msg string) string {
	for _, k := range []string{r.keys.Jev, r.keys.Anthropic} {
		if k != "" {
			msg = strings.ReplaceAll(msg, k, "[redacted]")
		}
	}
	return msg
}

func ptr(v float64) *float64 { return &v }

func errText(err error) string {
	return fmt.Sprint(err)
}
