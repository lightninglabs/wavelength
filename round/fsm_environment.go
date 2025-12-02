package round

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// OperatorTerms wraps the lib OperatorTerms with additional policy knobs the
// client actor layer needs (sweep keys, fee targets, confirmation thresholds).
type OperatorTerms struct {
	*types.OperatorTerms

	// SweepKey is the operator key used in VTXT sweep paths.
	SweepKey *btcec.PublicKey

	// SweepDelay is the batch-wide absolute timelock (blocks).
	SweepDelay uint32

	// DustLimit enforces minimum output value for boarding/funding flows.
	DustLimit btcutil.Amount

	// MinBoardingAmount is the minimum amount clients must contribute.
	MinBoardingAmount btcutil.Amount

	// MaxBoardingAmount caps the amount accepted per request (optional).
	MaxBoardingAmount btcutil.Amount

	// FeeRate reflects the operator's target package feerate (sat/vByte).
	FeeRate btcutil.Amount

	// MinConfirmations is the minimum confs required on boarding inputs.
	MinConfirmations uint32
}

// ClientEnvironment provides the client boarding state machine with access to
// external systems and storage. This follows the protofsm pattern where the
// environment contains all dependencies needed for state transitions.
//
// Note: Boarding address and intent persistence is handled by the wallet actor.
// The FSM only needs RoundStore for round checkpointing.
type ClientEnvironment struct {
	// Name identifies this FSM instance.
	name string

	// RoundStore provides persistence for round coordination and
	// checkpointing.
	RoundStore RoundStore

	// VTXOStore provides persistence for off-chain balance.
	VTXOStore VTXOStore

	// Wallet provides signing capabilities for round participation.
	Wallet ClientWallet

	// OperatorTerms contains the operator's parameters.
	OperatorTerms *OperatorTerms

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params
}

// Name returns the unique identifier for this FSM instance.
func (e *ClientEnvironment) Name() string {
	return "round_fsm"
}

// NewClientEnvironment creates a new client environment with the provided
// dependencies.
func NewClientEnvironment(roundStore RoundStore, vtxoStore VTXOStore,
	wallet ClientWallet, terms *OperatorTerms,
	chainParams *chaincfg.Params) *ClientEnvironment {

	return &ClientEnvironment{
		RoundStore:    roundStore,
		VTXOStore:     vtxoStore,
		Wallet:        wallet,
		OperatorTerms: terms,
		ChainParams:   chainParams,
	}
}
