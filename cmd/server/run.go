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
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/config"
	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/k8s"
	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/pgstore"
	"github.com/kalanas210/runmesh/internal/policy"
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
		Backoff: engine.Backoff{
			Base:   cfg.BackoffBase,
			Max:    cfg.BackoffMax,
			Factor: cfg.BackoffFactor,
			Jitter: cfg.BackoffJitter,
		},
	}, engine.Deps{Store: store, Executor: executor, Sandbox: sandbox, Clock: clk, Log: log})
	if err != nil {
		log.Error("could not build the engine", "err", err)
		return 1
	}

	handler, api, err := httpapi.New(httpapi.Deps{
		Store:           store,
		Tools:           registry,
		Sandbox:         sandbox,
		Runtime:         eng,
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
	})
	if err != nil {
		log.Error("could not build the API", "err", err)
		return 1
	}

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

	return shutdown(srv, eng, store, api, log, cfg)
}

// shutdown runs the ordered drain. Every phase is bounded, and the last resort
// is a hard exit: a process that will not stop is worse than one that stops
// untidily, because an orchestrator has to SIGKILL it and nothing gets to
// record why.
func shutdown(srv *http.Server, eng *engine.Engine, store store,
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

	// 3. Close the store. Safe even if a straggler is still writing: Close is a
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
