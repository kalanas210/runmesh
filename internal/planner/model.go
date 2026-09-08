package planner

import (
	"context"

	"github.com/kalanas210/runmesh/internal/gemini"
)

// geminiModel adapts the Gemini client to the pipeline's Model interface.
//
// The adapter is three lines of field copying, and it is worth having: the
// pipeline's tests drive a Model that returns a fixed string, so nothing in the
// planning logic depends on Gemini existing — and a second provider is this
// file again, not a refactor.
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
	}, err
}
