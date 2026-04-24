package rounds

import (
	"fmt"
	"time"

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

// Default values for seal-time fee handshake quote governance. The
// Environment fields below pull these defaults when the actor's
// ActorConfig leaves them zero.
const (
	// DefaultQuoteTTL is the per-quote acceptance window. Clients
	// that neither accept nor reject within this window are rolled
	// into the reseal decision as timed-out.
	DefaultQuoteTTL = 10 * time.Second

	// DefaultMaxSealPasses is the maximum number of reseal passes a
	// round will attempt before finalizing with the latest pass's
	// accepted set. Bounds how much fee-rate volatility a single
	// reject-happy client can force the operator to absorb.
	DefaultMaxSealPasses uint32 = 3

	// DefaultMaxClientRejects is the maximum number of rejects a
	// single client may send across a round's seal passes before it
	// is dropped from the round entirely (forfeit / boarding locks
	// released). Timeouts do NOT count against this cap — only
	// explicit rejects do.
	DefaultMaxClientRejects uint32 = 3
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

	// QuoteTTL is how long the server waits for each client to
	// accept or reject a JoinRoundQuote before flipping the client's
	// status to QuoteTimedOut. Zero means DefaultQuoteTTL.
	QuoteTTL time.Duration

	// MaxSealPasses caps the number of reseal iterations per round.
	// Once hit, the round finalizes with the last pass's accepted
	// set instead of resealing again. Zero means
	// DefaultMaxSealPasses.
	MaxSealPasses uint32

	// MaxClientRejects caps how many explicit JoinRoundReject
	// messages a single client may send across a round's seal
	// passes. Timeouts are not counted here (see DefaultMaxClientRejects
	// doc). Zero means DefaultMaxClientRejects.
	MaxClientRejects uint32

	// SkipQuoteHandshake makes SealEvent transition directly to
	// BatchBuildingState, bypassing the #270 QuoteSentState quote
	// fan-out. Intended for focused unit tests that pre-date the
	// seal-time fee handshake; production code leaves this false.
	SkipQuoteHandshake bool
}

// quoteTTL returns the effective quote acceptance window, applying
// DefaultQuoteTTL when the environment leaves the field unset.
func (e *Environment) quoteTTL() time.Duration {
	if e.QuoteTTL == 0 {
		return DefaultQuoteTTL
	}

	return e.QuoteTTL
}

// maxSealPasses returns the effective reseal cap, applying
// DefaultMaxSealPasses when the environment leaves the field unset.
func (e *Environment) maxSealPasses() uint32 {
	if e.MaxSealPasses == 0 {
		return DefaultMaxSealPasses
	}

	return e.MaxSealPasses
}

// maxClientRejects returns the effective per-client reject cap,
// applying DefaultMaxClientRejects when the environment leaves the
// field unset.
func (e *Environment) maxClientRejects() uint32 {
	if e.MaxClientRejects == 0 {
		return DefaultMaxClientRejects
	}

	return e.MaxClientRejects
}

// Name returns the unique identifier for this FSM instance.
func (e *Environment) Name() string {
	return fmt.Sprintf("server_round_fsm_%s", e.RoundID)
}
