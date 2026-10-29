package unroll

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
)

// SignedAncestry contains the validated transactions an external broadcaster
// needs to materialize a VTXO. It contains neither private keys nor a final
// sweep. The external inputs are local assertions about commitment outputs;
// a broadcaster must bind them to its own view of the Bitcoin chain.
type SignedAncestry struct {
	Proof          *recovery.Proof
	ExternalInputs []AncestryInput
}

// AncestryInput identifies an output outside the proof graph consumed by it.
type AncestryInput struct {
	Outpoint wire.OutPoint
	Output   *wire.TxOut
}

// BuildSignedAncestry assembles and verifies every input witness against its
// previous output. Unlike the internal planner, this export boundary refuses
// unsigned transactions. It performs no chain I/O, funding, or signing.
func BuildSignedAncestry(mat *LineageMaterial) (*SignedAncestry, error) {
	proof, err := BuildProofFromMaterial(mat)
	if err != nil {
		return nil, err
	}

	// Include the commitment outputs so an independent executor can verify
	// root signatures without the user's wallet database. Reject conflicts
	// rather than allowing the order of ancestry fragments to choose one.
	external := make(map[wire.OutPoint]*wire.TxOut)
	for _, path := range mat.TreePaths {
		output := path.BatchOutput
		if output == nil || output.Value <= 0 ||
			output.Value > btcutil.MaxSatoshi ||
			len(output.PkScript) == 0 {
			return nil, fmt.Errorf("%w: missing or invalid "+
				"commitment output %s", ErrUnrollProofInvalid,
				path.BatchOutpoint)
		}

		previous, ok := external[path.BatchOutpoint]
		if ok && (previous.Value != output.Value ||
			!bytes.Equal(previous.PkScript, output.PkScript)) {
			return nil, fmt.Errorf("%w: conflicting commitment "+
				"output %s", ErrUnrollProofInvalid,
				path.BatchOutpoint)
		}
		external[path.BatchOutpoint] = output
	}

	result := &SignedAncestry{Proof: proof}
	exportedInputs := make(map[wire.OutPoint]bool)

	// Resolve every prevout before constructing sighash caches, which
	// require complete input data even when validating only one input.
	for _, layer := range proof.Layers() {
		for _, txid := range layer {
			node, _ := proof.Node(txid)
			if err := blockchain.CheckTransactionSanity(
				btcutil.NewTx(node.Tx),
			); err != nil {
				return nil, fmt.Errorf("%w: transaction %s: %w",
					ErrUnrollProofInvalid, txid, err)
			}
			prevouts := make(map[wire.OutPoint]*wire.TxOut)
			var inputValue int64
			for i, input := range node.Tx.TxIn {
				outpoint := input.PreviousOutPoint
				output := external[outpoint]
				if parent, ok := proof.Node(outpoint.Hash); ok {
					output, err = parent.Output(
						outpoint.Index,
					)
					if err != nil {
						return nil, err
					}
				} else if output != nil &&
					!exportedInputs[outpoint] {

					result.ExternalInputs = append(
						result.ExternalInputs,
						AncestryInput{
							Outpoint: outpoint,
							Output:   output,
						},
					)
					exportedInputs[outpoint] = true
				}
				if output == nil || len(input.Witness) == 0 {
					return nil, fmt.Errorf("%w: "+
						"transaction %s input %d "+
						"missing prevout or witness",
						ErrUnrollProofInvalid, txid, i)
				}
				inputValue += output.Value
				if inputValue > btcutil.MaxSatoshi {
					return nil, fmt.Errorf("%w: "+
						"transaction %s input value "+
						"exceeds maximum",
						ErrUnrollProofInvalid, txid)
				}
				prevouts[outpoint] = output
			}
			for _, output := range node.Tx.TxOut {
				inputValue -= output.Value
			}
			if inputValue < 0 {
				return nil, fmt.Errorf("%w: transaction %s "+
					"spends more than its inputs",
					ErrUnrollProofInvalid, txid)
			}

			if err := verifyTxWitnesses(
				node.Tx, prevouts,
			); err != nil {
				return nil, fmt.Errorf("%w: transaction %s: %w",
					ErrUnrollProofInvalid, txid, err)
			}
		}
	}

	slices.SortFunc(result.ExternalInputs, func(a, b AncestryInput) int {
		if order := bytes.Compare(
			a.Outpoint.Hash[:], b.Outpoint.Hash[:],
		); order != 0 {
			return order
		}

		return cmp.Compare(a.Outpoint.Index, b.Outpoint.Index)
	})

	return result, nil
}
