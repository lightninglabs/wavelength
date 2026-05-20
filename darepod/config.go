package darepod

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/darepo-client/chainbackends"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/oor"
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

	// DefaultRPCGatewayHost is the default listen address for the
	// daemon's HTTP/JSON gateway.
	DefaultRPCGatewayHost = "localhost:10031"

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
	// Mainnet blocks land roughly every 10 minutes, so a 30 s cadence
	// stays comfortably within one block's worth of latency while
	// keeping the request volume well under the public mempool.space
	// rate limits. Tests / regtest environments that mine blocks on
	// demand should override this to a sub-second value via the
	// `wallet.pollinterval` config knob.
	DefaultEsploraPollInterval = 30 * time.Second

	// DefaultRecoveryWindow is the default address look-ahead window
	// used during lwwallet recovery.
	DefaultRecoveryWindow = 100

	// DefaultMaxOperatorFeeSat is the default client-side cap on
	// the per-round operator fee under the seal-time fee
	// handshake. 0.01 BTC — generous for regtest/testnet and well
	// below any reasonable mainnet abuse threshold. Operators that
	// need a stricter cap override via the `maxoperatorfeesat`
	// config knob.
	DefaultMaxOperatorFeeSat int64 = 1_000_000

	// RPCTransportGRPC selects native gRPC for daemon-owned outbound RPCs.
	RPCTransportGRPC = "grpc"

	// RPCTransportREST selects grpc-gateway HTTP/JSON for daemon-owned
	// outbound RPCs.
	RPCTransportREST = "rest"
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

	// LogDirPath overrides the network-scoped directory used by the CLI
	// for persistent daemon log files. When empty, logs are written under
	// DataDir/logs/<network>.
	LogDirPath string `mapstructure:"logdir"`

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

	// RPCServiceRegistrars are programmatic hooks that may register
	// optional subservers on the daemon gRPC server after DaemonService is
	// registered. They are not loaded from config files because they wire
	// compiled-in runtime capabilities, such as swapruntime, rather than
	// user-provided daemon settings.
	RPCServiceRegistrars []RPCServiceRegistrar

	// RPCGatewayRegistrars are programmatic hooks that may register
	// optional subservers on the daemon HTTP/JSON gateway after
	// DaemonService is registered.
	RPCGatewayRegistrars []RPCGatewayRegistrar

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

	// Swap configures the optional swapruntime subserver. The fields are
	// inert in default builds because the SwapClientService is not
	// registered unless the daemon is compiled with the swapruntime tag.
	Swap *SwapConfig `mapstructure:"swap"`

	// SwapWallet configures the optional walletrpc subserver (the
	// simplified high-level wallet facade composed over the swap
	// runtime and the cooperative-leave subsystem). The fields are
	// inert in default builds because the WalletService is not
	// registered unless the daemon is compiled with both the
	// walletrpc and swapruntime tags.
	SwapWallet *SwapWalletConfig `mapstructure:"swapwallet"`

	// MaxOperatorFeeSat caps the per-round operator fee the client
	// is willing to pay under the #270 seal-time fee handshake.
	// Every JoinRoundQuote is compared against this value before
	// the client accepts; a quote above the cap is rejected with
	// JoinRoundRejectOutbox and the FSM transitions to
	// ClientFailedState without signing. A zero / negative value
	// fails closed (every quote rejected) so an unset cap cannot
	// silently disable the protection. Defaults to 1_000_000 sats
	// (0.01 BTC), generous enough for regtest/testnet but well
	// below any reasonable mainnet abuse threshold.
	MaxOperatorFeeSat int64 `mapstructure:"maxoperatorfeesat"`

	// OOR configures off-band receive/send actor behavior.
	OOR *OORConfig `mapstructure:"oor"`

	// EagerRoundJoin makes the wallet actor drive round-joining
	// without waiting for a follow-up Board / LeaveVTXOs RPC. With
	// the flag on, every freshly confirmed boarding UTXO runs the
	// existing Board path inline, and cooperative-leave intents are
	// forwarded with TriggerRegistration=true so the round FSM
	// leaves PendingRoundAssembly immediately. Off (the default)
	// keeps the batched semantics that operator-driven hosts rely
	// on (darepocli, server deployments); wallet-shaped SDK hosts
	// (sdk/walletdk) opt in so user-visible "deposit" and "exit"
	// actions translate into a full round join end-to-end.
	EagerRoundJoin bool `mapstructure:"eagerroundjoin"`
}

// RPCServiceRegistrar registers one optional daemon gRPC subserver on the
// daemon's existing listener.
//
// Registrars are invoked after the core DaemonService is registered but before
// the server begins accepting requests. A registrar may return a cleanup
// function for any resources it owns, such as background workers, stores, or
// upstream gRPC connections; that cleanup is called during daemon shutdown.
type RPCServiceRegistrar func(
	ctx context.Context, grpcServer *grpc.Server, rpcServer *RPCServer,
	cfg *Config,
) (func(), error)

// RPCGatewayRegistrar registers one optional daemon HTTP/JSON subserver on
// the daemon gateway.
type RPCGatewayRegistrar func(
	ctx context.Context, mux *runtime.ServeMux, endpoint string,
	opts []grpc.DialOption, rpcServer *RPCServer, cfg *Config,
) error

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

// OORConfig configures off-band transfer actor behavior.
type OORConfig struct {
	// Limits configures advanced incoming OOR receive safety caps.
	Limits *OORLimitsConfig `mapstructure:"limits"`
}

// OORLimitsConfig configures advanced incoming OOR receive safety caps.
type OORLimitsConfig struct {
	// MaxCheckpoints caps checkpoint transactions allowed in one incoming
	// OOR transfer.
	MaxCheckpoints uint32 `mapstructure:"maxcheckpoints"`

	// MaxVTXOMatches caps VTXOs returned by one indexer lookup during
	// incoming OOR receive.
	MaxVTXOMatches uint32 `mapstructure:"maxvtxomatches"`

	// MaxMailboxItems caps items decoded from one stored mailbox message.
	MaxMailboxItems uint32 `mapstructure:"maxmailboxitems"`

	// MaxMailboxScriptBytes caps address-script bytes decoded from one
	// stored mailbox message.
	MaxMailboxScriptBytes uint32 `mapstructure:"maxmailboxscriptbytes"`
}

// minOORMailboxScriptBytes is the smallest standard script cap accepted by
// daemon config validation. It covers a v1 P2TR output script.
const minOORMailboxScriptBytes uint32 = 34

// defaultOORConfig returns daemon defaults for OOR actor settings.
func defaultOORConfig() *OORConfig {
	limits := oor.DefaultReceiveLimits()

	return &OORConfig{
		Limits: &OORLimitsConfig{
			MaxCheckpoints:        limits.MaxCheckpoints,
			MaxVTXOMatches:        limits.MaxVTXOMatches,
			MaxMailboxItems:       limits.MaxMailboxItems,
			MaxMailboxScriptBytes: limits.MaxMailboxScriptBytes,
		},
	}
}

// OORReceiveLimits returns the incoming OOR receive limits configured for this
// daemon.
func (c *Config) OORReceiveLimits() oor.ReceiveLimits {
	if c == nil || c.OOR == nil || c.OOR.Limits == nil {
		return oor.DefaultReceiveLimits()
	}

	return oor.ReceiveLimits{
		MaxCheckpoints:        c.OOR.Limits.MaxCheckpoints,
		MaxVTXOMatches:        c.OOR.Limits.MaxVTXOMatches,
		MaxMailboxItems:       c.OOR.Limits.MaxMailboxItems,
		MaxMailboxScriptBytes: c.OOR.Limits.MaxMailboxScriptBytes,
	}
}

// SwapConfig configures the optional daemon-owned swap executor.
//
// The struct is present in all builds so configuration files can be stable, but
// the fields are only consumed when the daemon is compiled with swapruntime and
// registers SwapClientService.
type SwapConfig struct {
	// ServerAddress is the swapdk-server endpoint used by the daemon
	// executor. Its meaning follows ServerTransport: host:port for gRPC,
	// or an HTTP gateway base URL for REST. Empty values fall back to the
	// local development default.
	ServerAddress string `mapstructure:"serveraddress"`

	// ServerTransport selects the daemon-owned swapdk-server transport.
	// Empty values default to gRPC.
	ServerTransport string `mapstructure:"servertransport"`

	// ServerTLSCertPath is an optional TLS certificate path for the
	// swapdk-server connection. When set, the daemon uses the certificate
	// instead of system roots or insecure local credentials.
	ServerTLSCertPath string `mapstructure:"servertlscertpath"`

	// ServerInsecure disables TLS for the swapdk-server connection. This
	// should only be used for explicit regtest/dev deployments.
	ServerInsecure bool `mapstructure:"serverinsecure"`

	// DatabaseFileName is the daemon-owned swap SQLite database path. When
	// empty, the daemon stores swaps under DataDir/swaps.db so restart
	// resume can discover pending sessions without CLI state.
	DatabaseFileName string `mapstructure:"databasefilename"`

	// SuppressResume disables swapclientserver's own synchronous
	// resume-on-startup sweep so a higher layer (walletrpc subserver) can
	// own the unified resume policy. Default false preserves identical
	// behavior for swapruntime-only builds: the swap subserver continues
	// to resume its pending sessions before Register returns. The flag is
	// set programmatically by the walletrpc registrar; it is not loaded
	// from config files.
	SuppressResume bool `mapstructure:"-"`

	// Backend is populated by swapclientserver.Register after the swap
	// subserver is fully wired. Higher layers (the walletrpc subserver)
	// read this handle to drive in-Go calls into the swap runtime without
	// going through the gRPC stub. The field is set programmatically by
	// the registrar; it is never loaded from config files.
	Backend SwapBackend `mapstructure:"-"`
}

// SwapBackend is the in-Go handle exposed by swapclientserver after Register
// completes. It lets higher-level subservers (such as the walletrpc subserver)
// drive the swap runtime without dialing the daemon's gRPC server from inside
// the same process. The interface is intentionally small and grows only as
// new wallet-layer needs arise.
type SwapBackend interface {
	// ResumePending re-arms background workers for every persisted
	// pending swap session. It is idempotent: payment hashes already
	// owned by an active worker are skipped. Callers invoke it once at
	// daemon startup so the gRPC server begins accepting requests with
	// every prior session already running.
	ResumePending(ctx context.Context)
}

// SwapWalletConfig configures the optional walletrpc subserver. The struct
// is present in all builds so configuration files stay stable, but the
// fields are only consumed when the daemon is compiled with both the
// walletrpc and swapruntime build tags.
type SwapWalletConfig struct {
	// Deadline is the wallet-level timeout applied to every PENDING
	// entry. When an entry is older than this duration without
	// transitioning to a terminal state, the runtime overlays its
	// status as FAILED with failure_reason="timed_out" so the user
	// surface never hangs on a stuck swap. Zero means use the
	// package default (30 minutes). The wallet deadline lives ABOVE
	// the swap FSM's own deadline; it never mutates underlying swap
	// state.
	Deadline time.Duration `mapstructure:"deadline"`

	// DefaultListLimit is the page size used when a List or
	// SubscribeWallet snapshot request omits a limit. Zero means use
	// the package default (100). The configured value is also clamped
	// to MaxListLimit so a misconfiguration cannot silently fan out
	// unbounded DB work.
	DefaultListLimit uint32 `mapstructure:"defaultlistlimit"`

	// MaxListLimit caps the per-call list page size. Larger callers
	// are clamped to this maximum. Zero means use the package default
	// (1000).
	MaxListLimit uint32 `mapstructure:"maxlistlimit"`

	// SubscribeBuffer is the per-subscriber channel buffer used by
	// SubscribeWallet. A slow consumer drops updates when its buffer
	// saturates; it can reconcile via List on reconnect. Zero means
	// use the package default (32).
	SubscribeBuffer uint32 `mapstructure:"subscribebuffer"`
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
	// Host is the ark operator endpoint. Its meaning follows Transport:
	// host:port for gRPC, or an HTTP gateway base URL for REST.
	Host string `mapstructure:"host"`

	// Transport selects the daemon-owned outbound transport for ArkService
	// and MailboxService clients. OOR traffic uses the mailbox edge, so it
	// follows this selector as well. Empty values default to gRPC.
	Transport string `mapstructure:"transport"`

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

	// Gateway contains the HTTP/JSON gateway configuration for the
	// daemon RPC server.
	Gateway *GatewayConfig `mapstructure:"gateway"`

	// TLSCertPath is the path to the daemon's TLS certificate. If empty,
	// one is auto-generated in the data directory.
	TLSCertPath string `mapstructure:"tlscertpath"`

	// TLSKeyPath is the path to the daemon's TLS private key. If empty,
	// one is auto-generated in the data directory.
	TLSKeyPath string `mapstructure:"tlskeypath"`
}

// GatewayConfig contains configuration for an HTTP/JSON grpc-gateway
// listener.
type GatewayConfig struct {
	// Enabled controls whether the HTTP gateway starts with its
	// owning gRPC server.
	Enabled bool `mapstructure:"enabled"`

	// ListenAddr is the network address the gateway binds to.
	ListenAddr string `mapstructure:"listenaddr"`

	// AllowedOrigins lists browser origins that may call the gateway.
	// Empty means no cross-origin browser access; requests without an
	// Origin header, such as CLI or local service calls, are still served.
	AllowedOrigins []string `mapstructure:"allowedorigins"`

	// Listener is an optional pre-created listener. When non-nil,
	// the gateway serves on this listener instead of binding to
	// ListenAddr. This is programmatic-only and is not loaded from
	// config files.
	Listener net.Listener
}

// DefaultGatewayConfig returns an enabled HTTP gateway config.
func DefaultGatewayConfig() *GatewayConfig {
	return &GatewayConfig{
		Enabled:    true,
		ListenAddr: DefaultRPCGatewayHost,
	}
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

	// DisableGlobalLoggers prevents btcwallet/neutrino package globals
	// from being wired to the daemon logger. This is intended for
	// parallel in-process tests that collect per-test log artifacts.
	DisableGlobalLoggers bool `mapstructure:"disable_btcwallet_global_logs"`
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
			Transport:    RPCTransportGRPC,
			MaxTreeNodes: roundpb.DefaultMaxTreeNodes,
		},
		RPC: &RPCConfig{
			ListenAddr: DefaultRPCHost,
			Gateway:    DefaultGatewayConfig(),
		},
		Wallet: &WalletConfig{
			Type:           DefaultWalletType,
			PollInterval:   DefaultEsploraPollInterval,
			RecoveryWindow: DefaultRecoveryWindow,
		},
		Swap: &SwapConfig{
			ServerAddress:   "localhost:10030",
			ServerTransport: RPCTransportGRPC,
		},
		MaxOperatorFeeSat: DefaultMaxOperatorFeeSat,
		OOR:               defaultOORConfig(),
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
		return fmt.Errorf("running on mainnet requires " +
			"--allow-mainnet flag or allow-mainnet=true in config")
	}

	// Under the seal-time fee handshake MaxOperatorFeeSat is the
	// sole client-side defense against a server quoting an
	// excessive operator fee. A zero / negative value is a
	// misconfiguration — not a "no cap" sentinel — so refuse to
	// start rather than silently accepting any fee.
	if c.MaxOperatorFeeSat <= 0 {
		return fmt.Errorf("maxoperatorfeesat must be positive: got %d",
			c.MaxOperatorFeeSat)
	}

	if c.OOR == nil {
		c.OOR = defaultOORConfig()
	}
	if c.OOR.Limits == nil {
		c.OOR.Limits = defaultOORConfig().Limits
	}
	if err := validateOORLimitsConfig(c.OOR.Limits); err != nil {
		return err
	}

	// Validate wallet config.
	if c.Wallet == nil {
		return fmt.Errorf("wallet config is required")
	}

	switch c.Wallet.Type {
	case WalletTypeLnd:
		// LND backend requires a valid lnd connection config.
		if c.Lnd == nil {
			return fmt.Errorf("lnd config is required when " +
				"wallet.type is lnd")
		}
		if c.Lnd.Host == "" {
			return fmt.Errorf("lnd host is required when " +
				"wallet.type is lnd")
		}

	case WalletTypeLwwallet:
		// Lightweight wallet requires an Esplora URL for
		// chain data.
		if c.Wallet.EsploraURL == "" {
			return fmt.Errorf("wallet.esploraurl is required " +
				"when wallet.type is lwwallet")
		}

	case WalletTypeBtcwallet:
		// Neutrino has no mempool visibility, so fee estimation
		// always requires an external API regardless of network.
		if c.Wallet.FeeURL == "" {
			return fmt.Errorf("wallet.feeurl is required when " +
				"wallet.type is btcwallet")
		}

	default:
		return fmt.Errorf("unknown wallet type %q, valid values: lnd, "+
			"lwwallet, btcwallet", c.Wallet.Type)
	}

	if c.Server == nil {
		return fmt.Errorf("server config is required")
	}
	if c.Server.Host == "" {
		return fmt.Errorf("server host is required")
	}
	if err := validateRPCTransport(
		"server.transport", c.Server.Transport,
	); err != nil {
		return err
	}
	if c.Swap != nil {
		if err := validateRPCTransport(
			"swap.servertransport", c.Swap.ServerTransport,
		); err != nil {
			return err
		}
	}
	if c.RPC == nil {
		return fmt.Errorf("rpc config is required")
	}
	if c.RPC.Listener == nil && c.RPC.ListenAddr == "" {
		return fmt.Errorf("rpc listen address or injected listener " +
			"is required")
	}
	if c.RPC.Gateway == nil {
		return fmt.Errorf("rpc gateway config is required")
	}
	if c.RPC.Gateway.Enabled && c.RPC.Gateway.Listener == nil &&
		c.RPC.Gateway.ListenAddr == "" {
		return fmt.Errorf("rpc gateway listen address or injected " +
			"listener is required")
	}
	if err := validateGatewayAllowedOrigins(
		c.RPC.Gateway.AllowedOrigins,
	); err != nil {
		return err
	}

	return nil
}

// validateGatewayAllowedOrigins rejects wildcard CORS grants on wallet-control
// APIs.
func validateGatewayAllowedOrigins(origins []string) error {
	for _, origin := range origins {
		switch origin {
		case "", "*":
			return fmt.Errorf("rpc.gateway.allowedorigins must "+
				"contain explicit origins, got %q", origin)
		}
	}

	return nil
}

// validateRPCTransport checks an optional RPC transport selector.
func validateRPCTransport(name, transport string) error {
	switch transport {
	case "", RPCTransportGRPC, RPCTransportREST:
		return nil

	default:
		return fmt.Errorf("%s must be %q or %q: got %q", name,
			RPCTransportGRPC, RPCTransportREST, transport)
	}
}

// validateOORLimitsConfig rejects OOR safety caps that would disable receive
// decoding or make one configured cap impossible to satisfy under another.
func validateOORLimitsConfig(limits *OORLimitsConfig) error {
	if limits.MaxCheckpoints == 0 {
		return fmt.Errorf("oor.limits.maxcheckpoints must be positive")
	}

	if limits.MaxVTXOMatches == 0 {
		return fmt.Errorf("oor.limits.maxvtxomatches must be positive")
	}

	if limits.MaxMailboxItems == 0 {
		return fmt.Errorf("oor.limits.maxmailboxitems must be positive")
	}

	if limits.MaxMailboxScriptBytes < minOORMailboxScriptBytes {
		return fmt.Errorf("oor.limits.maxmailboxscriptbytes must be "+
			"at least %d bytes", minOORMailboxScriptBytes)
	}

	if limits.MaxMailboxItems < limits.MaxCheckpoints {
		return fmt.Errorf("oor.limits.maxmailboxitems must be >= " +
			"oor.limits.maxcheckpoints")
	}

	if limits.MaxMailboxItems < limits.MaxVTXOMatches {
		return fmt.Errorf("oor.limits.maxmailboxitems must be >= " +
			"oor.limits.maxvtxomatches")
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
	if c.LogDirPath != "" {
		return expandTilde(c.LogDirPath)
	}

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
