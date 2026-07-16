package virtualchannel

import (
	"fmt"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// CosignInput is the operator-side metadata required to complete one backing
// VTXO input witness.
type CosignInput struct {
	BackingVTXO

	PkScript        []byte
	PolicyTemplate  []byte
	ClientSignature []byte
}

// CosignBackingTx verifies the client's owner signatures, adds the operator's
// signatures, and returns a fully witnessed backing parent.
func CosignBackingTx(signer input.Signer, operatorKey keychain.KeyDescriptor,
	backingTx *wire.MsgTx, inputs []CosignInput) (*wire.MsgTx, error) {

	switch {
	case signer == nil:
		return nil, fmt.Errorf("signer must be provided")

	case operatorKey.PubKey == nil:
		return nil, fmt.Errorf("operator key must be provided")

	case backingTx == nil:
		return nil, fmt.Errorf("backing tx must be provided")

	case len(backingTx.TxIn) == 0:
		return nil, fmt.Errorf("backing tx must have inputs")

	case len(inputs) == 0:
		return nil, fmt.Errorf("cosign inputs must be provided")
	}

	inputByOutpoint := make(map[wire.OutPoint]CosignInput, len(inputs))
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(inputs))
	for _, cosignInput := range inputs {
		if cosignInput.Amount <= 0 {
			return nil, fmt.Errorf("backing VTXO %s amount must "+
				"be positive", cosignInput.OutPoint)
		}
		if len(cosignInput.PkScript) == 0 {
			return nil, fmt.Errorf("backing VTXO %s pkScript "+
				"is empty", cosignInput.OutPoint)
		}
		if len(cosignInput.ClientSignature) == 0 {
			return nil, fmt.Errorf("backing VTXO %s client "+
				"signature is empty", cosignInput.OutPoint)
		}
		if _, ok := inputByOutpoint[cosignInput.OutPoint]; ok {
			return nil, fmt.Errorf("duplicate cosign input %s",
				cosignInput.OutPoint)
		}

		prevOut := &wire.TxOut{
			Value:    int64(cosignInput.Amount),
			PkScript: append([]byte(nil), cosignInput.PkScript...),
		}
		inputByOutpoint[cosignInput.OutPoint] = cosignInput
		prevOuts[cosignInput.OutPoint] = prevOut
	}

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(backingTx, prevFetcher)
	signedTx := backingTx.Copy()

	for inputIndex, txIn := range backingTx.TxIn {
		outpoint := txIn.PreviousOutPoint
		cosignInput, ok := inputByOutpoint[outpoint]
		if !ok {
			return nil, fmt.Errorf("missing cosign input for %s",
				outpoint)
		}

		spendPath, err := operatorCollabSpendPath(
			cosignInput.PolicyTemplate, cosignInput.PkScript,
			operatorKey,
		)
		if err != nil {
			return nil, fmt.Errorf("backing VTXO %s: %w", outpoint,
				err)
		}
		if txIn.Sequence != spendPath.RequiredSequence {
			return nil, fmt.Errorf("backing input %s sequence %d "+
				"does not match collaborative leaf %d",
				outpoint, txIn.Sequence,
				spendPath.RequiredSequence)
		}
		if backingTx.LockTime != spendPath.RequiredLockTime {
			return nil, fmt.Errorf("backing tx locktime %d does "+
				"not match collaborative leaf %d",
				backingTx.LockTime, spendPath.RequiredLockTime)
		}

		clientSig, err := input.ParseSignature(
			cosignInput.ClientSignature,
		)
		if err != nil {
			return nil, fmt.Errorf("parse client signature for "+
				"%s: %w", outpoint, err)
		}

		prevOut := prevOuts[outpoint]
		signDesc := spendPath.SpendInfo.BuildSignDescriptor(
			operatorKey, prevOut, sigHashes, prevFetcher,
			inputIndex,
		)
		operatorSig, err := signer.SignOutputRaw(backingTx, signDesc)
		if err != nil {
			return nil, fmt.Errorf("sign backing input %s: %w",
				outpoint, err)
		}

		witness, err := spendPath.Witness(
			operatorSig.Serialize(), clientSig.Serialize(),
		)
		if err != nil {
			return nil, fmt.Errorf("build witness for %s: %w",
				outpoint, err)
		}
		signedTx.TxIn[inputIndex].Witness = witness

		engine, err := txscript.NewEngine(
			prevOut.PkScript, signedTx, inputIndex,
			txscript.StandardVerifyFlags, nil, sigHashes,
			prevOut.Value, prevFetcher,
		)
		if err != nil {
			return nil, fmt.Errorf("build script engine for %s: %w",
				outpoint, err)
		}
		if err := engine.Execute(); err != nil {
			return nil, fmt.Errorf("verify backing input %s: %w",
				outpoint, err)
		}
	}

	return signedTx, nil
}

func operatorCollabSpendPath(policyTemplate, pkScript []byte,
	operatorKey keychain.KeyDescriptor) (*arkscript.SpendPath, error) {

	template, params, err := standardVTXOPolicy(
		policyTemplate, pkScript,
	)
	if err != nil {
		return nil, err
	}
	if !sameXOnlyPubKey(params.OperatorKey, operatorKey.PubKey) {
		return nil, fmt.Errorf("operator key does not match VTXO " +
			"policy")
	}

	return collabSpendPath(template, params, pkScript)
}
