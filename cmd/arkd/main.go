package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/btcsuite/btclog/v2"
	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo/build"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

func main() {
	ctx, shutdown := setupInterceptor()

	// 1) Load the server's config.
	cfg, err := darepo.LoadConfig()
	if err != nil {
		err := fmt.Errorf("error loading config: %w", err)
		_, _ = fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}

	cfg.Shutdown = shutdown

	// 2) Set up logging. A single handler is shared across all
	// subsystem loggers so that output flows to one destination with
	// consistent formatting.
	logHandler := btclog.NewDefaultHandler(os.Stdout)
	loggers := darepo.SetupLoggers(logHandler)

	if err := darepo.ApplyDebugLevel(
		loggers, cfg.LogLevel,
	); err != nil {
		err := fmt.Errorf("error setting log level: %w", err)
		_, _ = fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}

	// Inject the server's own logger into the config. Subsystem
	// loggers for child components are extracted from the loggers
	// map during NewServer.
	serverLog := loggers[darepo.Subsystem]
	cfg.Log = fn.Some(serverLog)
	cfg.Loggers = loggers

	// Attach the root server logger to the context for fallback
	// use by any code that calls build.LoggerFromContext(ctx).
	ctx = build.ContextWithLogger(ctx, serverLog)

	// 3) Construct the server.
	ctxt, cancel := context.WithTimeout(ctx, time.Second*10)

	server, err := darepo.NewServer(ctxt, cfg)
	if err != nil {
		err := fmt.Errorf("error creating server: %w", err)
		_, _ = fmt.Fprintln(os.Stderr, err)

		// Cancel the context to clean up resources.
		cancel()

		os.Exit(1)
	}

	// 4) Run the server until shutdown.
	if err := server.RunUntilShutdown(ctx); err != nil {
		err := fmt.Errorf("error starting server: %w", err)
		_, _ = fmt.Fprintln(os.Stderr, err)

		// Cancel the context to clean up resources.
		cancel()

		os.Exit(1)
	}

	// Normal exit path: cancel the context.
	cancel()
}

// setupInterceptor sets up a context that is canceled when an interrupt signal
// is received.
func setupInterceptor() (context.Context, context.CancelFunc) {
	// Create a channel to receive OS signals.
	sigChan := make(chan os.Signal, 1)

	signalsToCatch := []os.Signal{
		os.Interrupt,
		os.Kill,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	}

	signal.Notify(sigChan, signalsToCatch...)

	// Create a context that we can cancel.
	ctx, cancel := context.WithCancel(context.Background())

	// Start a goroutine that waits for a signal and cancels the context.
	go func() {
		select {
		case <-sigChan:
			cancel()

		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}
