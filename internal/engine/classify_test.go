package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Classify is where every policy opinion in RunMesh lives, so this is the
// table that has to be exhaustive: five stop reasons crossed with every kind
// of error a tool can produce, crossed with budget remaining or exhausted.
//
// It is also entirely pure — no clock, no store, no goroutine — which is
// exactly why the interesting decisions were pushed into it.
func TestClassify(t *testing.T) {
	t.Parallel()

	b := engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2}

	tests := []struct {
		name        string
		stop        engine.Stop
		err         error
		failures    int
		maxAttempts int

		wantState   runmesh.State
		wantCount   bool
		wantRelease bool
		wantDiscard bool
		wantRetryIn time.Duration
		wantCode    string
	}{
		// ------------------------------------------------ the tool returned
		{
			name: "success", stop: engine.StopNone, err: nil, maxAttempts: 3,
			wantState: runmesh.Succeeded,
		},
		{
			name: "retryable with budget left",
			stop: engine.StopNone, err: runmesh.Retry("upstream_503", "gateway"),
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Retrying, wantCount: true,
			wantRetryIn: time.Second, wantCode: "upstream_503",
		},
		{
			name: "retryable on the last attempt is terminal",
			stop: engine.StopNone, err: runmesh.Retry("upstream_503", "gateway"),
			failures: 2, maxAttempts: 3,
			wantState: runmesh.Failed, wantCount: true, wantCode: "upstream_503",
		},
		{
			name: "retryable ladder position two",
			stop: engine.StopNone, err: runmesh.Retry("x", "x"),
			failures: 1, maxAttempts: 5,
			wantState: runmesh.Retrying, wantCount: true, wantRetryIn: 2 * time.Second, wantCode: "x",
		},
		{
			name: "server Retry-After overrides the computed backoff",
			stop: engine.StopNone, err: runmesh.RetryIn(7*time.Second, "rate_limited", "429"),
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Retrying, wantCount: true,
			wantRetryIn: 7 * time.Second, wantCode: "rate_limited",
		},
		{
			name: "Retry-After is still capped by Max",
			stop: engine.StopNone, err: runmesh.RetryIn(24*time.Hour, "rate_limited", "hostile"),
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Retrying, wantCount: true,
			wantRetryIn: time.Minute, wantCode: "rate_limited",
		},
		{
			name: "terminal error is never retried",
			stop: engine.StopNone, err: runmesh.Fatal("bad_input", "column missing"),
			failures: 0, maxAttempts: 5,
			wantState: runmesh.Failed, wantCount: true, wantCode: "bad_input",
		},
		{
			name:     "wrapped ToolError is still recognised",
			stop:     engine.StopNone,
			err:      fmt.Errorf("calling upstream: %w", runmesh.Retry("upstream_503", "gateway")),
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Retrying, wantCount: true,
			wantRetryIn: time.Second, wantCode: "upstream_503",
		},
		{
			name: "unclassified error is TERMINAL",
			stop: engine.StopNone, err: io.EOF,
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Failed, wantCount: true, wantCode: runmesh.CodeUnclassified,
		},
		{
			name: "tool reporting context.Canceled with nothing cancelled broke its contract",
			stop: engine.StopNone, err: context.Canceled,
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Failed, wantCount: true, wantCode: runmesh.CodeContractBroken,
		},
		{
			name: "tool reporting DeadlineExceeded with nothing expired broke its contract",
			stop: engine.StopNone, err: context.DeadlineExceeded,
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Failed, wantCount: true, wantCode: runmesh.CodeContractBroken,
		},

		// ------------------------------------------------------- the timeout
		{
			name: "timeout with budget left is retried",
			stop: engine.StopTimeout, failures: 0, maxAttempts: 3,
			wantState: runmesh.Retrying, wantCount: true,
			wantRetryIn: time.Second, wantCode: runmesh.CodeTimeout,
		},
		{
			name: "final timeout reports TIMED_OUT, not FAILED",
			stop: engine.StopTimeout, failures: 2, maxAttempts: 3,
			wantState: runmesh.TimedOut, wantCount: true, wantCode: runmesh.CodeTimeout,
		},
		{
			name: "a timing-out tool's own error is ignored",
			stop: engine.StopTimeout, err: runmesh.Fatal("misleading", "ignore me"),
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Retrying, wantCount: true,
			wantRetryIn: time.Second, wantCode: runmesh.CodeTimeout,
		},

		// -------------------------------------------------- the cancellation
		{
			// THE regression case. A tool that returns a retryable error on its
			// way out the door must not be able to buy itself a retry: the stop
			// reason is consulted first and its error is never read.
			name: "cancel beats a retryable error the tool returned on the way out",
			stop: engine.StopCancel, err: runmesh.Retry("connection_reset", "read: connection reset"),
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Cancelled, wantCount: false, wantCode: runmesh.CodeCancelled,
		},
		{
			name: "cancel spends no budget even with attempts remaining",
			stop: engine.StopCancel, failures: 0, maxAttempts: 5,
			wantState: runmesh.Cancelled, wantCount: false, wantCode: runmesh.CodeCancelled,
		},
		{
			name: "cancel on the last attempt is still a cancel",
			stop: engine.StopCancel, failures: 2, maxAttempts: 3,
			wantState: runmesh.Cancelled, wantCount: false, wantCode: runmesh.CodeCancelled,
		},

		// ------------------------------------------------------ the lease loss
		{
			// The new lease-holder owns this step. Writing anything at all is
			// the zombie overwrite the fencing token exists to prevent.
			name: "lease lost writes nothing",
			stop: engine.StopLost, err: runmesh.Fatal("ignored", "ignored"),
			failures: 0, maxAttempts: 3,
			wantDiscard: true,
		},
		{
			name: "lease lost after a successful run still writes nothing",
			stop: engine.StopLost, err: nil, failures: 0, maxAttempts: 3,
			wantDiscard: true,
		},

		// --------------------------------------------------------- the drain
		{
			// Our failure, not the step's. A rolling restart must cost zero
			// retries, or a deploy silently eats every in-flight job's budget.
			name: "shutdown releases without spending budget",
			stop: engine.StopShutdown, failures: 1, maxAttempts: 3,
			wantState: runmesh.Queued, wantRelease: true, wantCount: false,
		},
		{
			name: "shutdown ignores whatever the tool returned",
			stop: engine.StopShutdown, err: runmesh.Fatal("ignored", "ignored"),
			failures: 0, maxAttempts: 3,
			wantState: runmesh.Queued, wantRelease: true, wantCount: false,
		},
		{
			name: "shutdown on the last attempt still spends nothing",
			stop: engine.StopShutdown, failures: 2, maxAttempts: 3,
			wantState: runmesh.Queued, wantRelease: true, wantCount: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := engine.Classify(tc.stop, tc.err, tc.failures, tc.maxAttempts, b)

			if got.Discard != tc.wantDiscard {
				t.Fatalf("Discard = %v, want %v", got.Discard, tc.wantDiscard)
			}
			if tc.wantDiscard {
				if got.Error != nil {
					t.Errorf("a discarded outcome carries an error: %+v", got.Error)
				}
				return
			}
			if got.State != tc.wantState {
				t.Errorf("State = %s, want %s", got.State, tc.wantState)
			}
			if got.CountFail != tc.wantCount {
				t.Errorf("CountFail = %v, want %v", got.CountFail, tc.wantCount)
			}
			if got.Release != tc.wantRelease {
				t.Errorf("Release = %v, want %v", got.Release, tc.wantRelease)
			}
			if got.RetryIn != tc.wantRetryIn {
				t.Errorf("RetryIn = %s, want %s", got.RetryIn, tc.wantRetryIn)
			}
			if tc.wantCode != "" {
				if got.Error == nil {
					t.Fatalf("no ErrorInfo; want code %q", tc.wantCode)
				}
				if got.Error.Code != tc.wantCode {
					t.Errorf("error code = %q, want %q", got.Error.Code, tc.wantCode)
				}
				if got.Error.Attempt != tc.failures+1 {
					t.Errorf("error attempt = %d, want %d", got.Error.Attempt, tc.failures+1)
				}
			}
			if tc.wantState == runmesh.Succeeded && got.Error != nil {
				t.Errorf("a successful outcome carries an error: %+v", got.Error)
			}
			// Only a RETRYING outcome may carry a delay: anything else would be
			// a delay nothing ever waits out.
			if got.State != runmesh.Retrying && got.RetryIn != 0 {
				t.Errorf("a %s outcome carries RetryIn=%s", got.State, got.RetryIn)
			}
		})
	}
}

// TestClassifyIsTotal: every combination must produce a decision. A stop
// reason or error shape that fell through would leave the step leased and
// silent until its lease expired.
func TestClassifyIsTotal(t *testing.T) {
	t.Parallel()
	b := engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2}

	stops := []engine.Stop{
		engine.StopNone, engine.StopTimeout, engine.StopCancel,
		engine.StopLost, engine.StopShutdown,
	}
	errs := []error{
		nil,
		io.EOF,
		context.Canceled,
		context.DeadlineExceeded,
		runmesh.Retry("a", "a"),
		runmesh.Fatal("b", "b"),
		runmesh.RetryIn(time.Second, "c", "c"),
		fmt.Errorf("wrapped: %w", runmesh.Retry("d", "d")),
		errors.New("plain"),
	}

	for _, stop := range stops {
		for _, err := range errs {
			for _, budget := range [][2]int{{0, 3}, {2, 3}, {0, 1}} {
				d := engine.Classify(stop, err, budget[0], budget[1], b)
				switch {
				case d.Discard:
					// A discard writes nothing; nothing more to check.
				case d.Release:
					if d.State != runmesh.Queued {
						t.Errorf("stop=%v err=%v: Release with state %s, want QUEUED", stop, err, d.State)
					}
				case d.State == runmesh.Unknown:
					t.Errorf("stop=%v err=%v budget=%v produced no decision", stop, err, budget)
				case !d.State.Terminal() && d.State != runmesh.Retrying:
					t.Errorf("stop=%v err=%v: state %s is neither terminal nor RETRYING", stop, err, d.State)
				}
			}
		}
	}
}

func TestStopString(t *testing.T) {
	t.Parallel()
	for stop, want := range map[engine.Stop]string{
		engine.StopNone:     "none",
		engine.StopTimeout:  "timeout",
		engine.StopCancel:   "cancel",
		engine.StopLost:     "lease_lost",
		engine.StopShutdown: "shutdown",
	} {
		if got := stop.String(); got != want {
			t.Errorf("Stop(%d).String() = %q, want %q", stop, got, want)
		}
	}
}
