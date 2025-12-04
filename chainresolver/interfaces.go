package chainresolver

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// Signer abstracts signing operations for the client. This interface is
// injected into ClientResolverActor for transaction signing.
type Signer interface {
	// SignTimeoutPath signs a transaction using the CSV timeout scriptspend
	// path. This is used for unilateral VTXO recovery after the exit delay.
	SignTimeoutPath(ctx context.Context, tx *wire.MsgTx,
		vtxoOutput *wire.TxOut, csvTimeout uint32) (*wire.MsgTx, error)

	// GetDestinationAddress returns an address to receive recovered funds.
	GetDestinationAddress(ctx context.Context) (btcutil.Address, error)
}

// VTXOMonitorHandle contains references to a running VTXOMonitorActor and
// its associated state.
type VTXOMonitorHandle struct {
	// VTXOOutpoint identifies the monitored VTXO.
	VTXOOutpoint wire.OutPoint

	// ServiceKeyName is the service key name for the sub-actor.
	ServiceKeyName string

	// Config is the original monitoring request configuration.
	Config *MonitorVTXORequest

	// ConfirmationHeight is the height at which the VTXO was confirmed.
	// Zero if not yet confirmed.
	ConfirmationHeight int32

	// CSVTimeoutHeight is the height at which CSV timeout is reached.
	// Calculated as ConfirmationHeight + ExitDelay.
	CSVTimeoutHeight int32

	// TimeoutNotified indicates whether CSV timeout notification was sent.
	TimeoutNotified bool

	// Spent indicates whether this VTXO has been spent.
	Spent bool

	// ExpectedSpend is true if we initiated the spend (unroll).
	ExpectedSpend bool
}

// BoardingMonitorHandle contains references for boarding address monitoring.
type BoardingMonitorHandle struct {
	// Address is the monitored boarding address.
	Address btcutil.Address

	// PkScript is the address's scriptPubKey.
	PkScript []byte

	// ExitDelay is the CSV timeout for boarding outputs.
	ExitDelay uint32

	// Deposits tracks detected deposits at this address.
	Deposits []BoardingDeposit
}

// BoardingDeposit represents a deposit at a boarding address.
type BoardingDeposit struct {
	// Outpoint identifies the deposit output.
	Outpoint wire.OutPoint

	// Amount is the deposit value.
	Amount btcutil.Amount

	// ConfirmationHeight is when the deposit was confirmed.
	ConfirmationHeight int32

	// CSVTimeoutHeight is when CSV timeout will be reached.
	CSVTimeoutHeight int32
}

// MonitoredVTXO represents a VTXO being monitored by the client.
type MonitoredVTXO struct {
	// Outpoint identifies the VTXO.
	Outpoint wire.OutPoint

	// Output is the TxOut.
	Output *wire.TxOut

	// TreePath is the user's path to this VTXO.
	TreePath *tree.Tree

	// ExitDelay is the CSV timeout.
	ExitDelay uint32

	// ConfirmationHeight is when the VTXO was confirmed on-chain.
	// Zero means not yet confirmed (still in unroll process).
	ConfirmationHeight int32

	// SpentHeight is when the VTXO was spent (zero if unspent).
	SpentHeight int32

	// SpentTxid is the transaction that spent this VTXO.
	SpentTxid fn.Option[wire.OutPoint]
}
