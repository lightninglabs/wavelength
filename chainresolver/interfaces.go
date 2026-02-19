package chainresolver

import (
	"context"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ChainResolverStore defines the persistence interface for chain resolver
// state. Each active resolver's state is persisted so that crash recovery
// can reconstruct the FSM and resume from the last known state.
type ChainResolverStore interface {
	// SaveResolverState persists the current state of a resolver
	// identified by its VTXO outpoint. The state name is a string
	// enum (e.g. "broadcasting_tree") and details is an opaque JSON
	// blob containing state-specific fields.
	SaveResolverState(
		ctx context.Context, outpoint wire.OutPoint,
		state string, details []byte,
	) error

	// GetResolverState loads the persisted state for a resolver. Returns
	// the state name and opaque details blob. Returns an error if no
	// state is found.
	GetResolverState(
		ctx context.Context, outpoint wire.OutPoint,
	) (string, []byte, error)

	// ListActiveResolvers returns the outpoints of all resolvers that
	// have persisted state. Used during startup to recover active
	// resolvers.
	ListActiveResolvers(ctx context.Context) ([]wire.OutPoint, error)

	// DeleteResolverState removes the persisted state for a resolver.
	// Called when a resolver reaches a terminal state.
	DeleteResolverState(
		ctx context.Context, outpoint wire.OutPoint,
	) error
}

// PackageRelayer submits parent+child transaction packages to the
// Bitcoin network via the submitpackage RPC. This is required for
// broadcasting zero-fee transactions that contain P2A ephemeral anchor
// outputs (BIP 431). Without package relay, the zero-fee parent would
// be rejected by mempool policy.
type PackageRelayer interface {
	// SubmitPackage atomically submits a package consisting of one
	// or more parent transactions and a single fee-paying child
	// transaction. Returns an error if the package is rejected by
	// the node.
	SubmitPackage(ctx context.Context, parents []*wire.MsgTx,
		child *wire.MsgTx) error
}

// CPFPWallet provides PSBT funding and signing for constructing
// fee-paying CPFP child transactions that spend P2A ephemeral anchor
// outputs. The wallet performs coin selection (adding wallet UTXOs for
// fees) and signs the wallet-controlled inputs. This is a subset of
// the rounds.WalletController interface, scoped to just the methods
// needed for CPFP construction.
type CPFPWallet interface {
	// FundPsbt performs coin selection and adds wallet inputs to
	// fund the PSBT. It also adds a change output if needed.
	// Returns the change output index (-1 if no change).
	FundPsbt(ctx context.Context, packet *psbt.Packet,
		minConfs int32, feeRate chainfee.SatPerKWeight,
		account string) (changeIndex int32, err error)

	// FinalizePsbt signs all wallet-controlled inputs and finalizes
	// the PSBT, making it ready for broadcast. Returns the finalized
	// raw transaction.
	FinalizePsbt(ctx context.Context,
		packet *psbt.Packet) (*wire.MsgTx, error)
}
