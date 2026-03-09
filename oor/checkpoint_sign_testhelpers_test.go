package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// coSignCheckpointPSBTsForTest attaches operator signatures to checkpoint
// PSBTs using the spend path described by in.SpendInfo. Tests use this to
// model submit-accepted artifacts returned by the server.
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
		if in.VTXO == nil {
			return fmt.Errorf("transfer input must include vtxo")
		}

		err := in.Validate()
		if err != nil {
			return err
		}

		if in.SpendInfo == nil {
			return fmt.Errorf("spend info must be provided")
		}

		inputByOutpoint[in.VTXO.Outpoint] = in
	}

	for i := range checkpoints {
		checkpoint := checkpoints[i]
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf(
				"checkpoint psbt must include unsigned tx",
			)
		}

		if len(checkpoint.UnsignedTx.TxIn) != 1 ||
			len(checkpoint.Inputs) != 1 {

			return fmt.Errorf(
				"checkpoint must have exactly one input",
			)
		}

		prevOutpoint := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
		in := inputByOutpoint[prevOutpoint]
		if in == nil {
			return fmt.Errorf(
				"unknown checkpoint input outpoint %s",
				prevOutpoint,
			)
		}

		vtxo := in.VTXO
		if vtxo == nil || vtxo.OperatorKey == nil {
			return fmt.Errorf("operator key must be provided")
		}

		prevOut := checkpoint.Inputs[0].WitnessUtxo
		if prevOut == nil {
			return fmt.Errorf(
				"checkpoint must include witness utxo",
			)
		}

		prevFetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, prevOut.Value,
		)
		sigHashes := txscript.NewTxSigHashes(
			checkpoint.UnsignedTx, prevFetcher,
		)

		// Use the caller-supplied spend path bytes directly so the
		// co-signer does not need to reconstruct the tapscript tree.
		leafScript := in.SpendInfo.WitnessScript
		signDesc := &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: vtxo.OperatorKey,
			},
			SignMethod:        input.TaprootScriptSpendSignMethod,
			Output:            prevOut,
			HashType:          txscript.SigHashDefault,
			SigHashes:         sigHashes,
			PrevOutputFetcher: prevFetcher,
			InputIndex:        0,
			WitnessScript:     leafScript,
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

		pInput := &checkpoint.Inputs[0]

		err = addTapLeafScriptRaw(
			pInput,
			in.SpendInfo.WitnessScript,
			in.SpendInfo.ControlBlock,
		)
		if err != nil {
			return err
		}

		err = psbtutil.AddTaprootScriptSpendSig(
			pInput,
			vtxo.OperatorKey,
			leafScript,
			sigBytes,
			signDesc.HashType,
		)
		if err != nil {
			return err
		}
	}

	return nil
}
