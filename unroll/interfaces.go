package unroll

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/input"
)

// ProofAssembler resolves the immutable local recovery proof for one target
// outpoint.
type ProofAssembler interface {
	// EnsureProof builds or retrieves the recovery proof for the target.
	EnsureProof(ctx context.Context, target wire.OutPoint) (
		*recovery.Proof, error,
	)
}

// SweepWallet provides the wallet operations needed to build and sign the
// final timeout sweep.
type SweepWallet interface {
	input.Signer

	// NewWalletPkScript returns a fresh wallet-managed destination script
	// for the sweep output.
	NewWalletPkScript(ctx context.Context) ([]byte, error)
}

// ChainSource is the subset of the chainsource actor API used by the unroll
// actor.
type ChainSource = chainsource.ChainSourceMsg

// TxConfirmRef is the shared tx-confirmation actor used by unroll jobs.
type TxConfirmRef = txconfirm.Msg

// VTXOStore is the descriptor store the actor uses to load its target input.
type VTXOStore = vtxo.VTXOStore
