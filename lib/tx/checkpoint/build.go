package checkpoint

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
)

// Input describes the VTXO input being transformed into a checkpoint output for
// an OOR transfer.
type Input struct {
	// Outpoint is the outpoint of the VTXO output being spent.
	Outpoint wire.OutPoint

	// WitnessUtxo is the previous output being spent (value + pkScript).
	//
	// This must match the server's stored VTXO descriptor later, but at the
	// primitive level we only need it so PSBT has enough material to be
	// signed and validated structurally.
	WitnessUtxo *wire.TxOut

	// OwnerLeafScript is the owner-controlled collaborative leaf script.
	// It should be committed to in the checkpoint output tap tree.
	//
	// This is deliberately a raw script for the draft implementation. Once
	// the closure system is canonical, higher layers should construct this
	// leaf using closure helpers and pass the resulting script bytes here.
	OwnerLeafScript []byte
}

// Result is the result of building a checkpoint PSBT.
type Result struct {
	// PSBT is the unsigned checkpoint transaction.
	PSBT *psbt.Packet

	// TapTreeEncoded is the v0 tap tree encoding for the checkpoint output.
	//
	// This is intended to be attached to the Ark tx PSBT inputs under the
	// `taptree` unknown key so finalization can later copy it onto the
	// checkpoint output metadata.
	TapTreeEncoded []byte
}

// BuildPSBT constructs an unsigned checkpoint PSBT that spends a VTXO input and
// pays the entire input value to a checkpoint P2TR output.
//
// The checkpoint output pkScript is derived deterministically from:
//
// - the operator checkpoint policy, and
// - the caller-provided owner leaf script.
//
// This function does not attempt to sign the checkpoint tx. It also does not
// validate that the owner leaf is a canonical Ark closure (draft phase).
func BuildPSBT(policy scripts.CheckpointPolicy, in Input) (*Result, error) {
	switch {
	case in.WitnessUtxo == nil:
		return nil, fmt.Errorf("witness utxo must be provided")

	case in.WitnessUtxo.Value <= 0:
		return nil, fmt.Errorf("witness utxo value must be " +
			"positive")

	case len(in.WitnessUtxo.PkScript) == 0:
		return nil, fmt.Errorf("witness utxo pkScript must be " +
			"provided")
	}

	tapscript, err := scripts.CheckpointTapScript(
		policy, in.OwnerLeafScript,
	)
	if err != nil {
		return nil, err
	}

	encodedTapTree, err := EncodeTapTree(tapLeafScripts(tapscript.Leaves))
	if err != nil {
		return nil, err
	}

	checkpointPkScript, err := scripts.CheckpointPkScript(
		policy, in.OwnerLeafScript,
	)
	if err != nil {
		return nil, err
	}

	tx := wire.NewMsgTx(arktx.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: in.Outpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    in.WitnessUtxo.Value,
		PkScript: checkpointPkScript,
	})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint psbt: %w",
			err)
	}

	pkt.Inputs[0].WitnessUtxo = in.WitnessUtxo

	return &Result{
		PSBT:           pkt,
		TapTreeEncoded: encodedTapTree,
	}, nil
}

// tapLeafScripts extracts raw script bytes from a list of tap leaves.
func tapLeafScripts(leaves []txscript.TapLeaf) [][]byte {
	scripts := make([][]byte, 0, len(leaves))
	for _, leaf := range leaves {
		scripts = append(scripts, leaf.Script)
	}

	return scripts
}
