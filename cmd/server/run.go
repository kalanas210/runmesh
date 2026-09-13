package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/config"
	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/eventbus"
	"github.com/kalanas210/runmesh/internal/gemini"
	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/k8s"
	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/metrics"
	"github.com/kalanas210/runmesh/internal/pgstore"
	"github.com/kalanas210/runmesh/internal/planner"
	"github.com/kalanas210/runmesh/internal/policy"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// store is the union of what the engine needs and what the API needs, declared
// HERE — in the one place that has to hold a concrete store — rather than
// exported by either. memstore and pgstore both satisfy it and neither knows
// the other exists, which is why swapping them is these twenty lines and not a
// refactor.
type store interface {
	engine.Store
	httpapi.Store
	// Dropped is the store's own count of events it could not hand to an
	// in-process subscriber because that subscriber was too slow. Both stores
	// already had it — the fan-out has always counted its drops rather than
	// blocking — and naming it here is what turns a number that existed into a
	// number an operator can see. internal/storetest declares it too, so both
	// implementations are held to it rather than merely happening to have it.
	Dropped() uint64
	// Subscribe is the live event feed the streaming route is built on. Both
	// stores already had it and internal/storetest already held both to it, so
	// naming it here widened no store file and changed no conformance suite —
	// which is the whole reason the union lives in this file rather than being
	// exported by either consumer.
	Subscribe(buf int) (<-chan runmesh.Event, func())
	Close() error
}

// newExecutor picks where tools run.
//
// This is the Week-1 seam being cashed in. tools.Local and *k8s.Executor
// satisfy the same one-method interface, so choosing between them is these
// twenty lines — the dispatcher, worker pool, lease handling, heartbeats,
// classification and state machine are identical either way, and none of them
// knows which one it got.
func newExecutor(cfg config.Config, registry tools.Registry,
	clk clock.Clock, log *slog.Logger) (tools.Executor, error) {

	if !cfg.RunsInKubernetes() {
		return tools.Local{
			Registry:       registry,
			MaxOutputBytes: cfg.MaxOutputBytes,
			Log:            log,
		}, nil
	}

	client, err := k8s.NewClient(cfg.K8sKubeconfig, cfg.K8sContext)
	if err != nil {
		return nil, err
	}
	exec, err := k8s.NewExecutor(client, k8s.Config{
		Namespace:          cfg.K8sNamespace,
		Image:              cfg.K8sImage,
		TaskServiceAccount: cfg.K8sTaskServiceAccount,
		TTLAfterFinished:   cfg.K8sTTLAfterFinished,
		DeadlineGrace:      cfg.K8sDeadlineGrace,
		PollInterval:       cfg.K8sPollInterval,
		MaxLogBytes:        cfg.MaxOutputBytes,
		CleanupTimeout:     cfg.K8sCleanupTimeout,
		Owner:              cfg.Owner,
		RunAsUser:          cfg.K8sRunAsUser,
		RunAsGroup:         cfg.K8sRunAsGroup,
		TerminationGrace:   cfg.K8sTerminationGrace,
	}, clk, log)
	if err != nil {
		return nil, err
	}
	log.Info("executing steps as Kubernetes Jobs",
		"namespace", cfg.K8sNamespace, "image", cfg.K8sImage,
		"task_service_account", cfg.K8sTaskServiceAccount)
	return exec, nil
}

// newSandbox builds the execution policy: the operator's ceiling on what any
// tool may have, paired with the registry it applies to.
//
// It is a hard boot failure when it does not build. A runtime that starts
// without a policy is a runtime executing model-authored plans with whatever
// limits the plans asked for, which is the one outcome Week 4 exists to make
// impossible.
func newSandbox(cfg config.Config, registry tools.Registry) (policy.Sandbox, error) {
	eng, err := policy.New(policy.Config{
		Mode:                    executionMode(cfg),
		DefaultCPU:              cfg.PolicyDefaultCPU,
		DefaultMemory:           cfg.PolicyDefaultMemory,
		DefaultEphemeralStorage: cfg.PolicyDefaultEphemeralStorage,
		MaxCPU:                  cfg.PolicyMaxCPU,
		MaxMemory:               cfg.PolicyMaxMemory,
		MaxEphemeralStorage:     cfg.PolicyMaxEphemeralStorage,
		// The plan-level ceilings are reused rather than duplicated as two more
		// variables. A step cannot be submitted above them, so a second, larger
		// policy ceiling would be dead configuration, and a second, smaller one
		// would silently contradict the 400 the API already returns.
		MaxTimeout:         cfg.Limits.MaxStepTimeout,
		MaxAttempts:        cfg.Limits.MaxAttempts,
		AllowNetwork:       cfg.PolicyAllowNetwork,
		AllowTools:         cfg.PolicyAllowTools,
		DenyTools:          cfg.PolicyDenyTools,
		AllowImagePrefixes: cfg.PolicyImagePrefixes,
		DefaultImage:       cfg.K8sImage,
	})
	if err != nil {
		return policy.Sandbox{}, err
	}
	return policy.NewSandbox(registry, eng), nil
}

// logPolicy states the sandbox once at boot, and names anything refused.
//
// A tool that is registered but denied is the single most confusing state this
// system has — plans 400 with a reason nobody reads, and the tool is right
// there in the catalogue — so it is said out loud at startup, with the reason,
// rather than left to be discovered from a rejected submission.
func logPolicy(log *slog.Logger, cfg config.Config, sandbox policy.Sandbox) {
	var denied []string
	for _, d := range sandbox.Descriptors() {
		if d.Denied {
			denied = append(denied, d.Name+" ("+d.DeniedReason+")")
		}
	}
	log.Info("execution policy",
		"mode", string(sandbox.Mode()),
		"default_cpu", cfg.PolicyDefaultCPU,
		"default_memory", cfg.PolicyDefaultMemory,
		"max_cpu", cfg.PolicyMaxCPU,
		"max_memory", cfg.PolicyMaxMemory,
		"max_ephemeral_storage", cfg.PolicyMaxEphemeralStorage,
		"network_allowed", cfg.PolicyAllowNetwork,
		"tools", sandbox.Registry().Names())
	if len(denied) > 0 {
		log.Warn("tools registered but refused by policy", "tools", denied)
	}
	if cfg.PolicyAllowNetwork {
		log.Warn("network tools are ENABLED: task pods labelled " +
			"runmesh.io/network=allow may reach addresses outside the cluster. " +
			"This is enforced by the NetworkPolicy in deploy/kubernetes, which " +
			"requires a CNI that implements it - kindnet does not.")
	}
}

// newPlanner builds the goal-to-plan pipeline, or returns nil.
//
// nil is a supported outcome and not a failure: a deployment with no planner
// serves every other route unchanged, and POST /api/v1/plans and /api/v1/goals
// answer 501 naming the variable. That is why the default is "none" rather than
// "heuristic" — a planning endpoint that quietly answers with rules over
// keywords, in a deployment where somebody believed they had configured a model,
// is a worse outcome than an honest refusal.
func newPlanner(cfg config.Config, sandbox policy.Sandbox, log *slog.Logger) (httpapi.Planner, error) {
	pcfg := planner.Config{
		MaxSteps:       cfg.PlannerMaxSteps,
		MaxRepairs:     cfg.PlannerMaxRepairs,
		DefaultTimeout: cfg.PlannerStepTimeout,
		Log:            log,
	}

	switch cfg.Planner {
	case config.PlannerNone:
		return nil, nil

	case config.PlannerHeuristic:
		log.Warn("the planner is HEURISTIC: plans are built by rules over the goal " +
			"text, not by a model. Set RUNMESH_PLANNER=gemini with an API key for " +
			"actual planning.")
		return planner.NewHeuristic(sandbox, pcfg), nil

	case config.PlannerGemini:
		client, err := gemini.New(gemini.Config{
			APIKey:          cfg.GeminiAPIKey,
			Model:           cfg.GeminiModel,
			BaseURL:         cfg.GeminiBaseURL,
			Timeout:         cfg.GeminiTimeout,
			MaxOutputTokens: cfg.GeminiMaxTokens,
			Temperature:     cfg.GeminiTemperature,
			Log:             log,
		})
		if err != nil {
			return nil, err
		}
		// The runtime's own plan limits are handed to the planner, so it
		// validates against exactly what the API will validate against. A
		// planner holding looser limits than the endpoint it feeds is a
		// machine for producing 400s.
		p, err := planner.New(planner.FromGemini(client), sandbox, cfg.Limits, pcfg)
		if err != nil {
			return nil, err
		}
		log.Info("planning with Gemini",
			"model", cfg.GeminiModel,
			"max_steps", cfg.PlannerMaxSteps, "max_repairs", cfg.PlannerMaxRepairs)
		return p, nil

	default:
		// config.Validate has already rejected this; the branch exists so a
		// future planner added to the constants without being added here is a
		// boot failure rather than a silent "none".
		return nil, fmt.Errorf("unknown planner %q", cfg.Planner)
	}
}

// executionMode is what GET /api/v1/tools reports. A registry describing its
// tools as in_process while every one of them runs in a pod would be a
// documentation bug that the dashboard and the Week-5 planner both inherit.
func executionMode(cfg config.Config) tools.ExecutionMode {
	if cfg.RunsInKubernetes() {
		return tools.ModeContainer
	}
	return tools.ModeInProcess
}

// openStore picks the store from configuration.
//
// The in-memory store is not a fallback that a misconfiguration can select by
// accident: it is chosen only by an EMPTY RUNMESH_DATABASE_URL, and a database
// URL that is set but unreachable fails the boot rather than quietly degrading
// to a store that loses every job. Silently starting non-durable is how an
// outage becomes data loss.
func openStore(ctx context.Context, cfg config.Config, log *slog.Logger) (store, error) {
	if !cfg.Durable() {
		return memstore.New(memstore.Options{
			JobEventBuffer:    cfg.JobEventBuffer,
			GlobalEventBuffer: cfg.GlobalEventBuffer,
		}), nil
	}
	s, err := pgstore.Open(ctx, cfg.DatabaseURL, pgstore.Options{
		MaxOpenConns:    cfg.DBMaxOpenConns,
		MaxIdleConns:    cfg.DBMaxIdleConns,
		ConnMaxLifetime: cfg.DBConnMaxLifetime,
		ConnMaxIdleTime: cfg.DBConnMaxIdleTime,
		ConnectTimeout:  cfg.DBConnectTimeout,
		Migrate:         cfg.MigrateOnBoot,
		Log:             log,
	})
	if err != nil {
		return nil, err
	}
	log.Info("state is DURABLE", "store", "postgres",
		"max_open_conns", cfg.DBMaxOpenConns, "migrate_on_boot", cfg.MigrateOnBoot)
	return s, nil
}

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)" ./cmd/server
var version = "dev"

// run is the ONLY wiring in the codebase. Every dependency is constructed
// here, in one readable sequence, and injected downwards; no package reaches
// out for a global, an environment variable, or a clock of its own.
//
// It takes its context, arguments, environment and output streams as
// parameters so the whole binary is testable: cmd/server/run_test.go boots it
// on port 0, drives a real DAG over real HTTP, and shuts it down with a real
// cancellation.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runmesh", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	addr := fs.String("addr", "", "listen address (overrides RUNMESH_HTTP_ADDR)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "runmesh %s\n\nUsage: runmesh [flags]\n\n"+
			"Configuration is read from the environment; see .env.example.\n\nFlags:\n", version)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "runmesh %s\n", version)
		return 0
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "configuration error:\n%v\n", err)
		return 2
	}
	if *addr != "" {
		cfg.HTTPAddr = *addr
	}

	log := newLogger(stdout, cfg)
	slog.SetDefault(log)

	clk := clock.System()

	// The metrics registry is constructed here and injected downwards, like
	// every other dependency. Nothing registers itself in an init() and nothing
	// reaches for a package-level default registerer — which is, on its own,
	// why promauto was never an option. See ADR 0012.
	reg := metrics.NewRegistry(clk)

	store, err := openStore(ctx, cfg, log)
	if err != nil {
		log.Error("could not open the store", "store", cfg.StoreName(), "err", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	registry := tools.Builtins(tools.Options{
		EnableTestTools: cfg.EnableTestTools,
		TaskImage:       cfg.TaskImage,
		PythonImage:     cfg.PythonImage,
	})
	// The metric set is built once the label vocabularies exist, and it is built
	// FROM them rather than from a hard-coded list: the tools come from the
	// registry that was just constructed and the routes from the API's own
	// table. That is what makes "the maximum number of time series this process
	// can export is a constant" true of the wiring rather than of a comment —
	// see internal/metrics/labels.go.
	mset := metrics.NewSet(reg, metrics.Vocabulary{
		Tools:  registry.Names(),
		Routes: httpapi.RoutePatterns(),
		// The API owns the fact that a route is long-lived, so it reports it
		// rather than this list repeating it. A stream's elapsed time is a
		// connection lifetime, not a service latency, and putting it in the
		// duration histogram would make every latency panel in the deployment
		// describe how long people leave browser tabs open — with nothing
		// failing to say so.
		StreamingRoutes: httpapi.StreamingRoutePatterns(),
		Version:         version,
		GoVersion:       runtime.Version(),
	})

	// Store latency, wrapped here rather than added to engine.Store. The
	// interface is untouched; see cmd/server/store_metrics.go for why that is
	// the whole point.
	store = observedStore{store: store, m: mset, clk: clk}

	// The live event fan-out behind GET /api/v1/jobs/{id}/stream.
	//
	// It is constructed here and not inside internal/engine, because that
	// package's doc fixes the goroutine census at 2 + Workers plus one per
	// in-flight step and a fan-out goroutine there would contradict a stated
	// invariant that no test would catch. It is an ACCELERATOR and not a source
	// of truth: the handler serves the durable timeline on a poll ticker and the
	// bus only shortens the wait, which is what makes the stream correct on a
	// multi-replica deployment rather than merely excused. Its Close is
	// registered in shutdown(), between the engine's drain and the store's
	// close, for reasons the ordering there spells out.
	bus := eventbus.New(store, log)
	bus.Start(ctx)

	executor, err := newExecutor(cfg, registry, clk, log)
	if err != nil {
		log.Error("could not build the executor", "executor", cfg.Executor, "err", err)
		return 1
	}

	// The execution policy, built once and shared by the API (submit-time
	// refusals), the engine (per-attempt limits) and GET /api/v1/tools (the
	// catalogue the Week-5 planner reads). One object, so the three cannot
	// disagree about what a tool is allowed to do.
	sandbox, err := newSandbox(cfg, registry)
	if err != nil {
		log.Error("could not build the execution policy", "err", err)
		return 2
	}
	logPolicy(log, cfg, sandbox)

	eng, err := engine.New(engine.Config{
		Owner:             cfg.Owner,
		Workers:           cfg.Workers,
		ClaimBatch:        cfg.ClaimBatch,
		PollInterval:      cfg.PollInterval,
		LeaseTTL:          cfg.LeaseTTL,
		HeartbeatInterval: cfg.HeartbeatInterval,
		StoreTimeout:      cfg.StoreTimeout,
		AbandonGrace:      cfg.AbandonGrace,
		ReconcileInterval: cfg.ReconcileInterval,
		ReconcileBatch:    cfg.ReconcileBatch,
		MaxOutputBytes:    cfg.MaxOutputBytes,

		MaxWorkers:           cfg.MaxWorkers,
		ConcurrencyInterval:  cfg.ConcurrencyInterval,
		ConcurrencyErrorRate: cfg.ConcurrencyErrorRate,
		ConcurrencyHeadroom:  cfg.ConcurrencyHeadroom,
		ConcurrencyStep:      cfg.ConcurrencyStep,
		Backoff: engine.Backoff{
			Base:   cfg.BackoffBase,
			Max:    cfg.BackoffMax,
			Factor: cfg.BackoffFactor,
			Jitter: cfg.BackoffJitter,
		},
	}, engine.Deps{Store: store, Executor: executor, Sandbox: sandbox,
		Observer: mset, Clock: clk, Log: log})
	if err != nil {
		log.Error("could not build the engine", "err", err)
		return 1
	}

	// The runtime and store gauges are PULLED at scrape time from the objects
	// that already own the numbers, so nothing is mirrored and nothing can drift
	// from engine.Stats(). Binding them also means the registry owns no
	// goroutine of its own — which matters, because the engine's package doc
	// fixes the goroutine census and an aggregation loop would contradict it
	// while nothing failed.
	mset.BindRuntime(eng)
	mset.BindStore(store)

	plannerImpl, err := newPlanner(cfg, sandbox, log)
	if err != nil {
		log.Error("could not build the planner", "planner", cfg.Planner, "err", err)
		return 2
	}

	handler, api, err := httpapi.New(httpapi.Deps{
		Store:           store,
		Tools:           registry,
		Sandbox:         sandbox,
		Planner:         plannerImpl,
		Runtime:         eng,
		Gatherer:        reg,
		Metrics:         mset,
		Stream:          bus,
		Clock:           clk,
		Log:             log,
		APIKeys:         cfg.APIKeys,
		Limits:          cfg.Limits,
		Defaults:        cfg.Defaults,
		MaxRequestBytes: cfg.MaxRequestBytes,
		MaxQueueDepth:   cfg.MaxQueueDepth,
		Durable:         cfg.Durable(),
		StoreName:       cfg.StoreName(),
		ExecutionMode:   executionMode(cfg),

		StreamMax:          cfg.StreamMax,
		StreamBuffer:       cfg.StreamBuffer,
		StreamHeartbeat:    cfg.StreamHeartbeat,
		StreamPollInterval: cfg.StreamPollInterval,
		StreamWriteTimeout: cfg.StreamWriteTimeout,
	})
	if err != nil {
		log.Error("could not build the API", "err", err)
		return 1
	}

	// Open streams are a GAUGE and not a histogram observation, and that is the
	// whole point of binding it here. Logger's duration observation fires when a
	// handler RETURNS, and a stream handler returns when somebody closes a
	// browser tab — so the request-duration histogram excludes this route (see
	// httpapi.StreamingRoutePatterns) and the count is pulled from the API's own
	// admission counter at scrape time instead. Nothing is mirrored, so the
	// gauge cannot drift from the number the 429 is decided against.
	mset.BindStreams(api)

	// A permissive default is defensible only if it cannot be held by accident,
	// so every key that carries no scope restriction is named once at boot.
	if unscoped := cfg.UnscopedKeyIDs(); len(unscoped) > 0 {
		log.Warn("API keys with no scope restriction can do anything",
			"keys", unscoped,
			"hint", "RUNMESH_API_KEYS=id:jobs.read+jobs.write=<key>")
	}

	// Answering 201 to a submission as though the job were safe, when it is
	// not, is the kind of thing that is fine until it is not. Saying so once at
	// boot, and again on every readiness probe, is the difference between a
	// known limitation and a nasty surprise.
	if !cfg.Durable() {
		log.Warn("state is IN MEMORY: a restart loses every job. " +
			"Set RUNMESH_DATABASE_URL to run against PostgreSQL.")
	}

	if err := eng.Start(ctx); err != nil {
		log.Error("could not start the engine", "err", err)
		return 1
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// Listening explicitly rather than through ListenAndServe means the port
	// is bound before this function reports readiness, so a test can ask the
	// listener which port it actually got when it asked for :0.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Error("could not listen", "addr", srv.Addr, "err", err)
		return 1
	}
	if ready, ok := ctx.Value(readyKey{}).(chan<- string); ok {
		ready <- ln.Addr().String()
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String(), "version", version)
		serveErr <- srv.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped unexpectedly", "err", err)
			return 1
		}
		return 0
	case <-ctx.Done():
	}

	return shutdown(srv, eng, bus, store, api, log, cfg)
}

// shutdown runs the ordered drain. Every phase is bounded, and the last resort
// is a hard exit: a process that will not stop is worse than one that stops
// untidily, because an orchestrator has to SIGKILL it and nothing gets to
// record why.
func shutdown(srv *http.Server, eng *engine.Engine, bus *eventbus.Broker, store store,
	api *httpapi.API, log *slog.Logger, cfg config.Config) int {

	log.Info("shutdown requested",
		"shutdown_grace", cfg.ShutdownGrace.String(),
		"drain_timeout", cfg.DrainTimeout.String())

	// Fail readiness first, so a load balancer stops sending traffic while the
	// listener is still accepting it.
	api.Draining()

	// The backstop. If any phase below wedges, the process still dies, with a
	// non-zero status an orchestrator can act on.
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(cfg.HardExitAfter):
			log.Error("shutdown exceeded its hard deadline; exiting immediately",
				"after", cfg.HardExitAfter.String())
			// Deliberately not a graceful return: at this point something is
			// stuck, and the only guarantee left is that the process ends.
			exitProcess(1)
		}
	}()
	defer close(done)

	code := 0

	// 1. Stop accepting; let in-flight HTTP requests finish. No new submissions
	//    and no new cancels arrive after this, which is what lets the engine
	//    drain against a queue that is no longer growing.
	hctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	if err := srv.Shutdown(hctx); err != nil {
		log.Warn("http server did not shut down cleanly", "err", err)
		code = 1
	}
	cancel()

	// 2. Drain the engine: stop claiming, let in-flight steps finish, and only
	//    cancel them if the deadline expires — at which point they are RELEASED
	//    with their retry budget untouched.
	dctx, cancel := context.WithTimeout(context.Background(), cfg.DrainTimeout)
	if err := eng.Shutdown(dctx); err != nil {
		log.Warn("engine drain incomplete", "err", err)
		code = 1
	}
	cancel()

	// 3. Stop the event fan-out, and THIS POSITION IS THE WHOLE SAFETY
	//    ARGUMENT. After the engine, so nothing is still producing events with
	//    the consumer gone; before the store, so the pump is not left reading a
	//    channel the store closed underneath it. Putting it in a defer beside
	//    the store's Close — the obvious-looking move — gets the order exactly
	//    backwards and leaves it to the hard-exit watchdog.
	//
	//    Any stream still attached has already seen the `bye` frame, because
	//    api.Draining() fired at the top of this function and every handler
	//    selects on it; this is the cleanup behind them, not the signal.
	if err := bus.Close(); err != nil {
		log.Warn("the event bus did not close cleanly", "err", err)
		code = 1
	}

	// 4. Close the store. Safe even if a straggler is still writing: Close is a
	//    state change, so a late write gets ErrClosed rather than a panic.
	if err := store.Close(); err != nil {
		log.Warn("store did not close cleanly", "err", err)
		code = 1
	}

	log.Info("stopped", "exit_code", code)
	return code
}

func newLogger(w io.Writer, cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h).With("service", "runmesh", "version", version)
}

// exitProcess is os.Exit, indirected so the whole-binary test can exercise
// the hard-exit backstop without killing the test runner.
var exitProcess = os.Exit

// readyKey lets a test learn the address the server actually bound when it
// asked for port 0. Production never sets it.
type readyKey struct{}

// withReadyChannel returns a context that reports the bound listen address.
func withReadyChannel(ctx context.Context, ch chan<- string) context.Context {
	return context.WithValue(ctx, readyKey{}, ch)
}
