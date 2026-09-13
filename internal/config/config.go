// Package config loads and validates every knob RunMesh has, from the
// environment, at boot. Nothing else in the codebase reads an environment
// variable, and there are no magic numbers scattered through the runtime: a
// value an operator can change lives here or it does not exist.
package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Config is the whole configuration surface. Every field has a default that
// makes a single-node development server work, except the API keys — starting
// unauthenticated must not be something a forgotten variable can cause.
type Config struct {
	// HTTP
	HTTPAddr          string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxRequestBytes   int64

	// APIKeys maps the sha256 digest of a presented key to what that key is
	// and what it may do. Storing the digest rather than the key means the map
	// lookup itself leaks no timing information about how many leading bytes of
	// a guess matched.
	APIKeys map[[32]byte]APIKey

	// Shutdown, in the order the phases run.
	ShutdownGrace time.Duration // stop accepting; let in-flight requests finish
	DrainTimeout  time.Duration // let in-flight steps finish
	HardExitAfter time.Duration // backstop: exit non-zero no matter what

	// Engine
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

	// Adaptive concurrency: size the worker pool from observed latency and
	// errors instead of holding it at Workers forever. MaxWorkers is the
	// ceiling; Workers stays the floor and the starting size. MaxWorkers
	// defaults to Workers, which makes the controller a no-op — the exact
	// fixed pool every deployment ran before this existed — so an operator
	// opts IN by widening the ceiling rather than opting out of a behaviour
	// change. See internal/engine.Concurrency for what ErrorRate and Headroom
	// decide and why neither is defaulted away from zero the way
	// ConcurrencyInterval and ConcurrencyStep are.
	MaxWorkers           int
	ConcurrencyInterval  time.Duration
	ConcurrencyErrorRate float64
	ConcurrencyHeadroom  float64
	ConcurrencyStep      int

	// Retry
	BackoffBase   time.Duration
	BackoffMax    time.Duration
	BackoffFactor float64
	BackoffJitter float64

	// Admission and plan limits
	MaxQueueDepth int
	Limits        runmesh.Limits
	Defaults      runmesh.Defaults

	// Store.
	//
	// DatabaseURL selects the store. Empty means the in-memory store, which is
	// a development convenience and NOT a deployment option: a restart loses
	// every job, GET /ready says so, and the server warns at boot.
	DatabaseURL       string
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration
	DBConnMaxIdleTime time.Duration
	DBConnectTimeout  time.Duration
	MigrateOnBoot     bool

	JobEventBuffer    int // per-job event ring, in-memory store only
	GlobalEventBuffer int // store-wide event ring, in-memory store only

	// Executor selects where tools run: "local" (this process) or
	// "kubernetes" (one Job per attempt). It is the Week-1 seam being used.
	Executor string

	// Kubernetes. Read only when Executor is "kubernetes".
	K8sNamespace          string
	K8sImage              string
	K8sTaskServiceAccount string
	K8sKubeconfig         string
	K8sContext            string
	K8sTTLAfterFinished   time.Duration
	K8sDeadlineGrace      time.Duration
	K8sPollInterval       time.Duration
	K8sCleanupTimeout     time.Duration

	// The pod security context every task runs under. These are configurable
	// and not constants because a hardened base image in somebody else's
	// registry may not use 65532 - but there is no way to switch the hardening
	// OFF, which is the point: runAsNonRoot, a read-only root filesystem, no
	// capabilities and no privilege escalation are properties of the spec
	// builder, not options.
	K8sRunAsUser        int64
	K8sRunAsGroup       int64
	K8sTerminationGrace time.Duration

	// Execution policy: the operator's ceiling on what any tool may have.
	// A descriptor asks; this grants. See internal/policy.
	PolicyDefaultCPU              string
	PolicyDefaultMemory           string
	PolicyDefaultEphemeralStorage string
	PolicyMaxCPU                  string
	PolicyMaxMemory               string
	PolicyMaxEphemeralStorage     string
	PolicyAllowNetwork            bool
	PolicyAllowTools              []string
	PolicyDenyTools               []string
	PolicyImagePrefixes           []string

	// Tools
	EnableTestTools bool
	MaxOutputBytes  int
	// TaskImage implements the container tool contract for the Go tools; it
	// defaults to K8sImage. PythonImage backs python_execute, and an empty one
	// means the tool is not registered at all - better an honest unknown_tool
	// than a tool that passes validation and fails every attempt.
	TaskImage   string
	PythonImage string

	// Planner. Selects who turns a goal into a plan: "none" (the planning
	// endpoints answer 501), "heuristic" (rules over the goal text, no API key
	// and no spend), or "gemini".
	Planner            string
	GeminiAPIKey       string
	GeminiModel        string
	GeminiBaseURL      string
	GeminiTimeout      time.Duration
	GeminiMaxTokens    int
	GeminiTemperature  float64
	PlannerMaxSteps    int
	PlannerMaxRepairs  int
	PlannerStepTimeout time.Duration

	// Observability
	LogLevel  slog.Level
	LogFormat string // "json" or "text"

	// Live event streaming: GET /api/v1/jobs/{id}/stream.
	//
	// StreamMax caps concurrent SSE connections, because each one costs a
	// goroutine, a store subscription and a socket for as long as a browser tab
	// is left open, and the refusal is a 429 a client can back off from.
	// StreamBuffer is one connection's share of the in-process fan-out; a full
	// one drops rather than blocking, and the handler repairs the drop from the
	// durable timeline. StreamHeartbeat keeps intermediaries from reaping a
	// correct but silent connection. StreamPollInterval is the leg that makes
	// this multi-replica correct: the maximum staleness of an event written by a
	// replica this connection's bus never saw. StreamWriteTimeout bounds one
	// frame's write so a dead-but-open peer cannot hold a handler goroutine for
	// the life of the process.
	StreamMax          int
	StreamBuffer       int
	StreamHeartbeat    time.Duration
	StreamPollInterval time.Duration
	StreamWriteTimeout time.Duration
}

// Load reads configuration from getenv (os.Getenv in production, a map in
// tests) and validates it. Every problem is reported at once via errors.Join,
// because an operator fixing a misconfigured deployment should not have to
// restart the process once per mistake.
func Load(getenv func(string) string) (Config, error) {
	l := &loader{getenv: getenv}

	c := Config{
		HTTPAddr:          l.str("RUNMESH_HTTP_ADDR", ":8080"),
		ReadHeaderTimeout: l.dur("RUNMESH_READ_HEADER_TIMEOUT", 5*time.Second),
		ReadTimeout:       l.dur("RUNMESH_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:      l.dur("RUNMESH_WRITE_TIMEOUT", 30*time.Second),
		IdleTimeout:       l.dur("RUNMESH_IDLE_TIMEOUT", 60*time.Second),
		MaxRequestBytes:   int64(l.num("RUNMESH_MAX_REQUEST_BYTES", 1<<20)),

		ShutdownGrace: l.dur("RUNMESH_SHUTDOWN_GRACE", 10*time.Second),
		DrainTimeout:  l.dur("RUNMESH_DRAIN_TIMEOUT", 30*time.Second),
		HardExitAfter: l.dur("RUNMESH_HARD_EXIT_AFTER", 60*time.Second),

		Owner:   l.str("RUNMESH_OWNER", defaultOwner()),
		Workers: l.num("RUNMESH_WORKERS", 8),
		// 0 means "track the pool size"; resolved below, once Workers is known.
		// A fixed default here would make RUNMESH_WORKERS=4 refuse to start.
		ClaimBatch:        l.num("RUNMESH_CLAIM_BATCH", 0),
		PollInterval:      l.dur("RUNMESH_POLL_INTERVAL", 250*time.Millisecond),
		LeaseTTL:          l.dur("RUNMESH_LEASE_TTL", 30*time.Second),
		HeartbeatInterval: l.dur("RUNMESH_HEARTBEAT_INTERVAL", 5*time.Second),
		StoreTimeout:      l.dur("RUNMESH_STORE_TIMEOUT", 5*time.Second),
		AbandonGrace:      l.dur("RUNMESH_ABANDON_GRACE", 10*time.Second),
		ReconcileInterval: l.dur("RUNMESH_RECONCILE_INTERVAL", 10*time.Second),
		ReconcileBatch:    l.num("RUNMESH_RECONCILE_BATCH", 100),

		// 0 means "no ceiling above Workers"; resolved below, once Workers is
		// known, for the same reason ClaimBatch is. RUNMESH_MAX_WORKERS=0
		// therefore cannot mean "unlimited" — it means "not set" — the same
		// choice ClaimBatch and DBMaxOpenConns already made for their own zero.
		MaxWorkers:          l.num("RUNMESH_MAX_WORKERS", 0),
		ConcurrencyInterval: l.dur("RUNMESH_CONCURRENCY_INTERVAL", 15*time.Second),
		// Unlike the two above, 0 here is not "not set" — it is a real answer
		// ("never shrink on errors" / "any latency above baseline pauses
		// growth"), the same way RUNMESH_BACKOFF_JITTER=0 is a real answer.
		// 0.2 and 0.5 are simply what an operator gets if they never say.
		ConcurrencyErrorRate: l.float("RUNMESH_CONCURRENCY_ERROR_RATE", 0.2),
		ConcurrencyHeadroom:  l.float("RUNMESH_CONCURRENCY_HEADROOM", 0.5),
		ConcurrencyStep:      l.num("RUNMESH_CONCURRENCY_STEP", 1),

		BackoffBase:   l.dur("RUNMESH_BACKOFF_BASE", time.Second),
		BackoffMax:    l.dur("RUNMESH_BACKOFF_MAX", 60*time.Second),
		BackoffFactor: l.float("RUNMESH_BACKOFF_FACTOR", 2.0),
		BackoffJitter: l.float("RUNMESH_BACKOFF_JITTER", 0.2),

		MaxQueueDepth: l.num("RUNMESH_MAX_QUEUE_DEPTH", 10_000),

		Limits: runmesh.Limits{
			MaxSteps:       l.num("RUNMESH_MAX_STEPS", 100),
			MaxDependsOn:   l.num("RUNMESH_MAX_DEPENDS_ON", 32),
			MaxParamsBytes: l.num("RUNMESH_MAX_PARAMS_BYTES", 64<<10),
			MaxStepTimeout: l.dur("RUNMESH_MAX_STEP_TIMEOUT", 15*time.Minute),
			MaxAttempts:    l.num("RUNMESH_MAX_ATTEMPTS_LIMIT", 10),
			MaxResultBytes: l.num("RUNMESH_MAX_RESULT_BYTES", 256<<10),
		},
		Defaults: runmesh.Defaults{
			StepTimeout: l.dur("RUNMESH_STEP_TIMEOUT", 30*time.Second),
			MaxAttempts: l.num("RUNMESH_MAX_ATTEMPTS", 3),
		},

		DatabaseURL: l.str("RUNMESH_DATABASE_URL", ""),
		// 0 means "size it from the worker pool"; resolved below, once Workers
		// is known, for the same reason ClaimBatch is.
		DBMaxOpenConns:    l.num("RUNMESH_DB_MAX_OPEN_CONNS", 0),
		DBMaxIdleConns:    l.num("RUNMESH_DB_MAX_IDLE_CONNS", 0),
		DBConnMaxLifetime: l.dur("RUNMESH_DB_CONN_MAX_LIFETIME", 30*time.Minute),
		DBConnMaxIdleTime: l.dur("RUNMESH_DB_CONN_MAX_IDLE_TIME", 5*time.Minute),
		DBConnectTimeout:  l.dur("RUNMESH_DB_CONNECT_TIMEOUT", 10*time.Second),
		MigrateOnBoot:     l.boolean("RUNMESH_MIGRATE_ON_BOOT", true),

		JobEventBuffer:    l.num("RUNMESH_JOB_EVENT_BUFFER", 512),
		GlobalEventBuffer: l.num("RUNMESH_GLOBAL_EVENT_BUFFER", 8192),

		Executor:              l.str("RUNMESH_EXECUTOR", ExecutorLocal),
		K8sNamespace:          l.str("RUNMESH_K8S_NAMESPACE", "runmesh-tasks"),
		K8sImage:              l.str("RUNMESH_K8S_IMAGE", ""),
		K8sTaskServiceAccount: l.str("RUNMESH_K8S_TASK_SERVICE_ACCOUNT", "runmesh-task"),
		K8sKubeconfig:         l.str("RUNMESH_K8S_KUBECONFIG", ""),
		K8sContext:            l.str("RUNMESH_K8S_CONTEXT", ""),
		K8sTTLAfterFinished:   l.dur("RUNMESH_K8S_TTL_AFTER_FINISHED", 5*time.Minute),
		K8sDeadlineGrace:      l.dur("RUNMESH_K8S_DEADLINE_GRACE", time.Minute),
		K8sPollInterval:       l.dur("RUNMESH_K8S_POLL_INTERVAL", 500*time.Millisecond),
		K8sCleanupTimeout:     l.dur("RUNMESH_K8S_CLEANUP_TIMEOUT", 15*time.Second),

		// 65532 is distroless' "nonroot" uid, which both shipped images
		// already use. The grace period is short because a task pod is deleted
		// exactly when RunMesh has decided it should stop: waiting 30 seconds
		// for a workload that is being cancelled is 30 seconds of a node held
		// by work nobody wants the answer to.
		K8sRunAsUser:        int64(l.num("RUNMESH_K8S_RUN_AS_USER", 65532)),
		K8sRunAsGroup:       int64(l.num("RUNMESH_K8S_RUN_AS_GROUP", 65532)),
		K8sTerminationGrace: l.dur("RUNMESH_K8S_TERMINATION_GRACE", 5*time.Second),

		// The shipped ceiling is small on purpose. A default that fits the
		// development kind cluster means the first surprise is "my step was
		// clamped", which is a log line, rather than "one plan filled the
		// node", which is an outage.
		PolicyDefaultCPU:              l.str("RUNMESH_POLICY_DEFAULT_CPU", "250m"),
		PolicyDefaultMemory:           l.str("RUNMESH_POLICY_DEFAULT_MEMORY", "128Mi"),
		PolicyDefaultEphemeralStorage: l.str("RUNMESH_POLICY_DEFAULT_EPHEMERAL_STORAGE", "64Mi"),
		PolicyMaxCPU:                  l.str("RUNMESH_POLICY_MAX_CPU", "1"),
		PolicyMaxMemory:               l.str("RUNMESH_POLICY_MAX_MEMORY", "512Mi"),
		PolicyMaxEphemeralStorage:     l.str("RUNMESH_POLICY_MAX_EPHEMERAL_STORAGE", "512Mi"),
		// Off. A runtime that executes model-authored plans should not reach
		// the internet because nobody said otherwise.
		PolicyAllowNetwork:  l.boolean("RUNMESH_POLICY_ALLOW_NETWORK", false),
		PolicyAllowTools:    l.csv("RUNMESH_POLICY_TOOLS_ALLOW"),
		PolicyDenyTools:     l.csv("RUNMESH_POLICY_TOOLS_DENY"),
		PolicyImagePrefixes: l.csv("RUNMESH_POLICY_IMAGE_PREFIXES"),

		EnableTestTools: l.boolean("RUNMESH_ENABLE_TEST_TOOLS", false),
		MaxOutputBytes:  l.num("RUNMESH_MAX_OUTPUT_BYTES", 64<<10),
		TaskImage:       l.str("RUNMESH_TASK_IMAGE", ""),
		PythonImage:     l.str("RUNMESH_PYTHON_IMAGE", ""),

		// "none" by default, not "heuristic". A planning endpoint that silently
		// answers with rules-over-keywords when an operator believed they had
		// configured a model is a worse outcome than a 501 naming the
		// variable, and RUNMESH_PLANNER is set once per deployment.
		Planner:         l.str("RUNMESH_PLANNER", PlannerNone),
		GeminiAPIKey:    l.str("RUNMESH_GEMINI_API_KEY", ""),
		GeminiModel:     l.str("RUNMESH_GEMINI_MODEL", "gemini-3.6-flash"), // as gemini.DefaultModel
		GeminiBaseURL:   l.str("RUNMESH_GEMINI_BASE_URL", ""),
		GeminiTimeout:   l.dur("RUNMESH_GEMINI_TIMEOUT", 30*time.Second),
		GeminiMaxTokens: l.num("RUNMESH_GEMINI_MAX_OUTPUT_TOKENS", 8192),
		// Zero, and deliberately. Planning is not a creative task: the same
		// goal against the same catalogue should produce the same DAG, because
		// a plan that varies run to run cannot be reviewed or cached.
		GeminiTemperature:  l.float("RUNMESH_GEMINI_TEMPERATURE", 0),
		PlannerMaxSteps:    l.num("RUNMESH_PLANNER_MAX_STEPS", 12),
		PlannerMaxRepairs:  l.num("RUNMESH_PLANNER_MAX_REPAIRS", 2),
		PlannerStepTimeout: l.dur("RUNMESH_PLANNER_STEP_TIMEOUT", 60*time.Second),

		LogLevel:  l.level("RUNMESH_LOG_LEVEL", slog.LevelInfo),
		LogFormat: l.str("RUNMESH_LOG_FORMAT", "json"),

		StreamMax:    l.num("RUNMESH_STREAM_MAX", 64),
		StreamBuffer: l.num("RUNMESH_STREAM_BUFFER", 256),
		// Fifteen seconds is under every default idle timeout an operator is
		// likely to have in front of this — nginx's proxy_read_timeout is 60s,
		// an AWS ALB's idle timeout is 60s, and this server's own is 60s.
		StreamHeartbeat: l.dur("RUNMESH_STREAM_HEARTBEAT", 15*time.Second),
		// One second is the worst-case staleness on a replica that did not write
		// the event. It is also a floor on cost — one indexed lookup per open
		// stream per second on an idle job — which is why a live event resets
		// the ticker and why a stream closes itself when its job is terminal.
		StreamPollInterval: l.dur("RUNMESH_STREAM_POLL_INTERVAL", time.Second),
		// Zero here means "not set"; the value is DERIVED below, because the
		// legal range for it depends on another variable.
		StreamWriteTimeout: l.dur("RUNMESH_STREAM_WRITE_TIMEOUT", 0),
	}
	c.APIKeys = l.apiKeys("RUNMESH_API_KEYS")
	// Resolved before ClaimBatch and DBMaxOpenConns, which are both bounded by
	// it rather than by Workers from here on: once adaptive sizing can grow
	// the pool past Workers, a claim batch or a connection pool still capped
	// at Workers is a ceiling on the feature this config exists to enable.
	if c.MaxWorkers == 0 {
		c.MaxWorkers = c.Workers
	}
	if c.ClaimBatch == 0 {
		c.ClaimBatch = c.Workers
	}
	// The pool has to hold every worker settling an outcome at once, plus the
	// dispatcher claiming, the reconciler sweeping, and a readiness probe. Size
	// it below that and a drain can deadlock: every connection held by a worker
	// waiting to write, and no connection left for the dispatcher to notice.
	// MaxWorkers, not Workers: adaptive sizing can grow the pool that far, and
	// every one of those workers can hold a connection at once too.
	if c.DBMaxOpenConns == 0 {
		c.DBMaxOpenConns = c.MaxWorkers + dbConnHeadroom
	}
	if c.DBMaxIdleConns == 0 {
		c.DBMaxIdleConns = c.DBMaxOpenConns
	}
	// One frame's write is bounded so that a TCP peer which is dead but not
	// closed, with a full socket buffer, cannot hold a handler goroutine for
	// ever. Ten seconds is the right bound — but it is only LEGAL while the
	// drain is at least that long, because a write that outlasts the drain
	// leaves srv.Shutdown blocked on a connection that will never go idle.
	//
	// So it is derived rather than fixed, exactly as ClaimBatch is derived from
	// Workers above. An operator who shortens RUNMESH_SHUTDOWN_GRACE has not
	// thereby asked for a boot failure about a knob they never set; an operator
	// who sets this one longer than the grace HAS asked for that connection, and
	// Validate still refuses it.
	if c.StreamWriteTimeout == 0 {
		c.StreamWriteTimeout = min(defaultStreamWriteTimeout, c.ShutdownGrace)
	}
	// The Go tools' image is the default task image unless it is overridden,
	// so a single-image deployment configures one variable and a two-image one
	// configures two.
	if c.TaskImage == "" {
		c.TaskImage = c.K8sImage
	}

	if err := errors.Join(append(l.errs, c.Validate()...)...); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

// Validate checks the cross-field invariants. Each one is here because
// violating it produces a subtle runtime failure rather than an obvious one,
// so it is better caught at boot with a sentence explaining why.
func (c Config) Validate() []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(c.APIKeys) == 0 {
		bad("RUNMESH_API_KEYS is required (format: id[:scope+scope]=key,...) - " +
			"refusing to start unauthenticated")
	}
	if c.Workers < 1 {
		bad("RUNMESH_WORKERS must be >= 1, got %d", c.Workers)
	}
	// Below Workers, the pool would start above its own ceiling. Equal to it
	// is the default and makes adaptive sizing a no-op; above it is the
	// opt-in.
	if c.MaxWorkers < c.Workers {
		bad("RUNMESH_MAX_WORKERS must be >= RUNMESH_WORKERS=%d, got %d", c.Workers, c.MaxWorkers)
	}
	if c.ClaimBatch < 1 || c.ClaimBatch > c.MaxWorkers {
		bad("RUNMESH_CLAIM_BATCH must be in [1, RUNMESH_MAX_WORKERS=%d], got %d", c.MaxWorkers, c.ClaimBatch)
	}
	if c.ConcurrencyInterval <= 0 {
		bad("RUNMESH_CONCURRENCY_INTERVAL must be > 0, got %s", c.ConcurrencyInterval)
	}
	if c.ConcurrencyErrorRate < 0 || c.ConcurrencyErrorRate > 1 {
		bad("RUNMESH_CONCURRENCY_ERROR_RATE must be in [0, 1], got %v", c.ConcurrencyErrorRate)
	}
	if c.ConcurrencyHeadroom < 0 {
		bad("RUNMESH_CONCURRENCY_HEADROOM must be >= 0, got %v", c.ConcurrencyHeadroom)
	}
	if c.ConcurrencyStep < 1 {
		bad("RUNMESH_CONCURRENCY_STEP must be >= 1, got %d", c.ConcurrencyStep)
	}
	if c.PollInterval <= 0 {
		bad("RUNMESH_POLL_INTERVAL must be > 0, got %s", c.PollInterval)
	}
	if c.LeaseTTL <= 0 {
		bad("RUNMESH_LEASE_TTL must be > 0, got %s", c.LeaseTTL)
	}
	// Three heartbeats inside one lease: two may be lost to a slow store or a
	// GC pause without the lease expiring underneath a step that is running
	// perfectly well.
	if c.HeartbeatInterval <= 0 || c.HeartbeatInterval > c.LeaseTTL/3 {
		bad("RUNMESH_HEARTBEAT_INTERVAL must be in (0, RUNMESH_LEASE_TTL/3=%s], got %s",
			c.LeaseTTL/3, c.HeartbeatInterval)
	}
	// A store write must not be able to outlive the drain it is part of.
	if c.StoreTimeout <= 0 || c.StoreTimeout >= c.DrainTimeout {
		bad("RUNMESH_STORE_TIMEOUT must be in (0, RUNMESH_DRAIN_TIMEOUT=%s), got %s",
			c.DrainTimeout, c.StoreTimeout)
	}
	// Waiting longer than a lease for a tool that ignores cancellation would
	// let the reconciler hand the step to somebody else while we still wait.
	if c.AbandonGrace <= 0 || c.AbandonGrace >= c.LeaseTTL {
		bad("RUNMESH_ABANDON_GRACE must be in (0, RUNMESH_LEASE_TTL=%s), got %s",
			c.LeaseTTL, c.AbandonGrace)
	}
	if c.ReconcileInterval <= 0 {
		bad("RUNMESH_RECONCILE_INTERVAL must be > 0, got %s", c.ReconcileInterval)
	}
	if c.ReconcileBatch < 1 {
		bad("RUNMESH_RECONCILE_BATCH must be >= 1, got %d", c.ReconcileBatch)
	}
	if c.BackoffBase <= 0 {
		bad("RUNMESH_BACKOFF_BASE must be > 0, got %s", c.BackoffBase)
	}
	if c.BackoffMax < c.BackoffBase {
		bad("RUNMESH_BACKOFF_MAX (%s) must be >= RUNMESH_BACKOFF_BASE (%s)", c.BackoffMax, c.BackoffBase)
	}
	if c.BackoffFactor < 1 {
		bad("RUNMESH_BACKOFF_FACTOR must be >= 1, got %v", c.BackoffFactor)
	}
	if c.BackoffJitter < 0 || c.BackoffJitter > 1 {
		bad("RUNMESH_BACKOFF_JITTER must be in [0, 1], got %v", c.BackoffJitter)
	}
	// The hard-exit backstop must fire strictly after the graceful path has had
	// its full budget, or it would abort a shutdown that was going to succeed.
	if c.HardExitAfter <= c.ShutdownGrace+c.DrainTimeout {
		bad("RUNMESH_HARD_EXIT_AFTER (%s) must exceed RUNMESH_SHUTDOWN_GRACE + RUNMESH_DRAIN_TIMEOUT (%s)",
			c.HardExitAfter, c.ShutdownGrace+c.DrainTimeout)
	}
	if c.Defaults.StepTimeout <= 0 || c.Defaults.StepTimeout > c.Limits.MaxStepTimeout {
		bad("RUNMESH_STEP_TIMEOUT must be in (0, RUNMESH_MAX_STEP_TIMEOUT=%s], got %s",
			c.Limits.MaxStepTimeout, c.Defaults.StepTimeout)
	}
	if c.Defaults.MaxAttempts < 1 || c.Defaults.MaxAttempts > c.Limits.MaxAttempts {
		bad("RUNMESH_MAX_ATTEMPTS must be in [1, RUNMESH_MAX_ATTEMPTS_LIMIT=%d], got %d",
			c.Limits.MaxAttempts, c.Defaults.MaxAttempts)
	}
	if c.MaxQueueDepth < 1 {
		bad("RUNMESH_MAX_QUEUE_DEPTH must be >= 1, got %d", c.MaxQueueDepth)
	}
	if c.Limits.MaxSteps < 1 {
		bad("RUNMESH_MAX_STEPS must be >= 1, got %d", c.Limits.MaxSteps)
	}
	if c.DatabaseURL != "" {
		if c.DBMaxOpenConns < c.MaxWorkers+dbConnHeadroom {
			bad("RUNMESH_DB_MAX_OPEN_CONNS must be at least RUNMESH_MAX_WORKERS + %d = %d, got %d "+
				"(every worker up to the adaptive ceiling may hold a connection to settle "+
				"its outcome while the dispatcher, the reconciler and a readiness probe "+
				"still need one)",
				dbConnHeadroom, c.MaxWorkers+dbConnHeadroom, c.DBMaxOpenConns)
		}
		if c.DBMaxIdleConns < 1 || c.DBMaxIdleConns > c.DBMaxOpenConns {
			bad("RUNMESH_DB_MAX_IDLE_CONNS must be in [1, RUNMESH_DB_MAX_OPEN_CONNS=%d], got %d",
				c.DBMaxOpenConns, c.DBMaxIdleConns)
		}
		// A connection that outlives the drain it is part of would keep a
		// half-finished transaction alive past the point the process claims to
		// have stopped.
		if c.DBConnectTimeout <= 0 {
			bad("RUNMESH_DB_CONNECT_TIMEOUT must be > 0, got %s", c.DBConnectTimeout)
		}
	}
	if c.JobEventBuffer < 1 {
		bad("RUNMESH_JOB_EVENT_BUFFER must be >= 1, got %d", c.JobEventBuffer)
	}
	if c.GlobalEventBuffer < 1 {
		bad("RUNMESH_GLOBAL_EVENT_BUFFER must be >= 1, got %d", c.GlobalEventBuffer)
	}
	switch c.Executor {
	case ExecutorLocal:
	case ExecutorKubernetes:
		if c.K8sNamespace == "" {
			bad("RUNMESH_K8S_NAMESPACE is required when RUNMESH_EXECUTOR=kubernetes")
		}
		if c.K8sImage == "" {
			bad("RUNMESH_K8S_IMAGE is required when RUNMESH_EXECUTOR=kubernetes: " +
				"a tool that names no image of its own has nothing to run")
		}
		if c.K8sPollInterval <= 0 {
			bad("RUNMESH_K8S_POLL_INTERVAL must be > 0, got %s", c.K8sPollInterval)
		}
		// The Kubernetes deadline is a BACKSTOP and must be strictly looser
		// than RunMesh's own, or the two race — and Kubernetes winning replaces
		// a classified TIMED_OUT carrying a timeline entry with an opaque
		// DeadlineExceeded.
		if c.K8sDeadlineGrace <= 0 {
			bad("RUNMESH_K8S_DEADLINE_GRACE must be > 0, got %s: the Kubernetes "+
				"deadline has to outlast RunMesh's own, not tie with it", c.K8sDeadlineGrace)
		}
		// A cleanup that outlives the drain would still be deleting workloads
		// after the process claims to have stopped.
		if c.K8sCleanupTimeout <= 0 || c.K8sCleanupTimeout >= c.DrainTimeout {
			bad("RUNMESH_K8S_CLEANUP_TIMEOUT must be in (0, RUNMESH_DRAIN_TIMEOUT=%s), got %s",
				c.DrainTimeout, c.K8sCleanupTimeout)
		}
	default:
		bad("RUNMESH_EXECUTOR must be %q or %q, got %q",
			ExecutorLocal, ExecutorKubernetes, c.Executor)
	}
	if c.MaxOutputBytes < 1 {
		bad("RUNMESH_MAX_OUTPUT_BYTES must be >= 1, got %d", c.MaxOutputBytes)
	}
	// runAsNonRoot with runAsUser: 0 is a spec the kubelet refuses at
	// admission, and it refuses it for every pod, so catching it here turns a
	// fleet-wide outage into a boot error naming the variable.
	if c.K8sRunAsUser < 1 {
		bad("RUNMESH_K8S_RUN_AS_USER must be >= 1, got %d: task pods run with "+
			"runAsNonRoot, and uid 0 is refused at admission", c.K8sRunAsUser)
	}
	if c.K8sRunAsGroup < 1 {
		bad("RUNMESH_K8S_RUN_AS_GROUP must be >= 1, got %d", c.K8sRunAsGroup)
	}
	if c.K8sTerminationGrace < 0 {
		bad("RUNMESH_K8S_TERMINATION_GRACE must not be negative, got %s", c.K8sTerminationGrace)
	}
	// A tool named on both lists is not a policy, it is a question. Deny wins
	// at runtime, but the operator who wrote both meant one of them.
	for _, name := range c.PolicyAllowTools {
		for _, denied := range c.PolicyDenyTools {
			if name == denied {
				bad("tool %q is on both RUNMESH_POLICY_TOOLS_ALLOW and "+
					"RUNMESH_POLICY_TOOLS_DENY; deny would win, so say so once", name)
			}
		}
	}
	// python_execute is registered only when it has an image, so allowlisting
	// it without one produces a plan that 400s with unknown_tool and an
	// operator who is certain they enabled it.
	if c.PythonImage == "" {
		for _, name := range c.PolicyAllowTools {
			if name == "python_execute" {
				bad("RUNMESH_POLICY_TOOLS_ALLOW names python_execute but " +
					"RUNMESH_PYTHON_IMAGE is empty, so the tool is not registered")
			}
		}
	}
	if c.MaxRequestBytes < 1 {
		bad("RUNMESH_MAX_REQUEST_BYTES must be >= 1, got %d", c.MaxRequestBytes)
	}
	switch c.Planner {
	case PlannerNone, PlannerHeuristic:
	case PlannerGemini:
		if c.GeminiAPIKey == "" {
			bad("RUNMESH_GEMINI_API_KEY is required when RUNMESH_PLANNER=gemini")
		}
		if c.GeminiModel == "" {
			bad("RUNMESH_GEMINI_MODEL must not be empty")
		}
		if c.GeminiTimeout <= 0 {
			bad("RUNMESH_GEMINI_TIMEOUT must be > 0, got %s", c.GeminiTimeout)
		}
		if c.GeminiMaxTokens < 256 {
			bad("RUNMESH_GEMINI_MAX_OUTPUT_TOKENS must be >= 256, got %d: a plan "+
				"cut off mid-JSON is reported as a truncation, and every one of "+
				"them costs a full round trip", c.GeminiMaxTokens)
		}
		if c.GeminiTemperature < 0 || c.GeminiTemperature > 2 {
			bad("RUNMESH_GEMINI_TEMPERATURE must be in [0, 2], got %v", c.GeminiTemperature)
		}
	default:
		bad("RUNMESH_PLANNER must be %q, %q or %q, got %q",
			PlannerNone, PlannerHeuristic, PlannerGemini, c.Planner)
	}
	if c.PlannerMaxSteps < 1 {
		bad("RUNMESH_PLANNER_MAX_STEPS must be >= 1, got %d", c.PlannerMaxSteps)
	}
	if c.PlannerMaxSteps > c.Limits.MaxSteps {
		bad("RUNMESH_PLANNER_MAX_STEPS (%d) must not exceed RUNMESH_MAX_STEPS (%d): "+
			"a planner allowed to produce plans the API will reject is a machine "+
			"for producing 400s", c.PlannerMaxSteps, c.Limits.MaxSteps)
	}
	if c.PlannerMaxRepairs < 0 || c.PlannerMaxRepairs > 5 {
		bad("RUNMESH_PLANNER_MAX_REPAIRS must be in [0, 5], got %d: a model that "+
			"cannot produce a valid plan given the schema, the catalogue and an "+
			"explicit list of what was wrong will not manage it on the sixth try",
			c.PlannerMaxRepairs)
	}
	if c.PlannerStepTimeout <= 0 || c.PlannerStepTimeout > c.Limits.MaxStepTimeout {
		bad("RUNMESH_PLANNER_STEP_TIMEOUT must be in (0, RUNMESH_MAX_STEP_TIMEOUT=%s], got %s",
			c.Limits.MaxStepTimeout, c.PlannerStepTimeout)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		bad("RUNMESH_LOG_FORMAT must be json or text, got %q", c.LogFormat)
	}
	if c.StreamMax < 1 {
		bad("RUNMESH_STREAM_MAX must be >= 1, got %d: zero would register the "+
			"stream route and then refuse every request to it", c.StreamMax)
	}
	if c.StreamBuffer < 1 {
		bad("RUNMESH_STREAM_BUFFER must be >= 1, got %d", c.StreamBuffer)
	}
	// A heartbeat slower than an intermediary's idle timeout is a heartbeat that
	// never arrives: the proxy reaps the connection first, the browser
	// reconnects, and the reconnect loop looks like an unstable backend. This
	// server's own IdleTimeout is the one such timeout the process can see, so
	// it is the one it can check.
	if c.StreamHeartbeat <= 0 || c.StreamHeartbeat >= c.IdleTimeout {
		bad("RUNMESH_STREAM_HEARTBEAT must be in (0, RUNMESH_IDLE_TIMEOUT=%s), got %s: "+
			"a heartbeat slower than an intermediary's idle timeout is a heartbeat "+
			"that never arrives", c.IdleTimeout, c.StreamHeartbeat)
	}
	if c.StreamPollInterval <= 0 {
		bad("RUNMESH_STREAM_POLL_INTERVAL must be > 0, got %s: the poll is what "+
			"makes the stream correct across replicas, and disabling it would make "+
			"the in-process bus the only source", c.StreamPollInterval)
	}
	// A write that can outlast the drain leaves srv.Shutdown blocked on a
	// connection that will never go idle, which is the failure the drain
	// handling in the stream handler exists to prevent.
	if c.StreamWriteTimeout <= 0 || c.StreamWriteTimeout > c.ShutdownGrace {
		bad("RUNMESH_STREAM_WRITE_TIMEOUT must be in (0, RUNMESH_SHUTDOWN_GRACE=%s], got %s: "+
			"a write that can outlast the drain leaves srv.Shutdown blocked on a "+
			"connection that will never go idle", c.ShutdownGrace, c.StreamWriteTimeout)
	}
	return errs
}

// Who turns a goal into a plan.
//
// PlannerHeuristic is not an "offline model" and does not pretend to be: it is
// a handful of rules over the words in the goal. It exists so the whole path —
// goal, plan, validation, DAG, execution — works on a laptop with no API key
// and no billing account, which is the plan's own instruction, and so that the
// end-to-end tests of that path are assertions rather than samples of a model's
// mood.
const (
	PlannerNone      = "none"
	PlannerHeuristic = "heuristic"
	PlannerGemini    = "gemini"
)

// The two places a tool can run. The seam between them was declared in Week 1
// as tools.Executor; this is the configuration that finally uses it.
const (
	ExecutorLocal      = "local"
	ExecutorKubernetes = "kubernetes"
)

// RunsInKubernetes reports whether steps execute as Kubernetes Jobs.
func (c Config) RunsInKubernetes() bool { return c.Executor == ExecutorKubernetes }

// dbConnHeadroom is how many connections the pool must hold beyond the worker
// pool: one for the dispatcher, one for the reconciler, and two for concurrent
// API traffic including the readiness probe.
const dbConnHeadroom = 4

// defaultStreamWriteTimeout is the ceiling on one SSE frame's write, before the
// shutdown grace clamps it. It is a constant rather than a literal in the
// loader because the loader reads zero for "not set" and the real default lives
// in the derivation a few lines below the Config literal.
const defaultStreamWriteTimeout = 10 * time.Second

// Durable reports whether the configured store survives a restart. It is the
// value GET /api/v1/ready publishes, and the reason the server warns at boot
// when it is false.
func (c Config) Durable() bool { return c.DatabaseURL != "" }

// StoreName names the configured store for logs and the readiness body.
func (c Config) StoreName() string {
	if c.Durable() {
		return "postgres"
	}
	return "memory"
}

// APIKeyID returns the id of the key with this digest, if it is configured.
func (c Config) APIKeyID(digest [32]byte) (string, bool) {
	key, ok := c.APIKeys[digest]
	return key.ID, ok
}

// UnscopedKeyIDs names the keys configured without a scope list, which are
// therefore granted everything. The server logs them at boot: a permissive
// default is defensible only if it is impossible to hold by accident.
func (c Config) UnscopedKeyIDs() []string {
	var out []string
	for _, k := range c.APIKeys {
		if k.Unscoped {
			out = append(out, k.ID)
		}
	}
	sort.Strings(out)
	return out
}

// KeyDigest is the lookup key for APIKeys.
func KeyDigest(presented string) [32]byte { return sha256.Sum256([]byte(presented)) }

// defaultOwner names this process in the leases it takes. Host plus pid, so
// two replicas on one machine cannot be confused for each other in the
// reclaimed-an-expired-lease log line — which is the first thing anyone reads
// when a worker dies.
func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// loader accumulates parse errors instead of returning on the first one.
type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) raw(key string) (string, bool) {
	v := strings.TrimSpace(l.getenv(key))
	return v, v != ""
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

func (l *loader) num(key string, def int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %q is not an integer", key, v))
		return def
	}
	return n
}

func (l *loader) float(key string, def float64) float64 {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %q is not a number", key, v))
		return def
	}
	return f
}

func (l *loader) dur(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %q is not a duration (e.g. 30s, 5m)", key, v))
		return def
	}
	return d
}

// csv reads a comma-separated list. Empty entries are dropped rather than
// producing an empty-string element, because an allowlist containing "" is an
// allowlist that quietly matches a tool whose name nobody set.
func (l *loader) csv(key string) []string {
	v, ok := l.raw(key)
	if !ok {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (l *loader) boolean(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %q is not a boolean", key, v))
		return def
	}
	return b
}

func (l *loader) level(key string, def slog.Level) slog.Level {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %q is not a log level (debug|info|warn|error)", key, v))
		return def
	}
	return lvl
}

// apiKeys parses the key list: comma-separated `id[:scope+scope]=key` entries.
//
// `dev=abc…` from Week 1 still parses, as an unscoped key — see APIKey.Unscoped
// for why an upgrade must not silently strip every running key of its access.
func (l *loader) apiKeys(key string) map[[32]byte]APIKey {
	v, ok := l.raw(key)
	if !ok {
		return nil
	}
	out := make(map[[32]byte]APIKey)
	for i, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, secret, err := parseAPIKeyEntry(part, i+1)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		switch {
		case secret == "":
			l.errs = append(l.errs, fmt.Errorf("%s: entry %d has an empty key", key, i+1))
		case len(secret) < 16:
			l.errs = append(l.errs, fmt.Errorf(
				"%s: key %q is %d characters; use at least 16 (openssl rand -hex 32)",
				key, parsed.ID, len(secret)))
		default:
			out[KeyDigest(secret)] = parsed
		}
	}
	return out
}
