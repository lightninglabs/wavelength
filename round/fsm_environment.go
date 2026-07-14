package round

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/types"
)

// ClientEnvironment provides the client round interaction state machine with
// access to external systems and storage. This follows the protofsm pattern
// where the environment contains all dependencies needed for state transitions.
//
// Note: Boarding address and intent persistence is handled by the wallet actor.
// The FSM only needs RoundStore for round checkpointing.
type ClientEnvironment struct {
	// RoundStore provides persistence for round coordination and
	// checkpointing.
	RoundStore RoundStore

	// VTXOStore provides persistence for off-chain balance.
	VTXOStore VTXOStore

	// Wallet provides signing capabilities for round participation.
	Wallet ClientWallet

	// SigningExecutor bounds independent VTXO MuSig2 work. A nil executor
	// falls back to serial execution for focused FSM tests.
	SigningExecutor SigningExecutor

	// OperatorTerms contains the operator's parameters including sweep
	// keys, fee targets, confirmation thresholds, and amount limits.
	OperatorTerms *types.OperatorTerms

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// MaxOperatorFee is the client-side cap on the per-round operator
	// fee under the seal-time fee handshake (#270). On every
	// JoinRoundQuote the server issues, QuoteReceivedState compares
	// the quoted OperatorFeeSat against this value; a quote above the
	// cap triggers a JoinRoundRejectOutbox and transitions the FSM to
	// ClientFailedState without signing. The cap is evaluated once
	// per seal pass — each reseal may recompute the quote against
	// fresh chain/treasury inputs, so an earlier reseal rejection
	// does not close the round, only this client's slot within it.
	// Expressed in satoshis.
	//
	// MUST be set to a positive value. A zero / negative value is
	// treated as an explicit misconfiguration and causes
	// evaluateQuote to reject every quote with a diagnostic
	// reason; callers that deliberately want an uncapped
	// environment must supply a sentinel like math.MaxInt64.
	MaxOperatorFee btcutil.Amount

	// Log is the logger for FSM transitions and operations.
	Log btclog.Logger

	// StartHeight is the block height when the FSM was created. This is
	// used as a HeightHint for confirmation registration, ensuring the
	// chain backend scans from the correct starting point. This avoids
	// missing confirmations if the transaction was broadcast before the
	// registration request is processed.
	StartHeight uint32

	// QueryBestHeight returns the current chain tip height. Join-auth uses
	// this to anchor intent validity metadata at signing time.
	QueryBestHeight func(context.Context) (uint32, error)

	// DisableJoinRequestAuth skips BIP-322 join authorization
	// generation. This should only be set in focused unit tests
	// that exercise FSM mechanics without real signing.
	DisableJoinRequestAuth bool

	// ForfeitCollectionTimeout is the timeout used while waiting for
	// forfeit signatures from VTXO actors.
	ForfeitCollectionTimeout time.Duration

	// RegistrationTimeout is the timeout used while parked in
	// IntentSentState waiting for the server's RoundJoined admission
	// watermark. A non-positive value disables arming the registration
	// timeout for the round.
	RegistrationTimeout time.Duration

	// RoundKey is the actor's map key for this round FSM (a TempRoundKey
	// string before admission, a RoundID string after re-keying). The
	// registration-timeout outbox messages carry it so the actor can
	// schedule/cancel a timeout for a round that has not yet been assigned
	// a server RoundID.
	RoundKey RoundKeyStr

	// OwnedScriptChecker determines whether a pkScript belongs to
	// the local wallet. Used by buildOwnedClientVTXOs to filter
	// VTXOs that should be persisted locally. This replaces the
	// IsOwner flag with a data-driven ownership check backed by
	// the owned receive scripts store.
	OwnedScriptChecker OwnedScriptChecker

	// Now returns the current wall-clock time. evaluateQuote
	// uses this to enforce the server-advertised
	// `quote_expires_at`; a nil value falls back to time.Now so
	// pre-existing callers that do not inject a clock keep
	// working. Tests drive the FSM against a deterministic clock
	// by supplying a closure.
	Now func() time.Time
}

// now returns the environment's injected clock if set, otherwise
// the wall-clock time. Keeps every caller honest about which
// timebase the FSM is using without forcing every construction
// site to populate the field.
func (e *ClientEnvironment) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}

	return time.Now()
}

// signingExecutor returns the configured shared executor or a serial fallback
// for construction sites that only exercise focused FSM behavior.
func (e *ClientEnvironment) signingExecutor() SigningExecutor {
	if e.SigningExecutor != nil {
		return e.SigningExecutor
	}

	return NewSigningExecutor(1)
}

// Name returns the unique identifier for this FSM instance.
func (e *ClientEnvironment) Name() string {
	return "round_fsm"
}

// NewClientEnvironment creates a new client environment with the provided
// dependencies. The startHeight parameter should be the current block height
// when the FSM is created, used as a HeightHint for confirmation
// registration. The queryBestHeight callback is used by join-auth to fetch
// signing-time chain tip height for intent validity anchoring.
func NewClientEnvironment(roundStore RoundStore, vtxoStore VTXOStore,
	wallet ClientWallet, terms *types.OperatorTerms,
	chainParams *chaincfg.Params, maxOperatorFee btcutil.Amount,
	logger btclog.Logger, startHeight uint32,
	queryBestHeight func(context.Context) (uint32, error),
	forfeitCollectionTimeout time.Duration) *ClientEnvironment {

	return &ClientEnvironment{
		RoundStore:               roundStore,
		VTXOStore:                vtxoStore,
		Wallet:                   wallet,
		SigningExecutor:          NewSigningExecutor(1),
		OperatorTerms:            terms,
		ChainParams:              chainParams,
		MaxOperatorFee:           maxOperatorFee,
		Log:                      logger,
		StartHeight:              startHeight,
		QueryBestHeight:          queryBestHeight,
		ForfeitCollectionTimeout: forfeitCollectionTimeout,
	}
}
