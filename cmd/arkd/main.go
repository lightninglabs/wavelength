package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver
	darepoclient "github.com/lightninglabs/darepo-client"
)

func main() {
	ctx, shutdown := setupInterceptor()

	// 1) Load the server's config.
	cfg, err := darepoclient.LoadConfig()
	if err != nil {
		err := fmt.Errorf("error loading config: %w", err)
		_, _ = fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}

	cfg.Shutdown = shutdown

	// 2) Construct the server.
	ctxt, cancel := context.WithTimeout(ctx, time.Second*10)

	server, err := darepoclient.NewServer(ctxt, cfg)
	if err != nil {
		err := fmt.Errorf("error creating server: %w", err)
		_, _ = fmt.Fprintln(os.Stderr, err)

		// Cancel the context to clean up resources.
		cancel()

		os.Exit(1)
	}

	// 3) Run the server until shutdown.
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
