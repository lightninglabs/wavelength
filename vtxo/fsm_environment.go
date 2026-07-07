package vtxo

import (
	"context"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// ForfeitParticipantSignRequest describes the exact forfeit transaction that
// a non-local participant is asked to sign. It is emitted only after the new
// round has assigned connector outputs, because the connector prevout is part
// of the taproot sighash.
type ForfeitParticipantSignRequest struct {
	// VTXO is the VTXO being forfeited.
	VTXO *Descriptor

	// SpendPath is the VTXO tapscript path being spent.
	SpendPath *arkscript.SpendPath

	// ForfeitTx is the exact transaction whose VTXO input must be signed.
	ForfeitTx *wire.MsgTx

	// ConnectorOutpoint identifies the connector output assigned by the new
	// round.
	ConnectorOutpoint wire.OutPoint

	// ConnectorPkScript is the connector output script.
	ConnectorPkScript []byte

	// ConnectorAmount is the connector output amount in satoshis.
	ConnectorAmount int64

	// ServerForfeitPkScript is the forfeit output script.
	ServerForfeitPkScript []byte
}

// ForfeitParticipantSigner obtains keyed signatures from non-local
// participants for custom VTXO policies.
type ForfeitParticipantSigner func(context.Context,
	*ForfeitParticipantSignRequest) ([]*types.ForfeitParticipantSig, error)

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

	// ForfeitParticipantSigner obtains signatures from non-local
	// participants when a custom VTXO policy requires more than the local
	// wallet signature before the operator can finalize the forfeit.
	ForfeitParticipantSigner ForfeitParticipantSigner
}

// Name returns the unique identifier for this FSM instance.
func (e *VTXOEnvironment) Name() string {
	return e.name
}

// NewVTXOEnvironment creates a new VTXO environment with the provided
// dependencies.
func NewVTXOEnvironment(name string, vtxoStore VTXOStore, wallet VTXOWallet,
	expiryConfig *ExpiryConfig, chainParams *chaincfg.Params,
	forfeitParticipantSigner ForfeitParticipantSigner) *VTXOEnvironment {

	return &VTXOEnvironment{
		name:                     name,
		VTXOStore:                vtxoStore,
		Wallet:                   wallet,
		ExpiryConfig:             expiryConfig,
		ChainParams:              chainParams,
		ForfeitParticipantSigner: forfeitParticipantSigner,
	}
}
