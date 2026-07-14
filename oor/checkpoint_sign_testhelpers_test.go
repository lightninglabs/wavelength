package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tx"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// coSignCheckpointPSBTsForTest attaches operator collaborative signatures to
// checkpoint PSBTs. Tests use this to model submit-accepted artifacts returned
// by the server.
func coSignCheckpointPSBTsForTest(signer input.Signer, inputs []TransferInput,
	checkpoints []*psbt.Packet) error {

	if signer == nil {
		return fmt.Errorf("signer must be provided")
	}

	if len(inputs) == 0 {
		return fmt.Errorf("transfer inputs must be provided")
	}

	if len(checkpoints) == 0 {
		return fmt.Errorf("checkpoint psbts must be provided")
	}

	inputByOutpoint := make(map[wire.OutPoint]*TransferInput, len(inputs))
	for i := range inputs {
		in := &inputs[i]
		if in == nil || in.VTXO == nil {
			return fmt.Errorf("transfer input must include vtxo")
		}

		err := in.Validate()
		if err != nil {
			return err
		}

		inputByOutpoint[in.VTXO.Outpoint] = in
	}

	for i := range checkpoints {
		checkpoint := checkpoints[i]
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf("checkpoint psbt must include " +
				"unsigned tx")
		}

		if len(checkpoint.UnsignedTx.TxIn) != 1 ||
			len(checkpoint.Inputs) != 1 {
			return fmt.Errorf("checkpoint must have exactly one " +
				"input")
		}

		prevOutpoint := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
		in := inputByOutpoint[prevOutpoint]
		if in == nil {
			return fmt.Errorf("unknown checkpoint input "+
				"outpoint %s", prevOutpoint)
		}

		vtxo := in.VTXO
		if vtxo == nil || vtxo.OperatorKey == nil {
			return fmt.Errorf("operator key must be provided")
		}

		prevOut := checkpoint.Inputs[0].WitnessUtxo
		if prevOut == nil {
			return fmt.Errorf("checkpoint must include witness " +
				"utxo")
		}

		prevFetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, prevOut.Value,
		)
		sigHashes := txscript.NewTxSigHashes(
			checkpoint.UnsignedTx, prevFetcher,
		)

		signDesc, spendInfo, err := tx.NewVTXOCollabSignDescriptor(
			&tx.VTXOSpendContext{
				Outpoint:  vtxo.Outpoint,
				Output:    prevOut,
				TapScript: vtxo.TapScript,
			},
			keychain.KeyDescriptor{PubKey: vtxo.OperatorKey},
			0,
			sigHashes,
			prevFetcher,
		)
		if err != nil {
			return err
		}

		sig, err := signer.SignOutputRaw(
			checkpoint.UnsignedTx, signDesc,
		)
		if err != nil {
			return err
		}

		sigBytes := sig.Serialize()
		if len(sigBytes) == 0 {
			return fmt.Errorf("signer returned empty signature")
		}

		input := &checkpoint.Inputs[0]

		err = psbtutil.AddTapLeafScript(input, spendInfo)
		if err != nil {
			return err
		}

		err = psbtutil.AddTaprootScriptSpendSig(
			input, vtxo.OperatorKey, spendInfo.WitnessScript,
			sigBytes, signDesc.HashType,
		)
		if err != nil {
			return err
		}
	}

	return nil
}
