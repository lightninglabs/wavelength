package main

import (
	"fmt"
	"os"

	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func main() {
	root := newRootCmd()

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd creates the top-level cobra command that starts the daemon.
func newRootCmd() *cobra.Command {
	cfg := darepod.DefaultConfig()

	cmd := &cobra.Command{
		Use:   "darepod",
		Short: "Ark client daemon",
		Long: "darepod is the Ark protocol client daemon. It connects " +
			"to an lnd node and an ark operator server, exposing " +
			"a gRPC API for wallet operations.",
		Version: build.Version(),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cfg)
		},
	}

	// Bind flags to config fields via viper. Flags use dotted names
	// that map to nested config struct fields.
	f := cmd.Flags()

	f.StringVar(
		&cfg.DataDir, "datadir", cfg.DataDir,
		"root data directory for daemon state",
	)
	f.StringVar(
		&cfg.Network, "network", cfg.Network,
		"bitcoin network (mainnet, testnet, regtest, simnet, signet)",
	)
	f.StringVar(
		&cfg.DebugLevel, "debuglevel", cfg.DebugLevel,
		"logging verbosity (trace, debug, info, warn, error, critical)",
	)

	// LND connection flags.
	f.StringVar(
		&cfg.Lnd.Host, "lnd.host", cfg.Lnd.Host,
		"lnd gRPC address",
	)
	f.StringVar(
		&cfg.Lnd.TLSPath, "lnd.tlspath", cfg.Lnd.TLSPath,
		"path to lnd TLS certificate",
	)
	f.StringVar(
		&cfg.Lnd.MacaroonPath, "lnd.macaroonpath",
		cfg.Lnd.MacaroonPath,
		"path to lnd admin macaroon",
	)

	// Ark server connection flags.
	f.StringVar(
		&cfg.Server.Host, "server.host", cfg.Server.Host,
		"ark operator mailbox server address",
	)
	f.StringVar(
		&cfg.Server.TLSCertPath, "server.tlscertpath",
		cfg.Server.TLSCertPath,
		"path to ark server TLS certificate",
	)
	f.BoolVar(
		&cfg.Server.Insecure, "server.insecure",
		cfg.Server.Insecure,
		"disable TLS for the server connection (dev only)",
	)

	// Daemon RPC server flags.
	f.StringVar(
		&cfg.RPC.ListenAddr, "rpc.listenaddr", cfg.RPC.ListenAddr,
		"daemon gRPC listen address",
	)

	// Allow config file and environment variable overrides via viper.
	viper.SetEnvPrefix("DAREPOD")
	viper.AutomaticEnv()

	return cmd
}

// run loads the config, starts signal interception, and launches the daemon.
func run(cfg *darepod.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// Intercept OS signals for graceful shutdown.
	shutdownInterceptor, err := signal.Intercept()
	if err != nil {
		return fmt.Errorf("unable to intercept signals: %w", err)
	}

	return darepod.Main(cfg, shutdownInterceptor)
}
