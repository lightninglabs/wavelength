package unroll

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// FeeInputPlanner checks the backing-wallet fee-input supply for a ready
// unroll frontier.
type FeeInputPlanner interface {
	// PlanFeeInputs returns the fee-input demand for the ready proof
	// frontier. The implementation may inspect wallet state, but must not
	// broadcast transactions or mutate the unroll planner state.
	PlanFeeInputs(ctx context.Context,
		ready []unrollplan.TxFrontier) (FeeInputPlan, error)
}

// FeeInputPlan describes the backing-wallet fee-input demand for the currently
// ready proof frontier.
type FeeInputPlan struct {
	// RequiredFeeInputsNow is the number of proof parents that would be
	// submitted concurrently under the current planner snapshot.
	RequiredFeeInputsNow int

	// UsableFeeInputs is the number of confirmed wallet UTXOs that are
	// large enough to serve as independent CPFP fee inputs.
	UsableFeeInputs int

	// FanoutOutputsNeeded is the number of wallet outputs the unroller
	// wants to create before submitting the ready frontier.
	FanoutOutputsNeeded int

	// RecommendedFanoutOutputAmountSat is the minimum amount each fanout
	// output should carry.
	RecommendedFanoutOutputAmountSat btcutil.Amount

	// FanoutFundingShortfallSat is positive when the wallet has too little
	// confirmed total balance to build the requested fanout.
	FanoutFundingShortfallSat btcutil.Amount

	// PendingFanoutTxid identifies an already-broadcast fanout transaction
	// that should be awaited instead of broadcasting a duplicate.
	PendingFanoutTxid fn.Option[chainhash.Hash]

	// PendingFanoutPkScript is the script to watch for the pending fanout
	// transaction. It is only set when PendingFanoutTxid is Some.
	PendingFanoutPkScript []byte
}

// NeedsFanout reports whether the actor must pause proof submission and create
// or await wallet fanout outputs first.
func (p FeeInputPlan) NeedsFanout() bool {
	return p.FanoutOutputsNeeded > 0 || p.PendingFanoutTxid.IsSome()
}
