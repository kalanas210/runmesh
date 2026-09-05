// Package engine is the runtime: the dispatcher that claims work, the bounded
// worker pool that executes it, the reconciler that recovers what a dead
// worker left behind, and the pure policy — Classify and Backoff — that
// decides what every outcome means.
//
// The goroutine census is fixed at boot and is exactly 2 + Workers, plus one
// transient goroutine per in-flight step: one dispatcher, one reconciler, N
// workers, and a tool runner for each step actually executing. There is no
// goroutine per job, no goroutine per HTTP request beyond net/http's own, and
// no supervisor tree.
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
	// Tools optionally restricts this engine to a subset of the registry. It
	// is how Week 3 splits in-process tools from container tools across
	// separate worker fleets without a second scheduler.
	Tools []string
}

func (c *Config) setDefaults() {
	if c.Workers < 1 {
		c.Workers = 1
	}
	if c.ClaimBatch < 1 || c.ClaimBatch > c.Workers {
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

// Deps are the collaborators an engine needs. They are all interfaces or
// values, so a test constructs an engine with a fake clock and an in-memory
// store and nothing else.
type Deps struct {
	Store    Store
	Executor tools.Executor
	Clock    clock.Clock
	Log      *slog.Logger
}

// Engine owns the runtime goroutines.
type Engine struct {
	cfg   Config
	store Store
	exec  tools.Executor
	clock clock.Clock
	log   *slog.Logger

	// leases is unbuffered: a lease is handed directly to a worker that is
	// already waiting, so a claimed step is never sitting in a queue nobody is
	// accounting for. Closed by the dispatcher, and only by the dispatcher.
	leases chan runmesh.Lease
	// idle holds one token per free worker. Buffered to Workers and never
	// closed, because it has many senders.
	idle chan struct{}

	wg      sync.WaitGroup // worker pool
	wgLoops sync.WaitGroup // dispatcher and reconciler

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
	cfg.setDefaults()

	return &Engine{
		cfg:          cfg,
		store:        d.Store,
		exec:         d.Executor,
		clock:        d.Clock,
		log:          d.Log.With("component", "engine"),
		leases:       make(chan runmesh.Lease),
		idle:         make(chan struct{}, cfg.Workers),
		shutdownDone: make(chan struct{}),
		workersDone:  make(chan struct{}),
		inflight:     make(map[string]runmesh.Lease),
	}, nil
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
	e.wg.Add(e.cfg.Workers)
	for i := range e.cfg.Workers {
		e.idle <- struct{}{}
		go e.runWorker(hardCtx, i)
	}

	e.wgLoops.Add(2)
	go func() {
		defer e.wgLoops.Done()
		e.runDispatcher(claimCtx)
	}()
	go e.runReconciler(claimCtx)

	e.log.Info("engine started",
		"workers", e.cfg.Workers, "claim_batch", e.cfg.ClaimBatch,
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

// Workers reports the configured size of the worker pool.
func (e *Engine) Workers() int { return e.cfg.Workers }

// Inflight reports how many steps are executing right now.
func (e *Engine) Inflight() int {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	return len(e.inflight)
}

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
		Workers:       e.cfg.Workers,
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
