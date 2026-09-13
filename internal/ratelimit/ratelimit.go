// Package ratelimit is the token-bucket rate limiter behind
// engine.RateLimiter: one bucket per tool name, one bucket per external host,
// state shared across every RunMesh replica through Redis. See
// ADR 0015 for why Redis and why the client that speaks to it is
// hand-written, and ADR 0002 for the role this fills — the one Redis was
// deferred FOR in Week 2, not deferred FROM.
//
// WHY THIS FILE IMPORTS internal/engine. Limiter implements
// engine.RateLimiter, which is declared by its consumer in
// internal/engine/ratelimit.go and names engine.RateDecision in its own
// return type — the same reasoning internal/metrics' Set gives for importing
// internal/engine to implement Observer: the alternative is an adapter in
// cmd/server that re-types one method call, and that adapter is a place for
// the two vocabularies to drift apart silently. The dependency is strictly
// one way: internal/engine does not import this package and cannot, because
// it declares the interface itself.
package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/redis"
)

// Rule is one dimension's configured limit.
type Rule struct {
	// RatePerSecond is how fast the bucket refills.
	RatePerSecond float64
	// Burst is the bucket's capacity: the largest instantaneous spend it
	// ever allows, whatever the refill rate says has accumulated since.
	Burst int
}

// Enabled reports whether Rule limits anything. RatePerSecond <= 0 would mean
// "never refills" if it were allowed through — which reads as "always
// denied" rather than "off" — so either field left at zero disables the
// whole dimension rather than producing a bucket that can hand out its first
// token and never another.
func (r Rule) Enabled() bool { return r.RatePerSecond > 0 && r.Burst > 0 }

// Config is the operator's half.
type Config struct {
	PerTool Rule
	PerHost Rule
	// KeyPrefix namespaces every bucket key this Limiter writes, so more than
	// one RunMesh deployment can share one Redis without their buckets
	// colliding. Defaults to "runmesh".
	KeyPrefix string
}

func (c *Config) setDefaults() {
	if c.KeyPrefix == "" {
		c.KeyPrefix = "runmesh"
	}
}

// Limiter is engine.RateLimiter, backed by Redis.
type Limiter struct {
	redis *redis.Client
	cfg   Config
}

// New returns a Limiter over an already-constructed *redis.Client. It dials
// nothing itself; the client connects lazily on first use, the same as every
// other caller of it.
func New(client *redis.Client, cfg Config) *Limiter {
	cfg.setDefaults()
	return &Limiter{redis: client, cfg: cfg}
}

var _ engine.RateLimiter = (*Limiter)(nil)

// Allow satisfies engine.RateLimiter. It checks PerTool first and PerHost
// second, stopping at the first refusal: an attempt that is not going to run
// has no reason to spend a second bucket's token finding that out, and a
// caller reading Key never has to wonder which dimension actually decided.
func (l *Limiter) Allow(ctx context.Context, tool string, params json.RawMessage) (engine.RateDecision, error) {
	// Starts as the "nothing applied" answer and is overwritten by the last
	// dimension actually consulted, so a successful Decision's Key still
	// names which bucket it came from instead of always reading "" — useful
	// on its own for a future log line, even though checkRateLimit today
	// only ever reads Key on a refusal.
	decision := engine.RateDecision{Allowed: true}

	if l.cfg.PerTool.Enabled() {
		d, err := l.take(ctx, "tool:"+tool, l.cfg.PerTool)
		if err != nil || !d.Allowed {
			return d, err
		}
		decision = d
	}
	if l.cfg.PerHost.Enabled() {
		if host, ok := targetHost(tool, params); ok {
			d, err := l.take(ctx, "host:"+host, l.cfg.PerHost)
			if err != nil || !d.Allowed {
				return d, err
			}
			decision = d
		}
	}
	return decision, nil
}

func (l *Limiter) take(ctx context.Context, bucket string, rule Rule) (engine.RateDecision, error) {
	key := l.cfg.KeyPrefix + ":ratelimit:" + bucket
	v, err := l.redis.Eval(ctx, tokenBucketScript, []string{key},
		strconv.Itoa(rule.Burst),
		strconv.FormatFloat(rule.RatePerSecond, 'f', -1, 64),
	)
	if err != nil {
		return engine.RateDecision{}, fmt.Errorf("ratelimit: %s: %w", bucket, err)
	}
	if len(v.Array) != 2 {
		return engine.RateDecision{}, fmt.Errorf(
			"ratelimit: %s: the token bucket script returned %d values, want 2", bucket, len(v.Array))
	}
	return engine.RateDecision{
		Allowed:    v.Array[0].Int == 1,
		RetryAfter: time.Duration(v.Array[1].Int) * time.Millisecond,
		Key:        bucket,
	}, nil
}

// targetHost extracts the host a step's attempt would contact, for the tools
// this package knows how to read a URL out of. A tool this switch does not
// name is simply exempt from PerHost — its PerTool bucket, if configured,
// still applies — because guessing at an unknown tool's parameter shape is
// how a rate limiter starts silently limiting the wrong field, or panicking
// on one that never had a "url" to begin with.
func targetHost(tool string, params json.RawMessage) (string, bool) {
	switch tool {
	case "http_request":
		return httpRequestHost(params)
	default:
		return "", false
	}
}

// httpRequestHost mirrors the parsing cmd/task/http.go's httpRequest does on
// the SAME field — "url" — deliberately staying just as forgiving: a
// malformed URL here is not this package's problem to report, it is the
// tool's own, and it already answers with a clear exit code and stderr line
// when the request actually runs. This function's only job is "is there a
// host to charge a bucket for", and false is always a safe answer to that.
func httpRequestHost(params json.RawMessage) (string, bool) {
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", false
	}
	u, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	// Lower-cased so Example.com and example.com share one bucket — DNS
	// names are case-insensitive, and a limiter that let case alone double
	// the effective rate would be a limiter with a hole in it.
	return strings.ToLower(u.Hostname()), true
}
