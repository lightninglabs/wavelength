package darepod

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/credit"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/metrics"
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

	// defaultSwapServerHost is the default address for the swap server.
	defaultSwapServerHost = "localhost:10030"

	// defaultSignetServerGRPCHost is the public signet Ark operator gRPC
	// endpoint.
	defaultSignetServerGRPCHost = "arkd-signet.staging." +
		"lightningcluster.com:443"

	// defaultSignetServerRESTHost is the public signet Ark operator REST
	// endpoint. The REST client adds the HTTPS scheme when the configured
	// host is bare.
	defaultSignetServerRESTHost = "arkd-signet-rest.staging." +
		"lightningcluster.com"

	// defaultSignetSwapServerGRPCHost is the public signet swap server gRPC
	// endpoint.
	defaultSignetSwapServerGRPCHost = "swapd-signet.staging." +
		"lightningcluster.com:443"

	// defaultSignetSwapServerRESTHost is the public signet swap server REST
	// endpoint. The REST client adds the HTTPS scheme when the configured
	// host is bare.
	defaultSignetSwapServerRESTHost = "swapd-signet-rest.staging." +
		"lightningcluster.com"

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

	// DefaultSwapRecoveryCooperativeFailureGracePeriod is how long the
	// daemon-owned swap runtime keeps retrying cooperative vHTLC settlement
	// after the first observed cooperative send failure before automatic
	// on-chain recovery may start.
	DefaultSwapRecoveryCooperativeFailureGracePeriod = time.Hour

	// DefaultSwapRecoveryMinMarginBlocks is the block-height safety margin
	// that lets receive-side claim recovery override the wall-clock grace
	// period before the sender refund locktime can make waiting unsafe.
	DefaultSwapRecoveryMinMarginBlocks = uint32(12)

	// DefaultSwapRecoveryMaxFeeRateSatPerKW caps swapruntime-armed vHTLC
	// exit spends at 100 sat/vbyte. The cap is copied into the recovery row
	// at arm time so later config changes cannot silently loosen an
	// existing job.
	DefaultSwapRecoveryMaxFeeRateSatPerKW int32 = 25_000
)

// Config holds all configuration for the darepod daemon.
type Config struct {
	// DataDir is the root data directory for all daemon state. Database
	// files, logs, and TLS material are stored under this directory.
	DataDir string `mapstructure:"datadir"`

	// Network selects the bitcoin network: mainnet, testnet, testnet4,
	// regtest, simnet, or signet.
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

	// FailUnrollBroadcastReason is a TEST-ONLY hook. When non-empty, the
	// unroll subsystem's tx-confirmation requests are rejected with this
	// reason before any broadcast, simulating a proof tx that cannot enter
	// the mempool (e.g. "min relay fee not met" on a sub-dust exit). It
	// lets integration tests reproduce the darepo-client#602 failure mode —
	// a unilateral exit that fails terminally with no on-chain footprint —
	// and verify the VTXO is recovered to live. Empty in production; not
	// serialized to config files.
	FailUnrollBroadcastReason string

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

	// UnaryServerInterceptors wrap every unary RPC handler on the daemon
	// gRPC server. Like RPCServiceRegistrars they wire compiled-in runtime
	// capabilities (such as mapping walletdkrpc sentinel errors to
	// machine-readable status codes), not user-provided daemon settings.
	UnaryServerInterceptors []grpc.UnaryServerInterceptor

	// RPCGatewayRegistrars are programmatic hooks that may register
	// optional subservers on the daemon HTTP/JSON gateway after
	// DaemonService is registered.
	RPCGatewayRegistrars []RPCGatewayRegistrar

	// WalletReadyHooks are programmatic hooks that run after the
	// wallet-derived mailbox transport and wallet-dependent actors are
	// online, but before daemon readiness is marked. They are used by
	// optional subservers that must register RPC surfaces while locked
	// but defer background work until the wallet can sign.
	WalletReadyHooks []WalletReadyHook

	// Wallet configures the wallet backend used for signing, key
	// derivation, and chain access.
	Wallet *WalletConfig `mapstructure:"wallet"`

	// ForfeitCollectionTimeout is the maximum wall-clock
	// duration to wait for forfeit signatures during a round.
	// If zero, the default of 2 minutes is used.
	//nolint:ll
	ForfeitCollectionTimeout time.Duration `mapstructure:"forfeitcollectiontimeout"`

	// RegistrationTimeout is the maximum wall-clock duration to
	// wait for the server's RoundJoined admission watermark after
	// sending a JoinRoundRequest. If zero, the round package
	// default is used; a negative value disables the timeout.
	RegistrationTimeout time.Duration `mapstructure:"registrationtimeout"`

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

	// SwapWallet configures the optional walletdkrpc subserver (the
	// simplified high-level wallet facade composed over the swap
	// runtime and the cooperative-leave subsystem). The fields are
	// inert in default builds because the WalletService is not
	// registered unless the daemon is compiled with both the
	// walletdkrpc and swapruntime tags.
	SwapWallet *SwapWalletConfig `mapstructure:"swapwallet"`

	// ActivityStore is the canonical activity-log projector the walletdkrpc
	// subserver writes to as wallet state advances. It is injected
	// programmatically by the server, never deserialized, and is a
	// top-level field (not under SwapWallet) because the subserver is
	// registered by build tag regardless of whether the operator supplied
	// a [swapwallet] config section. A nil value disables projection.
	ActivityStore ActivityStore `mapstructure:"-"`

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

	// FeeEstimation configures optional external chain fee providers for
	// the lnd wallet backend (e.g. mempool.space). Disabled by default.
	FeeEstimation *FeeEstimationConfig `mapstructure:"feeestimation"`

	// EagerRoundJoin makes the wallet actor drive round-joining
	// without waiting for a follow-up Board / LeaveVTXOs RPC. With
	// the flag on, every freshly confirmed boarding UTXO runs the
	// existing Board path inline, and cooperative-leave intents are
	// forwarded with TriggerRegistration=true so the round FSM
	// leaves PendingRoundAssembly immediately. Off keeps the
	// batched semantics that operator-driven hosts rely on
	// (darepocli, server deployments); wallet-shaped SDK hosts
	// (sdk/walletdk) get the eager behavior by default so
	// user-visible "deposit" and "exit" actions translate into a
	// full round join end-to-end.
	//
	// DefaultConfig seeds this from defaultEagerRoundJoin(), which
	// is build-tag-aware: false on the standalone non-walletdkrpc
	// build and true when darepod is compiled with the walletdkrpc
	// build tag (both the standalone cmd/darepod binary and the
	// sdk/walletdk embedded path).
	EagerRoundJoin bool `mapstructure:"eagerroundjoin"`

	// DB groups the per-backend database tuning knobs under the db.sqlite.*
	// and db.postgres.* namespaces. A value type so a zero-value Config can
	// never carry a nil sub-config into the start path.
	DB DBConfig `mapstructure:"db"`

	// Pprof configures the optional net/http/pprof debug server. It is
	// disabled by default and must be explicitly opted into via a
	// non-empty listen address. A value type so a zero-value Config can
	// never carry a nil pprof config into the start path.
	Pprof PprofConfig `mapstructure:"pprof"`

	// Metrics configures the optional Prometheus /metrics HTTP server.
	// It is disabled by default and must be explicitly opted into via a
	// non-empty listen address. A value type so a zero-value Config can
	// never carry a nil metrics config into the start path.
	Metrics metrics.ServerConfig `mapstructure:"metrics"`
}

// DBConfig groups the per-backend database tuning knobs. Only the SQLite
// knobs are wired today; the daemon always opens SQLite, so the Postgres
// namespace is reserved for a future Postgres-tuning change.
type DBConfig struct {
	// Sqlite holds the SQLite-backend durability knobs, exposed on the
	// daemon under the db.sqlite.* namespace.
	Sqlite DBSqliteConfig `mapstructure:"sqlite"`

	// Postgres is reserved for future Postgres-backend tuning knobs. It is
	// intentionally empty today: the daemon always opens SQLite, and
	// Postgres durability tuning is deferred to a separate change.
	Postgres DBPostgresConfig `mapstructure:"postgres"`
}

// DBSqliteConfig holds the SQLite-backend durability knobs exposed on the
// daemon under the db.sqlite.* namespace.
type DBSqliteConfig struct {
	// Synchronous selects the SQLite synchronous (commit durability)
	// level. One of "full", "normal", or "off"; an empty value resolves to
	// the safe default ("normal"). Under WAL mode "normal" omits the
	// per-commit WAL fsync of "full" for substantially higher write
	// throughput, replaying any tail dropped on power loss via the
	// idempotent persistence stack. See db.SqliteConfig.Synchronous.
	Synchronous string `mapstructure:"synchronous"`

	// NoFullfsync disables the SQLite fullfsync pragma. The pragma only
	// matters on macOS, where it makes flushes wait on a full hardware
	// cache flush; with the default synchronous=normal level it governs
	// the WAL checkpoint sync, which recurs continuously under sustained
	// write load. Write-heavy macOS deployments that accept the weaker
	// flush guarantee can disable it for substantially higher throughput.
	// No effect on other platforms. See db.SqliteConfig.NoFullfsync.
	NoFullfsync bool `mapstructure:"nofullfsync"`
}

// DBPostgresConfig is reserved for future Postgres-backend tuning knobs. It
// is intentionally empty: the daemon always opens SQLite, and Postgres
// durability tuning is deferred to a separate change.
type DBPostgresConfig struct{}

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

// WalletReadyHook runs once after the daemon wallet is unlocked and all
// wallet-dependent daemon services have started.
type WalletReadyHook func(ctx context.Context) error

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

// FeeEstimationConfig groups optional external chain fee providers used by the
// lnd wallet backend's fee estimator. It is namespaced separately from the
// wallet config so its mempool.space provider is not confused with the
// Esplora/mempool.space chain backend that powers the lwwallet.
type FeeEstimationConfig struct {
	// MempoolSpace configures the optional mempool.space fee provider.
	MempoolSpace *MempoolSpaceFeeConfig `mapstructure:"mempoolspace"`
}

// MempoolSpaceFeeConfig configures the optional mempool.space fee provider.
// When enabled, the lnd chain backend composes a minimum-selecting fee
// estimator over the local WalletKit estimator and a mempool.space estimator,
// choosing the lower of the two live estimates.
type MempoolSpaceFeeConfig struct {
	// Enabled turns on the mempool.space fee provider. It applies only to
	// the lnd wallet backend; the lwwallet and btcwallet backends own their
	// own fee sources.
	Enabled bool `mapstructure:"enabled"`

	// URL optionally overrides the network-default mempool.space
	// recommended-fee endpoint. It must be an absolute https URL (plaintext
	// http is rejected except for a loopback host). When empty, the
	// network-default endpoint is used.
	URL string `mapstructure:"url"`
}

// MempoolSpaceFeeEnabled reports whether the mempool.space fee provider is
// enabled. It is nil-safe so callers do not need to probe the nested config.
func (c *Config) MempoolSpaceFeeEnabled() bool {
	return c.FeeEstimation != nil &&
		c.FeeEstimation.MempoolSpace != nil &&
		c.FeeEstimation.MempoolSpace.Enabled
}

// MempoolSpaceFeeURL returns the configured mempool.space endpoint override, or
// the empty string when none is set (the network default is then used).
func (c *Config) MempoolSpaceFeeURL() string {
	if c.FeeEstimation == nil || c.FeeEstimation.MempoolSpace == nil {
		return ""
	}

	return c.FeeEstimation.MempoolSpace.URL
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
	// empty, the daemon stores swaps under NetworkDir()/swaps.db so a
	// network DB reset clears persisted swap activity with the main daemon
	// DB.
	DatabaseFileName string `mapstructure:"databasefilename"`

	// VHTLCRecovery controls when the daemon-owned swap runtime escalates
	// an already-armed vHTLC recovery row from cooperative retry into
	// on-chain unroll.
	VHTLCRecovery SwapVHTLCRecoveryConfig `mapstructure:"vhtlcrecovery"`

	// Credit configures the daemon-owned credit subsystem, chiefly the
	// wallet-owned auto-redeem policy.
	Credit CreditConfig `mapstructure:"credit"`

	// SuppressResume disables swapclientserver's own synchronous
	// resume-on-startup sweep so a higher layer (walletdkrpc subserver) can
	// own the unified resume policy. Default false preserves identical
	// behavior for swapruntime-only builds: the swap subserver continues
	// to resume its pending sessions before Register returns. The flag is
	// set programmatically by the walletdkrpc registrar; it is not loaded
	// from config files.
	SuppressResume bool `mapstructure:"-"`

	// Backend is populated by swapclientserver.Register after the swap
	// subserver is fully wired. Higher layers (the walletdkrpc subserver)
	// read this handle to drive in-Go calls into the swap runtime without
	// going through the gRPC stub. The field is set programmatically by
	// the registrar; it is never loaded from config files.
	Backend SwapBackend `mapstructure:"-"`

	// CreditServer and CreditDaemon are populated by
	// swapclientserver.Register (only under the swapruntime build tag) so
	// the daemon can construct the credit durable-actor subsystem with the
	// swap-server credit surface and the wallet/daemon surface. Both are
	// nil in builds without the swap runtime, in which case the credit
	// subsystem is not started. They are set programmatically by the
	// registrar; never loaded from config files.
	CreditServer credit.CreditServer `mapstructure:"-"`
	CreditDaemon credit.CreditDaemon `mapstructure:"-"`

	// CreditRegistry is a lazy service-key reference to the credit registry
	// actor, published by the daemon before the swap registrars run so the
	// walletdkrpc subserver can route credit-backed Send/Recv through the
	// credit subsystem. It resolves at Tell/Ask time, after the registry
	// has registered under the credit service key. Nil until the daemon
	// publishes it; set programmatically, never loaded from config files.
	CreditRegistry actor.ActorRef[credit.CreditMsg,
		credit.CreditResp] `mapstructure:"-"`

	// CreditEarmarkSetter wires the wallet's credit-earmark provider into
	// the auto-redeem policy. The daemon populates it when it builds the
	// credit registry; the walletdkrpc subserver calls it once its
	// prepared-send store exists, so the sweep never redeems credits a
	// pending credit-backed send is about to spend. Nil in builds without
	// the credit subsystem; set programmatically, never from config files.
	CreditEarmarkSetter func(credit.EarmarkFunc) `mapstructure:"-"`
}

// CreditConfig configures the daemon-owned credit subsystem.
type CreditConfig struct {
	// AutoRedeemDisabled turns off the wallet-owned auto-redeem that
	// materializes idle available credits back into a vTXO. Auto-redeem is
	// on by default; operators who prefer to manage credit redemption
	// manually set this to true.
	AutoRedeemDisabled bool `mapstructure:"autoredeemdisabled"`

	// AutoRedeemMinSat is the available-credit threshold above which a
	// settled receive triggers a redeem. Zero defaults to the operator dust
	// limit, the smallest amount that can legally become a vTXO.
	AutoRedeemMinSat uint64 `mapstructure:"autoredeemminsat"`
}

// SwapVHTLCRecoveryConfig controls automatic escalation from cooperative vHTLC
// retry to daemon-owned on-chain recovery. Arming is still immediate and cheap;
// this policy only gates the expensive unroll transition.
type SwapVHTLCRecoveryConfig struct {
	// AutoEscalate allows the daemon-owned swap runtime to start on-chain
	// recovery without a manual command once the grace/deadline policy says
	// cooperative retry is no longer safe or useful.
	AutoEscalate bool `mapstructure:"autoescalate"`

	// CooperativeFailureGracePeriod is measured from the first cooperative
	// vHTLC send failure. While the period is open, the swap runtime keeps
	// retrying cooperative settlement unless deadline pressure overrides
	// the wait.
	CooperativeFailureGracePeriod time.Duration `mapstructure:"cooperativefailuregraceperiod"` //nolint:ll

	// MinRecoveryMarginBlocks is the minimum block margin preserved before
	// a refund locktime. Receive-side claim recovery may override the grace
	// period when the current height plus this margin reaches the refund
	// locktime.
	MinRecoveryMarginBlocks uint32 `mapstructure:"minrecoverymarginblocks"`

	// MaxFeeRateSatPerKW caps the final vHTLC recovery exit-spend fee rate.
	MaxFeeRateSatPerKW int32 `mapstructure:"maxfeeratesatperkw"`
}

// WithDefaults fills unset numeric vHTLC recovery fields with production
// defaults while preserving AutoEscalate. This lets operators set
// autoescalate=false without that explicit manual-recovery mode being rewritten
// back to the default automatic policy.
func (c SwapVHTLCRecoveryConfig) WithDefaults() SwapVHTLCRecoveryConfig {
	if c.MinRecoveryMarginBlocks == 0 {
		c.MinRecoveryMarginBlocks = DefaultSwapRecoveryMinMarginBlocks
	}
	if c.MaxFeeRateSatPerKW == 0 {
		c.MaxFeeRateSatPerKW = DefaultSwapRecoveryMaxFeeRateSatPerKW
	}

	return c
}

// SwapBackend is the in-Go handle exposed by swapclientserver after Register
// completes. It lets higher-level subservers (such as the walletdkrpc
// subserver) drive the swap runtime without dialing the daemon's gRPC server
// from inside the same process. The interface is intentionally small and grows
// only as new wallet-layer needs arise.
type SwapBackend interface {
	// ResumePending re-arms background workers for every persisted
	// pending swap session. It is idempotent: payment hashes already
	// owned by an active worker are skipped. Callers invoke it once
	// when the active resume policy is allowed to start background
	// workers.
	ResumePending(ctx context.Context)
}

// ActivityStore is the walletdkrpc subserver's handle on the canonical
// activity log. *db.ActivityPersistenceStore satisfies it; the projector
// writes through ProjectEntry from the emit sites and the startup backfill,
// and the List read path pages current-state rows through ListEntries. The
// interface keeps the daemon-side store out of the swapwallet build-tag
// boundary and lets tests pass nil.
type ActivityStore interface {
	// ProjectEntry advances the activity row to the projected state and
	// records the transition, atomically. It returns the event_seq assigned
	// to the appended transition, or 0 when the projection was
	// change-suppressed (no transition, so nothing to emit).
	ProjectEntry(ctx context.Context,
		p db.ActivityProjection) (int64, error)

	// ListEntries returns up to limit current-state rows newest-first,
	// starting after the (cursorCreated, cursorID) keyset. A cursorCreated
	// of 0 starts from the newest row.
	ListEntries(ctx context.Context, cursorCreated int64, cursorID string,
		limit int32) ([]sqlc.ActivityEntry, error)

	// ListEntriesByKindStatus returns up to limit rows of the given kind
	// and status, paged by canonical_id ascending after cursorID. It backs
	// the startup rehydration of the wallet-local pending map, scanning
	// only the matching rows rather than decoding the whole feed.
	ListEntriesByKindStatus(ctx context.Context, kind, status int64,
		cursorID string, limit int32) ([]sqlc.ActivityEntry, error)

	// PullEvents returns up to limit append-only transition rows with
	// event_seq strictly greater than cursor, oldest-first. It is the
	// resumable-subscribe replay primitive: a reconnecting client passes
	// the last event_seq it processed and receives everything after it.
	PullEvents(ctx context.Context, cursor int64,
		limit int32) ([]sqlc.ActivityEvent, error)

	// CountByStatus returns the number of current-state rows in the given
	// status. It backs the wallet status summary's full-feed pending count,
	// which the paginated List path cannot report.
	CountByStatus(ctx context.Context, status int64) (int64, error)
}

// SwapWalletConfig configures the optional walletdkrpc subserver. The struct
// is present in all builds so configuration files stay stable, but the
// fields are only consumed when the daemon is compiled with both the
// walletdkrpc and swapruntime build tags.
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
// serverconn runtime. The base client already carries the daemon's configured
// operator RPC auth, so wrappers must delegate to it rather than rebuilding a
// client from the raw connection.
type MailboxEdgeFactory func(
	conn grpc.ClientConnInterface, base mailboxpb.MailboxServiceClient,
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

	// MacaroonPath is the path to the operator macaroon used for
	// outbound ArkService and MailboxService requests.
	MacaroonPath string `mapstructure:"macaroonpath"`

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

	// NoTLS disables TLS on the daemon RPC listener. This should only be
	// used by explicit local development or injected-listener paths.
	NoTLS bool `mapstructure:"notls"`

	// MacaroonPath is the path to the daemon RPC macaroon. If empty, one
	// is auto-generated under the network data directory.
	MacaroonPath string `mapstructure:"macaroonpath"`

	// NoMacaroons disables daemon RPC macaroon authentication. This should
	// only be used by explicit local development or injected-listener
	// paths.
	NoMacaroons bool `mapstructure:"no-macaroons"`
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
	// Use "*" to allow browser requests from any origin.
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

// PprofConfig configures the optional net/http/pprof debug server.
//
// SECURITY: pprof exposes sensitive runtime and debug data — goroutine
// stacks, heap and CPU profiles, the command line, and the symbol table —
// any of which can leak internal state or enable denial-of-service. It is
// opt-in (disabled by default) and is served on its own private HTTP mux
// that is never attached to the daemon's gRPC/gateway listeners. Operators
// who enable it should bind ListenAddr to a loopback or firewalled address
// (e.g. "127.0.0.1:6060") and never expose it to untrusted networks.
type PprofConfig struct {
	// ListenAddr is the network address the pprof HTTP server binds to.
	// An empty value (the default) disables pprof entirely. A non-empty
	// value such as "127.0.0.1:6060" starts a dedicated HTTP server
	// exposing the standard net/http/pprof endpoints.
	ListenAddr string `mapstructure:"listen"`

	// BlockProfileRate, when greater than zero, is passed to
	// runtime.SetBlockProfileRate at startup to enable blocking-profile
	// collection. Zero (the default) leaves block profiling disabled.
	BlockProfileRate int `mapstructure:"blockprofilerate"`

	// MutexProfileFraction, when greater than zero, is passed to
	// runtime.SetMutexProfileFraction at startup to enable mutex-contention
	// profiling. Zero (the default) leaves mutex profiling disabled.
	MutexProfileFraction int `mapstructure:"mutexprofilefraction"`
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
	// an existing wallet database, the daemon unlocks the wallet
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

	// BtcwBlockSource is a local file path or HTTP(S)
	// URL that neutrino imports block headers from on startup.
	// Only used when Type is "btcwallet".
	BtcwBlockSource string `mapstructure:"btcwallet_blockheaderssource"`

	// BtcwFilterSource is a local file path or HTTP(S)
	// URL that neutrino imports compact filter headers from on
	// startup. Only used when Type is "btcwallet".
	BtcwFilterSource string `mapstructure:"btcwallet_filterheaderssource"`

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
	swapRecoveryGrace := DefaultSwapRecoveryCooperativeFailureGracePeriod
	swapRecoveryMargin := DefaultSwapRecoveryMinMarginBlocks
	swapRecoveryFeeCap := DefaultSwapRecoveryMaxFeeRateSatPerKW
	swapRecovery := SwapVHTLCRecoveryConfig{
		AutoEscalate:                  false,
		CooperativeFailureGracePeriod: swapRecoveryGrace,
		MinRecoveryMarginBlocks:       swapRecoveryMargin,
		MaxFeeRateSatPerKW:            swapRecoveryFeeCap,
	}

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
			ServerAddress:   defaultSwapServerHost,
			ServerTransport: RPCTransportGRPC,
			VHTLCRecovery:   swapRecovery,
		},
		MaxOperatorFeeSat: DefaultMaxOperatorFeeSat,
		OOR:               defaultOORConfig(),
		FeeEstimation: &FeeEstimationConfig{
			MempoolSpace: &MempoolSpaceFeeConfig{},
		},
		EagerRoundJoin: defaultEagerRoundJoin(),
	}
}

// Validate checks the config for internal consistency and returns an error
// if any required fields are missing or invalid.
func (c *Config) Validate() error {
	if err := c.expandPaths(); err != nil {
		return err
	}

	switch c.Network {
	case "mainnet", "testnet", "testnet4", "regtest", "simnet", "signet":
	default:
		return fmt.Errorf("unknown network %q", c.Network)
	}

	c.applyNetworkEndpointDefaults()

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
		err := c.Wallet.validateBtcwalletHeadersImportConfig()
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown wallet type %q, valid values: lnd, "+
			"lwwallet, btcwallet", c.Wallet.Type)
	}

	// The mempool.space fee provider only composes with the lnd backend's
	// fee estimator; the lwwallet and btcwallet backends own their own fee
	// sources, so enabling it there would silently do nothing.
	if c.MempoolSpaceFeeEnabled() && c.Wallet.Type != WalletTypeLnd {
		return fmt.Errorf("feeestimation.mempoolspace is only "+
			"supported with wallet.type lnd, not %q", c.Wallet.Type)
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
	if err := c.validateRPCSecurity(); err != nil {
		return err
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
	if c.Swap != nil {
		c.Swap.VHTLCRecovery = c.Swap.VHTLCRecovery.WithDefaults()
		recoveryPolicy := c.Swap.VHTLCRecovery
		gracePeriod := recoveryPolicy.CooperativeFailureGracePeriod
		if gracePeriod < 0 {
			return fmt.Errorf("swap vhtlc recovery cooperative " +
				"failure grace period must be non-negative")
		}
		if recoveryPolicy.AutoEscalate && gracePeriod == 0 {
			return fmt.Errorf("swap vhtlc recovery cooperative " +
				"failure grace period must be positive when " +
				"auto escalation is enabled")
		}
		if c.Swap.VHTLCRecovery.MaxFeeRateSatPerKW <= 0 {
			return fmt.Errorf("swap vhtlc recovery max fee rate " +
				"must be positive")
		}
	}

	return nil
}

// applyNetworkEndpointDefaults replaces the local development endpoints with
// the public signet deployment when the caller selects signet without
// configuring an operator or swap server. An insecure connection or a pinned
// certificate keeps the local value intact, since either setting indicates an
// intentional development override.
func (c *Config) applyNetworkEndpointDefaults() {
	if c.Network != "signet" {
		return
	}

	if c.Server != nil && c.Server.Host == DefaultServerHost &&
		!c.Server.Insecure && c.Server.TLSCertPath == "" {

		switch c.Server.Transport {
		case "", RPCTransportGRPC:
			c.Server.Host = defaultSignetServerGRPCHost

		case RPCTransportREST:
			c.Server.Host = defaultSignetServerRESTHost
		}
	}

	if c.Swap != nil && c.Swap.ServerAddress == defaultSwapServerHost &&
		!c.Swap.ServerInsecure && c.Swap.ServerTLSCertPath == "" {

		switch c.Swap.ServerTransport {
		case "", RPCTransportGRPC:
			c.Swap.ServerAddress = defaultSignetSwapServerGRPCHost

		case RPCTransportREST:
			c.Swap.ServerAddress = defaultSignetSwapServerRESTHost
		}
	}
}

// validateBtcwalletHeadersImportConfig checks that header import sources are
// configured as the pair that neutrino requires.
func (w *WalletConfig) validateBtcwalletHeadersImportConfig() error {
	blockSet := w.BtcwBlockSource != ""
	filterSet := w.BtcwFilterSource != ""

	if blockSet != filterSet {
		return fmt.Errorf("both wallet.btcwallet_blockheaderssource " +
			"and wallet.btcwallet_filterheaderssource must be " +
			"specified together for headers import")
	}

	return nil
}

// validateGatewayAllowedOrigins rejects empty CORS origins. The wildcard
// origin "*" is allowed for public browser gateways whose RPCs authenticate
// explicitly per request rather than through ambient browser credentials.
func validateGatewayAllowedOrigins(origins []string) error {
	for _, origin := range origins {
		if strings.TrimSpace(origin) == "" {
			return fmt.Errorf("rpc.gateway.allowedorigins must "+
				"contain explicit origins or '*', got %q",
				origin)
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

	case "testnet4":
		return &chaincfg.TestNet4Params, nil

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
// /home/user/.darepod/data/regtest). Path fields are normalized by
// Validate via expandPaths, so callers must validate before reaching
// this helper.
func (c *Config) NetworkDir() string {
	return filepath.Join(c.DataDir, "data", c.Network)
}

// LogDir returns the network-scoped log directory. Path fields are
// normalized by Validate via expandPaths, so callers must validate
// before reaching this helper.
func (c *Config) LogDir() string {
	if c.LogDirPath != "" {
		return c.LogDirPath
	}

	return filepath.Join(c.DataDir, "logs", c.Network)
}

// rpcTLSCertPath returns the default daemon RPC TLS certificate path.
func (c *Config) rpcTLSCertPath() string {
	return filepath.Join(c.NetworkDir(), "tls.cert")
}

// rpcTLSKeyPath returns the default daemon RPC TLS key path.
func (c *Config) rpcTLSKeyPath() string {
	return filepath.Join(c.NetworkDir(), "tls.key")
}

// rpcMacaroonPath returns the default daemon RPC macaroon path.
func (c *Config) rpcMacaroonPath() string {
	return filepath.Join(c.NetworkDir(), "admin.macaroon")
}

// validateRPCSecurity normalizes daemon RPC TLS and macaroon paths.
func (c *Config) validateRPCSecurity() error {
	if c.Network == "mainnet" && c.RPC.Listener == nil {
		if c.RPC.NoTLS {
			return fmt.Errorf("rpc.notls cannot be used on " +
				"mainnet TCP listeners")
		}
		if c.RPC.NoMacaroons {
			return fmt.Errorf("rpc.no-macaroons cannot be used " +
				"on mainnet TCP listeners")
		}
	}

	if !c.RPC.NoTLS {
		switch {
		case c.RPC.TLSCertPath == "" && c.RPC.TLSKeyPath == "":
			c.RPC.TLSCertPath = c.rpcTLSCertPath()
			c.RPC.TLSKeyPath = c.rpcTLSKeyPath()

		case c.RPC.TLSCertPath == "" || c.RPC.TLSKeyPath == "":
			return fmt.Errorf("rpc.tlscertpath and " +
				"rpc.tlskeypath must be set together")
		}
	}

	if !c.RPC.NoMacaroons && c.RPC.MacaroonPath == "" {
		c.RPC.MacaroonPath = c.rpcMacaroonPath()
	}

	return nil
}

// expandPaths normalizes filesystem path fields by expanding a leading tilde to
// the user's home directory. expandTilde is a no-op on empty strings and on
// paths that don't start with "~", so URL-or-path fields (e.g. BtcwBlockSource,
// BtcwFilterSource) pass through unchanged.
func (c *Config) expandPaths() error {
	fields := []*string{
		&c.DataDir,
		&c.LogDirPath,
	}

	if c.Lnd != nil {
		fields = append(fields,
			&c.Lnd.TLSPath, &c.Lnd.MacaroonPath,
		)
	}

	if c.Server != nil {
		fields = append(
			fields, &c.Server.TLSCertPath, &c.Server.MacaroonPath,
		)
	}

	if c.RPC != nil {
		fields = append(
			fields, &c.RPC.TLSCertPath, &c.RPC.TLSKeyPath,
			&c.RPC.MacaroonPath,
		)
	}

	if c.Wallet != nil {
		fields = append(
			fields, &c.Wallet.PasswordFile,
			&c.Wallet.BtcwalletDataDir, &c.Wallet.BtcwBlockSource,
			&c.Wallet.BtcwFilterSource,
		)
	}

	if c.Swap != nil {
		fields = append(
			fields, &c.Swap.DatabaseFileName,
			&c.Swap.ServerTLSCertPath,
		)
	}

	for _, p := range fields {
		expanded, err := expandTilde(*p)
		if err != nil {
			return fmt.Errorf("expand path %q: %w", *p, err)
		}
		*p = expanded
	}

	return nil
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
