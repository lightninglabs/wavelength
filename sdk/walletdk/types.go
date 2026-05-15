package walletdk

import (
	"io"
	"time"

	"github.com/lightninglabs/darepo-client/darepod"
	"google.golang.org/grpc"
)

// Config controls the embedded daemon and wallet facade.
type Config struct {
	// DaemonConfig supplies the full daemon config. When nil, walletdk
	// starts from darepod.DefaultConfig and applies the convenience fields
	// below.
	DaemonConfig *darepod.Config

	// DataDir is the root directory for daemon and wallet state.
	DataDir string

	// Network selects the bitcoin network.
	Network string

	// DebugLevel controls daemon logging verbosity.
	DebugLevel string

	// LogWriter receives daemon logs. Nil uses darepod's default stdout.
	LogWriter io.Writer

	// AllowMainnet must be true when Network is mainnet. This is an
	// enable-only convenience override; set DaemonConfig directly when
	// a caller-owned config needs an explicit false value.
	AllowMainnet bool

	// ServerAddress is the Ark operator mailbox edge server address.
	ServerAddress string

	// ServerTLSCertPath pins the Ark operator TLS certificate.
	ServerTLSCertPath string

	// ServerInsecure disables TLS for the Ark operator connection. This
	// is an enable-only convenience override; set DaemonConfig directly
	// when a caller-owned config needs an explicit false value.
	ServerInsecure bool

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

	// SwapServerInsecure disables TLS for the swap server connection.
	// This is an enable-only convenience override; set DaemonConfig
	// directly when a caller-owned config needs an explicit false value.
	SwapServerInsecure bool

	// SwapDatabaseFileName is the daemon-owned swap SQLite database path.
	SwapDatabaseFileName string

	// MaxOperatorFeeSat caps the per-round operator fee the daemon accepts.
	MaxOperatorFeeSat int64

	// BufferSize overrides the bufconn listener buffer size.
	BufferSize int
}

// ConnectConfig controls a walletdk client connected to an external daemon.
type ConnectConfig struct {
	// Address is the gRPC target of a daemon exposing walletrpc.
	Address string

	// DialOptions are appended to the default dial options. When empty,
	// walletdk uses insecure transport credentials for local development.
	DialOptions []grpc.DialOption
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

// UnlockWalletRequest unlocks an existing embedded daemon wallet.
type UnlockWalletRequest struct {
	WalletPassword []byte
}

// UnlockWalletResult returns the daemon identity after unlock.
type UnlockWalletResult struct {
	IdentityPubKey string
}

// Balance is the wallet-level balance view.
type Balance struct {
	ConfirmedSat  int64
	PendingInSat  int64
	PendingOutSat int64
}

// DepositRequest creates a tracked boarding address.
type DepositRequest struct {
	AmountSatHint uint64
}

// DepositResult returns a boarding address and its initial activity entry.
type DepositResult struct {
	Address string
	Entry   Entry
}

// ReceiveRequest creates a Lightning invoice payable into the wallet.
type ReceiveRequest struct {
	AmountSat uint64
	Memo      string
}

// ReceiveResult contains the invoice and initial wallet entry.
type ReceiveResult struct {
	Invoice string
	Entry   Entry
}

// SendRequest dispatches an outbound payment.
type SendRequest struct {
	Invoice        string
	OnchainAddress string
	AmountSat      uint64
	Note           string
	MaxFeeSat      uint64
}

// SendResult contains the initial wallet entry for an outbound payment.
type SendResult struct {
	Entry Entry
}

// ListRequest controls wallet activity listing.
type ListRequest struct {
	PendingOnly bool
	Kinds       []EntryKind
	Limit       uint32
	Offset      uint32
}

// ListResult returns wallet activity entries and the unpaginated total.
type ListResult struct {
	Entries []Entry
	Total   uint32
}

// Status summarizes wallet readiness and pending activity.
type Status struct {
	Ready        bool
	Unlocked     bool
	Network      string
	Balance      Balance
	PendingCount uint32
}

// SubscribeRequest controls wallet activity subscriptions.
type SubscribeRequest struct {
	IncludeExisting bool
	Kinds           []EntryKind
}

// EntryKind is the user-visible wallet activity category.
type EntryKind string

const (
	// EntryKindSend is an outbound wallet payment.
	EntryKindSend EntryKind = "send"

	// EntryKindReceive is an inbound Lightning-to-wallet receive.
	EntryKindReceive EntryKind = "receive"

	// EntryKindDeposit is a boarding on-chain deposit.
	EntryKindDeposit EntryKind = "deposit"

	// EntryKindExit is a cooperative wallet-to-on-chain exit.
	EntryKindExit EntryKind = "exit"
)

// EntryStatus is the collapsed wallet activity state.
type EntryStatus string

const (
	// EntryStatusPending means the activity is still in flight.
	EntryStatusPending EntryStatus = "pending"

	// EntryStatusComplete means the activity finished successfully.
	EntryStatusComplete EntryStatus = "complete"

	// EntryStatusFailed means the activity reached a terminal failure.
	EntryStatusFailed EntryStatus = "failed"
)

// Entry is the wallet-facing activity row used by UI and bridge layers.
type Entry struct {
	ID            string
	Kind          EntryKind
	Status        EntryStatus
	AmountSat     int64
	FeeSat        int64
	Counterparty  string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Note          string
	FailureReason string
}
