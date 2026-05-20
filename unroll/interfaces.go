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
	EnsureProof(ctx context.Context,
		target wire.OutPoint) (*recovery.Proof, error)
}

// SweepWallet provides the wallet operations needed to build and sign the
// final timeout sweep.
type SweepWallet interface {
	input.Signer

	// NewWalletPkScript returns a fresh wallet-managed destination script
	// for the sweep output.
	NewWalletPkScript(ctx context.Context) ([]byte, error)
}

const (
	// StandardVTXOTimeoutExitPolicyKind identifies the built-in timeout
	// sweep policy for normal Ark VTXOs.
	StandardVTXOTimeoutExitPolicyKind = "standard_vtxo_timeout"
)

// ExitSpendRequest carries the materialized on-chain output and local signing
// context needed to build the final exit spend.
type ExitSpendRequest struct {
	// TargetOutpoint is the materialized on-chain output to spend.
	TargetOutpoint wire.OutPoint

	// TargetOutput is the output being spent.
	TargetOutput *wire.TxOut

	// DestinationPkScript receives the recovered funds.
	DestinationPkScript []byte

	// FeeRateSatPerVByte is the selected fee rate for the exit spend.
	FeeRateSatPerVByte int64

	// CurrentHeight is the last persisted best chain height from the
	// unroll job. It may lag the live chain tip and is unused by the
	// standard policy, but is available to future policies that need
	// height context for timelock validation.
	CurrentHeight int32

	// Signer signs the exit spend.
	Signer input.Signer
}

// ExitSpendPolicyRequest identifies the durable exit policy to reconstruct.
type ExitSpendPolicyRequest struct {
	// Kind is the durable policy kind persisted with the unroll job.
	Kind string

	// Ref is an optional policy-specific durable-state reference. The
	// built-in standard timeout policy requires this to be empty;
	// non-standard policy kinds require a non-empty ref so restart can
	// reconstruct the same policy from domain-owned SQL state.
	Ref string

	// StandardDescriptor is the descriptor used by the built-in standard
	// VTXO timeout policy. Custom resolvers may ignore it when their
	// policy-specific state is addressed by Kind and Ref.
	StandardDescriptor *vtxo.Descriptor
}

// ExitSpendPolicy describes how unroll spends the materialized target output
// once the Ark lineage has been brought on chain.
type ExitSpendPolicy interface {
	// Kind returns the durable policy kind persisted with the unroll job.
	Kind() string

	// CSVDelay returns the relative delay required by this policy.
	CSVDelay() uint32

	// ValidateTarget verifies this policy can spend the materialized target
	// output.
	ValidateTarget(target *wire.TxOut) error

	// BuildSpendTx builds and signs the exit transaction.
	BuildSpendTx(ctx context.Context,
		req ExitSpendRequest) (*wire.MsgTx, error)
}

// ExitSpendPolicyResolver reconstructs a policy from the durable identity
// stored with an unroll job. Custom actor factories can inject resolvers for
// their policy families; the built-in actor default handles standard VTXO
// timeout jobs.
type ExitSpendPolicyResolver interface {
	// ResolveExitSpendPolicy returns the policy for the given kind/ref
	// pair.
	ResolveExitSpendPolicy(ctx context.Context,
		req ExitSpendPolicyRequest) (ExitSpendPolicy, error)
}

// ChainSource is the subset of the chainsource actor API used by the unroll
// actor.
type ChainSource = chainsource.ChainSourceMsg

// TxConfirmRef is the shared tx-confirmation actor used by unroll jobs.
type TxConfirmRef = txconfirm.Msg

// VTXOStore is the descriptor store the actor uses to load its target input.
type VTXOStore = vtxo.VTXOStore
