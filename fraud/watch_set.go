package fraud

import "github.com/btcsuite/btcd/wire/v2"

// ancestorWatch is the per-outpoint state the watcher keeps for one
// ancestor it is monitoring on chain. The targets set doubles as a
// reference count: a chainsource spend watch stays armed for
// outpoint while len(targets) > 0, and is unregistered when the set
// drains to empty.
type ancestorWatch struct {
	// point captures the chainsource registration parameters
	// (pkScript and height hint) used when the watch was first armed.
	// Kept alongside the targets set so the watcher can rebuild the
	// matching UnregisterSpendRequest when the last target releases
	// (or at shutdown).
	point WatchPoint

	// targets is the set of recipient VTXOs that share interest in
	// this ancestor outpoint. Each target retains the watch exactly
	// once, so len(targets) is the watcher's reference count for
	// outpoint.
	targets map[wire.OutPoint]struct{}
}

// ancestorWatches collects every ancestor outpoint the watcher is
// monitoring. The collection encapsulates the refcounted-watch
// bookkeeping so the surrounding actor never has to keep parallel
// maps in sync.
type ancestorWatches map[wire.OutPoint]*ancestorWatch

// newAncestorWatches returns an empty collection ready for use.
func newAncestorWatches() ancestorWatches {
	return make(ancestorWatches)
}

// firstRefFor reports whether outpoint is currently untracked, i.e.
// the next addTarget on it will be the first reference. Callers use
// this to gate a chainsource registerSpendWatch call so the watch is
// armed exactly once for each distinct ancestor outpoint.
func (w ancestorWatches) firstRefFor(outpoint wire.OutPoint) bool {
	return w[outpoint] == nil
}

// addTarget records target as interested in point.Outpoint. Creates
// the entry on the first reference. Idempotent for repeated calls by
// the same target (the set semantics deduplicate).
func (w ancestorWatches) addTarget(point WatchPoint, target wire.OutPoint) {
	aw, ok := w[point.Outpoint]
	if !ok {
		aw = &ancestorWatch{
			point:   point,
			targets: make(map[wire.OutPoint]struct{}),
		}
		w[point.Outpoint] = aw
	}
	aw.targets[target] = struct{}{}
}

// dropTarget removes target from the watch for outpoint. Returns the
// stored WatchPoint and true when the watch becomes empty: the caller
// should unregister the chainsource spend watch using that point. The
// entry is removed from the collection before the return, so a later
// addTarget will treat the outpoint as a fresh first reference.
//
// When the watch still has other targets the bool is false and the
// returned point is zero-valued; the chainsource watch stays armed.
func (w ancestorWatches) dropTarget(outpoint, target wire.OutPoint) (WatchPoint,
	bool) {

	aw, ok := w[outpoint]
	if !ok {
		return WatchPoint{}, false
	}

	delete(aw.targets, target)
	if len(aw.targets) > 0 {
		return WatchPoint{}, false
	}

	point := aw.point
	delete(w, outpoint)

	return point, true
}

// targetsOf returns the set of recipient VTXOs interested in outpoint.
// The returned map is the live collection — callers must not mutate
// it. Nil when outpoint is not tracked.
func (w ancestorWatches) targetsOf(
	outpoint wire.OutPoint) map[wire.OutPoint]struct{} {

	if aw, ok := w[outpoint]; ok {
		return aw.targets
	}

	return nil
}

// outpoints returns every tracked ancestor outpoint in a freshly
// allocated slice. Used for diagnostic / shutdown iteration where the
// caller wants a stable snapshot independent of the underlying map.
func (w ancestorWatches) outpoints() []wire.OutPoint {
	ops := make([]wire.OutPoint, 0, len(w))
	for op := range w {
		ops = append(ops, op)
	}

	return ops
}

// pointAt returns the WatchPoint stored for outpoint, if any. Useful
// at shutdown when the watcher must reconstruct the unregister
// request without dropping the entry from the collection.
func (w ancestorWatches) pointAt(outpoint wire.OutPoint) (WatchPoint, bool) {
	if aw, ok := w[outpoint]; ok {
		return aw.point, true
	}

	return WatchPoint{}, false
}
