package walletcore

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// LockID is a 32-byte caller-scoped identifier assigned when leasing a
// wallet output, and re-supplied when releasing it. Each subsystem should
// derive its own LockID from a stable, human-readable prefix (for example
// the first 32 bytes of sha256("txconfirm")) so that two subsystems cannot
// accidentally release each other's leases and so the ID is stable across
// restarts.
//
// The type lives here in walletcore (rather than in wallet) so packages that
// would otherwise create an import cycle with wallet — notably txconfirm,
// which both consumes leases and is consumed by wallet — can depend on a
// single canonical type without going through wallet.
type LockID [32]byte

// Utxo represents an unspent transaction output returned by ListUnspent.
// This is a simplified representation focused on the information needed
// for boarding-UTXO detection and fee-input selection.
type Utxo struct {
	// Outpoint uniquely identifies this UTXO.
	Outpoint wire.OutPoint

	// PkScript is the output script this UTXO pays to.
	PkScript []byte

	// Amount is the value of this UTXO in satoshis.
	Amount btcutil.Amount

	// Confirmations is the number of confirmations this UTXO has.
	Confirmations int32
}

// OutputLeaser is implemented by wallet backends that let callers exclude
// specific UTXOs from the wallet's own coin-selection pool for a bounded
// duration. The two-method shape matches the canonical interface exposed by
// btcwallet and lndclient's WalletKit so a concrete backend can delegate
// directly without translating between type systems.
type OutputLeaser interface {
	// LeaseOutput locks the named outpoint against the caller's LockID
	// for at least the supplied expiry, returning the absolute time at
	// which the lock will auto-release. The lease can be extended by
	// calling LeaseOutput again with the same LockID before the
	// previous lease expires.
	LeaseOutput(ctx context.Context, id LockID, op wire.OutPoint,
		expiry time.Duration) (time.Time, error)

	// ReleaseOutput drops the caller's lease on the named outpoint. The
	// supplied LockID must match the one used at LeaseOutput time; a
	// mismatch is an error to keep subsystems from interfering with
	// each other's reservations.
	ReleaseOutput(ctx context.Context, id LockID, op wire.OutPoint) error
}
