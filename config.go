package darepo

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/mailbox"
	"github.com/lightninglabs/darepo/metrics"
	"github.com/lightninglabs/darepo/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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

	// DefaultRPCGatewayListen is the default listen address for the
	// client-facing HTTP/JSON gateway.
	DefaultRPCGatewayListen = "localhost:7071"

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

// GatewayConfig contains configuration for an HTTP/JSON grpc-gateway
// listener.
type GatewayConfig struct {
	// Enabled controls whether the HTTP gateway starts with its
	// owning gRPC server.
	Enabled bool `mapstructure:"enabled"`

	// ListenAddr is the network address the gateway binds to.
	ListenAddr string `mapstructure:"listen"`

	// AllowedOrigins lists browser origins that may call the gateway.
	// Empty means browser requests fail closed while non-browser
	// requests without an Origin header continue to work.
	AllowedOrigins []string `mapstructure:"allowedorigins"`

	// Listener is an optional pre-created listener. When non-nil,
	// the gateway serves on this listener instead of binding to
	// ListenAddr. This is programmatic-only and is not loaded from
	// config files.
	Listener net.Listener
}

// DefaultGatewayConfig returns an enabled HTTP gateway config.
func DefaultGatewayConfig(listenAddr string) *GatewayConfig {
	return &GatewayConfig{
		Enabled:    true,
		ListenAddr: listenAddr,
	}
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

	// ConnectorTreeRadix is the branching factor for connector trees.
	// It is independent of TreeRadix because the two trees optimize
	// for different objectives: VTXO trees trade off client
	// unilateral-exit cost, whereas connector trees are
	// operator-owned and the depth of a connector path bounds the
	// number of serial confirmations the operator must publish
	// during fraud response before the stored forfeit transaction
	// can confirm.
	ConnectorTreeRadix uint32 `mapstructure:"connectortreeradix"`

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

	// RoundTickInterval is the cadence at which the round actor
	// checks if the current round should be sealed. On each fire the
	// round FSM evaluates participants + the configured seal
	// predicate; an empty round (no clients joined) is a no-op.
	// Zero disables periodic ticks (event-driven only).
	//
	// Distinct from RegistrationTimeout: the registration timeout is
	// scheduled on the first client join and unconditionally seals
	// when it fires. The tick is scheduled at round creation, fires
	// repeatedly, and only seals if registrations clear the seal
	// predicate. Both can coexist; whichever fires first wins.
	RoundTickInterval time.Duration `mapstructure:"roundtickinterval"`
}

// DefaultRoundsConfig returns a RoundsConfig with sensible defaults
// suitable for development and regtest.
func DefaultRoundsConfig() *RoundsConfig {
	return &RoundsConfig{
		SweepDelay:                    1008,
		MaxVTXOsPerTree:               128,
		TreeRadix:                     2,
		MaxConnectorsPerTree:          32,
		ConnectorTreeRadix:            4,
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
		RoundTickInterval:             time.Minute,
	}
}

// MailboxConfig controls safety limits for the in-process mailbox
// transport shared by the client-facing mailbox RPC and the internal
// per-client bridge.
type MailboxConfig struct {
	// MaxEnvelopeBytes is the maximum protobuf-encoded envelope size
	// accepted by the mailbox store. Zero disables the limit.
	MaxEnvelopeBytes int `mapstructure:"maxenvelopebytes"`

	// MaxEnvelopesPerMailbox is the maximum number of outstanding
	// (unacked) envelopes retained per mailbox. Zero disables the
	// limit.
	MaxEnvelopesPerMailbox int `mapstructure:"maxenvelopespermailbox"`

	// RequireTLSBindingSig, when true, makes the TLS-leaf-to-
	// mailbox-key binding signature (header
	// x-mailbox-tls-bind-sig) mandatory on first-contact Send:
	// envelopes that lack it, or whose binding does not verify
	// against the observed TLS leaf, are rejected and no
	// fingerprint binding is recorded.
	//
	// Default true: clients must prove that their mailbox key
	// controls the TLS leaf used on the first-contact Send.
	// Operators that still need to accept pre-#448 clients can
	// temporarily set this to false during their upgrade window.
	RequireTLSBindingSig bool `mapstructure:"requiretlsbindingsig"`
}

// DefaultMailboxConfig returns a MailboxConfig with quota limits disabled and
// TLS binding signatures required by default.
func DefaultMailboxConfig() *MailboxConfig {
	return &MailboxConfig{
		RequireTLSBindingSig: true,
	}
}

// FeesConfig holds operator fee schedule parameters that can be
// updated at runtime via the admin RPC. These values seed the
// initial fee schedule; subsequent changes are applied via
// UpdateFeeSchedule without a restart.
type FeesConfig struct {
	// AnnualRate is the annualized BTC-denominated cost of
	// capital (e.g. 0.05 for 5%).
	AnnualRate float64 `mapstructure:"annualrate"`

	// BaseMarginSat is the fixed operator margin in satoshis
	// per liquidity-requiring operation.
	BaseMarginSat int64 `mapstructure:"basemarginsat"`

	// UtilizationThresholdBPS is the treasury utilization
	// level (basis points, e.g. 7000 = 70%) above which the
	// congestion spread activates.
	UtilizationThresholdBPS uint32 `mapstructure:"utilthresholdbps"`

	// UtilizationSpreadDelta0BPS is the base congestion
	// spread (basis points) added to AnnualRate when
	// utilization exceeds the threshold.
	UtilizationSpreadDelta0BPS uint32 `mapstructure:"utilspreaddelta0bps"`

	// UtilizationSpreadDelta1BPS is the linear congestion
	// spread coefficient (basis points per unit utilization
	// above threshold).
	UtilizationSpreadDelta1BPS uint32 `mapstructure:"utilspreaddelta1bps"`

	// MinViableVTXOPolicy controls dust enforcement: "reject"
	// rejects VTXOs below the viability threshold, "warn"
	// accepts but flags them.
	MinViableVTXOPolicy string `mapstructure:"minviablepolicy"`

	// MinViableVTXOPct is the max fee-to-amount ratio (%) that
	// defines economic viability. VTXOs where fee exceeds this
	// fraction trigger the dust policy.
	MinViableVTXOPct uint32 `mapstructure:"minviablepct"`

	// MinRefreshDeltaBlocks is the minimum block-delta floor used
	// when computing the liquidity-fee component of a refresh. A
	// refresh whose VTXO still has more than this many blocks of
	// remaining time pays liquidity on the actual delta; a refresh
	// with less remaining time pays liquidity on this floor. Zero
	// disables the floor (production default: 144).
	MinRefreshDeltaBlocks uint32 `mapstructure:"minrefreshdeltablocks"`

	// StaticFeeRateSatKW, when non-zero, installs a static fee
	// estimator at this rate (sat/kW) instead of the chain-backed
	// WalletKit estimator. The SatKW suffix is explicit so an
	// operator coming from a sat/vB mental model does not assume
	// the knob is in sat/vB and misconfigure by ~4x. The harness +
	// systest pin this to FeePerKwFloor so their rounds stay
	// deterministic under regtest mempool conditions; production
	// leaves this at zero so the chain-backed estimator from
	// lndbackend is used.
	StaticFeeRateSatKW int64 `mapstructure:"staticfeeratesatkw"`
}

// DefaultFeesConfig returns a FeesConfig with sensible defaults
// suitable for development and regtest.
func DefaultFeesConfig() *FeesConfig {
	return &FeesConfig{
		AnnualRate:                 0.05,
		BaseMarginSat:              100,
		UtilizationThresholdBPS:    7000,
		UtilizationSpreadDelta0BPS: 100,
		UtilizationSpreadDelta1BPS: 500,
		MinViableVTXOPolicy:        "reject",
		MinViableVTXOPct:           50,
		MinRefreshDeltaBlocks:      144,
		StaticFeeRateSatKW:         0,
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

	// Bitcoind configures the operator's direct bitcoind RPC
	// connection. The server uses this both for boarding UTXO
	// validation and for the v3/TRUC package relay required by fraud
	// response broadcasts.
	Bitcoind *BitcoindConfig `mapstructure:"bitcoind"`

	// AdminRPC contains the admin RPC server configuration.
	AdminRPC *AdminRPCConfig `mapstructure:"adminrpc"`

	// RPC contains the client-facing RPC server configuration.
	RPC *RPCConfig `mapstructure:"rpc"`

	// Rounds configures the round subsystem policy (tree shape,
	// timeouts, confirmation targets).
	Rounds *RoundsConfig `mapstructure:"rounds"`

	// Mailbox configures the in-process mailbox transport limits.
	Mailbox *MailboxConfig `mapstructure:"mailbox"`

	// Metrics configures the Prometheus metrics HTTP server.
	// When nil, the metrics endpoint is disabled.
	Metrics *metrics.ServerConfig `mapstructure:"metrics"`

	// Fees configures the operator fee schedule (annual rate,
	// margins, utilization-based congestion pricing, dust
	// policy). These values seed the initial schedule and can
	// be updated at runtime via the admin RPC.
	Fees *FeesConfig `mapstructure:"fees"`

	// MaxOORLineageVBytes caps the cumulative on-chain virtual
	// bytes the operator will accept across the resolved input
	// lineage of an OOR submit. Submits whose summed unique-by-txid
	// signed-tx vbytes exceed this cap are rejected with the typed
	// OOR_REJECT_LINEAGE_TOO_LARGE code. Zero disables the check
	// entirely. The cap is a starting hard threshold; future work
	// replaces it with a metered fee schedule.
	MaxOORLineageVBytes uint32 `mapstructure:"maxoorlineagevbytes"`

	// PackageSubmitter is the runtime v3/TRUC package relay submitter
	// wired into the operator's chain backend. Fraud response broadcasts
	// OOR checkpoints and timeout sweeps as zero-fee parents with CPFP
	// children, so production wires this from bitcoind RPC while
	// integration tests inject the same submitter from the harness. Not
	// serialized to config files.
	PackageSubmitter chainbackends.PackageSubmitter

	// Fraud configures the fraud-response subsystem. When nil, defaults
	// from DefaultFraudConfig() are used.
	Fraud *FraudConfig `mapstructure:"fraud"`

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

// FraudConfig holds tunables for the fraud-response subsystem.
//
// The fee-rate cap matters in adversarial conditions: a fraud event is
// timed by the attacker, who can choose to act during a fee spike. The
// operator's CPFP child must escalate to whatever feerate clears the
// mempool race; a low cap loses the race and the recipient loses funds.
// This is operator policy, not protocol — surface it as config so a
// deployment can raise the ceiling for high-value contexts.
type FraudConfig struct {
	// MaxResponseFeeRateSatPerVByte caps the CPFP child fee rate
	// txconfirm will pay when broadcasting a fraud-response package
	// (checkpoint or timeout sweep). A value of 0 falls back to
	// DefaultFraudMaxResponseFeeRate.
	MaxResponseFeeRateSatPerVByte int64 `mapstructure:"maxresponsefeerate"`

	// Disabled skips the operator fraud-response actor entirely. The
	// batchwatcher will not notify any fraud detector, so the operator
	// will not react to on-chain fraud events. Setting this to true on
	// a production deployment loses user funds when an OOR fraud event
	// occurs: only meaningful in non-mainnet test environments that
	// exercise client-side recipient fraud recovery without operator
	// interference. Validate() refuses the flag on mainnet.
	Disabled bool `mapstructure:"disabled"`
}

// DefaultFraudMaxResponseFeeRate is the default cap on the CPFP child fee
// rate for fraud-response broadcasts. 100 sat/vB clears most non-spike
// conditions on mainnet; operators in adversarial environments should
// override.
const DefaultFraudMaxResponseFeeRate int64 = 100

// DefaultFraudConfig returns the default FraudConfig.
func DefaultFraudConfig() *FraudConfig {
	return &FraudConfig{
		MaxResponseFeeRateSatPerVByte: DefaultFraudMaxResponseFeeRate,
	}
}

// MaxResponseFeeRate returns the configured cap, falling back to
// DefaultFraudMaxResponseFeeRate when c is nil or unset.
func (c *FraudConfig) MaxResponseFeeRate() int64 {
	if c == nil || c.MaxResponseFeeRateSatPerVByte == 0 {
		return DefaultFraudMaxResponseFeeRate
	}

	return c.MaxResponseFeeRateSatPerVByte
}

// BitcoindConfig holds connection parameters for the operator's direct
// bitcoind RPC connection. The operator uses this connection for package
// relay during fraud response and for direct boarding UTXO validation.
type BitcoindConfig struct {
	// Host is the bitcoind RPC address (host:port).
	Host string `mapstructure:"host"`

	// User is the RPC username. Ignored when CookiePath is set.
	User string `mapstructure:"user"`

	// Pass is the RPC password. Ignored when CookiePath is set.
	Pass string `mapstructure:"pass"`

	// CookiePath is the optional path to bitcoind's cookie auth file
	// (typically `<datadir>/.cookie`). When set, the cookie file is
	// re-read on each daemon start to derive the RPC user and password,
	// taking precedence over User/Pass. This matches Bitcoin Core's
	// preferred local-auth mechanism and avoids hard-coding plaintext
	// credentials in arkd's config.
	CookiePath string `mapstructure:"cookiepath"`
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
		AdminRPC:            DefaultAdminRPCConfig(),
		RPC:                 DefaultRPCConfig(),
		Rounds:              DefaultRoundsConfig(),
		Mailbox:             DefaultMailboxConfig(),
		Metrics:             metrics.DefaultServerConfig(),
		Fees:                DefaultFeesConfig(),
		MaxOORLineageVBytes: oor.DefaultMaxOORLineageVBytes,
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
	if c.RPC.Gateway == nil {
		return fmt.Errorf("rpc gateway config is required")
	}
	if c.RPC.Gateway.Enabled && c.RPC.Gateway.Listener == nil &&
		c.RPC.Gateway.ListenAddr == "" {
		return fmt.Errorf("rpc gateway listen address or injected " +
			"listener is required")
	}
	if err := validateGatewayAllowedOrigins(
		"rpc.gateway.allowedorigins", c.RPC.Gateway.AllowedOrigins,
	); err != nil {
		return err
	}
	if c.Rounds == nil {
		return fmt.Errorf("rounds config is required")
	}
	if c.Mailbox == nil {
		return fmt.Errorf("mailbox config is required")
	}
	if c.Mailbox.MaxEnvelopeBytes < 0 {
		return fmt.Errorf("mailbox max envelope bytes must be >= 0")
	}
	if c.Mailbox.MaxEnvelopesPerMailbox < 0 {
		return fmt.Errorf("mailbox max envelopes per mailbox must be " +
			">= 0")
	}
	if c.Rounds.ConnectorDustAmount <= 0 {
		return fmt.Errorf("rounds connector dust amount must be > 0")
	}

	// fraud.disabled silently leaves the operator with no on-chain
	// fraud-response defense, which loses user funds the first time a
	// client cheats. Permitted only on non-mainnet so itests can
	// exercise client-side recovery paths without operator
	// interference; refuse it on mainnet so a typo or lifted-from-
	// regtest config cannot ship a defenseless production daemon.
	if c.Fraud != nil && c.Fraud.Disabled && c.Network == "mainnet" {
		return fmt.Errorf("fraud.disabled is not permitted on " +
			"mainnet; see FraudConfig.Disabled docs")
	}

	// Validate the fees.staticfeeratesatkw override if the operator
	// pinned one. Zero means "use the chain-backed estimator" and
	// needs no check. Negative is always a misconfiguration.
	// A positive value below FeePerKwFloor would install a static
	// estimator quoting below the bitcoin relay fee floor, so rounds
	// would build transactions the network would not even propagate.
	// A value above the sanity ceiling almost always indicates a
	// unit confusion (sat/vB typed as sat/kW, or sat-per-tx instead
	// of sat-per-kw). Reject loud rather than have the operator
	// quote 1000x real fees.
	if c.Fees != nil && c.Fees.StaticFeeRateSatKW != 0 {
		if c.Fees.StaticFeeRateSatKW < 0 {
			return fmt.Errorf("fees.staticfeeratesatkw must be "+
				"non-negative, got %d",
				c.Fees.StaticFeeRateSatKW)
		}

		floor := int64(chainfee.FeePerKwFloor)
		if c.Fees.StaticFeeRateSatKW < floor {
			return fmt.Errorf("fees.staticfeeratesatkw %d sat/kW "+
				"is below the bitcoin relay fee floor "+
				"%d sat/kW", c.Fees.StaticFeeRateSatKW, floor)
		}

		// 10_000_000 sat/kW = 10_000 sat/vB; any operator who
		// genuinely needs to pin above that is probably using the
		// wrong knob, but the ceiling is loose enough not to
		// trip honest users in extreme mempool conditions.
		const maxStaticFeeRateSatKW = int64(10_000_000)
		if c.Fees.StaticFeeRateSatKW > maxStaticFeeRateSatKW {
			return fmt.Errorf("fees.staticfeeratesatkw %d sat/kW "+
				"exceeds sanity ceiling %d sat/kW; check unit "+
				"(sat/kW vs sat/vB)", c.Fees.StaticFeeRateSatKW,
				maxStaticFeeRateSatKW)
		}
	}

	// When --rpc.notls is set, explicitly disable TLS regardless
	// of what viper populated in the TLS sub-struct.
	if c.RPC.NoTLS {
		c.RPC.TLS = nil
	}

	// Validate TLS config: if a cert path is set, a key path is
	// required, and vice versa. Viper may populate the TLS sub-struct
	// with zero values when flags are registered but not set.
	if c.RPC.TLS != nil {
		tls := c.RPC.TLS
		hasTLS := tls.CertPath != "" || tls.KeyPath != "" ||
			tls.AutoCert

		if !hasTLS {
			return fmt.Errorf("no TLS config provided for client " +
				"RPC; set --rpc.tls.autocert, provide " +
				"cert/key paths, or pass --rpc.notls")
		}

		if tls.CertPath != "" && tls.KeyPath == "" {
			return fmt.Errorf("rpc.tls.keypath is required when " +
				"rpc.tls.certpath is set")
		}
		if tls.KeyPath != "" && tls.CertPath == "" {
			return fmt.Errorf("rpc.tls.certpath is required when " +
				"rpc.tls.keypath is set")
		}
	}

	return nil
}

// validateGatewayAllowedOrigins rejects wildcard CORS grants on wallet-control
// gateways.
func validateGatewayAllowedOrigins(name string, origins []string) error {
	for _, origin := range origins {
		if strings.TrimSpace(origin) == "" || origin == "*" {
			return fmt.Errorf("%s must list explicit "+
				"trusted origins", name)
		}
	}

	return nil
}

// ValidatePackageRelay checks runtime-only package relay wiring.
//
// PackageSubmitter is not serialized into config files: cmd/arkd builds it
// after viper hydrates the Bitcoind config, while itests inject it directly
// through the harness. Keep this separate from Validate so pure config tests
// can still validate file-backed fields without constructing an RPC client.
func (c *Config) ValidatePackageRelay() error {
	if c.PackageSubmitter != nil {
		return nil
	}

	return fmt.Errorf("bitcoind package relay is required for fraud " +
		"response; set bitcoind.host, bitcoind.user, and bitcoind.pass")
}

// mailboxStoreOptions derives mailbox store options from the
// current daemon config, omitting disabled limits.
func (c *Config) mailboxStoreOptions() []mailbox.StoreOption {
	if c == nil || c.Mailbox == nil {
		return nil
	}

	opts := make([]mailbox.StoreOption, 0, 2)
	if c.Mailbox.MaxEnvelopeBytes > 0 {
		opts = append(
			opts, mailbox.WithMaxEnvelopeBytes(
				c.Mailbox.MaxEnvelopeBytes,
			),
		)
	}
	if c.Mailbox.MaxEnvelopesPerMailbox > 0 {
		opts = append(
			opts, mailbox.WithMaxEnvelopesPerMailbox(
				c.Mailbox.MaxEnvelopesPerMailbox,
			),
		)
	}

	return opts
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
