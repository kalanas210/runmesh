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
	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/tools"
)

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
	store := memstore.New(memstore.Options{
		JobEventBuffer:    cfg.JobEventBuffer,
		GlobalEventBuffer: cfg.GlobalEventBuffer,
	})
	defer func() { _ = store.Close() }()

	registry := tools.Builtins(cfg.EnableTestTools)
	executor := tools.Local{
		Registry:       registry,
		MaxOutputBytes: cfg.MaxOutputBytes,
		Log:            log,
	}

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
	}, engine.Deps{Store: store, Executor: executor, Clock: clk, Log: log})
	if err != nil {
		log.Error("could not build the engine", "err", err)
		return 1
	}

	handler, api, err := httpapi.New(httpapi.Deps{
		Store:           store,
		Tools:           registry,
		Runtime:         eng,
		Clock:           clk,
		Log:             log,
		APIKeys:         cfg.APIKeys,
		Limits:          cfg.Limits,
		Defaults:        cfg.Defaults,
		MaxRequestBytes: cfg.MaxRequestBytes,
		MaxQueueDepth:   cfg.MaxQueueDepth,
		Durable:         false,
		StoreName:       "memory",
	})
	if err != nil {
		log.Error("could not build the API", "err", err)
		return 1
	}

	// Week 1 answers 201 to a submission as though the job were safe, and it
	// is not. Saying so once at boot, and again on every readiness probe, is
	// the difference between a known limitation and a nasty surprise.
	log.Warn("state is IN MEMORY: a restart loses every job. PostgreSQL arrives in Week 2.")

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
func shutdown(srv *http.Server, eng *engine.Engine, store *memstore.Store,
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
