package vtxo

import (
	"github.com/btcsuite/btcd/chaincfg"
)

// VTXOEnvironment provides the VTXO state machine with access to external
// systems and storage. This follows the protofsm pattern where the environment
// contains all dependencies needed for state transitions.
type VTXOEnvironment struct {
	// name identifies this FSM instance (typically the VTXO outpoint
	// string).
	name string

	// VTXOStore provides persistence for VTXO state.
	VTXOStore VTXOStore

	// Wallet provides signing capabilities for forfeit transactions.
	Wallet VTXOWallet

	// ExpiryConfig contains thresholds for expiry monitoring.
	ExpiryConfig *ExpiryConfig

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params
}

// Name returns the unique identifier for this FSM instance.
func (e *VTXOEnvironment) Name() string {
	return e.name
}

// NewVTXOEnvironment creates a new VTXO environment with the provided
// dependencies.
func NewVTXOEnvironment(
	name string,
	vtxoStore VTXOStore,
	wallet VTXOWallet,
	expiryConfig *ExpiryConfig,
	chainParams *chaincfg.Params,
) *VTXOEnvironment {

	return &VTXOEnvironment{
		name:         name,
		VTXOStore:    vtxoStore,
		Wallet:       wallet,
		ExpiryConfig: expiryConfig,
		ChainParams:  chainParams,
	}
}
