package vtxo

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

	default:
		return "unknown"
	}
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

// CalculateCriticalThreshold returns the dynamic critical threshold for a VTXO
// based on its tree depth and CSV delay.
func (c *ExpiryConfig) CalculateCriticalThreshold(vtxo *Descriptor) int32 {
	treeDepthBuffer := int32(vtxo.MaxTreeDepth()) * c.TreeDepthMultiplier
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
