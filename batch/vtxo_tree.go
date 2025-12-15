package batch

import (
	"bytes"
	"fmt"
	"iter"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// VTXOBatches returns an iterator that yields each batch of VTXOs along with
// the batch index. VTXOs are split into batches of at most maxPerBatch
// elements. This lets us split large numbers of VTXOs for a single batch into
// multiple trees.
//
// TODO: in future, we might want to make this more sophisticated by grouping
// VTXOs by other criteria such as maximum amount per tree.
func VTXOBatches(vtxos []tree.VTXODescriptor,
	maxPerBatch uint32) (iter.Seq2[int, []tree.VTXODescriptor], error) {

	if maxPerBatch == 0 {
		return nil, fmt.Errorf("maxPerBatch must be greater than 0")
	}

	return func(yield func(int, []tree.VTXODescriptor) bool) {
		batchIdx := 0

		for i := 0; i < len(vtxos); i += int(maxPerBatch) {
			end := min(i+int(maxPerBatch), len(vtxos))
			if !yield(batchIdx, vtxos[i:end]) {
				return
			}
			batchIdx++
		}
	}, nil
}

// TreeContext holds the context needed to build VTXO trees in batches given
// a set of VTXO descriptors, the batch terms, and the batch outputs (ie, the
// outputs spending to the roots of the VTXO trees).
type TreeContext struct {
	// terms holds the batch terms used to create the VTXO trees.
	terms *Terms

	// batches holds the VTXO descriptors and associated batch output for
	// each VTXO tree.
	batches []batchVtxoTree
}

// batchVtxoTree is a helper struct that groups a batch output with its
// corresponding VTXO descriptors.
type batchVtxoTree struct {
	output *wire.TxOut
	vtxos  []tree.VTXODescriptor
}

// BuildTreeContext creates a TreeContext containing batch outputs for VTXO
// trees from VTXO descriptors. It splits VTXOs into multiple batches based on
// the MaxVTXOsPerTree configuration, creating one batch output per tree. The
// context can be used to later build the full VTXO trees once the commitment
// transaction is known.
//
// NOTE: we cannot immediately build the trees here because we don't yet know
// the commitment transaction outpoints that will fund the roots of the trees.
// First, we need to build the batch outputs, then we can create the commitment
// transaction to add them to. At this point we will know the outputs being
// spent by the trees, and can finally build them.
func BuildTreeContext(terms *Terms, vtxos []tree.VTXODescriptor) (*TreeContext,
	error) {

	vtxoBatches, err := VTXOBatches(vtxos, terms.MaxVTXOsPerTree)
	if err != nil {
		return nil, fmt.Errorf("failed to create VTXO batches: %w", err)
	}

	var batches []batchVtxoTree
	for idx, vtxos := range vtxoBatches {
		batchOutput, err := tree.BuildBatchOutput(
			vtxos, terms.OperatorKey.PubKey, terms.SweepKey.PubKey,
			terms.SweepDelay,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to build batch output "+
				"for batch %d: %w", idx, err)
		}

		batches = append(batches, batchVtxoTree{
			output: batchOutput,
			vtxos:  vtxos,
		})
	}

	return &TreeContext{
		terms:   terms,
		batches: batches,
	}, nil
}

// Outputs returns the batch outputs that should be added to the commitment tx.
func (c *TreeContext) Outputs() []*wire.TxOut {
	var outputs []*wire.TxOut
	for _, batch := range c.batches {
		outputs = append(outputs, batch.output)
	}

	return outputs
}

// BuildVTXOTreesForCommitmentTx builds the full VTXO trees for a commitment
// transaction using explicit output indices. It creates trees for each batch
// output, using the actual outpoints from the commitment transaction. The trees
// are indexed by their output index in the commitment transaction.
//
// NOTE: BuildTreeContext would have returned the outputs to be added to the
// commitment transaction and so the caller must ensure that the provided
// batchOutputIndices correspond to those outputs in the commitment tx.
func (c *TreeContext) BuildVTXOTreesForCommitmentTx(tx *wire.MsgTx,
	batchOutputIndices []int) (map[int]*tree.Tree, error) {

	if len(batchOutputIndices) != len(c.batches) {
		return nil, fmt.Errorf("number of batch output indices %d "+
			"does not match number of outputs %d",
			len(batchOutputIndices), len(c.batches))
	}

	vtxoTrees := make(map[int]*tree.Tree)

	for idx, batch := range c.batches {
		outputIdx := batchOutputIndices[idx]

		// Get the batch output from the commitment transaction.
		if outputIdx >= len(tx.TxOut) {
			return nil, fmt.Errorf("batch output index %d exceeds "+
				"commitment tx outputs", outputIdx)
		}
		batchOutput := tx.TxOut[outputIdx]

		// Sanity check: verify the output at this index matches what
		// we expect. This catches mismatches where the caller provided
		// incorrect indices.
		expectedOutput := batch.output
		pkScriptMatch := bytes.Equal(
			batchOutput.PkScript, expectedOutput.PkScript,
		)
		if batchOutput.Value != expectedOutput.Value || !pkScriptMatch {
			return nil, fmt.Errorf("output at index %d does not "+
				"match expected batch output: got value=%d "+
				"pkscript=%x, want value=%d pkscript=%x",
				outputIdx, batchOutput.Value,
				batchOutput.PkScript, expectedOutput.Value,
				expectedOutput.PkScript)
		}

		// Create the batch outpoint from the commitment transaction.
		batchOutpoint := wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: uint32(outputIdx),
		}

		// Build the VTXO tree.
		vtxoTree, err := tree.BuildVTXOTree(
			batchOutpoint, batchOutput, batch.vtxos,
			c.terms.OperatorKey.PubKey, c.terms.SweepKey.PubKey,
			c.terms.SweepDelay, int(c.terms.TreeRadix),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to build VTXO tree for "+
				"output %d: %w", outputIdx, err)
		}

		vtxoTrees[outputIdx] = vtxoTree
	}

	return vtxoTrees, nil
}

// ExtractClientVTXOPaths extracts the relevant VTXO tree paths for a client
// based on their signing keys. For each tree, it identifies which of the
// client's cosigner keys are present in that tree and extracts the minimal
// path containing those keys. This is useful for when the server is
// constructing the precise info to send a specific client.
func ExtractClientVTXOPaths(vtxoTrees map[int]*tree.Tree,
	clientKeys []*btcec.PublicKey) (map[int]*tree.Tree, error) {

	// For each VTXO tree, extract the path for this client's cosigner keys.
	clientPaths := make(map[int]*tree.Tree)
	for treeIdx, vtxoTree := range vtxoTrees {
		clientPath, err := vtxoTree.ExtractPathForCoSigners(
			clientKeys...,
		)
		if err != nil {
			return nil, fmt.Errorf("extract path for tree %d: %w",
				treeIdx, err)
		}

		// If the client has a path in this tree, include it.
		if clientPath != nil {
			clientPaths[treeIdx] = clientPath
		}
	}

	return clientPaths, nil
}
