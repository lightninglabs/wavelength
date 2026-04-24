package rounds

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightninglabs/darepo/ledger"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// Environment provides the round state machine with access to external systems
// and storage. This follows the protofsm pattern where the environment contains
// all dependencies needed for state transitions.
type Environment struct {
	// RoundID identifies this FSM instance.
	RoundID RoundID

	// ChainParams specifies the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// BoardingInputLocker manages locks on boarding inputs to prevent
	// double-spending across concurrent rounds.
	BoardingInputLocker BoardingInputLocker

	// ChainSource provides access to blockchain data for UTXO validation.
	// When nil, the server relies on client-provided TxProofs instead.
	ChainSource ChainSource

	// HeaderVerifier validates that a block header exists on the best
	// chain at the claimed height. Used to verify TxProofs when
	// ChainSource is nil.
	HeaderVerifier proof.HeaderVerifier

	// Terms contains the operator's terms for this round.
	Terms *batch.Terms

	// ForfeitScript is the output script that clients must use for the
	// penalty output in forfeit transactions. This allows the server to
	// claim forfeited VTXO funds.
	ForfeitScript []byte

	// Log is the logger for the FSM.
	Log btclog.Logger

	// WalletController provides PSBT funding and signing operations.
	WalletController WalletController

	// FeeEstimator provides fee rate estimation for transactions.
	FeeEstimator chainfee.Estimator

	// WalletAccount is the lnd wallet account name to use for coin
	// selection.
	WalletAccount string

	// ConfTarget is the confirmation target to use for fee estimation.
	ConfTarget uint32

	// MinConfs is the minimum number of confirmations required for wallet
	// UTXOs to be used for funding.
	MinConfs int32

	// RoundStore provides persistent storage for rounds.
	RoundStore RoundStore

	// VTXOStore provides persistent storage for VTXOs.
	VTXOStore VTXOStore

	// VTXOLocker provides cross-subsystem locking for VTXO outpoints.
	//
	// This is the single lock authority shared with OOR, so rounds and OOR
	// always use the same lock semantics.
	VTXOLocker vtxo.Locker

	// StartHeight is the block height when this round was created. Used as
	// the height hint when subscribing to confirmation notifications to
	// ensure we don't miss confirmations that occur between round creation
	// and broadcast.
	StartHeight uint32

	// DisableJoinRequestAuth skips join-request BIP-322 validation.
	// This should only be enabled in focused unit tests.
	DisableJoinRequestAuth bool

	// ShouldSeal is an optional predicate evaluated after each
	// successful client join. When it returns true the round is
	// sealed immediately without waiting for the registration
	// timeout. A nil predicate is equivalent to "never seal early".
	ShouldSeal SealPredicate

	// FeeCalculator computes dynamic fees based on the current
	// fee schedule and treasury utilization. When nil, the
	// flat MinOperatorFee from Terms is used instead.
	FeeCalculator *fees.Calculator

	// TreasuryTracker provides current utilization for
	// congestion pricing. When nil, utilization is assumed to
	// be zero.
	TreasuryTracker *fees.TreasuryTracker

	// LedgerRef is the actor reference for the ledger
	// accounting actor. When non-nil, round lifecycle events
	// are forwarded via fire-and-forget Tell.
	LedgerRef actor.TellOnlyRef[ledger.LedgerMsg]

	// SubsidizeThinRounds controls the batch-size divisor used by
	// validateOperatorFee when sizing the on-chain share of the
	// round cost. When true, the legacy pre-#268 behavior is kept:
	// both the EstimateFee quote surface and validateOperatorFee
	// size on-chain cost against MaxVTXOsPerTree, which dilutes
	// thin-round cost across the theoretical maximum tree size
	// (operator subsidy). When false (the new default),
	// validation charges at the actual registered participant
	// count so a 4-client round pays the full ComputeBoardingFee /
	// ComputeForfeitFee per input rather than 1/32 of it.
	SubsidizeThinRounds bool
}

// Name returns the unique identifier for this FSM instance.
func (e *Environment) Name() string {
	return fmt.Sprintf("server_round_fsm_%s", e.RoundID)
}
