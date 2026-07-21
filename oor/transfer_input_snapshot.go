package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
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
	OwnerLeafScript []byte

	// OwnerLeafPolicy is the semantic owner-leaf policy encoding that
	// corresponds to OwnerLeafScript.
	OwnerLeafPolicy []byte

	// VTXOPolicyTemplate is the semantic arkscript policy encoding for the
	// spent input VTXO.
	VTXOPolicyTemplate []byte

	// TaprootAssetRoot is the optional root of the Taproot Asset
	// commitment anchored in the spent VTXO.
	TaprootAssetRoot *chainhash.Hash

	// TaprootAssetRef is the opaque SDK-level identity carried by the input
	// VTXO.
	TaprootAssetRef string

	// TaprootAssetAmount is the number of asset units carried by the input
	// VTXO. AmountSat remains the Bitcoin carrier amount.
	TaprootAssetAmount uint64

	// PkScript is the VTXO pkscript. Stored for custom spend paths
	// where the pkscript cannot be derived from keys + exit delay.
	PkScript []byte

	// SpendWitnessScript is the leaf script for custom VTXO types.
	SpendWitnessScript []byte

	// SpendControlBlock is the BIP-341 control block for the custom
	// spend leaf.
	SpendControlBlock []byte

	// ConditionWitness holds extra witness elements (e.g., preimage).
	ConditionWitness [][]byte

	// RequiredSequence is the nSequence value required by a custom spend
	// path. It must survive snapshots so resumed custom OOR spends rebuild
	// byte-identical checkpoint and Ark transactions.
	RequiredSequence uint32

	// RequiredLockTime is the nLockTime value required by a custom spend
	// path. It must survive snapshots together with RequiredSequence for
	// CLTV-gated leaves such as vHTLC refunds.
	RequiredLockTime uint32

	// ExternalSignatures are pre-collected tapscript signatures needed to
	// resume custom OOR spends after restart.
	ExternalSignatures []ExternalTaprootScriptSignature
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

	snap := &TransferInputSnapshot{
		Outpoint:           i.VTXO.Outpoint,
		AmountSat:          int64(i.VTXO.Amount),
		ClientKeyFamily:    int32(i.VTXO.ClientKey.KeyLocator.Family),
		ClientKeyIndex:     i.VTXO.ClientKey.KeyLocator.Index,
		OperatorPubKey:     operatorKey.SerializeCompressed(),
		ExitDelay:          exitDelay,
		OwnerLeafScript:    i.OwnerLeafScript,
		OwnerLeafPolicy:    i.OwnerLeafPolicy,
		VTXOPolicyTemplate: i.VTXOPolicyTemplate,
		PkScript:           i.VTXO.PkScript,
	}
	if i.TaprootAssetRoot != nil {
		root := *i.TaprootAssetRoot
		snap.TaprootAssetRoot = &root
	}
	snap.TaprootAssetRef = i.VTXO.TaprootAssetRef
	snap.TaprootAssetAmount = i.VTXO.TaprootAssetAmount

	if i.VTXO.ClientKey.PubKey != nil {
		snap.ClientPubKey =
			i.VTXO.ClientKey.PubKey.SerializeCompressed()
	}

	if i.CustomSpend != nil {
		snap.ConditionWitness = i.CustomSpend.Conditions

		if i.CustomSpend.SpendInfo != nil {
			snap.SpendWitnessScript =
				i.CustomSpend.SpendInfo.WitnessScript
			snap.SpendControlBlock =
				i.CustomSpend.SpendInfo.ControlBlock
		}

		snap.RequiredSequence = i.CustomSpend.RequiredSequence
		snap.RequiredLockTime = i.CustomSpend.RequiredLockTime
	}
	snap.ExternalSignatures = cloneExternalSignatures(i.ExternalSignatures)

	return snap, nil
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

	if len(snap.OwnerLeafScript) == 0 && len(snap.OwnerLeafPolicy) == 0 {
		return TransferInput{}, fmt.Errorf("owner leaf script or " +
			"policy must be provided")
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

	tapScript, err := arkscript.VTXOTapScript(
		clientPub, operatorPub, snap.ExitDelay,
	)
	if err != nil {
		return TransferInput{}, fmt.Errorf("rebuild vtxo tapscript: %w",
			err)
	}

	tapKey, err := arkscript.VTXOTapKey(
		clientPub, operatorPub, snap.ExitDelay,
	)
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
		Outpoint:       snap.Outpoint,
		Amount:         btcutil.Amount(snap.AmountSat),
		PolicyTemplate: bytes.Clone(snap.VTXOPolicyTemplate),
		PkScript:       pkScript,
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(
					snap.ClientKeyFamily,
				),
				Index: snap.ClientKeyIndex,
			},
			PubKey: clientPub,
		},
		TaprootAssetRoot:   snap.TaprootAssetRoot,
		TaprootAssetRef:    snap.TaprootAssetRef,
		TaprootAssetAmount: snap.TaprootAssetAmount,
		OperatorKey:        operatorPub,
		TapScript:          tapScript,
		RelativeExpiry:     snap.ExitDelay,
		Status:             vtxo.VTXOStatusLive,
	}

	result := TransferInput{
		VTXO:               desc,
		OwnerLeafScript:    snap.OwnerLeafScript,
		OwnerLeafPolicy:    snap.OwnerLeafPolicy,
		VTXOPolicyTemplate: snap.VTXOPolicyTemplate,
		TaprootAssetRoot:   snap.TaprootAssetRoot,
	}

	if len(snap.SpendWitnessScript) > 0 {
		result.CustomSpend = &arkscript.SpendPath{
			SpendInfo: &arkscript.SpendInfo{
				WitnessScript: snap.SpendWitnessScript,
				ControlBlock:  snap.SpendControlBlock,
			},
			Conditions:       snap.ConditionWitness,
			RequiredSequence: snap.RequiredSequence,
			RequiredLockTime: snap.RequiredLockTime,
		}
	}
	result.ExternalSignatures = cloneExternalSignatures(
		snap.ExternalSignatures,
	)

	// For custom spend paths, use the stored PkScript instead of
	// deriving from keys.
	if len(snap.PkScript) > 0 {
		desc.PkScript = snap.PkScript
	}

	return result, nil
}

// cloneExternalSignatures deep-copies custom input external signatures.
func cloneExternalSignatures(
	sigs []ExternalTaprootScriptSignature,
) []ExternalTaprootScriptSignature {

	if len(sigs) == 0 {
		return nil
	}

	result := make([]ExternalTaprootScriptSignature, len(sigs))
	for i := range sigs {
		// btcec.PublicKey is treated as immutable; clone the mutable
		// byte slices while sharing the parsed key object.
		result[i] = ExternalTaprootScriptSignature{
			PubKey:        sigs[i].PubKey,
			WitnessScript: bytes.Clone(sigs[i].WitnessScript),
			Signature:     bytes.Clone(sigs[i].Signature),
			SigHash:       sigs[i].SigHash,
		}
	}

	return result
}
