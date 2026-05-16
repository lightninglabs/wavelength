package walletdk

import (
	"io"
	"time"

	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// Config controls the embedded daemon and wallet facade.
type Config struct {
	// DaemonConfig supplies the full daemon config. When nil, walletdk
	// starts from darepod.DefaultConfig and applies the convenience fields
	// below.
	DaemonConfig *darepod.Config

	// DisableSwaps starts the daemon without registering the daemon-owned
	// swap executor. Send and Receive require swaps to be enabled.
	DisableSwaps bool

	// DataDir is the root directory for daemon and wallet state.
	DataDir string

	// Network selects the bitcoin network.
	Network string

	// DebugLevel controls daemon logging verbosity.
	DebugLevel string

	// LogWriter receives daemon logs. Nil uses darepod's default stdout.
	LogWriter io.Writer

	// AllowMainnet is the tri-state mainnet-allow flag. fn.None defers to
	// DaemonConfig; fn.Some(true)/fn.Some(false) forces that value.
	AllowMainnet fn.Option[bool]

	// ServerAddress is the Ark operator mailbox edge server address.
	ServerAddress string

	// ServerTLSCertPath pins the Ark operator TLS certificate.
	ServerTLSCertPath string

	// ServerInsecure is the tri-state TLS-disable flag for the Ark
	// operator connection. fn.None defers to DaemonConfig;
	// fn.Some(true)/fn.Some(false) forces that value.
	ServerInsecure fn.Option[bool]

	// WalletType selects the backing wallet implementation.
	WalletType string

	// WalletEsploraURL is used by the lwwallet backend.
	WalletEsploraURL string

	// WalletPasswordFile enables daemon auto-unlock for lwwallet.
	WalletPasswordFile string

	// WalletPollInterval overrides the lwwallet chain poll interval.
	WalletPollInterval time.Duration

	// WalletRecoveryWindow overrides the wallet address look-ahead window.
	WalletRecoveryWindow uint32

	// WalletFeeURL is the fee estimator endpoint used by btcwallet.
	WalletFeeURL string

	// SwapServerAddress is the swapdk-server gRPC address.
	SwapServerAddress string

	// SwapServerTLSCertPath pins the swapdk-server TLS certificate.
	SwapServerTLSCertPath string

	// SwapServerInsecure is the tri-state TLS-disable flag for the swap
	// server connection. fn.None defers to DaemonConfig; fn.Some(true)/
	// fn.Some(false) forces that value.
	SwapServerInsecure fn.Option[bool]

	// SwapDatabaseFileName is the daemon-owned swap SQLite database path.
	SwapDatabaseFileName string

	// MaxOperatorFeeSat caps the per-round operator fee the daemon accepts.
	MaxOperatorFeeSat int64

	// BufferSize overrides the bufconn listener buffer size.
	BufferSize int
}

// Info summarizes daemon readiness for wallet applications.
type Info struct {
	Version         string
	Commit          string
	Network         string
	BlockHeight     uint32
	ServerConnected bool
	WalletType      string
	WalletReady     bool
	IdentityPubKey  string
}

// CreateWalletRequest creates or imports a daemon wallet.
type CreateWalletRequest struct {
	Mnemonic       []string
	SeedPassphrase []byte
	WalletPassword []byte
}

// CreateWalletResult returns the seed words and daemon identity.
type CreateWalletResult struct {
	Mnemonic       []string
	EncipheredSeed []byte
	IdentityPubKey string
}

// UnlockWalletRequest unlocks an existing daemon wallet.
type UnlockWalletRequest struct {
	WalletPassword []byte
}

// UnlockWalletResult returns the daemon identity after unlock.
type UnlockWalletResult struct {
	IdentityPubKey string
}

// Balance is the simplified wallet balance view.
type Balance struct {
	BoardingConfirmedSat      int64
	BoardingUnconfirmedSat    int64
	VTXOBalanceSat            int64
	TotalConfirmedSat         int64
	OnchainWalletConfirmedSat int64
}

// OnchainAddress is a fresh boarding address.
type OnchainAddress struct {
	Address string
}

// ReceiveRequest starts a Lightning-to-Ark receive swap.
type ReceiveRequest struct {
	AmountSat int64
}

// ReceiveResult contains the invoice and initial durable swap state.
type ReceiveResult struct {
	PaymentHash string
	Invoice     string
	Swap        SwapSummary
}

// SendRequest starts an Ark-to-Lightning payment.
type SendRequest struct {
	Invoice   string
	MaxFeeSat uint64
}

// SendResult contains the payment hash and initial durable swap state.
type SendResult struct {
	PaymentHash string
	Swap        SwapSummary
}

// ListSwapsRequest controls swap listing.
type ListSwapsRequest struct {
	PendingOnly bool
}

// GetSwapRequest fetches one swap by payment hash.
type GetSwapRequest struct {
	PaymentHash string
}

// ResumeSwapRequest wakes one pending daemon-owned swap worker.
type ResumeSwapRequest struct {
	PaymentHash string
	Direction   SwapDirection
}

// SubscribeSwapsRequest controls swap update subscriptions.
type SubscribeSwapsRequest struct {
	IncludeExisting bool
	PendingOnly     bool
}

// SwapDirection identifies the swap direction.
type SwapDirection string

const (
	// SwapDirectionPay is an Ark-to-Lightning payment.
	SwapDirectionPay SwapDirection = "pay"

	// SwapDirectionReceive is a Lightning-to-Ark receive.
	SwapDirectionReceive SwapDirection = "receive"
)

// SwapSummary is the wrapper-friendly view of one persisted swap.
type SwapSummary struct {
	Direction        SwapDirection
	PaymentHash      string
	State            string
	Pending          bool
	AmountSat        int64
	FeeSat           uint64
	MaxFeeSat        uint64
	VHTLCOutpoint    string
	VHTLCAmountSat   int64
	FundingSessionID string
	ClaimSessionID   string
	RefundSessionID  string
	TerminalReason   string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Deadline         time.Time
	RefundLocktime   uint32
}
