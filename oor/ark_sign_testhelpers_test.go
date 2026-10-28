package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// coSignArkPSBTForTest attaches the operator half of each Ark input's
// collaborative witness to a client-signed test package.
func coSignArkPSBTForTest(signer input.Signer, operatorKey *btcec.PublicKey,
	ark *psbt.Packet) error {

	if signer == nil || operatorKey == nil || ark == nil ||
		ark.UnsignedTx == nil {
		return fmt.Errorf("signer, operator key and ark psbt required")
	}

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(ark.Inputs))
	for i, txIn := range ark.UnsignedTx.TxIn {
		prevOuts[txIn.PreviousOutPoint] = ark.Inputs[i].WitnessUtxo
	}
	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(ark.UnsignedTx, prevFetcher)

	for i := range ark.Inputs {
		in := &ark.Inputs[i]
		if len(in.TaprootLeafScript) != 1 || in.WitnessUtxo == nil {
			return fmt.Errorf("ark input %d missing test leaf "+
				"or prevout", i)
		}
		leaf := in.TaprootLeafScript[0]
		signDesc := &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: operatorKey,
			},
			SignMethod:        input.TaprootScriptSpendSignMethod,
			Output:            in.WitnessUtxo,
			HashType:          txscript.SigHashDefault,
			SigHashes:         sigHashes,
			PrevOutputFetcher: prevFetcher,
			InputIndex:        i,
			WitnessScript:     leaf.Script,
			ControlBlock:      leaf.ControlBlock,
		}
		sig, err := signer.SignOutputRaw(ark.UnsignedTx, signDesc)
		if err != nil {
			return fmt.Errorf("sign ark input %d: %w", i, err)
		}
		err = psbtutil.AddTaprootScriptSpendSig(
			in, operatorKey, leaf.Script, sig.Serialize(),
			signDesc.HashType,
		)
		if err != nil {
			return fmt.Errorf("attach ark input %d signature: %w",
				i, err)
		}
	}

	return nil
}
