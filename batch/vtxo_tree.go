package batch

import (
	"fmt"
	"iter"

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

	// batches holds the VTXO descriptors for each batch/tree.
	// The number of batches matches the number of outputs.
	batches [][]tree.VTXODescriptor

	// outputs holds the batch outputs spending to the roots of each VTXO
	// tree. The number of outputs matches the number of batches.
	outputs []*wire.TxOut
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

	var (
		batchOutputs []*wire.TxOut
		vtxoBatches  [][]tree.VTXODescriptor
	)

	batches, err := VTXOBatches(vtxos, terms.MaxVTXOsPerTree)
	if err != nil {
		return nil, fmt.Errorf("failed to create VTXO batches: %w", err)
	}

	for idx, vtxos := range batches {
		batchOutput, err := tree.BuildBatchOutput(
			vtxos, terms.OperatorKey.PubKey,
			terms.SweepKey.PubKey, terms.SweepDelay,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to build batch output "+
				"for batch %d: %w", idx, err)
		}

		batchOutputs = append(batchOutputs, batchOutput)
		vtxoBatches = append(vtxoBatches, vtxos)
	}

	return &TreeContext{
		terms:   terms,
		batches: vtxoBatches,
		outputs: batchOutputs,
	}, nil
}

// Outputs returns the batch outputs that should be added to the commitment tx.
func (c *TreeContext) Outputs() []*wire.TxOut {
	return c.outputs
}
