package darepod

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/chainbackends"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"google.golang.org/grpc"
)

const (
	// DefaultDataDir is the default root data directory for darepod. It
	// lives under the user's home directory.
	DefaultDataDir = "~/.darepod"

	// DefaultNetwork is the default bitcoin network the daemon operates
	// on.
	DefaultNetwork = "mainnet"

	// DefaultRPCHost is the default listen address for the daemon's own
	// gRPC server.
	DefaultRPCHost = "localhost:10029"

	// DefaultLndHost is the default address for connecting to the local
	// lnd instance.
	DefaultLndHost = "localhost:10009"

	// DefaultServerHost is the default address for the ark operator's
	// mailbox edge server.
	DefaultServerHost = "localhost:10010"

	// DefaultIndexerServerID is the canonical operator identifier used
	// in signed indexer proofs.
	DefaultIndexerServerID = "arkd"

	// DefaultRPCTimeout is the default timeout for RPC calls to lnd.
	DefaultRPCTimeout = 30 * time.Second

	// DefaultDebugLevel is the default logging verbosity.
	DefaultDebugLevel = "info"

	// DefaultShutdownTimeout is the maximum duration to wait for
	// graceful shutdown of the actor system and subsystems.
	DefaultShutdownTimeout = 10 * time.Second

	// DefaultForfeitCollectionTimeout is the default wall-clock
	// deadline for collecting forfeit signatures from VTXO actors
	// during a round.
	DefaultForfeitCollectionTimeout = 2 * time.Minute

	// DefaultWalletType is the default wallet backend. The "lwwallet"
	// backend uses an in-process lightweight wallet backed by
	// btcwallet and Esplora, requiring no external lnd node.
	DefaultWalletType = "lwwallet"

	// WalletTypeLnd selects lnd as the wallet backend.
	WalletTypeLnd = "lnd"

	// WalletTypeLwwallet selects the lightweight in-process wallet
	// backed by btcwallet and Esplora.
	WalletTypeLwwallet = "lwwallet"

	// WalletTypeBtcwallet selects the in-process wallet backed by
	// btcwallet and neutrino (BIP 157/158 compact block filters).
	WalletTypeBtcwallet = "btcwallet"

	// DefaultEsploraPollInterval is the default interval at which the
	// lwwallet polls the Esplora API for new blocks and transactions.
	DefaultEsploraPollInterval = 5 * time.Second

	// DefaultRecoveryWindow is the default address look-ahead window
	// used during lwwallet recovery.
	DefaultRecoveryWindow = 100
)

// Config holds all configuration for the darepod daemon.
type Config struct {
	// DataDir is the root data directory for all daemon state. Database
	// files, logs, and TLS material are stored under this directory.
	DataDir string `mapstructure:"datadir"`

	// Network selects the bitcoin network: mainnet, testnet, regtest, or
	// simnet.
	Network string `mapstructure:"network"`

	// DebugLevel controls the verbosity of daemon logging. A single value
	// sets the global level for all subsystems (e.g. "info"). A
	// comma-separated list of subsystem=level pairs sets per-subsystem
	// levels (e.g. "ROND=debug,OORC=trace,info"). The last bare level in
	// the list (without a '=') sets the default for unlisted subsystems.
	// Valid levels: trace, debug, info, warn, error, critical, off.
	DebugLevel string `mapstructure:"debuglevel"`

	// LogWriter is the sink for daemon log output. When nil, darepod
	// writes logs to stdout.
	LogWriter io.Writer

	// MailboxEdgeFactory optionally wraps the mailbox transport edge used
	// by the serverconn runtime. Test harnesses use this to intercept
	// durable transport traffic without changing production config files.
	MailboxEdgeFactory MailboxEdgeFactory

	// PackageSubmitter optionally provides atomic parent+child
	// package submission for the unroll subsystem. Typically backed
	// by a direct bitcoind RPC client. Set programmatically by the
	// test harness; not serialized to config files.
	PackageSubmitter chainbackends.PackageSubmitter

	// Lnd configures the connection to the backing lnd node.
	Lnd *LndConfig `mapstructure:"lnd"`

	// Server configures the connection to the ark operator's mailbox
	// edge server.
	Server *ServerConfig `mapstructure:"server"`

	// RPC configures the daemon's own gRPC server that external tools
	// (CLI, GUI) connect to.
	RPC *RPCConfig `mapstructure:"rpc"`

	// Wallet configures the wallet backend used for signing, key
	// derivation, and chain access.
	Wallet *WalletConfig `mapstructure:"wallet"`

	// ForfeitCollectionTimeout is the maximum wall-clock
	// duration to wait for forfeit signatures during a round.
	// If zero, the default of 2 minutes is used.
	//nolint:ll
	ForfeitCollectionTimeout time.Duration `mapstructure:"forfeitcollectiontimeout"`

	// AllowMainnet must be set to true explicitly to run the daemon
	// on mainnet. This guard prevents accidentally operating on
	// mainnet during development, since DefaultNetwork is "mainnet".
	AllowMainnet bool `mapstructure:"allow-mainnet"`

	// Unroll configures the unilateral-exit subsystem.
	Unroll *UnrollConfig `mapstructure:"unroll"`
}

// UnrollConfig configures the unilateral-exit subsystem.
type UnrollConfig struct {
	// BumpAfterBlocks is the number of blocks after which unroll
	// will attempt a fee-bump rebroadcast. Zero uses the default
	// of 6.
	BumpAfterBlocks int32 `mapstructure:"bumpafterblocks"`

	// MaxFeeRateSatPerVByte caps fee estimates to prevent runaway
	// fees. Zero uses the default of 100 sat/vB.
	MaxFeeRateSatPerVByte int64 `mapstructure:"maxfeeratesatpervbyte"`
}

// MailboxEdgeFactory constructs the mailbox edge client used by the
// serverconn runtime from the underlying gRPC connection to the operator.
type MailboxEdgeFactory func(
	conn grpc.ClientConnInterface,
) mailboxpb.MailboxServiceClient

// LndConfig holds connection parameters for the backing lnd node.
type LndConfig struct {
	// Host is the network address of the lnd gRPC interface.
	Host string `mapstructure:"host"`

	// TLSPath is the path to lnd's TLS certificate file. If empty, the
	// default lnd TLS cert location is used.
	TLSPath string `mapstructure:"tlspath"`

	// MacaroonPath is the full path to the lnd admin macaroon. If empty,
	// the default lnd macaroon location for the active network is used.
	MacaroonPath string `mapstructure:"macaroonpath"`

	// RPCTimeout is the maximum duration for individual RPC calls to
	// lnd. If zero, DefaultRPCTimeout is used.
	RPCTimeout time.Duration `mapstructure:"rpctimeout"`
}

// ServerConfig holds connection parameters for the ark operator's mailbox
// edge server.
type ServerConfig struct {
	// Host is the gRPC address of the ark operator's mailbox edge
	// service.
	Host string `mapstructure:"host"`

	// TLSCertPath is the path to the operator's TLS certificate for
	// verifying the server connection. If empty, the system cert pool
	// is used.
	TLSCertPath string `mapstructure:"tlscertpath"`

	// Insecure disables TLS for the server connection. This should only
	// be used in regtest or development environments.
	Insecure bool `mapstructure:"insecure"`

	// MaxTreeNodes caps the number of nodes accepted in a VTXO tree
	// received from the server. This prevents memory exhaustion from
	// oversized tree payloads. If zero, the default of
	// roundpb.DefaultMaxTreeNodes (50,000) is used.
	MaxTreeNodes int `mapstructure:"maxtreenodes"`
}

// RPCConfig holds configuration for the daemon's own gRPC server.
type RPCConfig struct {
	// ListenAddr is the network address the gRPC server binds to when the
	// daemon opens its own TCP listener. Valid RPC configurations either
	// set ListenAddr to a non-empty address or supply Listener
	// programmatically.
	ListenAddr string `mapstructure:"listenaddr"`

	// Listener is an optional pre-created listener. When non-nil, the
	// daemon serves on this listener instead of binding to ListenAddr.
	// This enables SDK-style embedding and in-memory transports such as
	// bufconn in tests. Listener is programmatic-only and is not loaded
	// from config files.
	Listener net.Listener

	// TLSCertPath is the path to the daemon's TLS certificate. If empty,
	// one is auto-generated in the data directory.
	TLSCertPath string `mapstructure:"tlscertpath"`

	// TLSKeyPath is the path to the daemon's TLS private key. If empty,
	// one is auto-generated in the data directory.
	TLSKeyPath string `mapstructure:"tlskeypath"`
}

// WalletConfig selects and configures the wallet backend.
type WalletConfig struct {
	// Type selects the wallet backend: "lnd" uses a connected lnd
	// node, "lwwallet" uses an in-process lightweight wallet backed
	// by btcwallet and Esplora.
	Type string `mapstructure:"type"`

	// EsploraURL is the base URL of the Esplora REST API used by the
	// lwwallet backend for chain data. Required when Type is
	// "lwwallet".
	EsploraURL string `mapstructure:"esploraurl"`

	// PollInterval controls how often the lwwallet backend polls the
	// Esplora API for new blocks. If zero,
	// DefaultEsploraPollInterval is used.
	PollInterval time.Duration `mapstructure:"pollinterval"`

	// RecoveryWindow is the address look-ahead window used during
	// lwwallet key recovery. If zero, DefaultRecoveryWindow is used.
	RecoveryWindow uint32 `mapstructure:"recoverywindow"`

	// PasswordFile is the path to a file containing the wallet
	// password for auto-unlock at daemon startup. The file contents
	// are read and trailing newlines are stripped. When set alongside
	// an existing encrypted seed file, the daemon unlocks the wallet
	// automatically without requiring an UnlockWallet RPC call.
	PasswordFile string `mapstructure:"password_file"`

	// BtcwalletPeers is a list of host:port addresses for neutrino
	// to connect to exclusively (no DNS seeding). Only used when
	// Type is "btcwallet".
	BtcwalletPeers []string `mapstructure:"btcwallet_peers"`

	// BtcwalletAddPeers is a list of additional persistent peers
	// for neutrino. DNS seeding still runs. Only used when Type is
	// "btcwallet".
	BtcwalletAddPeers []string `mapstructure:"btcwallet_addpeers"`

	// BtcwalletDataDir is the directory for neutrino's chain data
	// (headers, cfilters). Defaults to the network data directory.
	// Only used when Type is "btcwallet".
	BtcwalletDataDir string `mapstructure:"btcwallet_datadir"`

	// FeeURL is the URL for the fee estimation API endpoint used by
	// the btcwallet/neutrino backend. Required on mainnet since
	// neutrino has no mempool visibility.
	FeeURL string `mapstructure:"feeurl"`

	// PersistFilters controls whether neutrino writes compact block
	// filters to disk in addition to the in-memory cache.
	PersistFilters bool `mapstructure:"persist_filters"`

	// DisableGlobalLogs prevents btcwallet/neutrino package globals from
	// being wired to the daemon logger. This is intended for parallel
	// in-process tests that collect per-test log artifacts.
	DisableGlobalLogs bool `mapstructure:"disable_btcwallet_global_logs"`
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		DataDir:    DefaultDataDir,
		Network:    DefaultNetwork,
		DebugLevel: DefaultDebugLevel,
		Lnd: &LndConfig{
			Host:       DefaultLndHost,
			RPCTimeout: DefaultRPCTimeout,
		},
		Server: &ServerConfig{
			Host:         DefaultServerHost,
			MaxTreeNodes: roundpb.DefaultMaxTreeNodes,
		},
		RPC: &RPCConfig{
			ListenAddr: DefaultRPCHost,
		},
		Wallet: &WalletConfig{
			Type:           DefaultWalletType,
			PollInterval:   DefaultEsploraPollInterval,
			RecoveryWindow: DefaultRecoveryWindow,
		},
	}
}

// Validate checks the config for internal consistency and returns an error
// if any required fields are missing or invalid.
func (c *Config) Validate() error {
	switch c.Network {
	case "mainnet", "testnet", "regtest", "simnet", "signet":
	default:
		return fmt.Errorf("unknown network %q", c.Network)
	}

	// Require explicit opt-in for mainnet to prevent accidental
	// use during development.
	if c.Network == "mainnet" && !c.AllowMainnet {
		return fmt.Errorf(
			"running on mainnet requires " +
				"--allow-mainnet flag or " +
				"allow-mainnet=true in config",
		)
	}

	// Validate wallet config.
	if c.Wallet == nil {
		return fmt.Errorf("wallet config is required")
	}

	switch c.Wallet.Type {
	case WalletTypeLnd:
		// LND backend requires a valid lnd connection config.
		if c.Lnd == nil {
			return fmt.Errorf("lnd config is required " +
				"when wallet.type is lnd")
		}
		if c.Lnd.Host == "" {
			return fmt.Errorf("lnd host is required " +
				"when wallet.type is lnd")
		}

	case WalletTypeLwwallet:
		// Lightweight wallet requires an Esplora URL for
		// chain data.
		if c.Wallet.EsploraURL == "" {
			return fmt.Errorf("wallet.esploraurl is " +
				"required when wallet.type is " +
				"lwwallet")
		}

	case WalletTypeBtcwallet:
		// Neutrino has no mempool visibility, so fee estimation
		// always requires an external API regardless of network.
		if c.Wallet.FeeURL == "" {
			return fmt.Errorf("wallet.feeurl is required " +
				"when wallet.type is btcwallet")
		}

	default:
		return fmt.Errorf(
			"unknown wallet type %q, valid values: "+
				"lnd, lwwallet, btcwallet",
			c.Wallet.Type,
		)
	}

	if c.Server == nil {
		return fmt.Errorf("server config is required")
	}
	if c.Server.Host == "" {
		return fmt.Errorf("server host is required")
	}
	if c.RPC == nil {
		return fmt.Errorf("rpc config is required")
	}
	if c.RPC.Listener == nil && c.RPC.ListenAddr == "" {
		return fmt.Errorf("rpc listen address or injected " +
			"listener is required")
	}

	return nil
}

// networkToChainParams maps a network name string to the corresponding
// btcd chain configuration parameters.
func networkToChainParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &chaincfg.MainNetParams, nil
	case "testnet":
		return &chaincfg.TestNet3Params, nil
	case "regtest":
		return &chaincfg.RegressionNetParams, nil
	case "simnet":
		return &chaincfg.SimNetParams, nil
	case "signet":
		return &chaincfg.SigNetParams, nil
	default:
		return nil, fmt.Errorf("unknown network %q", network)
	}
}

// NetworkDir returns the network-scoped data directory (e.g.,
// ~/.darepod/data/regtest).
func (c *Config) NetworkDir() (string, error) {
	dataDir, err := expandTilde(c.DataDir)
	if err != nil {
		return "", err
	}

	return filepath.Join(dataDir, "data", c.Network), nil
}

// LogDir returns the network-scoped log directory.
func (c *Config) LogDir() (string, error) {
	dataDir, err := expandTilde(c.DataDir)
	if err != nil {
		return "", err
	}

	return filepath.Join(dataDir, "logs", c.Network), nil
}

// expandTilde replaces a leading ~ or ~/ with the user's home
// directory. For example, "~/.darepod" becomes "/home/user/.darepod".
// It returns an error if the home directory cannot be determined.
func expandTilde(path string) (string, error) {
	if len(path) == 0 || path[0] != '~' {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}

	// Strip the leading "~" and any path separator that follows
	// it, so filepath.Join receives a relative suffix. Without
	// this, "~/.darepod" would produce path[1:] == "/.darepod"
	// which is absolute and causes Join to discard home.
	suffix := path[1:]
	if len(suffix) > 0 && os.IsPathSeparator(suffix[0]) {
		suffix = suffix[1:]
	}

	return filepath.Join(home, suffix), nil
}
