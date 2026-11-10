// Command wavenotifyd runs the publisher-owned APNs notification relay.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lightninglabs/wavelength/notifyrelay"
)

// version is populated by the image build without embedding runtime secrets.
var version = "dev"

// main gives the HTTP server and worker one process-owned cancellation scope.
func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		slog.Error("Notification relay exited", slog.Any("error", err))
		os.Exit(1)
	}
}

// run loads publisher policy and mounted credentials, then transfers store
// ownership to serve. It never accepts provider keys through an HTTP request.
func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("wavenotifyd", flag.ContinueOnError)
	listen := flags.String(
		"listen", "127.0.0.1:8080",
		"HTTP listen address behind TLS ingress",
	)
	database := flags.String(
		"database", "relay.db", "dedicated relay database file",
	)
	appsFile := flags.String(
		"apps-config", "", "JSON publisher app policy file",
	)
	keyFile := flags.String(
		"apns-key-file", "", "mounted Apple PKCS#8 provider key",
	)
	keyID := flags.String(
		"apns-key-id", "", "Apple provider key identifier",
	)
	teamID := flags.String(
		"apns-team-id", "", "Apple publisher team identifier",
	)
	behindTLS := flags.Bool(
		"behind-tls-proxy", false,
		"allow non-loopback HTTP behind trusted TLS ingress",
	)
	showVersion := flags.Bool(
		"version", false, "print build version and exit",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)

		return nil
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	address := net.ParseIP(host)
	if !*behindTLS && (address == nil || !address.IsLoopback()) {
		return errors.New("external HTTP requires --behind-tls-proxy")
	}
	if *appsFile == "" || *keyFile == "" {
		return errors.New("apps-config and apns-key-file are required")
	}
	policy, err := os.ReadFile(*appsFile)
	if err != nil {
		return fmt.Errorf("load publisher policy: %w", err)
	}
	var apps []notifyrelay.AppConfig
	decoder := json.NewDecoder(bytes.NewReader(policy))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&apps); err != nil {
		return errors.New("invalid publisher policy JSON")
	}
	keyBytes, err := os.ReadFile(*keyFile)
	if err != nil {
		return fmt.Errorf("load APNs key: %w", err)
	}
	key, err := notifyrelay.ParseAPNSKey(keyBytes)
	if err != nil {
		return err
	}
	provider, err := notifyrelay.NewAPNSProvider(notifyrelay.APNSConfig{
		TeamID: *teamID, KeyID: *keyID, PrivateKey: key,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*database), 0700); err != nil {
		return err
	}
	store, err := notifyrelay.OpenStore(*database)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	server, err := notifyrelay.NewServer(notifyrelay.Config{
		Applications: apps, Logger: logger,
	}, store, provider)
	if err != nil {
		return errors.Join(err, store.Close())
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return errors.Join(err, store.Close())
	}
	logger.InfoContext(
		ctx, "Notification relay started",
		slog.Int("apps", len(apps)), slog.String("version", version),
	)

	return serve(ctx, server, store, listener)
}

// serve joins HTTP handlers and the worker before closing storage. If shutdown
// cannot join within its budget, it leaves the store open and returns an error;
// main then exits, preserving Bolt's process-crash transaction boundary.
func serve(ctx context.Context, relay *notifyrelay.Server,
	store *notifyrelay.Store, listener net.Listener) error {

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpServer := &http.Server{
		Handler: relay.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: time.Minute, MaxHeaderBytes: 16 * 1024,
		BaseContext: func(net.Listener) context.Context {
			return runCtx
		},
	}
	workerDone := make(chan error, 1)
	httpDone := make(chan error, 1)
	go func() {
		workerDone <- relay.Run(runCtx)
	}()
	go func() {
		httpDone <- httpServer.Serve(listener)
	}()
	var workerErr, httpErr error
	workerJoined, httpJoined := false, false
	select {
	case <-ctx.Done():
	case workerErr = <-workerDone:
		workerJoined = true
		if workerErr == nil && ctx.Err() == nil {
			workerErr = errors.New("notification worker stopped " +
				"unexpectedly")
		}

	case httpErr = <-httpDone:
		httpJoined = true
	}
	cancel()
	shutdownCtx, stop := context.WithTimeout(
		context.WithoutCancel(ctx), 30*time.Second,
	)
	defer stop()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()

		return errors.Join(workerErr, httpErr, err)
	}
	if !workerJoined {
		select {
		case workerErr = <-workerDone:
		case <-shutdownCtx.Done():
			return shutdownCtx.Err()
		}
	}
	if !httpJoined {
		httpErr = <-httpDone
	}
	if errors.Is(httpErr, http.ErrServerClosed) {
		httpErr = nil
	}

	return errors.Join(workerErr, httpErr, store.Close())
}
