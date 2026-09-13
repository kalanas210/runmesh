// Package engine is the runtime: the dispatcher that claims work, the bounded
// worker pool that executes it, the reconciler that recovers what a dead
// worker left behind, the adaptive controller that resizes the pool between
// Workers and MaxWorkers, and the pure policy — Classify, Backoff and
// Concurrency — that decides what every outcome means.
//
// The goroutine census is fixed at boot and is exactly 2 + MaxWorkers, plus
// one transient goroutine per in-flight step: one dispatcher, one reconciler,
// MaxWorkers workers, and a tool runner for each step actually executing.
// Only Workers of the pool's goroutines hold a capacity token at boot; the
// rest sit parked on the shared lease channel, exactly as ready to receive
// one, until resize (see resize.go) mints more.
//
// A THIRD loop, the adaptive-concurrency controller, joins that census —
// making it 3 + MaxWorkers — but only when MaxWorkers is actually wider than
// Workers: at MaxWorkers == Workers, Concurrency.Next is mathematically
// constant, so a goroutine that would tick forever to recompute the same
// answer simply does not start (see Start). A deployment that never sets
// RUNMESH_MAX_WORKERS therefore gets the exact census this runtime has always
// had — 2 + Workers — with nothing new running at all, which is what makes
// adaptive sizing something an operator opts INTO rather than a behaviour
// change to what shipped before it existed. There is no goroutine per job, no
// goroutine per HTTP request beyond net/http's own, and no supervisor tree.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/policy"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// ErrDrainIncomplete means the drain deadline passed and at least one tool was
// still ignoring its context. The process exits anyway — never hanging is more
// important than a tidy shutdown — and the affected steps are recovered by the
// next process through their expired leases.
var ErrDrainIncomplete = errors.New("engine: drain incomplete; steps were abandoned")

// Config is the engine's half of the application configuration. It is a plain
// struct rather than a reference to config.Config so the engine can be
// constructed in a test without an environment.
type Config struct {
	Owner             string
	Workers           int
	ClaimBatch        int
	PollInterval      time.Duration
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	StoreTimeout      time.Duration
	AbandonGrace      time.Duration
	ReconcileInterval time.Duration
	ReconcileBatch    int
	MaxOutputBytes    int
	Backoff           Backoff

	// Adaptive concurrency. MaxWorkers is the ceiling the pool may grow to;
	// Workers remains both the floor it never shrinks below and the size it
	// starts at. MaxWorkers == Workers — what setDefaults falls back to —
	// makes Concurrency.Next constant at Workers, which is the exact fixed
	// pool every RUNMESH_WORKERS deployment already ran before this existed:
	// adaptive sizing is something an operator opts INTO by widening the
	// ceiling, never a behaviour change to what shipped before it.
	MaxWorkers int
	// ConcurrencyInterval is how often the controller reduces its window and
	// decides. ConcurrencyErrorRate and ConcurrencyHeadroom feed Concurrency
	// directly — see its doc for what each one means and why neither is
	// floored to a non-zero default here the way ConcurrencyInterval and
	// ConcurrencyStep are: a zero ErrorRate or Headroom is itself a real,
	// intentional setting, the same way Backoff.Jitter's zero value is.
	ConcurrencyInterval  time.Duration
	ConcurrencyErrorRate float64
	ConcurrencyHeadroom  float64
	ConcurrencyStep      int

	// RateLimitTimeout bounds one Deps.RateLimiter.Allow call, the same way
	// StoreTimeout bounds one store call. A limiter that cannot answer inside
	// it is treated as unreachable (CodeRateLimitUnavailable) rather than
	// left to hold a worker for as long as the step's own timeout allows.
	RateLimitTimeout time.Duration

	// Tools optionally restricts this engine to a subset of the registry. It
	// is how Week 3 splits in-process tools from container tools across
	// separate worker fleets without a second scheduler.
	Tools []string
}

func (c *Config) setDefaults() {
	if c.Workers < 1 {
		c.Workers = 1
	}
	// MaxWorkers is resolved before ClaimBatch on purpose: a directly-
	// constructed Config that does not mention MaxWorkers at all — which,
	// before this feature, was every Config there was — gets MaxWorkers ==
	// Workers here, below Workers being nonsensical (the pool would start
	// above its own ceiling) and corrected up rather than validated, the same
	// treatment ClaimBatch gets against it next. Config.Validate enforces the
	// same floor for a Config that DID set one, so this branch only ever
	// fires for a caller that left it unset.
	if c.MaxWorkers < c.Workers {
		c.MaxWorkers = c.Workers
	}
	if c.ConcurrencyInterval <= 0 {
		c.ConcurrencyInterval = 15 * time.Second
	}
	if c.ConcurrencyStep < 1 {
		c.ConcurrencyStep = 1
	}
	// ClaimBatch's ceiling is MaxWorkers, not Workers: once adaptive sizing
	// has grown the pool past Workers, a dispatcher still capped at claiming
	// Workers leases per round trip just takes more round trips to fill the
	// capacity it now has — not wrong, but the ceiling this validates against
	// should be the same one the pool can actually reach.
	if c.ClaimBatch < 1 || c.ClaimBatch > c.MaxWorkers {
		c.ClaimBatch = c.Workers
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = c.LeaseTTL / 3
	}
	if c.StoreTimeout <= 0 {
		c.StoreTimeout = 5 * time.Second
	}
	if c.RateLimitTimeout <= 0 {
		c.RateLimitTimeout = 250 * time.Millisecond
	}
	if c.AbandonGrace <= 0 {
		c.AbandonGrace = c.LeaseTTL / 3
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = 10 * time.Second
	}
	if c.ReconcileBatch < 1 {
		c.ReconcileBatch = 100
	}
	if c.MaxOutputBytes < 1 {
		c.MaxOutputBytes = 64 << 10
	}
	if c.Backoff.Base <= 0 {
		c.Backoff.Base = time.Second
	}
	if c.Backoff.Max < c.Backoff.Base {
		c.Backoff.Max = 60 * time.Second
	}
	if c.Backoff.Factor < 1 {
		c.Backoff.Factor = 2
	}
	if c.Owner == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "unknown"
		}
		c.Owner = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
}

// Sandbox resolves the security envelope for an attempt: the CPU, memory,
// scratch space, image and network access the operator's execution policy
// grants this tool, as opposed to what the tool asked for.
//
// It is consulted per ATTEMPT rather than once at submission on purpose. A step
// can sit queued behind a retry backoff for minutes and behind a dead worker's
// lease for longer; resolving at dispatch means a policy tightened in between —
// a tool denied, the network switched off — binds the very next attempt instead
// of only new submissions.
//
// A refusal is a classified terminal error, and the worker settles it like any
// other terminal outcome: the step fails, the timeline records why, and nothing
// retries a decision that will not change.
type Sandbox interface {
	Limits(tool string, req policy.Request) (tools.Limits, error)
}

// Deps are the collaborators an engine needs. They are all interfaces or
// values, so a test constructs an engine with a fake clock and an in-memory
// store and nothing else.
type Deps struct {
	Store    Store
	Executor tools.Executor
	Clock    clock.Clock
	Log      *slog.Logger
	// Sandbox is optional in a test and mandatory in the server. Nil means
	// policy.Passthrough: the tool gets exactly what it asked for, which is
	// what every Week-1 and Week-2 test assumes and is why they did not have to
	// change when the policy engine arrived.
	Sandbox Sandbox
	// Observer is optional in a test and mandatory in the server, exactly as
	// Sandbox is. Nil means nopObserver, which is why the arrival of telemetry
	// in Week 6 changed no existing engine test. Every method on it is called
	// on a dispatcher or worker goroutine; see observer.go for what that
	// obliges an implementation to be.
	Observer Observer
	// RateLimiter is optional everywhere, including the server: nil means
	// nopRateLimiter, which is what RUNMESH_REDIS_URL being unset resolves to
	// in cmd/server/run.go. Every existing test and every deployment that
	// predates this feature is unaffected by its arrival for exactly that
	// reason — see ratelimit.go.
	RateLimiter RateLimiter
}

// Engine owns the runtime goroutines.
type Engine struct {
	cfg     Config
	store   Store
	exec    tools.Executor
	box     Sandbox
	obs     Observer
	limiter RateLimiter
	clock   clock.Clock
	log     *slog.Logger

	// leases is unbuffered: a lease is handed directly to a worker that is
	// already waiting, so a claimed step is never sitting in a queue nobody is
	// accounting for. Closed by the dispatcher, and only by the dispatcher.
	leases chan runmesh.Lease
	// idle holds one token per free worker. Buffered to MaxWorkers — not
	// Workers, since resize may mint tokens up to that ceiling — and never
	// closed, because it has many senders.
	idle chan struct{}

	// size is the pool's current target: Config.Workers at boot, moved only by
	// resize. retireDebt is how many tokens returning to idle (returnToken)
	// must be swallowed instead of returned before actual circulation catches
	// up with size. window accumulates AttemptSettled evidence between
	// controller ticks; concurrency is the pure policy consulted on each one,
	// built once from cfg and never mutated after. See resize.go.
	size        atomic.Int64
	retireDebt  atomic.Int64
	window      *concurrencyAccumulator
	concurrency Concurrency

	wg      sync.WaitGroup // worker pool
	wgLoops sync.WaitGroup // dispatcher, reconciler and the concurrency controller

	cancelClaim context.CancelFunc // stops taking on new work
	cancelHard  context.CancelFunc // kills work already in flight

	started      atomic.Bool
	once         sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
	workersDone  chan struct{}

	inflightMu sync.Mutex
	inflight   map[string]runmesh.Lease

	claimErrors   atomic.Uint64
	leasesExpired atomic.Uint64
}

// New builds an engine. It does not start any goroutine; Start does that.
func New(cfg Config, d Deps) (*Engine, error) {
	if d.Store == nil {
		return nil, errors.New("engine: a Store is required")
	}
	if d.Executor == nil {
		return nil, errors.New("engine: an Executor is required")
	}
	if d.Clock == nil {
		d.Clock = clock.System()
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Sandbox == nil {
		d.Sandbox = policy.Passthrough{}
	}
	if d.Observer == nil {
		d.Observer = nopObserver{}
	}
	if d.RateLimiter == nil {
		d.RateLimiter = nopRateLimiter{}
	}
	cfg.setDefaults()

	e := &Engine{
		cfg:          cfg,
		store:        d.Store,
		exec:         d.Executor,
		box:          d.Sandbox,
		obs:          d.Observer,
		limiter:      d.RateLimiter,
		clock:        d.Clock,
		log:          d.Log.With("component", "engine"),
		leases:       make(chan runmesh.Lease),
		idle:         make(chan struct{}, cfg.MaxWorkers),
		shutdownDone: make(chan struct{}),
		workersDone:  make(chan struct{}),
		inflight:     make(map[string]runmesh.Lease),
		window:       &concurrencyAccumulator{},
		concurrency: Concurrency{
			Min: cfg.Workers, Max: cfg.MaxWorkers,
			ErrorRate: cfg.ConcurrencyErrorRate, Headroom: cfg.ConcurrencyHeadroom,
			Step: cfg.ConcurrencyStep,
		},
	}
	// Not part of the literal: atomic.Int64 has no exported way to construct
	// one already holding a value.
	e.size.Store(int64(cfg.Workers))
	return e, nil
}

// Start launches the reconciler, the dispatcher and the worker pool.
//
// The boot-time sweep runs BEFORE the dispatcher, so a restart reclaims
// whatever the previous process was holding rather than racing it.
//
// ctx bounds only that boot sweep. The engine deliberately does not stop when
// it is cancelled: the lifecycle is owned by Shutdown, which drains in a
// defined order, and a context cancellation offers no way to express "stop
// claiming but let in-flight steps finish".
func (e *Engine) Start(ctx context.Context) error {
	if !e.started.CompareAndSwap(false, true) {
		return errors.New("engine: already started")
	}

	claimCtx, cancelClaim := context.WithCancel(context.WithoutCancel(ctx))
	hardCtx, cancelHard := context.WithCancel(context.WithoutCancel(ctx))
	e.cancelClaim, e.cancelHard = cancelClaim, cancelHard

	if n := e.Reconcile(ctx); n > 0 {
		e.log.Warn("reclaimed steps left behind by a previous run", "count", n)
	}

	// Add is called once, before any goroutine exists, so an Add can never
	// race a Wait — the classic WaitGroup misuse this shape rules out.
	//
	// Every one of MaxWorkers goroutines starts now — the census is fixed at
	// boot, per the package doc — but only the first Workers of them start
	// holding a capacity token. The rest are already parked on `range
	// e.leases`, exactly as able to receive a lease as any other, waiting for
	// resize to mint the tokens that make that happen. Which index gets a
	// seed token is arbitrary; only the COUNT (Workers, out of MaxWorkers) is.
	e.wg.Add(e.cfg.MaxWorkers)
	for i := range e.cfg.MaxWorkers {
		if i < e.cfg.Workers {
			e.idle <- struct{}{}
		}
		go e.runWorker(hardCtx, i)
	}

	e.wgLoops.Add(2)
	go func() {
		defer e.wgLoops.Done()
		e.runDispatcher(claimCtx)
	}()
	go e.runReconciler(claimCtx)

	// The controller only ever starts when it has a range to work in. At
	// MaxWorkers == Workers, Concurrency.Next is mathematically constant (see
	// its own doc), so a goroutine ticking every ConcurrencyInterval to compute
	// the same answer forever is pure overhead — for production, and for every
	// test built before this feature existed, which is the sharper reason.
	// clock.Fake counts registered waiters, and several existing tests
	// synchronise on an EXACT count via BlockUntilContext; an always-on ticker
	// this goroutine would register adds one to that count for the rest of the
	// process and desyncs every one of them. Gating it on the ceiling actually
	// being wider keeps the fake clock's waiter census identical to what it was
	// before this feature existed, for the deployments and the tests that never
	// asked for it.
	if e.cfg.MaxWorkers > e.cfg.Workers {
		e.wgLoops.Add(1)
		go e.runConcurrency(claimCtx)
	}

	e.log.Info("engine started",
		"workers", e.cfg.Workers, "max_workers", e.cfg.MaxWorkers, "claim_batch", e.cfg.ClaimBatch,
		"lease_ttl", e.cfg.LeaseTTL.String(), "owner", e.cfg.Owner)
	return nil
}

// RunOnce claims and executes at most one step ON THE CALLING GOROUTINE, and
// reports whether it found work.
//
// It shares every path with the pool — the same Claim, the same execute, the
// same Classify, the same Finish — which is what makes the bulk of the engine
// tests straight-line synchronous code with no goroutines, no fake clock and
// no possibility of a timing flake.
func (e *Engine) RunOnce(ctx context.Context) (bool, error) {
	leases, err := e.store.Claim(ctx, runmesh.ClaimRequest{
		Owner:    e.cfg.Owner,
		Limit:    1,
		LeaseTTL: e.cfg.LeaseTTL,
		Now:      e.clock.Now(),
		Tools:    e.cfg.Tools,
	})
	if err != nil {
		return false, err
	}
	if len(leases) == 0 {
		return false, nil
	}
	e.execute(ctx, e.log.With("mode", "run_once"), leases[0])
	return true, nil
}

// Shutdown drains the engine. Every caller receives the same result, and it is
// bounded at every step: this function cannot hang.
func (e *Engine) Shutdown(ctx context.Context) error {
	if !e.started.Load() {
		return nil
	}
	e.once.Do(func() { go e.drain(ctx) })
	select {
	case <-e.shutdownDone:
		return e.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// drain implements the shutdown sequence.
//
// The ordering is the point. Stop claiming first, so no new work is taken on
// while in-flight work finishes; leave running steps completely alone until
// the drain deadline actually expires; and only then cancel them, at which
// point they classify as StopShutdown and are RELEASED — back to QUEUED with
// their retry budget untouched, because a deploy is our failure and not
// theirs.
//
// The heartbeat lives inside each worker's own select loop rather than in a
// separate goroutine, so it is torn down exactly when its step is. That is
// what stops the lease of a step we are politely draining from expiring
// underneath us and being picked up concurrently by another replica.
func (e *Engine) drain(ctx context.Context) {
	defer close(e.shutdownDone) // happens-after the shutdownErr write below

	e.cancelClaim() // (a) dispatcher stops claiming and closes `leases`
	e.wgLoops.Wait()

	go func() { e.wg.Wait(); close(e.workersDone) }()

	select {
	case <-e.workersDone:
		e.log.Info("engine drained cleanly")
		return // (b) every in-flight step ran to completion
	case <-ctx.Done():
	}

	e.log.Warn("drain deadline expired; cancelling in-flight steps",
		"inflight", e.InflightAttempts())
	e.cancelHard() // (c) steps see hard.Err() != nil, classify StopShutdown

	// (d) A final bounded wait. A tool that ignores its context must not be
	// able to make the process hang, so this deadline is real time and the
	// engine gives up on it.
	hard, cancel := clock.WithWriteDeadline(context.Background(),
		e.cfg.AbandonGrace+2*e.cfg.StoreTimeout)
	defer cancel()

	select {
	case <-e.workersDone:
		e.shutdownErr = ctx.Err()
	case <-hard.Done():
		e.shutdownErr = ErrDrainIncomplete
		e.log.Error("drain incomplete; steps abandoned", "stuck", e.InflightAttempts())
	}
}

// Workers reports the pool's CURRENT target size: Config.Workers at boot,
// and wherever the adaptive controller has since moved it (see resize.go),
// always within [Config.Workers, Config.MaxWorkers]. A deployment that never
// sets RUNMESH_MAX_WORKERS has MaxWorkers == Workers, so this reads exactly
// as it always has — "the configured size" — for every caller that predates
// adaptive sizing, this package's own tests included.
func (e *Engine) Workers() int { return int(e.size.Load()) }

// MaxWorkers reports the ceiling adaptive sizing may not cross. Unlike
// Workers it never changes after Start: MaxWorkers is the shape of the pool
// (how many goroutines exist), Workers is how many of them are lit up.
func (e *Engine) MaxWorkers() int { return e.cfg.MaxWorkers }

// Inflight reports how many steps are executing right now.
func (e *Engine) Inflight() int {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	return len(e.inflight)
}

// IdleWorkers reports how much capacity the dispatcher has not yet spent.
//
// It is deliberately NOT Workers() minus Inflight(), and the gap between the
// two is the interesting part. A capacity token is taken by the dispatcher
// before it calls Claim and is not consumed until the worker takes the lease
// off the channel, so during the claim-to-hand-off window a pool can have zero
// idle tokens and zero steps in flight at the same instant. That window is
// exactly the one that widens when the store is slow to claim, which is what
// makes the two gauges worth exporting separately.
func (e *Engine) IdleWorkers() int { return len(e.idle) }

// InflightAttempts lists the attempt ids currently executing. It is what the
// drain-incomplete log line names, so an operator can see which tool hung.
func (e *Engine) InflightAttempts() []string {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	out := make([]string, 0, len(e.inflight))
	for id := range e.inflight {
		out = append(out, id)
	}
	return out
}

// Stats are the engine's counters, for GET /api/v1/ready and, in Week 3, for
// the Prometheus exporter.
type Stats struct {
	Workers       int    `json:"workers"`
	Inflight      int    `json:"inflight"`
	ClaimErrors   uint64 `json:"claim_errors"`
	LeasesExpired uint64 `json:"leases_expired"`
}

// Stats returns a snapshot of the engine's counters.
func (e *Engine) Stats() Stats {
	return Stats{
		Workers:       e.Workers(),
		Inflight:      e.Inflight(),
		ClaimErrors:   e.claimErrors.Load(),
		LeasesExpired: e.leasesExpired.Load(),
	}
}

func (e *Engine) track(l runmesh.Lease) {
	e.inflightMu.Lock()
	e.inflight[l.AttemptID] = l
	e.inflightMu.Unlock()
}

func (e *Engine) untrack(l runmesh.Lease) {
	e.inflightMu.Lock()
	delete(e.inflight, l.AttemptID)
	e.inflightMu.Unlock()
}
