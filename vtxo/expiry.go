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
}

// DefaultExpiryConfig returns sensible defaults for expiry configuration.
// These values provide approximately 1 day of warning for refresh and 6 hours
// of buffer before critical escalation on mainnet block times.
func DefaultExpiryConfig() *ExpiryConfig {
	return &ExpiryConfig{
		RefreshThresholdBlocks:  144, // ~1 day before batch expiry.
		CriticalThresholdBlocks: 36,  // ~6 hours before batch expiry.
		MinRefreshBuffer:        72,  // At least ~12 hours buffer.
		TreeDepthMultiplier:     6,   // ~1 hour per tree level.
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
	vtxo *VTXODescriptor, currentHeight int32,
) ExpiryStatus {

	blocksRemaining := vtxo.BatchExpiry - currentHeight

	// If batch has already expired, status is expired.
	if blocksRemaining <= 0 {
		return ExpiryStatusExpired
	}

	// Calculate dynamic threshold based on tree depth. Deeper trees need
	// more time for unilateral exit since each level requires broadcasting
	// a transaction and waiting for confirmation.
	treeDepthBuffer := int32(vtxo.TreeDepth) * c.TreeDepthMultiplier

	// Add CSV delay - after the VTXO appears on-chain, we need to wait
	// for the relative timelock before we can spend via the unilateral
	// path.
	csvBuffer := int32(vtxo.RelativeExpiry)

	// Total buffer needed for safe unilateral exit.
	safeExitBuffer := treeDepthBuffer + csvBuffer

	// Critical threshold: must have enough time for unilateral exit.
	criticalThreshold := max(c.CriticalThresholdBlocks, safeExitBuffer)

	if blocksRemaining <= criticalThreshold {
		return ExpiryStatusCritical
	}

	// Refresh threshold: should refresh well before critical.
	refreshThreshold := max(
		c.RefreshThresholdBlocks,
		criticalThreshold+c.MinRefreshBuffer,
	)

	if blocksRemaining <= refreshThreshold {
		return ExpiryStatusNeedsRefresh
	}

	return ExpiryStatusSafe
}

// BlocksUntilExpiry returns the number of blocks until batch expiry.
func BlocksUntilExpiry(vtxo *VTXODescriptor, currentHeight int32) int32 {
	return vtxo.BatchExpiry - currentHeight
}

// DetermineRefreshUrgency maps blocks remaining to urgency level.
func (c *ExpiryConfig) DetermineRefreshUrgency(
	blocksRemaining int32,
) RefreshUrgency {

	if blocksRemaining <= c.CriticalThresholdBlocks {
		return RefreshUrgencyCritical
	}

	// Elevated if less than half the refresh threshold remaining.
	if blocksRemaining <= c.RefreshThresholdBlocks/2 {
		return RefreshUrgencyElevated
	}

	return RefreshUrgencyNormal
}

// CalculateCriticalThreshold returns the dynamic critical threshold for a VTXO
// based on its tree depth and CSV delay.
func (c *ExpiryConfig) CalculateCriticalThreshold(vtxo *VTXODescriptor) int32 {
	treeDepthBuffer := int32(vtxo.TreeDepth) * c.TreeDepthMultiplier
	csvBuffer := int32(vtxo.RelativeExpiry)
	safeExitBuffer := treeDepthBuffer + csvBuffer

	return max(c.CriticalThresholdBlocks, safeExitBuffer)
}

// CalculateRefreshThreshold returns the dynamic refresh threshold for a VTXO.
func (c *ExpiryConfig) CalculateRefreshThreshold(vtxo *VTXODescriptor) int32 {
	criticalThreshold := c.CalculateCriticalThreshold(vtxo)

	return max(
		c.RefreshThresholdBlocks,
		criticalThreshold+c.MinRefreshBuffer,
	)
}
