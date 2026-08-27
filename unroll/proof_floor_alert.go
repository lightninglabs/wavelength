package unroll

import (
	"sync"

	"github.com/btcsuite/btcd/chainhash/v2"
)

// proofNodeFloorAlertDeduper limits the confirmation-floor warning to one
// alert per proof transaction and process lifetime. Multiple target VTXOs can
// share the same proof ancestor, so per-target warnings describe one recovery
// condition and create redundant alert instances.
type proofNodeFloorAlertDeduper struct {
	mu   sync.Mutex
	seen map[chainhash.Hash]struct{}
}

// newProofNodeFloorAlertDeduper creates an empty process-local alert set.
func newProofNodeFloorAlertDeduper() *proofNodeFloorAlertDeduper {
	return &proofNodeFloorAlertDeduper{
		seen: make(map[chainhash.Hash]struct{}),
	}
}

// first reports whether txid has not produced a warning through this deduper.
// It records txid before returning so concurrent child actors cannot both
// claim the first alert.
func (d *proofNodeFloorAlertDeduper) first(txid chainhash.Hash) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.seen[txid]; ok {
		return false
	}

	d.seen[txid] = struct{}{}

	return true
}
