package darepo

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// DefaultNetwork is the default bitcoin network the daemon operates
	// on.
	DefaultNetwork = "regtest"

	// DefaultLogLevel is the default logging verbosity.
	DefaultLogLevel = "info"

	// DefaultAdminRPCListen is the default listen address for the admin
	// gRPC server.
	DefaultAdminRPCListen = "localhost:8081"

	// DefaultRPCListen is the default listen address for the
	// client-facing gRPC server.
	DefaultRPCListen = "localhost:7070"

	// DefaultLndHost is the default address for connecting to the
	// local lnd instance.
	DefaultLndHost = "localhost:10009"

	// DefaultRPCTimeout is the default timeout for RPC calls to lnd.
	DefaultRPCTimeout = 30 * time.Second

	// DefaultShutdownTimeout is the maximum duration to wait for
	// graceful shutdown of the actor system and subsystems.
	DefaultShutdownTimeout = 10 * time.Second

	// defaultLogDirname is the default directory name for log
	// files.
	defaultLogDirname = "logs"
)

var (
	// defaultDataDir is the default directory where arkd tries to
	// find its configuration file and store its data. This is a
	// directory in the user's application data, for example:
	//   C:\Users\<username>\AppData\Local\Arkd on Windows
	//   ~/.arkd on Linux
	//   ~/Library/Application Support/Arkd on MacOS
	defaultDataDir = btcutil.AppDataDir("arkd", false)
)

// TLSConfig holds TLS certificate paths for the client-facing gRPC
// server. When nil, the server runs without TLS (suitable for
// development and regtest).
type TLSConfig struct {
	// CertPath is the path to the TLS certificate file.
	CertPath string `mapstructure:"certpath"`

	// KeyPath is the path to the TLS private key file.
	KeyPath string `mapstructure:"keypath"`

	// AutoCert enables automatic TLS certificate generation using a
	// self-signed CA. When true, CertPath and KeyPath are used as
	// output paths for the generated material.
	AutoCert bool `mapstructure:"autocert"`
}

// RoundsConfig holds operator policy for the round subsystem. These
// fields map directly to batch.Terms entries that do not require key
// material. Key-dependent fields (OperatorKey, SweepKey,
// ConnectorAddress) are resolved separately once key management is
// wired.
type RoundsConfig struct {
	// SweepDelay is the CSV delay for the sweep path in VTXO
	// trees (blocks).
	SweepDelay uint32 `mapstructure:"sweepdelay"`

	// MaxVTXOsPerTree is the maximum number of VTXOs in a single
	// batch tree.
	MaxVTXOsPerTree uint32 `mapstructure:"maxvtxospertree"`

	// TreeRadix is the branching factor for VTXO trees.
	TreeRadix uint32 `mapstructure:"treeradix"`

	// MaxConnectorsPerTree is the maximum number of connector
	// leaves per connector tree.
	MaxConnectorsPerTree uint32 `mapstructure:"maxconnectorspertree"`

	// ConnectorDustAmount is the amount assigned to each connector
	// leaf output (satoshis).
	ConnectorDustAmount int64 `mapstructure:"connectordustamount"`

	// BoardingExitDelay is the minimum exit delay for boarding
	// inputs (blocks).
	BoardingExitDelay uint32 `mapstructure:"boardingexitdelay"`

	// BoardingExitDelaySafetyMargin is how many blocks before the
	// exit delay we stop accepting boarding inputs.
	BoardingExitDelaySafetyMargin uint32 `mapstructure:"boardingexitdelaymargin"` //nolint:ll

	// MinBoardingConfirmations is the minimum confirmation count
	// for boarding inputs.
	MinBoardingConfirmations uint32 `mapstructure:"minboardingconfirmations"` //nolint:ll

	// MinVTXOAmount is the minimum amount for a VTXO output
	// (satoshis).
	MinVTXOAmount int64 `mapstructure:"minvtxoamount"`

	// MaxVTXOAmount is the maximum amount for a VTXO output
	// (satoshis).
	MaxVTXOAmount int64 `mapstructure:"maxvtxoamount"`

	// MinOperatorFee is the minimum operator fee per round
	// (satoshis).
	MinOperatorFee int64 `mapstructure:"minoperatorfee"`

	// VTXOExitDelay is the minimum exit delay for VTXOs (blocks).
	VTXOExitDelay uint32 `mapstructure:"vtxoexitdelay"`

	// RegistrationTimeout is how long to wait for client
	// registrations before sealing a round.
	RegistrationTimeout time.Duration `mapstructure:"registrationtimeout"`

	// SignatureCollectionTimeout is how long to wait for nonces
	// and signatures during each collection phase.
	SignatureCollectionTimeout time.Duration `mapstructure:"sigcollecttimeout"` //nolint:ll

	// FundPsbtLockDuration is how long LND holds the UTXO lease
	// when FundPsbt is called. Must be longer than
	// RegistrationTimeout + 3*SignatureCollectionTimeout.
	FundPsbtLockDuration time.Duration `mapstructure:"fundpsbtlockduration"`

	// ConfTarget is the confirmation target for fee estimation.
	ConfTarget uint32 `mapstructure:"conftarget"`

	// MinConfs is the minimum confirmation count for wallet
	// UTXOs used in batch funding.
	MinConfs int32 `mapstructure:"minconfs"`

	// ConfirmationTarget is the number of on-chain confirmations
	// required before transitioning a round to confirmed.
	ConfirmationTarget uint32 `mapstructure:"confirmationtarget"`

	// MaxRoundClients seals a round as soon as this many clients
	// have joined. Zero disables the limit.
	MaxRoundClients int `mapstructure:"maxroundclients"`

	// MaxRoundOutputAmount seals a round once the total output
	// value (VTXOs + leaves) reaches this amount in satoshis.
	// Zero disables the limit.
	MaxRoundOutputAmount btcutil.Amount `mapstructure:"maxroundoutputamount"` //nolint:ll
}

// DefaultRoundsConfig returns a RoundsConfig with sensible defaults
// suitable for development and regtest.
func DefaultRoundsConfig() *RoundsConfig {
	return &RoundsConfig{
		SweepDelay:                    1008,
		MaxVTXOsPerTree:               128,
		TreeRadix:                     2,
		MaxConnectorsPerTree:          32,
		ConnectorDustAmount:           330,
		BoardingExitDelay:             512,
		BoardingExitDelaySafetyMargin: 48,
		MinBoardingConfirmations:      1,
		MinVTXOAmount:                 1000,
		MaxVTXOAmount:                 100_000_000_000,
		MinOperatorFee:                1000,
		VTXOExitDelay:                 144,
		RegistrationTimeout:           10 * time.Second,
		SignatureCollectionTimeout:    10 * time.Second,
		FundPsbtLockDuration:          30 * time.Minute,
		ConfTarget:                    6,
		MinConfs:                      1,
		ConfirmationTarget:            1,
		MaxRoundClients:               128,
		MaxRoundOutputAmount:          0,
	}
}

// Config is the main configuration struct for the operator server.
type Config struct {
	// DataDir is the root data directory for all daemon state.
	// Database files, logs, and TLS material are stored under this
	// directory.
	DataDir string `mapstructure:"datadir"`

	// Network selects the bitcoin network: mainnet, testnet, regtest,
	// or signet.
	Network string `mapstructure:"network"`

	// DebugLevel controls the verbosity of daemon logging. Valid
	// values include trace, debug, info, warn, error, and critical.
	DebugLevel string `mapstructure:"debuglevel"`

	// LogFilePath is the path to write the log file.
	LogFilePath string `mapstructure:"logfile"`

	// LogWriter is an optional sink for daemon log output when the
	// server is started programmatically rather than via cmd/arkd.
	// When nil, NewServer leaves logging disabled unless the caller
	// provides cfg.Loggers and cfg.Log explicitly.
	LogWriter io.Writer

	// DB contains the database configuration (sqlite or postgres).
	DB *db.Config `mapstructure:"db"`

	// Lnd configures the connection to the backing lnd node.
	Lnd *LndConfig `mapstructure:"lnd"`

	// Bitcoind configures an optional direct bitcoind RPC
	// connection for UTXO validation. When set, boarding requests
	// are validated via GetTxOut rather than client TxProofs.
	Bitcoind *BitcoindConfig `mapstructure:"bitcoind"`

	// AdminRPC contains the admin RPC server configuration.
	AdminRPC *AdminRPCConfig `mapstructure:"adminrpc"`

	// RPC contains the client-facing RPC server configuration.
	RPC *RPCConfig `mapstructure:"rpc"`

	// Rounds configures the round subsystem policy (tree shape,
	// timeouts, confirmation targets).
	Rounds *RoundsConfig `mapstructure:"rounds"`

	// Log is an optional logger for the server itself. When None,
	// logging is disabled.
	Log fn.Option[btclog.Logger]

	// Loggers holds per-subsystem loggers created at startup. Child
	// components extract their own logger from this map during
	// construction. When nil, each component falls back to
	// btclog.Disabled.
	Loggers SubLoggers

	// Shutdown is a callback that triggers graceful server shutdown.
	Shutdown func()
}

// BitcoindConfig holds optional connection parameters for a direct
// bitcoind RPC connection. When configured, the operator validates
// boarding UTXOs via GetTxOut instead of relying on client-provided
// TxProofs. This is strongly recommended for production deployments.
type BitcoindConfig struct {
	// Host is the bitcoind RPC address (host:port).
	Host string `mapstructure:"host"`

	// User is the RPC username.
	User string `mapstructure:"user"`

	// Pass is the RPC password.
	Pass string `mapstructure:"pass"`
}

// LndConfig holds connection parameters for the backing lnd node.
type LndConfig struct {
	// Host is the network address of the lnd gRPC interface.
	Host string `mapstructure:"host"`

	// TLSPath is the path to lnd's TLS certificate file. If empty,
	// the default lnd TLS cert location is used.
	TLSPath string `mapstructure:"tlspath"`

	// MacaroonPath is the full path to the lnd admin macaroon. If
	// empty, the default lnd macaroon location for the active
	// network is used.
	MacaroonPath string `mapstructure:"macaroonpath"`

	// RPCTimeout is the maximum duration for individual RPC calls to
	// lnd. If zero, DefaultRPCTimeout is used.
	RPCTimeout time.Duration `mapstructure:"rpctimeout"`
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		DataDir:    defaultDataDir,
		Network:    DefaultNetwork,
		DebugLevel: DefaultLogLevel,
		LogFilePath: filepath.Join(
			defaultDataDir, defaultLogDirname,
		),
		DB: db.DefaultConfig(defaultDataDir),
		Lnd: &LndConfig{
			Host:       DefaultLndHost,
			RPCTimeout: DefaultRPCTimeout,
		},
		AdminRPC: DefaultAdminRPCConfig(),
		RPC:      DefaultRPCConfig(),
		Rounds:   DefaultRoundsConfig(),
	}
}

// Validate checks the config for internal consistency and returns an
// error if any required fields are missing or invalid.
func (c *Config) Validate() error {
	switch c.Network {
	case "mainnet", "testnet", "regtest", "simnet", "signet":

	default:
		return fmt.Errorf("unknown network %q", c.Network)
	}

	if c.Lnd == nil {
		return fmt.Errorf("lnd config is required")
	}
	if c.Lnd.Host == "" {
		return fmt.Errorf("lnd host is required")
	}

	if c.DB == nil {
		return fmt.Errorf("db config is required")
	}

	if c.AdminRPC == nil {
		return fmt.Errorf("admin rpc config is required")
	}
	if c.AdminRPC.ListenAddr == "" {
		return fmt.Errorf("admin rpc listen address is required")
	}

	if c.RPC == nil {
		return fmt.Errorf("rpc config is required")
	}
	if c.RPC.ListenAddr == "" {
		return fmt.Errorf("rpc listen address is required")
	}
	if c.Rounds == nil {
		return fmt.Errorf("rounds config is required")
	}
	if c.Rounds.ConnectorDustAmount <= 0 {
		return fmt.Errorf(
			"rounds connector dust amount must be > 0",
		)
	}

	// Validate TLS config: if a cert path is set, a key path is
	// required, and vice versa.
	if c.RPC.TLS != nil {
		tls := c.RPC.TLS
		if tls.CertPath != "" && tls.KeyPath == "" {
			return fmt.Errorf(
				"rpc.tls.keypath is required when " +
					"rpc.tls.certpath is set",
			)
		}
		if tls.KeyPath != "" && tls.CertPath == "" {
			return fmt.Errorf(
				"rpc.tls.certpath is required when " +
					"rpc.tls.keypath is set",
			)
		}
	}

	return nil
}

// NetworkDir returns the network-scoped data directory (e.g.,
// ~/.arkd/data/regtest).
func (c *Config) NetworkDir() string {
	return filepath.Join(
		expandTilde(c.DataDir), "data", c.Network,
	)
}

// LogDir returns the network-scoped log directory.
func (c *Config) LogDir() string {
	return filepath.Join(
		expandTilde(c.DataDir), "logs", c.Network,
	)
}

// expandTilde replaces a leading ~ or ~/ with the user's home
// directory. For example, "~/.arkd" becomes "/home/user/.arkd".
func expandTilde(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	// Strip the leading "~" and any path separator that follows
	// it, so filepath.Join receives a relative suffix. Without
	// this, "~/.arkd" would produce path[1:] == "/.arkd" which
	// is absolute and causes Join to discard home.
	suffix := path[1:]
	if len(suffix) > 0 && os.IsPathSeparator(suffix[0]) {
		suffix = suffix[1:]
	}

	return filepath.Join(home, suffix)
}
