package checkpoint

import (
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
)

// MinCheckpointCSVDelay is the minimum acceptable checkpoint CSV delay for v0
// OOR checkpoint policies.
const MinCheckpointCSVDelay = uint32(10)

// EncodedLeafPolicy is the serialized semantic policy encoding for one
// checkpoint owner leaf.
type EncodedLeafPolicy = []byte

// Input describes the VTXO input being transformed into a checkpoint output for
// an OOR transfer.
type Input struct {
	// SpentVTXO identifies and describes the VTXO output being spent.
	SpentVTXO SpentVTXORef

	// OwnerLeafScript is the spent VTXO's collaborative leaf script.
	//
	// It is committed into the checkpoint output tap tree together with
	// the operator timeout leaf.
	//
	// This is deliberately a raw script for the draft implementation. Once
	// the closure system is canonical, higher layers should construct this
	// leaf using closure helpers and pass the resulting script bytes here.
	OwnerLeafScript []byte

	// OwnerLeafPolicy is the semantic owner-leaf policy encoding that
	// corresponds to OwnerLeafScript. When present, higher layers can
	// reconstruct the owner leaf without decompiling raw script.
	OwnerLeafPolicy EncodedLeafPolicy
}

// SpentVTXORef groups the spent VTXO outpoint and output data in one value so
// callers cannot accidentally mismatch identity and witness material.
type SpentVTXORef struct {
	// Outpoint is the outpoint of the VTXO output being spent.
	Outpoint wire.OutPoint

	// Output is the previous output being spent (value + pkScript).
	//
	// This must match the server's stored VTXO descriptor later, but at the
	// primitive level we only need it so PSBT has enough material to be
	// signed and validated structurally.
	Output *wire.TxOut
}

// Result is the result of building a checkpoint PSBT.
type Result struct {
	// PSBT is the unsigned checkpoint transaction.
	PSBT *psbt.Packet

	// TapTreeEncoded is the v0 tap tree encoding for the checkpoint output.
	//
	// It mirrors PSBT output metadata so callers can derive tapleaf proofs
	// without re-encoding the tree from the output.
	TapTreeEncoded []byte

	// OwnerLeafScript is the canonical script committed into the checkpoint
	// tree.
	OwnerLeafScript []byte

	// OwnerLeafPolicy is the semantic owner-leaf policy encoding attached
	// to this checkpoint.
	OwnerLeafPolicy EncodedLeafPolicy
}

// BuildPSBT constructs an unsigned checkpoint PSBT that spends a VTXO input,
// pays the entire input value to a checkpoint P2TR output, and appends a
// zero-value anchor output.
//
// The checkpoint output pkScript is derived deterministically from:
//
// - the operator checkpoint policy, and
// - the caller-provided owner leaf script.
//
// This function does not attempt to sign the checkpoint tx. It also does not
// validate that the owner leaf is a canonical Ark closure (draft phase).
func BuildPSBT(policy arkscript.CheckpointPolicy, in Input) (*Result, error) {
	switch {
	case policy.CSVDelay < MinCheckpointCSVDelay:
		return nil, fmt.Errorf("checkpoint csv delay %d below "+
			"minimum %d", policy.CSVDelay, MinCheckpointCSVDelay)

	case in.SpentVTXO.Output == nil:
		return nil, fmt.Errorf("spent output must be provided")

	case in.SpentVTXO.Output.Value <= 0:
		return nil, fmt.Errorf("spent output value must be positive")

	case len(in.SpentVTXO.Output.PkScript) == 0:
		return nil, fmt.Errorf("spent output pkScript must be provided")
	}

	if len(in.OwnerLeafScript) == 0 && len(in.OwnerLeafPolicy) > 0 {
		leaf, err := arkscript.DecodeLeafTemplate(in.OwnerLeafPolicy)
		if err != nil {
			return nil, fmt.Errorf("decode owner leaf policy: %w",
				err)
		}

		in.OwnerLeafScript, err = leaf.Script()
		if err != nil {
			return nil, fmt.Errorf("compile owner leaf policy: %w",
				err)
		}
	}

	tapscript, err := arkscript.CheckpointTapScript(
		policy, in.OwnerLeafScript,
	)
	if err != nil {
		return nil, err
	}

	encodedTapTree, err := EncodeTapTree(tapLeafScripts(tapscript.Leaves))
	if err != nil {
		return nil, err
	}

	tapKey, err := tapscript.TaprootKey()
	if err != nil {
		return nil, fmt.Errorf("unable to compute taproot key: %w", err)
	}

	checkpointPkScript, err := txscript.PayToTaprootScript(tapKey)
	if err != nil {
		return nil, fmt.Errorf("unable to create p2tr script: %w", err)
	}

	tx := wire.NewMsgTx(arktx.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: in.SpentVTXO.Outpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    in.SpentVTXO.Output.Value,
		PkScript: checkpointPkScript,
	})
	tx.AddTxOut(arkscript.AnchorOutput())

	pkt, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint psbt: %w",
			err)
	}

	pkt.Inputs[0].WitnessUtxo = in.SpentVTXO.Output
	pkt.Outputs[0].TaprootTapTree = encodedTapTree

	return &Result{
		PSBT:            pkt,
		TapTreeEncoded:  encodedTapTree,
		OwnerLeafScript: in.OwnerLeafScript,
		OwnerLeafPolicy: in.OwnerLeafPolicy,
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
