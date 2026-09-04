package vtxo

import (
	"errors"
	"fmt"
)

// validateBackingBatchAge verifies that a VTXO is backed by a known,
// unexpired batch whose confirmation age satisfies the caller's limit. A zero
// limit deliberately bypasses every metadata check for backward compatibility.
func validateBackingBatchAge(desc *Descriptor, currentHeight int32,
	maxAgeBlocks uint32) error {

	if maxAgeBlocks == 0 {
		return nil
	}

	switch {
	case currentHeight <= 0:
		return fmt.Errorf("current chain height %d is unknown",
			currentHeight)

	case desc == nil:
		return errors.New("vtxo descriptor is missing")

	case desc.CreatedHeight <= 0:
		return fmt.Errorf("backing batch creation height %d is unknown",
			desc.CreatedHeight)

	case desc.CreatedHeight > currentHeight:
		return fmt.Errorf("backing batch creation height %d is above "+
			"current height %d", desc.CreatedHeight, currentHeight)

	case !HasUsableBatchExpiry(desc):
		return fmt.Errorf("backing batch expiry %d is unknown or "+
			"inconsistent with creation height %d",
			desc.BatchExpiry, desc.CreatedHeight)

	case desc.BatchExpiry <= currentHeight:
		return fmt.Errorf("backing batch expired at height %d "+
			"(current height %d)", desc.BatchExpiry, currentHeight)
	}

	age := uint32(currentHeight - desc.CreatedHeight)
	if age > maxAgeBlocks {
		return fmt.Errorf("backing batch age %d exceeds maximum %d",
			age, maxAgeBlocks)
	}

	return nil
}
