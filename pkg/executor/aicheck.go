package executor

// maestro-d: assertWithAI and assertNoDefectsWithAI. The executor runs
// them for every driver: the screenshot comes from the driver's own
// Screenshot(), and the waterfall (pkg/aicheck) judges it with the keys
// the session was given.

import (
	"context"
	"errors"
	"fmt"

	"github.com/devicelab-dev/maestro-runner/pkg/aicheck"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// aiRequest is the check a step asks for.
func aiRequest(step flow.Step) (aicheck.Request, error) {
	var opts flow.AIOptions
	var assertion string
	switch s := step.(type) {
	case *flow.AssertWithAIStep:
		opts, assertion = s.AIOptions, s.Assertion
	case *flow.AssertNoDefectsWithAIStep:
		opts, assertion = s.AIOptions, aicheck.NoDefectsAssertion
	default:
		return aicheck.Request{}, fmt.Errorf("not an AI check: %s", step.Type())
	}
	req := aicheck.Request{Assertion: assertion, BudgetUSD: opts.BudgetUSD}
	if t := opts.Thresholds; t != nil {
		req.Thresholds = aicheck.Thresholds{JevPass: t.JevPass, JevFail: t.JevFail, Haiku: t.Haiku, Sonnet: t.Sonnet, Opus: t.Opus}
	}
	return req, nil
}

// executeAICheck runs one AI check. Every verdict, fail and unsure
// included, is a successful step whose data is the result: only a missing
// screenshot or a malformed step fails it.
func (fr *FlowRunner) executeAICheck(step flow.Step) *core.CommandResult {
	req, err := aiRequest(step)
	if err != nil {
		return errorResult(err)
	}
	shot, err := fr.driver.Screenshot()
	if err != nil {
		return errorResult(fmt.Errorf("%s: screenshot failed: %w", step.Type(), err))
	}
	checker := fr.ai
	if checker == nil {
		checker = &aicheck.Checker{}
	}
	ctx := fr.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := checker.Check(ctx, shot, req)
	if errors.Is(err, aicheck.ErrNoAssertion) {
		return errorResult(&UsageError{Err: fmt.Errorf("%s: %w", step.Type(), err)})
	}
	if err != nil {
		return errorResult(fmt.Errorf("%s: %w", step.Type(), err))
	}
	msg := fmt.Sprintf("%s: %s", step.Type(), res.Verdict)
	if res.Model != "" {
		msg += fmt.Sprintf(" (%s, confidence %.2f)", res.Model, res.Confidence)
	}
	return &core.CommandResult{Success: true, Message: msg, Data: res}
}
