package vtxo

import "math"

// ExpiryStatus represents the result of an expiry check.
type ExpiryStatus int

const (
	// ExpiryStatusSafe indicates the VTXO has plenty of time remaining.
	ExpiryStatusSafe ExpiryStatus = iota

	// ExpiryStatusNeedsRefresh indicates the VTXO should request refresh.
	ExpiryStatusNeedsRefresh

	// ExpiryStatusCritical indicates the VTXO must be sent to chain
	// resolver.
	ExpiryStatusCritical

	// ExpiryStatusExpired indicates the batch has already expired.
	ExpiryStatusExpired

	// ExpiryStatusUnknown indicates the VTXO carries no usable batch
	// expiry, so no expiry conclusion can be drawn from it. Callers must
	// decline to act rather than assume either extreme: treating a
	// missing expiry as "expired" would surrender live funds, and
	// treating it as "safe" would silently skip the refresh a real
	// deadline needs. It is appended last so the numeric values of the
	// existing statuses are unchanged.
	ExpiryStatusUnknown
)

// String returns a human-readable representation of the expiry status.
func (s ExpiryStatus) String() string {
	switch s {
	case ExpiryStatusSafe:
		return "safe"

	case ExpiryStatusNeedsRefresh:
		return "needs_refresh"

	case ExpiryStatusCritical:
		return "critical"

	case ExpiryStatusExpired:
		return "expired"

	case ExpiryStatusUnknown:
		return "unknown"

	default:
		return "unknown"
	}
}

// HasUsableBatchExpiry reports whether the descriptor carries a batch expiry
// that an expiry decision may be based on.
//
// A zero expiry is not a benign default. It is copied verbatim from the wire
// (see the incoming-VTXO handler) and every expiry calculation is
// `BatchExpiry - currentHeight`, so a zero reads back as "expired by the
// entire height of the chain" and would route a brand-new VTXO straight to
// the expiry path. An expiry earlier than the height the VTXO was created at
// is the same class of corruption: a VTXO cannot expire before it existed.
func HasUsableBatchExpiry(vtxo *Descriptor) bool {
	if vtxo == nil || vtxo.BatchExpiry <= 0 {
		return false
	}

	// CreatedHeight is not always populated (recovery paths leave it
	// zero), so only cross-check when it is.
	if vtxo.CreatedHeight > 0 && vtxo.BatchExpiry < vtxo.CreatedHeight {
		return false
	}

	return true
}

// ExpiryConfig holds configurable thresholds for VTXO expiry monitoring. These
// thresholds determine when refresh requests are sent and when VTXOs are
// escalated to the chain resolver.
//
// The dynamic calculation accounts for tree depth (each level requires an
// additional on-chain transaction for unilateral exit) and CSV delays.
type ExpiryConfig struct {
	// RefreshThresholdBlocks is the base number of blocks before batch
	// expiry at which a refresh request should be sent. The actual
	// threshold is adjusted based on tree depth.
	RefreshThresholdBlocks int32

	// CriticalThresholdBlocks is the base number of blocks before batch
	// expiry at which the VTXO is escalated to the chain resolver for
	// unilateral exit. Must be less than RefreshThresholdBlocks.
	CriticalThresholdBlocks int32

	// MinRefreshBuffer is the minimum buffer (blocks) between refresh and
	// critical thresholds, regardless of other calculations.
	MinRefreshBuffer int32

	// TreeDepthMultiplier is multiplied by tree depth to calculate the
	// additional blocks needed for safe unilateral exit. Each tree level
	// requires broadcasting and confirming an additional transaction.
	TreeDepthMultiplier int32

	// FreeRefreshWindow returns the operator-advertised number of blocks
	// before batch expiry in which refresh fees are waived. A nil callback
	// or zero window disables subsidy-aware scheduling. The callback lets a
	// long-lived VTXO actor observe a refreshed operator-terms snapshot
	// without weakening the local critical-expiry safety policy.
	FreeRefreshWindow func() uint32
}

// DefaultExpiryConfig returns sensible defaults for expiry configuration.
// These values provide approximately 1 day of warning for refresh and 6 hours
// of buffer before critical escalation on mainnet block times.
func DefaultExpiryConfig() *ExpiryConfig {
	return &ExpiryConfig{
		// ~1 day before batch expiry.
		RefreshThresholdBlocks: 144,

		// ~6 hours before batch expiry.
		CriticalThresholdBlocks: 36,

		// At least ~12 hours buffer.
		MinRefreshBuffer: 72,

		// ~1 hour per tree level.
		TreeDepthMultiplier: 6,
	}
}

// CheckExpiry evaluates a VTXO's expiry status given the current block height.
// It considers both the batch expiry and the tree depth (which affects
// unilateral exit time).
//
// The calculation ensures that:
//  1. Critical threshold always allows enough time for unilateral exit
//     (tree depth * multiplier + CSV delay).
//  2. Refresh threshold is always at least MinRefreshBuffer blocks before
//     critical.
func (c *ExpiryConfig) CheckExpiry(
	vtxo *Descriptor, currentHeight int32,
) ExpiryStatus {

	// Refuse to draw any conclusion from an expiry we cannot trust. This
	// must come first: every branch below is derived from BatchExpiry, so
	// a corrupt value would otherwise be indistinguishable from a real
	// deadline that has already passed.
	if !HasUsableBatchExpiry(vtxo) {
		return ExpiryStatusUnknown
	}

	blocksRemaining := vtxo.BatchExpiry - currentHeight

	// If batch has already expired, status is expired.
	if blocksRemaining <= 0 {
		return ExpiryStatusExpired
	}

	// Critical threshold: must have enough time for unilateral exit.
	criticalThreshold := c.CalculateCriticalThreshold(vtxo)

	if blocksRemaining <= criticalThreshold {
		return ExpiryStatusCritical
	}

	// Refresh threshold: should refresh well before critical.
	refreshThreshold := c.CalculateRefreshThreshold(vtxo)

	if blocksRemaining <= refreshThreshold {
		return ExpiryStatusNeedsRefresh
	}

	return ExpiryStatusSafe
}

// BlocksUntilExpiry returns the number of blocks until batch expiry.
func BlocksUntilExpiry(vtxo *Descriptor, currentHeight int32) int32 {
	return vtxo.BatchExpiry - currentHeight
}

// DetermineRefreshUrgency maps blocks remaining to urgency level using dynamic
// thresholds based on the VTXO's tree depth and CSV delay. This ensures that
// VTXOs with deeper trees or larger CSV delays are correctly prioritized since
// they require more time for unilateral exit.
func (c *ExpiryConfig) DetermineRefreshUrgency(
	vtxo *Descriptor, blocksRemaining int32,
) RefreshUrgency {

	criticalThreshold := c.CalculateCriticalThreshold(vtxo)
	if blocksRemaining <= criticalThreshold {

		// This case should ideally not be hit if called after
		// CheckExpiry, but is kept for robustness.
		return RefreshUrgencyCritical
	}

	refreshThreshold := c.CalculateRefreshThreshold(vtxo)

	// Elevated if less than half the dynamic refresh threshold remaining.
	if blocksRemaining <= refreshThreshold/2 {
		return RefreshUrgencyElevated
	}

	return RefreshUrgencyNormal
}

// exitTxDepth returns the number of transactions that must confirm in
// sequence before a unilateral exit can even begin its final CSV wait.
//
// Two segments stack. First the deepest commitment-tree path, since a VTXO
// with several ancestry fragments must land them all and they confirm in
// parallel, so the worst branch sets the pace. Then one recovery transaction
// per OOR hop between that commitment and this VTXO: those are strictly
// sequential, because each checkpoint spends the previous one. unroll already
// budgets fees this way (one recovery tx per ChainDepth hop), so the time
// budget has to agree or an exit is admitted with no room to finish.
func exitTxDepth(vtxo *Descriptor) int32 {
	depth := int64(vtxo.MaxTreeDepth())

	// A negative ChainDepth is rejected as invalid elsewhere; treat it as
	// zero here rather than letting it shorten the budget.
	if vtxo.ChainDepth > 0 {
		depth += int64(vtxo.ChainDepth)
	}

	if depth > math.MaxInt32 {
		return math.MaxInt32
	}

	return int32(depth)
}

// CalculateCriticalThreshold returns the dynamic critical threshold for a VTXO
// based on the depth of its unilateral-exit transaction chain and its CSV
// delay.
func (c *ExpiryConfig) CalculateCriticalThreshold(vtxo *Descriptor) int32 {
	treeDepthBuffer := exitTxDepth(vtxo) * c.TreeDepthMultiplier
	csvBuffer := int32(vtxo.RelativeExpiry)
	safeExitBuffer := treeDepthBuffer + csvBuffer

	return max(c.CriticalThresholdBlocks, safeExitBuffer)
}

// CalculateRefreshThreshold returns the dynamic refresh threshold for a VTXO.
func (c *ExpiryConfig) CalculateRefreshThreshold(vtxo *Descriptor) int32 {
	criticalThreshold := c.CalculateCriticalThreshold(vtxo)
	refreshThreshold := max(
		c.RefreshThresholdBlocks, criticalThreshold+c.MinRefreshBuffer,
	)

	if c.FreeRefreshWindow == nil {
		return refreshThreshold
	}

	windowBlocks := c.FreeRefreshWindow()
	if windowBlocks == 0 ||
		uint64(windowBlocks) >= uint64(refreshThreshold) {
		return refreshThreshold
	}

	// Never trade away the configured retry buffer merely to chase a fee
	// waiver. If the operator opens its free window later than the local
	// safety floor, the wallet refreshes earlier and pays the normal fee.
	safetyFloor := criticalThreshold + c.MinRefreshBuffer
	if uint64(windowBlocks) < uint64(safetyFloor) {
		return refreshThreshold
	}

	return int32(windowBlocks)
}
