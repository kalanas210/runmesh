// Command server runs the RunMesh API and execution engine.
//
// Configuration comes entirely from the environment; see internal/config or
// .env.example for the full list. RUNMESH_API_KEYS is the only variable
// without a default, because starting unauthenticated must not be something a
// forgotten variable can cause.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// The signal context is created here rather than inside run so that run
	// stays a plain function a test can call with its own context, on its own
	// port, without touching process-wide signal handling.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
