package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/btcsuite/btclog/v2"
	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver.
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo/build"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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

// newRootCmd creates the top-level cobra command that starts the
// daemon.
func newRootCmd() *cobra.Command {
	cfg := darepo.DefaultConfig()
	v := viper.New()

	cmd := &cobra.Command{
		Use:   "arkd",
		Short: "Ark operator daemon",
		Long: "arkd is the Ark protocol operator daemon. " +
			"It manages rounds, VTXOs, and serves " +
			"client connections via gRPC and mailbox " +
			"transport.",
		Version: build.Version(),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Merge flags, environment variables, and
			// config file into the config struct. Viper
			// handles the precedence: flags > env >
			// config file > defaults.
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
		"bitcoin network (mainnet, testnet, regtest, "+
			"simnet, signet)",
	)
	f.String(
		"debuglevel", cfg.DebugLevel,
		"logging verbosity (trace, debug, info, warn, "+
			"error, critical)",
	)
	f.String(
		"logfile", cfg.LogFilePath,
		"path to write the log file",
	)

	// LND connection flags.
	f.String("lnd.host", cfg.Lnd.Host, "lnd gRPC address")
	f.String("lnd.tlspath", cfg.Lnd.TLSPath,
		"path to lnd TLS certificate",
	)
	f.String("lnd.macaroonpath", cfg.Lnd.MacaroonPath,
		"path to lnd admin macaroon",
	)

	// Database flags.
	f.String("db.backend", cfg.DB.Backend,
		"database backend (sqlite or postgres)",
	)
	f.String("db.sqlite.dbfile",
		cfg.DB.Sqlite.DatabaseFileName,
		"full path to the SQLite database file",
	)
	f.Bool("db.sqlite.skipmigrations",
		cfg.DB.Sqlite.SkipMigrations,
		"skip applying migrations on startup (sqlite)",
	)
	f.Bool("db.sqlite.skipmigrationdbbackup",
		cfg.DB.Sqlite.SkipMigrationDBBackup,
		"skip creating a backup before migrations (sqlite)",
	)
	f.String("db.postgres.host", cfg.DB.Postgres.Host,
		"postgres server hostname",
	)
	f.Int("db.postgres.port", cfg.DB.Postgres.Port,
		"postgres server port",
	)
	f.String("db.postgres.user", cfg.DB.Postgres.User,
		"postgres user",
	)
	f.String("db.postgres.password", cfg.DB.Postgres.Password,
		"postgres password",
	)
	f.String("db.postgres.dbname", cfg.DB.Postgres.DBName,
		"postgres database name",
	)
	f.Bool("db.postgres.skipmigrations",
		cfg.DB.Postgres.SkipMigrations,
		"skip applying migrations on startup (postgres)",
	)
	f.Bool("db.postgres.requiressl",
		cfg.DB.Postgres.RequireSSL,
		"require SSL when connecting to postgres",
	)

	// Admin RPC server flags.
	f.String("adminrpc.listen", cfg.AdminRPC.ListenAddr,
		"admin gRPC listen address",
	)

	// Client RPC server flags.
	f.String("rpc.listen", cfg.RPC.ListenAddr,
		"client gRPC listen address",
	)
	f.String("rpc.tls.certpath", "",
		"path to TLS certificate for client gRPC",
	)
	f.String("rpc.tls.keypath", "",
		"path to TLS private key for client gRPC",
	)
	f.Bool("rpc.tls.autocert", false,
		"enable automatic TLS certificate generation",
	)

	// Bind all flags to viper so Unmarshal populates the config
	// struct from the combined flag/env/file sources.
	v.SetEnvPrefix("ARKD")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := v.BindPFlags(f); err != nil {
		panic(fmt.Sprintf("failed to bind flags: %v", err))
	}

	return cmd
}

// run validates the config, sets up logging, starts signal
// interception, and launches the daemon.
func run(cfg *darepo.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// Set up logging. A single handler is shared across all
	// subsystem loggers so that output flows to one destination
	// with consistent formatting.
	logHandler := btclog.NewDefaultHandler(os.Stdout)
	loggers := darepo.SetupLoggers(logHandler)

	if err := darepo.ApplyDebugLevel(
		loggers, cfg.DebugLevel,
	); err != nil {
		return fmt.Errorf("error setting log level: %w",
			err)
	}

	// Inject the server's own logger into the config. Subsystem
	// loggers for child components are extracted from the loggers
	// map during NewServer.
	serverLog := loggers[darepo.Subsystem]
	cfg.Log = fn.Some(serverLog)
	cfg.Loggers = loggers

	// Intercept OS signals for graceful shutdown.
	shutdownInterceptor, err := signal.Intercept()
	if err != nil {
		return fmt.Errorf("unable to intercept "+
			"signals: %w", err)
	}

	return darepo.Main(cfg, shutdownInterceptor)
}
