package unroll

import (
	"sync"
)

// proofNodeFloorAlertDeduper limits the legacy block-1 fallback warning to one
// per unroll registry and process lifetime. Per-target Debug logs retain the
// affected outpoints without repeating the warning for every proof
// transaction.
type proofNodeFloorAlertDeduper struct {
	mu     sync.Mutex
	warned bool
}

// newProofNodeFloorAlertDeduper creates an unused process-local alert gate.
func newProofNodeFloorAlertDeduper() *proofNodeFloorAlertDeduper {
	return &proofNodeFloorAlertDeduper{}
}

// first reports whether no child has produced the registry's fallback warning.
// It records the claim before returning so concurrent children cannot both
// emit it.
func (d *proofNodeFloorAlertDeduper) first() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.warned {
		return false
	}

	d.warned = true

	return true
}
