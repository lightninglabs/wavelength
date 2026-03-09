package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
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
	Outpoint wire.OutPoint

	// AmountSat is the input VTXO amount in satoshis.
	AmountSat int64

	// ClientKeyFamily is the key family for the client signing key.
	ClientKeyFamily int32

	// ClientKeyIndex is the key index for the client signing key.
	ClientKeyIndex uint32

	// ClientPubKey is the compressed client public key.
	ClientPubKey []byte

	// OperatorPubKey is the compressed operator public key.
	OperatorPubKey []byte

	// ExitDelay is the VTXO CSV delay used to derive the VTXO script.
	ExitDelay uint32

	// OwnerLeafScript is the leaf script committed to the checkpoint tap
	// tree.
	//
	// This is currently a draft implementation, and may change as the
	// checkpoint policy is refined.
	OwnerLeafScript []byte

	// PkScript is the raw output script of the input VTXO. When set, it
	// is used directly during snapshot restoration, bypassing the standard
	// VTXO pkscript derivation from client/operator keys. This preserves
	// custom spend scripts (e.g. vHTLC) through the TLV round-trip without
	// loss of fidelity.
	PkScript []byte

	// SpendWitnessScript is the tapscript leaf script for the spend path
	// used when signing checkpoint PSBTs. Corresponds to
	// SpendInfo.WitnessScript on TransferInput.
	SpendWitnessScript []byte

	// SpendControlBlock is the BIP-341 control block for the spend leaf.
	// Corresponds to SpendInfo.ControlBlock on TransferInput.
	SpendControlBlock []byte

	// ConditionWitness contains extra witness items (e.g. preimage)
	// required between the signatures and the leaf script. Corresponds to
	// TransferInput.ConditionWitness.
	ConditionWitness [][]byte
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

	// SpendInfo is required so the restored snapshot can sign checkpoints
	// without re-deriving the spend path from key material.
	if i.SpendInfo == nil {
		return nil, fmt.Errorf("spend info must be provided for " +
			"snapshot")
	}

	return &TransferInputSnapshot{
		Outpoint:           i.VTXO.Outpoint,
		AmountSat:          int64(i.VTXO.Amount),
		ClientKeyFamily:    int32(i.VTXO.ClientKey.KeyLocator.Family),
		ClientKeyIndex:     i.VTXO.ClientKey.KeyLocator.Index,
		ClientPubKey:       i.VTXO.ClientKey.PubKey.SerializeCompressed(),
		OperatorPubKey:     operatorKey.SerializeCompressed(),
		ExitDelay:          exitDelay,
		OwnerLeafScript:    i.OwnerLeafScript,
		PkScript:           i.VTXO.PkScript,
		SpendWitnessScript: i.SpendInfo.WitnessScript,
		SpendControlBlock:  i.SpendInfo.ControlBlock,
		ConditionWitness:   i.ConditionWitness,
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

	if len(snap.SpendWitnessScript) == 0 {
		return TransferInput{}, fmt.Errorf("spend witness script " +
			"must be provided")
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

	// Always derive the standard VTXO pkscript and tapscript from the
	// key material first. If the snapshot contains an explicit PkScript
	// that differs from the derived one, the input uses a custom spend
	// script (e.g. vHTLC); in that case the stored bytes are used
	// verbatim and TapScript is left nil because the input relies on a
	// custom signing path. Standard VTXOs — including old snapshots
	// that pre-date the PkScript field — always produce a non-nil
	// TapScript via the normal derivation path.
	derivedTapScript, buildErr := scripts.VTXOTapScript(
		clientPub, operatorPub, snap.ExitDelay,
	)
	if buildErr != nil {
		return TransferInput{}, fmt.Errorf(
			"rebuild vtxo tapscript: %w", buildErr,
		)
	}

	tapKey, buildErr := scripts.VTXOTapKey(
		clientPub, operatorPub, snap.ExitDelay,
	)
	if buildErr != nil {
		return TransferInput{}, fmt.Errorf(
			"rebuild vtxo tapkey: %w", buildErr,
		)
	}

	derivedPkScript, buildErr := txscript.PayToTaprootScript(tapKey)
	if buildErr != nil {
		return TransferInput{}, fmt.Errorf(
			"build vtxo pkscript: %w", buildErr,
		)
	}

	// Determine the effective pkscript and tapscript. When the stored
	// PkScript is absent or matches the standard derivation, treat the
	// input as a standard VTXO and use the derived tapscript. Otherwise
	// the stored pkscript is a custom script and TapScript stays nil.
	var (
		pkScript  []byte
		tapScript *waddrmgr.Tapscript
	)

	if len(snap.PkScript) == 0 ||
		bytes.Equal(snap.PkScript, derivedPkScript) {

		pkScript = derivedPkScript
		tapScript = derivedTapScript

	} else {
		pkScript = snap.PkScript
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

	// Reconstruct the spend path descriptor from the stored bytes. We
	// do not restore RequiredSequence/RequiredLockTime since those are
	// only used during transaction construction, not checkpoint signing.
	spendInfo := &arkscript.SpendInfo{
		WitnessScript: snap.SpendWitnessScript,
		ControlBlock:  snap.SpendControlBlock,
	}

	return TransferInput{
		VTXO:             desc,
		OwnerLeafScript:  snap.OwnerLeafScript,
		SpendInfo:        spendInfo,
		ConditionWitness: snap.ConditionWitness,
	}, nil
}
