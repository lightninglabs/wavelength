package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

// TransferInputSnapshot is a portable encoding of the client-side signing
// context required to finalize checkpoint PSBTs.
//
// The goal is to persist enough information to reconstruct the VTXO tapscript
// deterministically, without serializing Go wallet types directly.
type TransferInputSnapshot struct {
	// Outpoint identifies the input VTXO being transferred.
	Outpoint wire.OutPoint `json:"outpoint"`

	// AmountSat is the input VTXO amount in satoshis.
	AmountSat int64 `json:"amount_sat"`

	// ClientKeyFamily is the key family for the client signing key.
	ClientKeyFamily int32 `json:"client_key_family"`

	// ClientKeyIndex is the key index for the client signing key.
	ClientKeyIndex uint32 `json:"client_key_index"`

	// ClientPubKey is the compressed client public key.
	ClientPubKey []byte `json:"client_pub_key"`

	// OperatorPubKey is the compressed operator public key.
	OperatorPubKey []byte `json:"operator_pub_key"`

	// ExitDelay is the VTXO CSV delay used to derive the VTXO script.
	ExitDelay uint32 `json:"exit_delay"`

	// OwnerLeafScript is the leaf script committed to the checkpoint tap
	// tree.
	//
	// This is currently a draft implementation, and may change as the
	// checkpoint policy is refined.
	OwnerLeafScript []byte `json:"owner_leaf_script"`
}

// ToSnapshot converts the transfer input into a portable snapshot.
func (i *TransferInput) ToSnapshot() (*TransferInputSnapshot, error) {
	err := i.Validate()
	if err != nil {
		return nil, err
	}

	operatorKey := i.VTXO.OperatorKey
	if operatorKey == nil {
		return nil, fmt.Errorf("vtxo operator key must be provided")
	}

	exitDelay := i.VTXO.RelativeExpiry
	if exitDelay == 0 {
		return nil, fmt.Errorf("vtxo exit delay must be provided")
	}

	return &TransferInputSnapshot{
		Outpoint:        i.VTXO.Outpoint,
		AmountSat:       int64(i.VTXO.Amount),
		ClientKeyFamily: int32(i.VTXO.ClientKey.KeyLocator.Family),
		ClientKeyIndex:  i.VTXO.ClientKey.KeyLocator.Index,
		ClientPubKey:    i.VTXO.ClientKey.PubKey.SerializeCompressed(),
		OperatorPubKey:  operatorKey.SerializeCompressed(),
		ExitDelay:       exitDelay,
		OwnerLeafScript: i.OwnerLeafScript,
	}, nil
}

// TransferInputFromSnapshot reconstructs a transfer input from a snapshot.
func TransferInputFromSnapshot(snap *TransferInputSnapshot) (TransferInput,
	error) {

	if snap == nil {
		return TransferInput{}, fmt.Errorf("snapshot must be provided")
	}

	if snap.AmountSat <= 0 {
		return TransferInput{}, fmt.Errorf("amount must be positive")
	}

	if len(snap.ClientPubKey) == 0 || len(snap.OperatorPubKey) == 0 {
		return TransferInput{}, fmt.Errorf("pubkeys must be provided")
	}

	if snap.ExitDelay == 0 {
		return TransferInput{}, fmt.Errorf("exit delay must be " +
			"provided")
	}

	if len(snap.OwnerLeafScript) == 0 {
		return TransferInput{}, fmt.Errorf("owner leaf script must " +
			"be provided")
	}

	clientPub, err := btcec.ParsePubKey(snap.ClientPubKey)
	if err != nil {
		return TransferInput{}, fmt.Errorf("parse client pubkey: %w",
			err)
	}

	operatorPub, err := btcec.ParsePubKey(snap.OperatorPubKey)
	if err != nil {
		return TransferInput{}, fmt.Errorf("parse operator pubkey: %w",
			err)
	}

	tapScript, err := scripts.VTXOTapScript(clientPub, operatorPub,
		snap.ExitDelay)
	if err != nil {
		return TransferInput{}, fmt.Errorf("rebuild vtxo tapscript: %w",
			err)
	}

	tapKey, err := scripts.VTXOTapKey(clientPub, operatorPub,
		snap.ExitDelay)
	if err != nil {
		return TransferInput{}, fmt.Errorf("rebuild vtxo tapkey: %w",
			err)
	}

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	if err != nil {
		return TransferInput{}, fmt.Errorf("build vtxo pkscript: %w",
			err)
	}

	desc := &vtxo.Descriptor{
		Outpoint: snap.Outpoint,
		Amount:   btcutil.Amount(snap.AmountSat),
		PkScript: pkScript,
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(
					snap.ClientKeyFamily,
				),
				Index: snap.ClientKeyIndex,
			},
			PubKey: clientPub,
		},
		OperatorKey:    operatorPub,
		TapScript:      tapScript,
		RelativeExpiry: snap.ExitDelay,
		Status:         vtxo.VTXOStatusLive,
	}

	return TransferInput{
		VTXO:            desc,
		OwnerLeafScript: snap.OwnerLeafScript,
	}, nil
}
