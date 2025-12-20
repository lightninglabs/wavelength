package oor

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
)

// CheckpointInput describes the VTXO input being transformed into a checkpoint
// output for an OOR transfer.
type CheckpointInput struct {
	// Outpoint is the outpoint of the VTXO output being spent.
	Outpoint wire.OutPoint

	// WitnessUtxo is the previous output being spent (value + pkScript).
	//
	// This must match the server's stored VTXO descriptor later, but at the
	// primitive level we only need it so PSBT has enough material to be
	// signed and validated structurally.
	WitnessUtxo *wire.TxOut

	// OwnerLeafScript is the VTXO-owner collaborative leaf script.
	//
	// "Owner" here means owner of the spent VTXO input, not owner of the
	// checkpoint CSV timeout path.
	//
	// The script should be committed to in the checkpoint output tap tree.
	//
	// This is deliberately a raw script for the draft implementation. Once
	// the closure system is canonical, higher layers should construct this
	// leaf using closure helpers and pass the resulting script bytes here.
	OwnerLeafScript []byte
}

// CheckpointResult is the result of building a checkpoint PSBT.
type CheckpointResult struct {
	// PSBT is the unsigned checkpoint transaction.
	PSBT *psbt.Packet

	// TapTreeEncoded is the v0 tap tree encoding for the checkpoint output.
	//
	// This is intended to be attached to the Ark tx PSBT inputs under the
	// `taptree` unknown key so finalization can later copy it onto the
	// checkpoint output metadata.
	TapTreeEncoded []byte
}

// RecipientOutput describes an Ark tx recipient output.
type RecipientOutput struct {
	// PkScript is the destination script.
	PkScript []byte

	// Value is the amount to send in satoshis.
	Value btcutil.Amount
}

// BuildCheckpointPSBT constructs an unsigned checkpoint PSBT that spends a VTXO
// input and pays the entire input value to a checkpoint P2TR output.
//
// The checkpoint output pkScript is derived deterministically from:
//
// - the operator checkpoint policy, and
// - the caller-provided VTXO-owner collaborative leaf script.
//
// This function does not attempt to sign the checkpoint tx. It also does not
// validate that the owner leaf is a canonical Ark closure (draft phase).
func BuildCheckpointPSBT(policy scripts.CheckpointPolicy,
	in CheckpointInput) (*CheckpointResult, error) {

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

	// Use v3 to be compatible with package relay policies (TRUC-style
	// constraints) when these txs are eventually submitted as a package.
	tx := wire.NewMsgTx(3)
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

	return &CheckpointResult{
		PSBT:           pkt,
		TapTreeEncoded: encodedTapTree,
	}, nil
}

// CheckpointOutput describes a checkpoint output that will be spent by the Ark
// transaction.
type CheckpointOutput struct {
	// Txid is the txid of the checkpoint transaction.
	Txid chainhash.Hash

	// Output is the checkpoint output being spent (value + pkScript).
	Output *wire.TxOut

	// TapTreeEncoded is the v0 tap tree encoding for the checkpoint output.
	TapTreeEncoded []byte
}

// BuildArkPSBT constructs a deterministic Ark tx PSBT spending the set of
// checkpoint outputs and producing the requested recipient outputs plus an
// anchor output.
//
// This is a v0 builder and enforces:
//
// - fee-less transfers (sum(inputs) == sum(outputs excluding anchor)),
// - anchor output is last output (P2A, value 0), and
// - canonical ordering rules for inputs/outputs (BIP69),
//
// It also attaches per-input `taptree` metadata using TapTreePSBTKey so the
// finalize step can later bind tap tree data onto checkpoint PSBT outputs.
func BuildArkPSBT(checkpoints []CheckpointOutput,
	recipients []RecipientOutput) (*psbt.Packet, error) {

	if len(checkpoints) == 0 {
		return nil, fmt.Errorf("checkpoint outputs must be provided")
	}

	if len(recipients) == 0 {
		return nil, fmt.Errorf("recipient outputs must be provided")
	}

	var sumInputs btcutil.Amount
	for _, cp := range checkpoints {
		if cp.Output == nil {
			return nil, fmt.Errorf(
				"checkpoint output must be provided",
			)
		}

		if len(cp.Output.PkScript) == 0 {
			return nil, fmt.Errorf("checkpoint pkScript must be " +
				"provided")
		}

		if cp.Output.Value <= 0 {
			return nil, fmt.Errorf("checkpoint output value must " +
				"be positive")
		}

		sumInputs += btcutil.Amount(cp.Output.Value)
	}

	var sumOutputs btcutil.Amount
	for _, out := range recipients {
		if len(out.PkScript) == 0 {
			return nil, fmt.Errorf("recipient pkScript must be " +
				"provided")
		}

		if out.Value <= 0 {
			return nil, fmt.Errorf("recipient value must be " +
				"positive")
		}

		sumOutputs += out.Value
	}

	if sumInputs != sumOutputs {
		return nil, fmt.Errorf("fee-less ark tx requires equal " +
			"input/output sums")
	}

	// Sort checkpoint inputs by outpoint (BIP69-style) to ensure
	// deterministic input order.
	checkpointsSorted := make([]CheckpointOutput, len(checkpoints))
	copy(checkpointsSorted, checkpoints)
	sort.SliceStable(checkpointsSorted, func(i, j int) bool {
		a := checkpointsSorted[i]
		b := checkpointsSorted[j]

		cmp := bytes.Compare(a.Txid[:], b.Txid[:])
		if cmp != 0 {
			return cmp < 0
		}

		// v0 always spends vout=0.
		return false
	})

	recipientOuts := make([]RecipientOutput, len(recipients))
	copy(recipientOuts, recipients)
	sort.SliceStable(recipientOuts, func(i, j int) bool {
		a := recipientOuts[i]
		b := recipientOuts[j]

		if a.Value != b.Value {
			return a.Value < b.Value
		}

		return bytes.Compare(a.PkScript, b.PkScript) < 0
	})

	// Use v3 to be compatible with package relay policies (TRUC-style
	// constraints) when this tx is submitted as part of a package.
	tx := wire.NewMsgTx(3)
	for _, cp := range checkpointsSorted {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  cp.Txid,
				Index: 0,
			},
			Sequence: wire.MaxTxInSequenceNum,
		})
	}

	for _, out := range recipientOuts {
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(out.Value),
			PkScript: out.PkScript,
		})
	}

	tx.AddTxOut(scripts.AnchorOutput())

	err := ValidateCanonicalArkTx(tx)
	if err != nil {
		return nil, fmt.Errorf("internal: built ark tx is not "+
			"canonical: %w", err)
	}

	pkt, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("unable to create ark psbt: %w", err)
	}

	// Attach witness UTXOs and tap tree metadata in the same order as
	// inputs.
	for i := range checkpointsSorted {
		cp := checkpointsSorted[i]

		pkt.Inputs[i].WitnessUtxo = cp.Output

		if len(cp.TapTreeEncoded) == 0 {
			return nil, fmt.Errorf("checkpoint tap tree must be " +
				"provided")
		}

		err := PutTapTreePSBTInput(pkt, i, cp.TapTreeEncoded)
		if err != nil {
			return nil, err
		}
	}

	return pkt, nil
}

// tapLeafScripts extracts raw script bytes from a list of tap leaves.
func tapLeafScripts(leaves []txscript.TapLeaf) [][]byte {
	scripts := make([][]byte, 0, len(leaves))
	for _, leaf := range leaves {
		scripts = append(scripts, leaf.Script)
	}

	return scripts
}
