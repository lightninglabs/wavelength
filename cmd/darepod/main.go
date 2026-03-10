package main

import (
	"fmt"
	"os"
	"strings"

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
	v := viper.New()

	cmd := &cobra.Command{
		Use:   "darepod",
		Short: "Ark client daemon",
		Long: "darepod is the Ark protocol client daemon. " +
			"It connects to an lnd node and an ark " +
			"operator server, exposing a gRPC API " +
			"for wallet operations.",
		Version: build.Version(),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Merge flags, environment variables, and config
			// file into the config struct. Viper handles the
			// precedence: flags > env > config file > defaults.
			return v.Unmarshal(cfg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cfg)
		},
	}

	// Register flags with defaults from the config struct. Viper
	// binds these automatically so that flag values, environment
	// variables, and config file entries all feed into Unmarshal.
	f := cmd.Flags()

	f.String(
		"datadir", cfg.DataDir,
		"root data directory for daemon state",
	)
	f.String(
		"network", cfg.Network,
		"bitcoin network (mainnet, testnet, regtest, simnet, signet)",
	)
	f.String(
		"debuglevel", cfg.DebugLevel,
		"logging verbosity (trace, debug, info, warn, error, "+
			"critical)",
	)

	// LND connection flags.
	f.String("lnd.host", cfg.Lnd.Host, "lnd gRPC address")
	f.String("lnd.tlspath", cfg.Lnd.TLSPath,
		"path to lnd TLS certificate",
	)
	f.String("lnd.macaroonpath", cfg.Lnd.MacaroonPath,
		"path to lnd admin macaroon",
	)

	// Ark server connection flags.
	f.String("server.host", cfg.Server.Host,
		"ark operator mailbox server address",
	)
	f.String("server.tlscertpath", cfg.Server.TLSCertPath,
		"path to ark server TLS certificate",
	)
	f.Bool("server.insecure", cfg.Server.Insecure,
		"disable TLS for the server connection (dev only)",
	)
	f.String("server.localmailboxid", cfg.Server.LocalMailboxID,
		"this client's mailbox identifier",
	)
	f.String("server.remotemailboxid", cfg.Server.RemoteMailboxID,
		"remote server's mailbox identifier",
	)

	// Wallet backend flags.
	f.String("wallet.type", cfg.Wallet.Type,
		"wallet backend type (lnd, lwwallet)",
	)
	f.String("wallet.esploraurl", cfg.Wallet.EsploraURL,
		"esplora REST API URL (required for lwwallet)",
	)
	f.Duration("wallet.pollinterval", cfg.Wallet.PollInterval,
		"chain poll interval for lwwallet backend",
	)
	f.Uint32("wallet.recoverywindow", cfg.Wallet.RecoveryWindow,
		"address recovery look-ahead window for lwwallet",
	)
	f.String("wallet.password_file", cfg.Wallet.PasswordFile,
		"path to file containing wallet password for "+
			"auto-unlock at startup (lwwallet mode)",
	)

	// Daemon RPC server flags.
	f.String("rpc.listenaddr", cfg.RPC.ListenAddr,
		"daemon gRPC listen address",
	)

	// Safety flag for mainnet operation.
	f.Bool("allow-mainnet", cfg.AllowMainnet,
		"allow the daemon to run on mainnet "+
			"(required when network=mainnet)",
	)

	// Bind all flags to viper so Unmarshal populates the config
	// struct from the combined flag/env/file sources.
	v.SetEnvPrefix("DAREPOD")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	_ = v.BindPFlags(f)

	return cmd
}

// run validates the config, starts signal interception, and launches the
// daemon.
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
