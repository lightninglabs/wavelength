package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/btcsuite/btclog/v2"
	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver.
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo-client/chainbackends/bitcoindrpc"
	"github.com/lightninglabs/darepo/build"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// main is the process entry point for the arkd CLI binary.
func main() {
	root := newRootCmd()

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd creates the top-level cobra command that starts the
// daemon.
//
//nolint:funlen
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
			if err := v.Unmarshal(cfg); err != nil {
				return err
			}

			// Wire the v3 package submitter so the operator's
			// chain backend can broadcast OOR checkpoint and
			// timeout-sweep packages (parent has zero fee, child
			// pays via ephemeral anchor CPFP). The runtime
			// validation in run() requires this submitter for
			// arkd startup.
			//
			// Credentials come from one of two sources:
			//   - bitcoind.cookiepath: preferred for local
			//     deployments — the cookie file is bitcoind's
			//     default auth mechanism and avoids storing
			//     plaintext creds in arkd's config; or
			//   - bitcoind.user + bitcoind.pass: explicit creds
			//     for setups where rpcuser/rpcpassword are used.
			//
			// Reject empty/unreadable creds at startup. Wiring
			// with empty User/Pass would silently produce
			// "Authorization: Basic Og==" requests that bitcoind
			// returns 401 for at the first fraud broadcast — too
			// late, and easy to miss in logs. Failing here forces
			// the operator to surface the misconfiguration before
			// the daemon starts serving.
			if cfg.Bitcoind != nil && cfg.Bitcoind.Host != "" {
				user, pass, err := bitcoindCreds(cfg.Bitcoind)
				if err != nil {
					return err
				}
				cfg.PackageSubmitter = bitcoindrpc.New(
					cfg.Bitcoind.Host, user, pass,
				)
			}

			return nil
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
		"datadir", cfg.DataDir, "root data directory for daemon state",
	)
	f.String(
		"network", cfg.Network,
		"bitcoin network (mainnet, testnet, regtest, simnet, signet)",
	)
	f.String(
		"debuglevel", cfg.DebugLevel,
		"logging verbosity (trace, debug, info, warn, error, critical)",
	)
	f.String(
		"logfile", cfg.LogFilePath,
		"path to write the log file",
	)

	// LND connection flags.
	f.String("lnd.host", cfg.Lnd.Host, "lnd gRPC address")
	f.String(
		"lnd.tlspath", cfg.Lnd.TLSPath, "path to lnd TLS certificate",
	)
	f.String(
		"lnd.macaroonpath", cfg.Lnd.MacaroonPath,
		"path to lnd admin macaroon",
	)

	// Database flags.
	f.String(
		"db.backend", cfg.DB.Backend,
		"database backend (sqlite or postgres)",
	)
	f.String(
		"db.sqlite.dbfile", cfg.DB.Sqlite.DatabaseFileName,
		"full path to the SQLite database file",
	)
	f.Bool(
		"db.sqlite.skipmigrations", cfg.DB.Sqlite.SkipMigrations,
		"skip applying migrations on startup (sqlite)",
	)
	f.Bool(
		"db.sqlite.skipmigrationdbbackup",
		cfg.DB.Sqlite.SkipMigrationDBBackup,
		"skip creating a backup before migrations (sqlite)",
	)
	f.String(
		"db.postgres.host", cfg.DB.Postgres.Host,
		"postgres server hostname",
	)
	f.Int(
		"db.postgres.port", cfg.DB.Postgres.Port,
		"postgres server port",
	)
	f.String("db.postgres.user", cfg.DB.Postgres.User,
		"postgres user",
	)
	f.String(
		"db.postgres.password", cfg.DB.Postgres.Password,
		"postgres password",
	)
	f.String(
		"db.postgres.dbname", cfg.DB.Postgres.DBName,
		"postgres database name",
	)
	f.Bool(
		"db.postgres.skipmigrations", cfg.DB.Postgres.SkipMigrations,
		"skip applying migrations on startup (postgres)",
	)
	f.Bool(
		"db.postgres.requiressl", cfg.DB.Postgres.RequireSSL,
		"require SSL when connecting to postgres",
	)

	// Rounds policy flags.
	rc := cfg.Rounds
	f.Uint32(
		"rounds.sweepdelay", rc.SweepDelay,
		"CSV delay for sweep path (blocks)",
	)
	f.Uint32(
		"rounds.maxvtxospertree", rc.MaxVTXOsPerTree,
		"max VTXOs per batch tree",
	)
	f.Uint32(
		"rounds.treeradix", rc.TreeRadix, "VTXO tree branching factor",
	)
	f.Uint32(
		"rounds.maxconnectorspertree", rc.MaxConnectorsPerTree,
		"max connector leaves per tree",
	)
	f.Uint32(
		"rounds.connectortreeradix", rc.ConnectorTreeRadix,
		"connector tree branching factor",
	)
	f.Int64(
		"rounds.connectordustamount", rc.ConnectorDustAmount,
		"connector dust amount per leaf (satoshis)",
	)
	f.Uint32(
		"rounds.boardingexitdelay", rc.BoardingExitDelay,
		"min exit delay for boarding inputs (blocks)",
	)
	f.Uint32(
		"rounds.minboardingconfirmations", rc.MinBoardingConfirmations,
		"min confirmations for boarding inputs",
	)
	f.Uint32(
		"rounds.vtxoexitdelay", rc.VTXOExitDelay,
		"min exit delay for VTXOs (blocks)",
	)
	f.Duration(
		"rounds.registrationtimeout", rc.RegistrationTimeout,
		"registration phase timeout",
	)
	f.Duration(
		"rounds.signaturecollectiontimeout",
		rc.SignatureCollectionTimeout,
		"signature collection phase timeout",
	)
	f.Duration(
		"rounds.tickinterval", rc.RoundTickInterval, "interval at "+
			"which the round actor checks if the current round "+
			"should be sealed; 0 disables periodic ticks",
	)
	f.Duration(
		"rounds.fundpsbtlockduration", rc.FundPsbtLockDuration,
		"LND UTXO lease duration for FundPsbt",
	)
	f.Uint32(
		"rounds.conftarget", rc.ConfTarget,
		"confirmation target for fee estimation",
	)
	f.Int32(
		"rounds.minconfs", rc.MinConfs,
		"min confirmations for wallet UTXOs",
	)
	f.Uint32(
		"rounds.confirmationtarget", rc.ConfirmationTarget,
		"confirmations before round is confirmed",
	)

	// Required bitcoind direct chain source flags.
	f.String(
		"bitcoind.host", "", "bitcoind RPC address (host:port); "+
			"required for direct UTXO validation and package relay",
	)
	f.String(
		"bitcoind.user", "",
		"bitcoind RPC username (ignored if bitcoind.cookiepath is set)",
	)
	f.String(
		"bitcoind.pass", "",
		"bitcoind RPC password (ignored if bitcoind.cookiepath is set)",
	)
	f.String(
		"bitcoind.cookiepath", "", "path to bitcoind's cookie auth "+
			"file (e.g. ~/.bitcoin/.cookie); preferred over "+
			"user/pass",
	)

	// Admin RPC server flags.
	f.String(
		"adminrpc.listen", cfg.AdminRPC.ListenAddr,
		"admin gRPC listen address",
	)

	// Client RPC server flags.
	f.String(
		"rpc.listen", cfg.RPC.ListenAddr, "client gRPC listen address",
	)
	f.Bool(
		"rpc.gateway.enabled", cfg.RPC.Gateway.Enabled,
		"enable client RPC HTTP/JSON gateway",
	)
	f.String(
		"rpc.gateway.listen", cfg.RPC.Gateway.ListenAddr,
		"client RPC HTTP/JSON gateway listen address",
	)
	f.StringSlice(
		"rpc.gateway.allowedorigins", cfg.RPC.Gateway.AllowedOrigins,
		"trusted browser origins for client RPC HTTP/JSON gateway",
	)
	f.String(
		"rpc.tls.certpath", "",
		"path to TLS certificate for client gRPC",
	)
	f.String(
		"rpc.tls.keypath", "",
		"path to TLS private key for client gRPC",
	)
	f.Bool(
		"rpc.tls.autocert", false,
		"enable automatic TLS certificate generation",
	)
	f.Bool(
		"rpc.notls", false,
		"explicitly disable TLS for client gRPC (regtest/dev only)",
	)

	// Mailbox transport flags.
	mc := cfg.Mailbox
	f.Int(
		"mailbox.maxenvelopebytes", mc.MaxEnvelopeBytes, "maximum "+
			"protobuf-encoded mailbox envelope size in bytes "+
			"(0 disables)",
	)
	f.Int(
		"mailbox.maxenvelopespermailbox", mc.MaxEnvelopesPerMailbox,
		"maximum outstanding envelopes retained per mailbox (0 "+
			"disables)",
	)

	// Metrics server flags.
	f.String(
		"metrics.listen", cfg.Metrics.ListenAddr,
		"Prometheus metrics HTTP listen address (empty to disable)",
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
	if err := cfg.ValidatePackageRelay(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// Set up logging. A single handler is shared across all
	// subsystem loggers so that output flows to one destination
	// with consistent formatting. When a log file path is
	// configured, logs are written to both stdout and the file.
	var logWriter io.Writer = os.Stdout

	if cfg.LogFilePath != "" {
		logDir := filepath.Dir(cfg.LogFilePath)
		if err := os.MkdirAll(logDir, 0700); err != nil {
			return fmt.Errorf("create log dir: %w", err)
		}

		logFile, err := os.OpenFile(
			cfg.LogFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND,
			0600,
		)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		defer logFile.Close()

		logWriter = io.MultiWriter(os.Stdout, logFile)
	}

	logHandler := btclog.NewDefaultHandler(logWriter)
	loggers := darepo.SetupLoggers(logHandler)

	if err := darepo.ApplyDebugLevel(
		loggers, cfg.DebugLevel,
	); err != nil {
		return fmt.Errorf("error setting log level: %w", err)
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
		return fmt.Errorf("unable to intercept signals: %w", err)
	}

	return darepo.Main(cfg, shutdownInterceptor)
}

// bitcoindCreds resolves the RPC username and password for the operator's
// bitcoind connection. When CookiePath is set, the cookie file is read and
// parsed (Bitcoin Core writes a single line of the form `user:password`),
// which takes precedence over explicit User/Pass. Otherwise the explicit
// fields are required.
func bitcoindCreds(cfg *darepo.BitcoindConfig) (string, string, error) {
	if cfg.CookiePath != "" {
		raw, err := os.ReadFile(cfg.CookiePath)
		if err != nil {
			return "", "", fmt.Errorf("read bitcoind cookie %q: %w",
				cfg.CookiePath, err)
		}

		// The cookie file is a single line of the shape
		// "user:password"; trim trailing whitespace just in case.
		line := strings.TrimSpace(string(raw))
		user, pass, ok := strings.Cut(line, ":")
		if !ok || user == "" || pass == "" {
			return "", "", fmt.Errorf("bitcoind cookie %q is "+
				"malformed: expected user:password",
				cfg.CookiePath)
		}

		return user, pass, nil
	}

	if cfg.User == "" || cfg.Pass == "" {
		return "", "", fmt.Errorf("bitcoind.host set but " +
			"bitcoind.user / bitcoind.pass are empty (and " +
			"bitcoind.cookiepath is unset): refusing to start " +
			"with unauthenticated RPC creds")
	}

	return cfg.User, cfg.Pass, nil
}
