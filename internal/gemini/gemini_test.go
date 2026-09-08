package gemini_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/gemini"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

const testKey = "AIza-test-key-0123456789"

// server returns a client pointed at a handler, so every test below exercises
// the real HTTP path — request encoding, headers, status handling, decoding —
// rather than a mocked-out method.
func server(t *testing.T, h http.HandlerFunc) *gemini.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := gemini.New(gemini.Config{
		APIKey:  testKey,
		BaseURL: srv.URL,
		Model:   "gemini-test",
		HTTP:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("gemini.New: %v", err)
	}
	return c
}

func ok(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": text}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount": 123, "candidatesTokenCount": 45,
			},
		})
	}
}

func classified(t *testing.T, err error) *runmesh.ToolError {
	t.Helper()
	var te *runmesh.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T (%v), want *runmesh.ToolError: the caller must not "+
			"have to guess whether another attempt could help", err, err)
	}
	return te
}

// TestGenerateSendsWhatTheAPIExpects.
func TestGenerateSendsWhatTheAPIExpects(t *testing.T) {
	t.Parallel()

	var got struct {
		SystemInstruction *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"system_instruction"`
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			Temperature      float64         `json:"temperature"`
			ResponseMIMEType string          `json:"responseMimeType"`
			ResponseSchema   json.RawMessage `json:"responseSchema"`
			CandidateCount   int             `json:"candidateCount"`
		} `json:"generationConfig"`
	}
	var path, key, query string

	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		path, key, query = r.URL.Path, r.Header.Get("x-goog-api-key"), r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		ok(`{"ok":true}`)(w, r)
	})

	resp, err := c.Generate(t.Context(), gemini.Request{
		System: "you are a planner",
		User:   "plan something",
		Schema: json.RawMessage(`{"type":"OBJECT"}`),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if path != "/v1beta/models/gemini-test:generateContent" {
		t.Errorf("path = %q", path)
	}
	if key != testKey {
		t.Errorf("the API key was not sent in x-goog-api-key")
	}
	// The key must NOT be in the query string: a query parameter is logged by
	// every proxy, load balancer and error tracker between here and Google.
	if strings.Contains(query, testKey) {
		t.Errorf("the API key is in the query string (%q), where every proxy logs it", query)
	}

	if got.SystemInstruction == nil || got.SystemInstruction.Parts[0].Text != "you are a planner" {
		t.Error("the system instruction was not sent separately from the user turn")
	}
	if len(got.Contents) != 1 || got.Contents[0].Parts[0].Text != "plan something" {
		t.Errorf("contents = %+v", got.Contents)
	}
	if got.GenerationConfig.ResponseMIMEType != "application/json" {
		t.Error("responseMimeType was not set, so the model is being ASKED for JSON " +
			"rather than constrained to it — which is the difference between a " +
			"decoder and a pile of string surgery")
	}
	if len(got.GenerationConfig.ResponseSchema) == 0 {
		t.Error("the schema was not sent")
	}
	if got.GenerationConfig.Temperature != 0 {
		t.Errorf("temperature = %v, want 0: the same goal should produce the same plan",
			got.GenerationConfig.Temperature)
	}
	if got.GenerationConfig.CandidateCount != 1 {
		t.Errorf("candidateCount = %d, want 1", got.GenerationConfig.CandidateCount)
	}

	if resp.Text != `{"ok":true}` {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.InputTokens != 123 || resp.OutputTokens != 45 {
		t.Errorf("tokens = %d/%d, want 123/45: an unaccounted call is an unbilled one",
			resp.InputTokens, resp.OutputTokens)
	}
}

// TestGenerateJoinsMultipleParts. A long response arrives split across parts,
// and concatenating them is the difference between valid JSON and a syntax
// error at a byte offset that explains nothing.
func TestGenerateJoinsMultipleParts(t *testing.T) {
	t.Parallel()

	c := server(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{
					map[string]any{"text": `{"a":`},
					map[string]any{"text": `1}`},
				}},
				"finishReason": "STOP",
			}},
		})
	})
	resp, err := c.Generate(t.Context(), gemini.Request{User: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Text != `{"a":1}` {
		t.Errorf("text = %q, want the parts joined", resp.Text)
	}
}

// TestFailuresAreClassified is the table that matters most in this package.
//
// Getting any of these wrong in the permissive direction is expensive: an
// invalid API key classified as retryable burns the whole budget of every
// planning request in the fleet before anybody sees the real cause.
func TestFailuresAreClassified(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		handler   http.HandlerFunc
		wantCode  string
		wantRetry bool
	}{
		"an invalid key": {
			status(http.StatusUnauthorized, `{"error":{"message":"API key not valid"}}`),
			gemini.CodeAuthFailed, false,
		},
		"a forbidden key": {
			status(http.StatusForbidden, `{"error":{"message":"permission denied"}}`),
			gemini.CodeAuthFailed, false,
		},
		"a rate limit": {
			status(http.StatusTooManyRequests, `{"error":{"message":"quota exceeded"}}`),
			gemini.CodeRateLimited, true,
		},
		"a bad request": {
			status(http.StatusBadRequest, `{"error":{"message":"invalid schema"}}`),
			runmesh.CodeContractBroken, false,
		},
		"an unknown model": {
			status(http.StatusNotFound, `{"error":{"message":"model not found"}}`),
			gemini.CodeModelNotFound, false,
		},
		"a server error": {
			status(http.StatusInternalServerError, `{"error":{"message":"internal"}}`),
			runmesh.CodeUnclassified, true,
		},
		"the prompt was blocked": {
			jsonBody(`{"promptFeedback":{"blockReason":"SAFETY"}}`),
			gemini.CodePromptBlocked, false,
		},
		"the response was blocked": {
			jsonBody(`{"candidates":[{"finishReason":"SAFETY","content":{"parts":[]}}]}`),
			gemini.CodeResponseBlocked, false,
		},
		"the response was truncated": {
			jsonBody(`{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"{\"a\":"}]}}]}`),
			gemini.CodeTruncated, true,
		},
		"no candidates at all": {
			jsonBody(`{"candidates":[]}`),
			runmesh.CodeContractBroken, true,
		},
		"a candidate with no text": {
			jsonBody(`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"  "}]}}]}`),
			runmesh.CodeContractBroken, true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := server(t, tc.handler).Generate(t.Context(), gemini.Request{User: "x"})
			if err == nil {
				t.Fatal("no error")
			}
			te := classified(t, err)
			if te.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", te.Code, tc.wantCode)
			}
			if te.Retryable != tc.wantRetry {
				t.Errorf("retryable = %v, want %v (%s)", te.Retryable, tc.wantRetry, te.Message)
			}
		})
	}
}

// TestATruncatedResponseIsNotAParseError. MAX_TOKENS produces JSON that will
// not parse; reporting it as malformed JSON sends whoever is debugging it to
// look at the schema instead of at the token limit.
func TestATruncatedResponseIsNotAParseError(t *testing.T) {
	t.Parallel()

	_, err := server(t, jsonBody(
		`{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"{\"steps\":["}]}}]}`,
	)).Generate(t.Context(), gemini.Request{User: "x"})

	te := classified(t, err)
	if !strings.Contains(strings.ToLower(te.Message), "token") {
		t.Errorf("message = %q; it should name the token limit", te.Message)
	}
}

// TestTheAPIKeyIsNeverInAnErrorMessage. An error string reaches a log, a
// timeline entry and possibly an HTTP response.
func TestTheAPIKeyIsNeverInAnErrorMessage(t *testing.T) {
	t.Parallel()

	// A server that closes the connection, so the error is a transport error
	// carrying the request URL — the one shape that could leak a key if it
	// were passed as a query parameter.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("the test server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	c, err := gemini.New(gemini.Config{APIKey: testKey, BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("gemini.New: %v", err)
	}
	_, err = c.Generate(t.Context(), gemini.Request{User: "x"})
	if err == nil {
		t.Skip("the connection did not fail")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("the API key is in the error message: %v", err)
	}
}

// TestNewRefusesAKeylessClient. A client with no key produces a 401 per call
// instead of one message at boot.
func TestNewRefusesAKeylessClient(t *testing.T) {
	t.Parallel()

	if _, err := gemini.New(gemini.Config{}); err == nil {
		t.Fatal("a client with no API key was built")
	}
	if _, err := gemini.New(gemini.Config{APIKey: "   "}); err == nil {
		t.Fatal("a client with a whitespace API key was built")
	}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func jsonBody(body string) http.HandlerFunc {
	return status(http.StatusOK, body)
}
