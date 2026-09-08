// Package gemini is a client for the Gemini Developer API, written against
// net/http and encoding/json and nothing else.
//
// The official SDK was not used, and that is a considered choice rather than
// stubbornness. What this package needs is one endpoint, one request shape and
// one response shape; what the SDK brings is a transitive dependency tree
// larger than the rest of this repository, into a binary whose whole argument
// (ADR 0007) is that it has almost none. The parts that are genuinely hard here
// — schema-constrained output, safety blocks, token accounting, retry
// classification — are hard in exactly the same way with an SDK, and are more
// legible without one.
//
// The API surface used:
//
//	POST {base}/v1beta/models/{model}:generateContent
//	x-goog-api-key: <key>
//
// One method, because the planner needs one thing: turn a prompt into JSON that
// conforms to a schema. Everything else Gemini can do is out of scope.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// DefaultBaseURL is the public endpoint. It is configurable so a test can point
// at an httptest server and so a deployment can front the API with a proxy —
// which is how an organisation puts logging, quota and key custody in front of
// a model without touching this code.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

// DefaultModel is a small, fast, cheap model, and the plan's own advice is to
// keep development off the expensive tier. Planning a five-step DAG from a
// sentence is not a task that needs the largest model available.
const DefaultModel = "gemini-2.0-flash"

// Config is what the client needs. APIKey is the only required field.
type Config struct {
	APIKey  string
	Model   string
	BaseURL string
	Timeout time.Duration

	// MaxOutputTokens bounds the response. A plan is a few hundred tokens; the
	// ceiling is here so a model that starts narrating cannot run up a bill or
	// hold a request open, and so a truncated response is reported as
	// MAX_TOKENS rather than as malformed JSON.
	MaxOutputTokens int

	// Temperature is 0 by default and that is deliberate. Planning is not a
	// creative task: the same goal against the same tool catalogue should
	// produce the same DAG, because a plan that varies run to run cannot be
	// reviewed, cached, or reasoned about when it goes wrong.
	Temperature float64

	HTTP *http.Client
	Log  *slog.Logger
}

// Client talks to one model.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger
}

// New validates the configuration and builds a client.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("gemini: an API key is required")
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if _, err := url.Parse(cfg.BaseURL); err != nil {
		return nil, fmt.Errorf("gemini: base URL: %w", err)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = 8192
	}
	c := &Client{cfg: cfg, http: cfg.HTTP, log: cfg.Log}
	if c.http == nil {
		// A timeout on the client as well as the context, because the context
		// belongs to the caller and this bound belongs to the dependency.
		c.http = &http.Client{Timeout: cfg.Timeout}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	c.log = c.log.With("component", "gemini", "model", cfg.Model)
	return c, nil
}

// Model reports which model this client talks to, for logs and the plan trace.
func (c *Client) Model() string { return c.cfg.Model }

// Request is one generation.
type Request struct {
	// System is the instruction the model is given about its role. Separated
	// from the user turn because Gemini treats it differently — and because
	// keeping the untrusted half (the goal) syntactically separate from the
	// trusted half (the instructions) is the only structural defence against
	// prompt injection that is available at all.
	System string
	// User is the turn carrying the goal. Attacker-controlled, in the general
	// case: a goal can come from anywhere, and it will contain whatever it
	// contains. See the planner's prompt for how that is bounded.
	User string
	// Schema constrains the response. When set, the model is asked for JSON
	// conforming to it — which is the difference between "please reply with
	// JSON" and a decoder that does not have to strip Markdown fences.
	Schema json.RawMessage
}

// Response is what came back, with enough of the metadata to make a failure
// diagnosable and a cost attributable.
type Response struct {
	Text         string
	FinishReason string
	InputTokens  int
	OutputTokens int
}

// Generate performs one call.
//
// Every error it returns is a classified *runmesh.ToolError, for the same
// reason the executors return them: the caller must not have to guess whether
// another attempt could help. Getting that wrong in the permissive direction is
// expensive here — an invalid API key that looks retryable burns the whole
// budget of every planning request in the fleet before anybody sees the cause.
func (c *Client) Generate(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(buildRequest(c.cfg, req))
	if err != nil {
		return Response{}, runmesh.Fatal(runmesh.CodeContractBroken,
			"gemini: encoding the request: %v", err)
	}

	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent",
		strings.TrimSuffix(c.cfg.BaseURL, "/"), url.PathEscape(c.cfg.Model))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, runmesh.Fatal(runmesh.CodeContractBroken, "gemini: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// The header form, not ?key=. A query parameter is logged by every proxy,
	// load balancer and error tracker between here and Google.
	httpReq.Header.Set("x-goog-api-key", c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, runmesh.Retry(runmesh.CodeUnclassified,
			"gemini: calling the API: %v", redact(err.Error(), c.cfg.APIKey))
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded, because the response is being decoded into memory and the far
	// end is not ours. maxResponseBytes is far above any plan.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Response{}, runmesh.Retry(runmesh.CodeUnclassified,
			"gemini: reading the response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Response{}, c.classifyStatus(resp, raw)
	}

	var decoded generateResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Response{}, runmesh.Retry(runmesh.CodeContractBroken,
			"gemini: the response is not the documented shape: %v", err)
	}
	return c.interpret(decoded)
}

const maxResponseBytes = 4 << 20

// interpret turns a 200 into either text or a classified failure.
//
// A 200 carrying no usable candidate is the interesting case and there are
// three distinct ones, each needing a different response from the caller: the
// prompt was blocked, the answer was blocked, or the answer was truncated.
// Collapsing them into "empty response" is how a safety block gets retried
// three times and then reported as a parse error.
func (c *Client) interpret(r generateResponse) (Response, error) {
	if fb := r.PromptFeedback; fb != nil && fb.BlockReason != "" {
		// The GOAL was refused, before any generation happened. Terminal: the
		// same prompt will be blocked again, and the caller needs to see this
		// rather than a timeout.
		return Response{}, runmesh.Fatal(CodePromptBlocked,
			"gemini refused the prompt: %s", fb.BlockReason)
	}
	if len(r.Candidates) == 0 {
		return Response{}, runmesh.Retry(runmesh.CodeContractBroken,
			"gemini returned no candidates and no block reason")
	}

	cand := r.Candidates[0]
	out := Response{FinishReason: cand.FinishReason}
	if r.UsageMetadata != nil {
		out.InputTokens = r.UsageMetadata.PromptTokenCount
		out.OutputTokens = r.UsageMetadata.CandidatesTokenCount
	}

	switch cand.FinishReason {
	case "", "STOP":
	case "MAX_TOKENS":
		// The JSON is truncated, so it will not parse, and parsing it would
		// report a syntax error at a byte offset that explains nothing.
		// Retryable: a shorter goal or a smaller catalogue may fit.
		return out, runmesh.Retry(CodeTruncated,
			"gemini stopped at the output token limit; the response is incomplete")
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII":
		return out, runmesh.Fatal(CodeResponseBlocked,
			"gemini blocked its own response: %s", cand.FinishReason)
	default:
		return out, runmesh.Retry(runmesh.CodeUnclassified,
			"gemini stopped for an unrecognised reason: %s", cand.FinishReason)
	}

	var sb strings.Builder
	for _, p := range cand.Content.Parts {
		sb.WriteString(p.Text)
	}
	out.Text = sb.String()
	if strings.TrimSpace(out.Text) == "" {
		return out, runmesh.Retry(runmesh.CodeContractBroken,
			"gemini returned a candidate with no text")
	}
	c.log.Debug("generated",
		"input_tokens", out.InputTokens, "output_tokens", out.OutputTokens,
		"finish_reason", out.FinishReason)
	return out, nil
}

// classifyStatus decides whether an HTTP failure is worth another attempt.
func (c *Client) classifyStatus(resp *http.Response, body []byte) error {
	detail := apiErrorMessage(body)
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		// The free tier's rate limit, most of the time. Retryable, and the
		// server's own hint is honoured over the computed backoff — capped by
		// Backoff.Max, so a hostile Retry-After cannot park a step for a week.
		return runmesh.RetryIn(retryAfter(resp), CodeRateLimited,
			"gemini rate limit: %s", detail)
	case http.StatusUnauthorized, http.StatusForbidden:
		// A bad or unauthorised key. No number of retries grants one, and
		// failing fast puts the real cause in front of somebody immediately.
		return runmesh.Fatal(CodeAuthFailed,
			"gemini rejected the API key (%d): %s", resp.StatusCode, detail)
	case http.StatusBadRequest:
		// The request this code built is wrong — usually the schema. A bug
		// here, not a blip.
		return runmesh.Fatal(runmesh.CodeContractBroken,
			"gemini rejected the request: %s", detail)
	case http.StatusNotFound:
		return runmesh.Fatal(CodeModelNotFound,
			"gemini has no model %q: %s", c.cfg.Model, detail)
	}
	if resp.StatusCode >= 500 {
		return runmesh.Retry(runmesh.CodeUnclassified,
			"gemini server error %d: %s", resp.StatusCode, detail)
	}
	return runmesh.Fatal(runmesh.CodeUnclassified,
		"gemini returned %d: %s", resp.StatusCode, detail)
}

// Codes this package produces, beyond the runtime's own. Stable and
// low-cardinality, so they are safe to alert on and safe to show a caller.
const (
	CodeAuthFailed      = "gemini_auth_failed"
	CodeRateLimited     = "gemini_rate_limited"
	CodeModelNotFound   = "gemini_model_not_found"
	CodePromptBlocked   = "gemini_prompt_blocked"
	CodeResponseBlocked = "gemini_response_blocked"
	CodeTruncated       = "gemini_truncated"
)

func retryAfter(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

// apiErrorMessage digs the human-readable message out of Google's error
// envelope, falling back to a bounded slice of the raw body.
func apiErrorMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	const max = 300
	if len(body) > max {
		return string(body[:max]) + "..."
	}
	return string(body)
}

// redact keeps the API key out of an error string. A transport error can carry
// the request URL, and an error message ends up in a log, a timeline entry and
// possibly an HTTP response.
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

// ---------------------------------------------------------------- the wire

func buildRequest(cfg Config, req Request) generateRequest {
	out := generateRequest{
		Contents: []content{{Role: "user", Parts: []part{{Text: req.User}}}},
		GenerationConfig: &generationConfig{
			Temperature:     cfg.Temperature,
			MaxOutputTokens: cfg.MaxOutputTokens,
			// One candidate. Sampling several and picking the best is a real
			// technique and the wrong one here: "best" would have to be judged
			// by something, and the only judge available is the validator that
			// already runs on the single candidate.
			CandidateCount: 1,
		},
	}
	if req.System != "" {
		out.SystemInstruction = &content{Parts: []part{{Text: req.System}}}
	}
	if len(req.Schema) > 0 {
		// Constrained decoding: the model is made to emit conforming JSON
		// rather than asked nicely for it. This is what removes the layer of
		// string surgery — stripping ```json fences, finding the first {,
		// hoping — that every prompt-only approach ends up growing.
		out.GenerationConfig.ResponseMIMEType = "application/json"
		out.GenerationConfig.ResponseSchema = req.Schema
	}
	return out
}

type generateRequest struct {
	SystemInstruction *content          `json:"system_instruction,omitempty"`
	Contents          []content         `json:"contents"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type generationConfig struct {
	Temperature      float64         `json:"temperature"`
	MaxOutputTokens  int             `json:"maxOutputTokens,omitempty"`
	CandidateCount   int             `json:"candidateCount,omitempty"`
	ResponseMIMEType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
}

type generateResponse struct {
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}
