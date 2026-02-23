package round

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/types"
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

	// OperatorTerms contains the operator's parameters including sweep
	// keys, fee targets, confirmation thresholds, and amount limits.
	OperatorTerms *types.OperatorTerms

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// MaxOperatorFee is the maximum fee the client is willing to pay to
	// the operator per round. This is the difference between total input
	// (boarding) amounts and total output (VTXO) amounts. If the fee would
	// exceed this limit, registration is rejected.
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
	// this to anchor validity windows at signing time.
	QueryBestHeight func(context.Context) (uint32, error)

	// DisableJoinRequestAuth skips BIP-322 join authorization
	// generation. This should only be set in focused unit tests
	// that exercise FSM mechanics without real signing.
	DisableJoinRequestAuth bool
}

// Name returns the unique identifier for this FSM instance.
func (e *ClientEnvironment) Name() string {
	return "round_fsm"
}

// NewClientEnvironment creates a new client environment with the provided
// dependencies. The startHeight parameter should be the current block height
// when the FSM is created, used as a HeightHint for confirmation
// registration. The queryBestHeight callback is used by join-auth to fetch
// signing-time chain tip height for block-window anchoring.
func NewClientEnvironment(roundStore RoundStore, vtxoStore VTXOStore,
	wallet ClientWallet, terms *types.OperatorTerms,
	chainParams *chaincfg.Params, maxOperatorFee btcutil.Amount,
	logger btclog.Logger, startHeight uint32,
	queryBestHeight func(context.Context) (uint32, error)) *ClientEnvironment {

	return &ClientEnvironment{
		RoundStore:      roundStore,
		VTXOStore:       vtxoStore,
		Wallet:          wallet,
		OperatorTerms:   terms,
		ChainParams:     chainParams,
		MaxOperatorFee:  maxOperatorFee,
		Log:             logger,
		StartHeight:     startHeight,
		QueryBestHeight: queryBestHeight,
	}
}
