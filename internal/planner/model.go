package planner

import (
	"context"
	"errors"

	"github.com/kalanas210/runmesh/internal/gemini"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// geminiModel adapts the Gemini client to the pipeline's Model interface.
//
// The adapter is a few lines of field copying and one classification, and it is
// worth having: the pipeline's tests drive a Model that returns a fixed string,
// so nothing in the planning logic depends on Gemini existing — and a second
// provider is this file again, not a refactor.
type geminiModel struct{ c *gemini.Client }

// FromGemini wraps a Gemini client as a planner Model.
func FromGemini(c *gemini.Client) Model { return geminiModel{c: c} }

func (m geminiModel) Name() string { return "gemini/" + m.c.Model() }

func (m geminiModel) Generate(ctx context.Context, req Request) (Response, error) {
	out, err := m.c.Generate(ctx, gemini.Request{
		System: req.System,
		User:   req.User,
		Schema: req.Schema,
	})
	// The response is returned even alongside an error: a truncated or blocked
	// generation still spent tokens, and the trace is meant to account for
	// them rather than report a plan that cost nothing.
	return Response{
		Text:         out.Text,
		InputTokens:  out.InputTokens,
		OutputTokens: out.OutputTokens,
	}, classifyGemini(err)
}

// The ways a model can fail that a caller has to be able to tell apart, named
// without naming a provider.
//
// The API answers each one differently — a refused goal is the caller's to
// rephrase, a rate limit is a wait, and a model this deployment cannot use is
// an operator's to fix — and it has to decide that without importing any
// provider's error codes, or httpapi would learn one vendor's vocabulary and a
// second provider would be a change there as well. So each adapter translates
// its provider's codes into these, in one function, and the pipeline returns
// the result untouched.
//
// A classified error still unwraps to the model's own *runmesh.ToolError, so
// the provider's stable code stays available to errors.As, and its text is the
// model's text, unchanged.
var (
	// ErrModelRefused: the provider's safety filters blocked the goal, or the
	// answer to it. The request was well formed; the same text will be refused
	// again, and different text may not be.
	ErrModelRefused = errors.New("planner: the model refused this goal")

	// ErrModelRateLimited: the provider's quota ran out. Waiting fixes it, and
	// the ToolError carries the provider's own hint of how long.
	ErrModelRateLimited = errors.New("planner: the model's rate limit was reached")

	// ErrModelUnusable: this deployment's model settings do not work — a key
	// the provider rejects, or a model it does not serve. Model names are
	// retired on the provider's schedule, so this is the failure a working
	// deployment meets without anybody having changed it. No retry helps until
	// an operator does.
	ErrModelUnusable = errors.New("planner: the configured model cannot be used")
)

// classifyGemini files a Gemini failure under one of the categories above.
// Anything it does not recognise is returned as it came, still classified
// retryable or terminal by the client.
func classifyGemini(err error) error {
	var te *runmesh.ToolError
	if !errors.As(err, &te) {
		return err
	}
	switch te.Code {
	case gemini.CodePromptBlocked, gemini.CodeResponseBlocked:
		return classified{kind: ErrModelRefused, err: err}
	case gemini.CodeRateLimited:
		return classified{kind: ErrModelRateLimited, err: err}
	case gemini.CodeAuthFailed, gemini.CodeModelNotFound:
		return classified{kind: ErrModelUnusable, err: err}
	}
	return err
}

// classified pairs a model error with its category. It prints as the model's
// error alone, so the trace and the log read exactly as they did before the
// categories existed.
type classified struct{ kind, err error }

func (c classified) Error() string   { return c.err.Error() }
func (c classified) Unwrap() []error { return []error{c.kind, c.err} }
