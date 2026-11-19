package arkchannel

import (
	"errors"
	"fmt"
)

// ErrSourceInadmissible means the selected inputs cannot safely fund this
// channel. An unavailable store or chain backend is not such a decision.
var ErrSourceInadmissible = errors.New("channel funding source inadmissible")

// SourceExpirySafetyMargin reserves forty blocks beyond the materialization
// allowance before the earliest source batch can be swept by the operator.
const SourceExpirySafetyMargin = 40

// SourceMaterializationHeight returns the last safe height to start automatic
// materialization. New channel sources must be admitted strictly before this
// height. Missing expiry, zero allowance, and unrepresentable deadlines fail
// closed rather than permitting a source with an unknown recovery window.
func SourceMaterializationHeight(batchExpiry int32,
	allowance uint32) (int32, error) {

	if batchExpiry <= 0 {
		return 0, fmt.Errorf("source batch expiry must be positive")
	}
	if allowance == 0 {
		return 0, fmt.Errorf("source materialization allowance must " +
			"be positive")
	}

	// Widen before adding so a uint32 allowance cannot wrap the reserve.
	reserve := uint64(allowance) + SourceExpirySafetyMargin
	if reserve >= uint64(batchExpiry) {
		return 0, fmt.Errorf("source batch expiry %d cannot reserve "+
			"%d blocks for materialization", batchExpiry, reserve)
	}

	return batchExpiry - int32(reserve), nil
}

// ValidateSourceLifetime requires a known current height strictly before the
// source's materialization deadline. Equality is already too late to admit new
// channel funding, even though recovery of an existing channel must continue.
func ValidateSourceLifetime(batchExpiry, height int32, allowance uint32) error {
	deadline, err := SourceMaterializationHeight(batchExpiry, allowance)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSourceInadmissible, err)
	}
	if height < 0 {
		return fmt.Errorf("source current height must not be negative")
	}
	if height >= deadline {
		return fmt.Errorf("%w: source lifetime exhausted at height "+
			"%d: materialization deadline %d (batch expiry %d)",
			ErrSourceInadmissible, height, deadline, batchExpiry)
	}

	return nil
}
