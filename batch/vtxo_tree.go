package batch

import (
	"fmt"
	"iter"

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
