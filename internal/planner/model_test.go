package planner_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kalanas210/runmesh/internal/gemini"
	"github.com/kalanas210/runmesh/internal/planner"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// TestTheGeminiAdapterNamesEachFailureForTheAPI.
//
// httpapi chooses between 422, 429 and 503 by the planner's categories, never
// by a Gemini code, so this mapping is the one place a provider's vocabulary
// becomes an answer a caller can act on. Every case goes through the real
// client against a fake endpoint: ToolErrors built by hand would prove the
// switch statement and nothing about the codes the client actually produces.
func TestTheGeminiAdapterNamesEachFailureForTheAPI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		body   string
		// want is the category, or nil for a failure that has none and must
		// still arrive as the client's classified *runmesh.ToolError.
		want error
	}{
		{"a rejected key", http.StatusForbidden, `{"error":{"message":"API key not valid"}}`, planner.ErrModelUnusable},
		{"a retired model", http.StatusNotFound, `{"error":{"message":"no longer available to new users"}}`, planner.ErrModelUnusable},
		{"a rate limit", http.StatusTooManyRequests, `{"error":{"message":"quota exceeded"}}`, planner.ErrModelRateLimited},
		{"a blocked goal", http.StatusOK, `{"promptFeedback":{"blockReason":"SAFETY"}}`, planner.ErrModelRefused},
		{"a blocked answer", http.StatusOK, `{"candidates":[{"finishReason":"SAFETY"}]}`, planner.ErrModelRefused},
		{"a server error", http.StatusInternalServerError, `{"error":{"message":"backend unavailable"}}`, nil},
	}
	categories := []error{planner.ErrModelRefused, planner.ErrModelRateLimited, planner.ErrModelUnusable}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			client, err := gemini.New(gemini.Config{
				APIKey:  "test-key",
				BaseURL: srv.URL,
				Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			if err != nil {
				t.Fatalf("gemini.New: %v", err)
			}
			_, err = planner.FromGemini(client).Generate(t.Context(), planner.Request{System: "s", User: "u"})

			var te *runmesh.ToolError
			if !errors.As(err, &te) {
				t.Fatalf("error = %v, want one that still unwraps to the client's *runmesh.ToolError", err)
			}
			if err.Error() != te.Error() {
				t.Errorf("classifying the error changed its text:\n got %q\nwant %q", err.Error(), te.Error())
			}
			for _, c := range categories {
				if got, want := errors.Is(err, c), c == tc.want; got != want {
					t.Errorf("errors.Is(err, %q) = %v, want %v", c, got, want)
				}
			}
		})
	}
}
