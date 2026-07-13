package chainsource

// PositiveDoneOrder preserves the causal positive-before-Done contract when a
// backend exposes those lifecycle facts on independent buffered channels. Go
// selects randomly among ready channels, so every transport boundary uses this
// small state machine instead of treating the selected order as causal order.
type PositiveDoneOrder struct {
	positive    bool
	pendingDone bool
}

// ObservePositive records delivery of the current positive observation and
// reports whether an earlier-selected Done signal may now be delivered.
func (o *PositiveDoneOrder) ObservePositive() bool {
	o.positive = true

	return o.pendingDone
}

// ObserveReorg clears current-positive and deferred-finality state. A Done
// selected before any positive is not allowed to survive an intervening reorg.
func (o *PositiveDoneOrder) ObserveReorg() {
	o.positive = false
	o.pendingDone = false
}

// ObserveDone records a terminal signal and reports whether a positive
// observation has already been delivered. False means the caller must retain
// the signal and wait rather than forwarding Done without identity metadata.
func (o *PositiveDoneOrder) ObserveDone() bool {
	if o.positive {
		return true
	}

	o.pendingDone = true

	return false
}
