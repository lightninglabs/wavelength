package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/chainbackends/bitcoindrpc"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

const daemonLogFileName = "waved.log"
const daemonConfigFileName = "waved.conf"

type bestEffortWriter struct {
	w io.Writer
}

func (b bestEffortWriter) Write(p []byte) (int, error) {
	if b.w != nil {
		_, _ = b.w.Write(p)
	}

	return len(p), nil
}

func main() {
	root := newRootCmd()

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd creates the top-level cobra command that starts the daemon.
func newRootCmd() *cobra.Command {
	cfg := waved.DefaultConfig()
	v := viper.New()

	cmd := &cobra.Command{
		Use:   "waved",
		Short: "Ark client daemon",
		Long: "waved is the Ark protocol client daemon. " +
			"It connects to an lnd node and an ark " +
			"operator server, exposing a gRPC API " +
			"for wallet operations.",
		Version: build.Version(),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if err := readConfigFile(v, cmd); err != nil {
				return err
			}

			// Merge flags, environment variables, and config
			// file into the config struct. Viper handles the
			// precedence: flags > env > config file > defaults.
			if err := v.Unmarshal(cfg); err != nil {
				return err
			}

			// Wire bitcoind package submitter for V3
			// ephemeral anchor package relay (unroll).
			err := configureBitcoindSubmitter(v, cfg)
			if err != nil {
				return err
			}

			configureSwapRuntime(cfg)
			configureWalletRPC(cfg)

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
		"network", cfg.Network, "bitcoin network (mainnet, "+
			"testnet, testnet4, regtest, simnet, signet)",
	)
	f.String(
		"debuglevel", cfg.DebugLevel,
		"logging verbosity (trace, debug, info, warn, error, critical)",
	)
	f.String(
		"logdir", cfg.LogDirPath,
		"directory for persistent daemon logs",
	)
	f.String(
		"configfile", filepath.Join(cfg.DataDir, daemonConfigFileName),
		"path to daemon config file",
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

	registerArkServerFlags(f, cfg)

	// Wallet backend flags.
	f.String(
		"wallet.type", cfg.Wallet.Type,
		"wallet backend type (lnd, lwwallet, btcwallet)",
	)
	f.String(
		"wallet.esploraurl", cfg.Wallet.EsploraURL,
		"esplora REST API URL (required for lwwallet)",
	)
	f.String(
		"wallet.feeurl", cfg.Wallet.FeeURL,
		"fee-estimate JSON endpoint URL (required for btcwallet)",
	)
	f.String(
		"wallet.btcwallet_blockheaderssource",
		cfg.Wallet.BtcwBlockSource,
		"block header import source for btcwallet fast sync",
	)
	f.String(
		"wallet.btcwallet_filterheaderssource",
		cfg.Wallet.BtcwFilterSource,
		"filter header import source for btcwallet fast sync",
	)
	f.Duration(
		"wallet.pollinterval", cfg.Wallet.PollInterval,
		"chain poll interval for lwwallet backend",
	)
	f.Uint32(
		"wallet.recoverywindow", cfg.Wallet.RecoveryWindow,
		"address recovery look-ahead window for lwwallet",
	)
	f.String(
		"wallet.password_file", cfg.Wallet.PasswordFile, "path to "+
			"file containing wallet password for auto-unlock "+
			"at startup (lwwallet/btcwallet)",
	)

	registerBitcoindFlags(f)

	registerDaemonRPCFlags(f, cfg)

	registerSwapRuntimeFlags(f, cfg)

	registerPprofFlags(f, cfg)

	registerMetricsFlags(f, cfg)

	// Safety flag for mainnet operation.
	f.Bool(
		"allow-mainnet", cfg.AllowMainnet, "allow the daemon to "+
			"run on mainnet (required when network=mainnet)",
	)

	registerProtocolSafetyFlags(f, cfg)

	// Bound concurrent MuSig2 work. Zero lets the daemon choose a safe
	// backend-aware default; one restores serial signing.
	f.Int(
		"signingworkers", cfg.SigningWorkers, "maximum VTXO "+
			"signing sessions processed in parallel; zero "+
			"selects a wallet-backend default",
	)

	// EagerRoundJoin makes the wallet actor drive round-joining
	// without a follow-up Board / LeaveVTXOs RPC. The default is
	// inherited from waved.DefaultConfig, which is build-tag
	// aware: false on the standalone non-wavewalletrpc build (operator-
	// driven hosts) and true under the wavewalletrpc build tag (wallet-
	// shaped hosts). Viper precedence (flag > env > config > default)
	// applies, so --eagerroundjoin=false still disables it under the
	// wavewalletrpc build.
	f.Bool(
		"eagerroundjoin", cfg.EagerRoundJoin, "drive round-joining "+
			"from the wallet without waiting for an explicit "+
			"Board / LeaveVTXOs RPC; defaults to true when "+
			"compiled with the wavewalletrpc build tag, false "+
			"otherwise",
	)

	// SQLite database durability knobs (db.sqlite.* namespace). The
	// db.postgres.* namespace is reserved for a future Postgres-tuning
	// change; the daemon always opens SQLite.
	f.String(
		"db.sqlite.synchronous", cfg.DB.Sqlite.Synchronous, "the "+
			"SQLite synchronous (commit durability) level: one "+
			"of full, normal, off; empty defaults to normal, "+
			"which under WAL mode omits the per-commit fsync "+
			"of full for higher write throughput",
	)
	f.Bool(
		"db.sqlite.nofullfsync", cfg.DB.Sqlite.NoFullfsync, "disable"+
			" the SQLite fullfsync pragma (macOS only); trades "+
			"power-loss flush guarantees for higher sustained "+
			"write throughput",
	)

	// OOR safety limits. These are advanced knobs; most operators
	// should keep the defaults unless a limit-exceeded error says
	// otherwise after a protocol upgrade or operator/indexer change.
	f.Uint32(
		"oor.limits.max-checkpoints", cfg.OOR.Limits.MaxCheckpoints,
		"maximum checkpoint transactions allowed in one incoming "+
			"OOR transfer; raise only if logs show \"max "+
			"checkpoints exceeded\" after an Ark-protocol upgrade",
	)
	f.Uint32(
		"oor.limits.max-vtxo-matches", cfg.OOR.Limits.MaxVTXOMatches,
		"maximum VTXOs returned by one indexer lookup during "+
			"incoming OOR receive; raise only if logs show "+
			"\"max metadata matches exceeded\"; higher values "+
			"use more memory per query",
	)
	f.Uint32(
		"oor.limits.max-mailbox-items", cfg.OOR.Limits.MaxMailboxItems,
		"safety cap on items decoded from one stored mailbox "+
			"message; protects against malformed or oversized "+
			"payloads",
	)
	f.Uint32(
		"oor.limits.max-mailbox-script-bytes",
		cfg.OOR.Limits.MaxMailboxScriptBytes, "safety cap on the "+
			"size of an address script stored in the mailbox, "+
			"in bytes; standard Bitcoin scripts are well under "+
			"100; must be at least 34",
	)

	registerFeeEstimationFlags(f, cfg)

	// Bind all flags to viper so Unmarshal populates the config
	// struct from the combined flag/env/file sources.
	v.SetEnvPrefix("WAVED")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()
	bindOORLimitFlags(v, f)
	_ = v.BindPFlags(f)

	return cmd
}

// registerFeeEstimationFlags registers the optional external fee-provider
// flags for the lnd wallet backend. Disabled by default; when enabled, the lnd
// fee estimator selects the lower of the local WalletKit estimate and the
// mempool.space estimate.
func registerFeeEstimationFlags(f *pflag.FlagSet, cfg *waved.Config) {
	f.Bool(
		"feeestimation.mempoolspace.enabled",
		cfg.MempoolSpaceFeeEnabled(),
		"enable the mempool.space fee provider for the lnd wallet "+
			"backend; the estimator then selects the lower of "+
			"the local lnd and mempool.space fee rates",
	)
	f.String(
		"feeestimation.mempoolspace.url", cfg.MempoolSpaceFeeURL(),
		"override the network-default mempool.space "+
			"recommended-fee endpoint; must be an absolute "+
			"https URL",
	)
}

// registerDaemonRPCFlags registers daemon-owned local RPC flags.
func registerDaemonRPCFlags(f *pflag.FlagSet, cfg *waved.Config) {
	f.String(
		"rpc.listenaddr", cfg.RPC.ListenAddr,
		"daemon gRPC listen address",
	)
	f.String(
		"rpc.tlscertpath", cfg.RPC.TLSCertPath,
		"path to daemon RPC TLS certificate",
	)
	f.String(
		"rpc.tlskeypath", cfg.RPC.TLSKeyPath,
		"path to daemon RPC TLS private key",
	)
	f.Bool(
		"rpc.notls", cfg.RPC.NoTLS,
		"disable TLS for daemon RPC (dev only)",
	)
	f.String(
		"rpc.macaroonpath", cfg.RPC.MacaroonPath,
		"path to daemon RPC macaroon",
	)
	f.Bool(
		"rpc.no-macaroons", cfg.RPC.NoMacaroons,
		"disable daemon RPC macaroon auth (dev only)",
	)
	f.Bool(
		"rpc.gateway.enabled", cfg.RPC.Gateway.Enabled,
		"enable daemon HTTP/JSON gateway",
	)
	f.String(
		"rpc.gateway.listenaddr", cfg.RPC.Gateway.ListenAddr,
		"daemon HTTP/JSON gateway listen address",
	)
	f.StringSlice(
		"rpc.gateway.allowedorigins", cfg.RPC.Gateway.AllowedOrigins,
		"trusted browser origins allowed to call the daemon gateway",
	)
}

// registerArkServerFlags registers the daemon's outbound Ark operator flags.
func registerArkServerFlags(f *pflag.FlagSet, cfg *waved.Config) {
	f.String(
		"server.host", cfg.Server.Host,
		"ark operator address for selected server transport",
	)
	f.String(
		"server.transport", cfg.Server.Transport,
		"ark operator RPC transport (grpc or rest)",
	)
	f.String(
		"server.tlscertpath", cfg.Server.TLSCertPath,
		"path to ark server TLS certificate",
	)
	f.Bool(
		"server.insecure", cfg.Server.Insecure,
		"disable TLS for the server connection (dev only)",
	)
	f.String(
		"server.macaroonpath", cfg.Server.MacaroonPath,
		"path to ark operator RPC macaroon",
	)
}

// registerProtocolSafetyFlags registers the client-side protocol safety knobs:
// the per-round operator fee cap and the reorg-safety / finality depth.
func registerProtocolSafetyFlags(f *pflag.FlagSet, cfg *waved.Config) {
	// Cap the per-round operator fee the client is willing to pay under the
	// #270 seal-time fee handshake. Zero is rejected at config-load time as
	// an explicit misconfiguration.
	f.Int64(
		"maxoperatorfeesat", cfg.MaxOperatorFeeSat, "maximum "+
			"operator fee (sats) the client will accept per "+
			"seal-time quote; must be positive",
	)

	// Reorg-safety / finality depth: the confirmation depth at which a
	// batch is treated as final and its reorg-aware chain watches are
	// released. Bounds the deepest reorg the daemon detects and recovers
	// from. Zero selects a network-aware default (6, or 100 on testnet,
	// whose minimum-difficulty rule produces deep reorgs).
	f.Uint32(
		"reorgsafetydepth", cfg.ReorgSafetyDepth, "confirmation "+
			"depth at which a batch is treated as final and "+
			"its reorg-aware chain watches are released; "+
			"bounds the deepest reorg the daemon recovers "+
			"from. 0 = network-aware default (6; 100 on testnet)",
	)
}

// registerSwapRuntimeFlags registers optional swapruntime flags.
func registerSwapRuntimeFlags(f *pflag.FlagSet, cfg *waved.Config) {
	f.String(
		"swap.serveraddress", cfg.Swap.ServerAddress,
		"swap server address for selected swapruntime transport",
	)
	f.String(
		"swap.servertransport", cfg.Swap.ServerTransport, "swap "+
			"server RPC transport for swapruntime builds (grpc "+
			"or rest)",
	)
	f.String(
		"swap.servertlscertpath", cfg.Swap.ServerTLSCertPath,
		"swap server TLS certificate path for swapruntime builds",
	)
	f.Bool(
		"swap.serverinsecure", cfg.Swap.ServerInsecure,
		"disable TLS for swap server in swapruntime builds",
	)
	f.String(
		"swap.databasefilename", cfg.Swap.DatabaseFileName,
		"swap session SQLite database path for swapruntime builds",
	)
	f.Bool(
		"swap.vhtlcrecovery.autoescalate",
		cfg.Swap.VHTLCRecovery.AutoEscalate, "automatically "+
			"escalate armed vHTLC recovery after grace or "+
			"deadline policy in swapruntime builds",
	)
	f.Duration(
		"swap.vhtlcrecovery.cooperativefailuregraceperiod",
		cfg.Swap.VHTLCRecovery.CooperativeFailureGracePeriod, "delay"+
			" after first cooperative vHTLC send failure "+
			"before automatic recovery in swapruntime builds",
	)
	f.Uint32(
		"swap.vhtlcrecovery.minrecoverymarginblocks",
		cfg.Swap.VHTLCRecovery.MinRecoveryMarginBlocks, "minimum "+
			"block margin preserved before claim recovery "+
			"deadlines in swapruntime builds",
	)
	f.Int32(
		"swap.vhtlcrecovery.maxfeeratesatperkw",
		cfg.Swap.VHTLCRecovery.MaxFeeRateSatPerKW, "maximum fee "+
			"rate in sat/kw for swapruntime vHTLC recovery "+
			"exit spends",
	)
}

// registerPprofFlags registers the optional pprof debug-server flags. pprof is
// disabled by default and only starts when pprof.listen is set to a non-empty
// address. The flag names match the mapstructure config keys, so viper binds
// them without explicit aliases.
func registerPprofFlags(f *pflag.FlagSet, cfg *waved.Config) {
	f.String(
		"pprof.listen", cfg.Pprof.ListenAddr, "address for the "+
			"pprof debug HTTP server (e.g. 127.0.0.1:6060); "+
			"empty disables pprof. Exposes sensitive runtime "+
			"data, so bind to a loopback or firewalled address "+
			"only",
	)
	f.Int(
		"pprof.blockprofilerate", cfg.Pprof.BlockProfileRate, "value"+
			" passed to runtime.SetBlockProfileRate at startup "+
			"when greater than zero; zero leaves block "+
			"profiling off",
	)
	f.Int(
		"pprof.mutexprofilefraction", cfg.Pprof.MutexProfileFraction,
		"value passed to runtime.SetMutexProfileFraction at "+
			"startup when greater than zero; zero leaves mutex "+
			"profiling off",
	)
}

// registerMetricsFlags registers the optional Prometheus metrics-server flag.
// Metrics are disabled by default and only start when metrics.listen is set to
// a non-empty address. The flag name matches the mapstructure config key, so
// viper binds it without an explicit alias.
func registerMetricsFlags(f *pflag.FlagSet, cfg *waved.Config) {
	f.String(
		"metrics.listen", cfg.Metrics.ListenAddr, "address for the "+
			"Prometheus /metrics HTTP server (e.g. "+
			"127.0.0.1:9092); empty disables metrics. Exposes "+
			"operational and balance data, so bind to a "+
			"loopback or firewalled address only",
	)
}

// bindOORLimitFlags binds hyphenated OOR CLI flags to the config keys used by
// mapstructure.
func bindOORLimitFlags(v *viper.Viper, f *pflag.FlagSet) {
	// The CLI uses hyphenated operator-facing flag names, while
	// mapstructure tags use the project's concatenated lowercase style.
	// Bind explicit viper keys before BindPFlags adds the literal flag-name
	// aliases.
	_ = v.BindPFlag(
		"oor.limits.maxcheckpoints",
		f.Lookup("oor.limits.max-checkpoints"),
	)
	_ = v.BindPFlag(
		"oor.limits.maxvtxomatches",
		f.Lookup("oor.limits.max-vtxo-matches"),
	)
	_ = v.BindPFlag(
		"oor.limits.maxmailboxitems",
		f.Lookup("oor.limits.max-mailbox-items"),
	)
	_ = v.BindPFlag(
		"oor.limits.maxmailboxscriptbytes",
		f.Lookup("oor.limits.max-mailbox-script-bytes"),
	)
}

// readConfigFile loads the daemon configuration from the selected properties
// file. Missing default config files are ignored so first startup works without
// bootstrapping an empty file; explicit config paths must exist.
func readConfigFile(v *viper.Viper, cmd *cobra.Command) error {
	configFile := v.GetString("configfile")
	if configFile == "" {
		return nil
	}

	configFile, err := expandCLIPath(configFile)
	if err != nil {
		return err
	}
	v.Set("configfile", configFile)

	_, err = os.Stat(configFile)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		if cmd.Flags().Changed("configfile") ||
			os.Getenv("WAVED_CONFIGFILE") != "" {
			return fmt.Errorf("config file %q does not exist",
				configFile)
		}

		return nil

	default:
		return fmt.Errorf("stat config file %q: %w", configFile, err)
	}

	config, err := readPropertiesConfig(configFile)
	if err != nil {
		return fmt.Errorf("read config file %q: %w", configFile, err)
	}

	if err := v.MergeConfigMap(config); err != nil {
		return fmt.Errorf("merge config file %q: %w", configFile, err)
	}

	return nil
}

// readPropertiesConfig parses a simple key=value config file into a nested map
// suitable for Viper.
func readPropertiesConfig(path string) (map[string]any, error) {
	content, err := os.ReadFile(path) //nolint:gosec // G304: operator path.
	if err != nil {
		return nil, err
	}

	config := make(map[string]any)
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key=value",
				i+1)
		}

		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", i+1)
		}

		value = trimInlineComment(value)
		insertConfigValue(config, strings.Split(key, "."), value)
	}

	return config, nil
}

// trimInlineComment removes whitespace-prefixed trailing comments from a config
// value.
func trimInlineComment(value string) string {
	for i := 0; i < len(value); i++ {
		if value[i] != '#' {
			continue
		}

		if i == 0 || value[i-1] == ' ' || value[i-1] == '\t' {
			return strings.TrimSpace(value[:i])
		}
	}

	return strings.TrimSpace(value)
}

// insertConfigValue stores a parsed config value in a nested map under a dotted
// key path.
func insertConfigValue(config map[string]any, path []string, value string) {
	key := path[0]
	if len(path) == 1 {
		config[key] = value

		return
	}

	child, ok := config[key].(map[string]any)
	if !ok {
		child = make(map[string]any)
		config[key] = child
	}

	insertConfigValue(child, path[1:], value)
}

// expandCLIPath expands a leading tilde in one CLI path.
func expandCLIPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}

		if path == "~" {
			return home, nil
		}

		return filepath.Join(home, path[2:]), nil
	}

	return filepath.Clean(path), nil
}

// run validates the config, starts signal interception, and launches the
// daemon.
func run(cfg *waved.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	logFile, err := configureDaemonLogWriter(cfg, os.Stdout)
	if err != nil {
		return fmt.Errorf("configure daemon log file: %w", err)
	}
	if logFile != nil {
		defer func() {
			_ = logFile.Close()
		}()
	}

	// Intercept OS signals for graceful shutdown.
	shutdownInterceptor, err := signal.Intercept()
	if err != nil {
		return fmt.Errorf("unable to intercept signals: %w", err)
	}

	return waved.Main(cfg, shutdownInterceptor)
}

// configureDaemonLogWriter makes the standalone daemon write logs to both
// stdout and a persistent log file. Embedders that provide LogWriter keep full
// control of the sink.
func configureDaemonLogWriter(cfg *waved.Config,
	stdout io.Writer) (*os.File, error) {

	if cfg.LogWriter != nil {
		return nil, nil
	}

	logDir := cfg.LogDir()
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory %q: %w", logDir,
			err)
	}

	logFilePath := filepath.Join(logDir, daemonLogFileName)
	logFile, err := os.OpenFile( //nolint:gosec // G304: operator log path.
		logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", logFilePath, err)
	}

	cfg.LogWriter = io.MultiWriter(bestEffortWriter{stdout}, logFile)

	return logFile, nil
}

// registerBitcoindFlags registers optional direct bitcoind package relay
// flags. Direct bitcoind relay is used for V3 ephemeral anchor transactions.
func registerBitcoindFlags(f *pflag.FlagSet) {
	f.String(
		"bitcoind.host", "",
		"bitcoind RPC address (host:port) for submitpackage support",
	)
	f.String("bitcoind.user", "",
		"bitcoind RPC username",
	)
	f.String(
		"bitcoind.pass", "", "bitcoind RPC password (prefer "+
			"bitcoind.rpccookie: command-line passwords are "+
			"visible to other users via 'ps')",
	)
	f.String(
		"bitcoind.rpccookie", "", "path to bitcoind's '.cookie' "+
			"auth file. Mutually exclusive with "+
			"bitcoind.user/bitcoind.pass; preferred because "+
			"the password never appears in process args or "+
			"persistent config",
	)
	f.String(
		"bitcoind.tlscertpath", "", "path to a PEM CA certificate "+
			"for HTTPS bitcoind RPC. Bare bitcoind.host values "+
			"use https:// when this is set",
	)
}

// configureBitcoindSubmitter wires the optional bitcoind package
// submitter onto cfg, reading the bitcoind.* config keys from v.
// Bitcoind support is opt-in: when bitcoind.host is unset the daemon
// runs without a direct submitpackage path (lnd v3 relay is used
// instead when available).
func configureBitcoindSubmitter(v *viper.Viper, cfg *waved.Config) error {
	host := v.GetString("bitcoind.host")
	if host == "" {
		return nil
	}

	user, pass, err := resolveBitcoindAuth(
		v.GetString("bitcoind.user"), v.GetString("bitcoind.pass"),
		v.GetString("bitcoind.rpccookie"),
	)
	if err != nil {
		return err
	}

	tlsCertPath := v.GetString("bitcoind.tlscertpath")
	submitter, err := bitcoindrpc.NewWithOptions(
		host, user, pass, bitcoindrpc.WithTLSCertPath(tlsCertPath),
	)
	if err != nil {
		return err
	}

	// Plain HTTP with Basic Auth exposes the cookie/password and the
	// submitpackage payload to anyone observing the network path. We
	// only warn (rather than refuse) because some operators deliberately
	// run bitcoind over a trusted local network; HTTPS can be enabled by
	// setting bitcoind.host to an https:// URL or by providing
	// bitcoind.tlscertpath with a bare host.
	if !isLoopbackHost(host) &&
		!isBitcoindHTTPSEndpoint(host, tlsCertPath) {

		fmt.Fprintf(
			os.Stderr, "WARNING: bitcoind.host %q is not "+
				"loopback. The RPC connection is plain HTTP "+
				"with Basic Auth, so credentials and "+
				"submitpackage payloads are sent in "+
				"cleartext over the network. Run bitcoind "+
				"on the same host (127.0.0.1) or front it "+
				"with a TLS reverse proxy reachable only "+
				"over a trusted link.\n", host,
		)
	}

	cfg.PackageSubmitter = submitter

	return nil
}

// isLoopbackHost reports whether the given bitcoind RPC address points
// at the local machine. It accepts a full URL ("http://...", "https://
// ..."), a bare "host:port", or a bare host. URLs are parsed via
// url.Parse + u.Hostname() so bracketed IPv6 addresses ("[::1]:8332")
// and trailing paths ("127.0.0.1:8332/wallet/foo") are handled
// correctly. The "localhost" hostname is treated as loopback even
// though it is technically a name lookup, matching the expectation
// operators have when they write it in a config file.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}

	// A bare IP literal (including IPv6 like "::1") doesn't parse
	// usefully through url.Parse — "http://::1" treats the ":" as
	// part of the scheme delimiter. Try the direct ParseIP path
	// first so this case is handled before any URL parsing.
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}

	// Synthesise a scheme when missing so url.Parse populates the
	// authority component (otherwise the input ends up in Path).
	// This is what makes bracketed IPv6 like "[::1]:8332" parse
	// correctly via u.Hostname(), which strips the brackets.
	parseURL := host
	if !strings.HasPrefix(host, "http://") &&
		!strings.HasPrefix(host, "https://") {

		parseURL = "http://" + host
	}

	u, err := url.Parse(parseURL)
	if err != nil {
		return false
	}

	raw := u.Hostname()
	if raw == "localhost" {
		return true
	}

	ip := net.ParseIP(raw)

	return ip != nil && ip.IsLoopback()
}

// isBitcoindHTTPSEndpoint reports whether configureBitcoindSubmitter
// will configure an HTTPS endpoint for the given host/TLS settings.
func isBitcoindHTTPSEndpoint(host, tlsCertPath string) bool {
	if strings.HasPrefix(host, "https://") {
		return true
	}
	if strings.Contains(host, "://") {
		return false
	}

	return tlsCertPath != ""
}

// resolveBitcoindAuth picks the bitcoind RPC credentials to use, given
// the operator-supplied user/pass values and an optional path to a
// bitcoind '.cookie' file. Cookie auth is preferred because it
// removes two leak paths that plain user/pass have: passwords on the
// command line are visible to every local process via 'ps', and
// passwords in a persistent config file survive across restarts and
// backups. The cookie file is regenerated on every bitcoind startup
// and is mode 0600 by bitcoind. Mutual exclusion mirrors lnd's
// behaviour and prevents silent precedence surprises.
func resolveBitcoindAuth(user, pass, cookiePath string) (string, string,
	error) {

	if cookiePath != "" && (user != "" || pass != "") {
		return "", "", fmt.Errorf("bitcoind.rpccookie is mutually " +
			"exclusive with bitcoind.user / bitcoind.pass; set " +
			"one or the other")
	}

	if cookiePath == "" {
		return user, pass, nil
	}

	//nolint:gosec // G304: cookie path comes from operator config.
	data, err := os.ReadFile(cookiePath)
	if err != nil {
		return "", "", fmt.Errorf("read bitcoind cookie %q: %w",
			cookiePath, err)
	}

	// bitcoind writes "<user>:<password>" with no trailing newline,
	// but tolerate stray whitespace from manual edits or copies.
	parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("bitcoind cookie %q has unexpected "+
			"format: want <user>:<password>", cookiePath)
	}

	return parts[0], parts[1], nil
}
