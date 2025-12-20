package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
)

// SignCheckpointPSBTs attaches the client-side collaborative VTXO spend
// signatures to each checkpoint PSBT.
//
// Each checkpoint PSBT is expected to spend exactly one VTXO (input index 0).
// The TransferInput slice is expected to match the checkpoint PSBT slice
// 1:1.
func SignCheckpointPSBTs(signer input.Signer, inputs []TransferInput,
	checkpoints []*psbt.Packet) error {

	switch {
	case signer == nil:
		return fmt.Errorf("signer must be provided")

	case len(inputs) == 0:
		return fmt.Errorf("transfer inputs must be provided")

	case len(checkpoints) == 0:
		return fmt.Errorf("checkpoint psbts must be provided")

	case len(inputs) != len(checkpoints):
		return fmt.Errorf("input count %d does not match checkpoint "+
			"count %d", len(inputs), len(checkpoints))
	}

	for i := range inputs {
		err := signCheckpointPSBT(signer, &inputs[i], checkpoints[i])
		if err != nil {
			return fmt.Errorf("sign checkpoint %d: %w", i, err)
		}
	}

	return nil
}

// signCheckpointPSBT signs checkpoint input 0 with the client key for the
// collaborative VTXO leaf path.
func signCheckpointPSBT(signer input.Signer, in *TransferInput,
	checkpoint *psbt.Packet) error {

	switch {
	case signer == nil:
		return fmt.Errorf("signer must be provided")

	case in == nil:
		return fmt.Errorf("transfer input must be provided")

	case checkpoint == nil || checkpoint.UnsignedTx == nil:
		return fmt.Errorf("checkpoint psbt must include unsigned tx")

	case len(checkpoint.Inputs) == 0:
		return fmt.Errorf("checkpoint psbt must have inputs")
	}

	err := in.Validate()
	if err != nil {
		return err
	}

	if len(checkpoint.UnsignedTx.TxIn) != 1 ||
		len(checkpoint.Inputs) != 1 {

		return fmt.Errorf("checkpoint psbt must have exactly one "+
			"input, got tx=%d psbt=%d",
			len(checkpoint.UnsignedTx.TxIn),
			len(checkpoint.Inputs))
	}

	prevOut := &wire.TxOut{
		Value:    int64(in.VTXO.Amount),
		PkScript: in.VTXO.PkScript,
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)

	sigHashes := txscript.NewTxSigHashes(
		checkpoint.UnsignedTx, prevFetcher,
	)

	signDesc, spendInfo, err := tx.NewVTXOCollabSignDescriptor(
		&tx.VTXOSpendContext{
			Outpoint:  in.VTXO.Outpoint,
			Output:    prevOut,
			TapScript: in.VTXO.TapScript,
		},
		in.VTXO.ClientKey,
		0,
		sigHashes,
		prevFetcher,
	)
	if err != nil {
		return err
	}

	sig, err := signer.SignOutputRaw(checkpoint.UnsignedTx, signDesc)
	if err != nil {
		return fmt.Errorf("sign output: %w", err)
	}

	sigBytes := sig.Serialize()
	if len(sigBytes) == 0 {
		return fmt.Errorf("signer returned empty signature")
	}

	err = psbtutil.AddTapLeafScript(&checkpoint.Inputs[0], spendInfo)
	if err != nil {
		return err
	}

	return psbtutil.AddTaprootScriptSpendSig(
		&checkpoint.Inputs[0], in.VTXO.ClientKey.PubKey,
		spendInfo.WitnessScript, sigBytes, signDesc.HashType,
	)
}
