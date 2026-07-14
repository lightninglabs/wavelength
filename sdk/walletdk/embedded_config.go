package walletdk

import (
	"io"
	"time"

	"github.com/lightninglabs/wavelength/waved"
)

// MaxSigningWorkers is the largest bounded MuSig2 worker count accepted by
// the embedded daemon and mobile configuration surfaces.
const MaxSigningWorkers = waved.MaxSigningWorkers

// Config controls the embedded daemon and wallet facade.
type Config struct {
	// DaemonConfig supplies the full daemon config. When nil, walletdk
	// starts from waved.DefaultConfig and applies the convenience fields
	// below.
	DaemonConfig *waved.Config

	// DataDir is the root directory for daemon and wallet state.
	DataDir string

	// Network selects the bitcoin network.
	Network string

	// DebugLevel controls daemon logging verbosity.
	DebugLevel string

	// LogWriter receives daemon logs. Nil uses waved's default stdout.
	LogWriter io.Writer

	// AllowMainnet must be true when Network is mainnet. This is an
	// enable-only convenience override; set DaemonConfig directly when
	// a caller-owned config needs an explicit false value.
	AllowMainnet bool

	// ServerAddress is the Ark operator mailbox edge server address. Empty
	// selects the daemon network+transport default.
	ServerAddress string

	// ServerTransport selects how the embedded daemon talks to the Ark
	// operator and mailbox edge. Empty defaults to gRPC.
	ServerTransport Transport

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

	// WalletBtcwalletBlockHeadersSource is a local file path or HTTP(S)
	// URL that btcwallet/neutrino imports block headers from on startup.
	WalletBtcwalletBlockHeadersSource string

	// WalletBtcwalletFilterHeadersSource is a local file path or HTTP(S)
	// URL that btcwallet/neutrino imports compact filter headers from on
	// startup.
	WalletBtcwalletFilterHeadersSource string

	// SwapServerAddress is the swapdk-server address for the selected
	// transport. Empty selects the daemon network+transport default.
	SwapServerAddress string

	// SwapServerTransport selects how the embedded daemon talks to
	// swapdk-server. Empty defaults to gRPC.
	SwapServerTransport Transport

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

	// SigningWorkers bounds concurrent VTXO MuSig2 signer sessions. Zero
	// selects the wallet-backend default and one forces serial signing.
	SigningWorkers int

	// EagerRoundJoin makes the embedded daemon's wallet drive
	// round-joining without waiting for a follow-up Board /
	// LeaveVTXOs RPC. With the flag on, freshly confirmed boarding
	// deposits join the next round automatically, and the wallet's
	// Exit / cooperative-leave path fires registration immediately
	// rather than batching. The walletdkrpc-tagged embedded build
	// that walletdk targets already defaults this to true via
	// waved.DefaultConfig, so leaving this field at the zero
	// value is the right choice for nearly every host. Set true
	// only to force the override when supplying a caller-owned
	// DaemonConfig that currently carries false. To force eager
	// round-join OFF, pass WithEagerRoundJoinDisabled() to Start
	// rather than mutating this field.
	EagerRoundJoin bool

	// BufferSize overrides the bufconn listener buffer size.
	BufferSize int
}
