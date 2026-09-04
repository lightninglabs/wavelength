package round

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
)

// validateRoundSweepPolicy proves that the round-advertised sweep key and
// delay are the policy committed by each VTXO tree. The later confirmation
// transition may then safely derive BatchExpiry as confirmationHeight+delay.
func validateRoundSweepPolicy(sweepKey *btcec.PublicKey, sweepDelay uint32,
	vtxoTrees map[int]*tree.Tree) error {

	if sweepKey == nil {
		return fmt.Errorf("sweep key must be provided")
	}
	if sweepDelay == 0 {
		return fmt.Errorf("sweep delay must be positive")
	}

	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey, sweepDelay,
	)
	if err != nil {
		return fmt.Errorf("build sweep leaf: %w", err)
	}
	sweepRoot := sweepLeaf.TapHash()

	for outputIdx, vtxoTree := range vtxoTrees {
		if vtxoTree == nil {
			return fmt.Errorf("vtxo tree at output %d is nil",
				outputIdx)
		}
		if !bytes.Equal(
			sweepRoot[:], vtxoTree.SweepTapscriptRoot,
		) {
			return fmt.Errorf("vtxo tree at output %d has a sweep "+
				"root that does not match the advertised key "+
				"and delay", outputIdx)
		}
	}

	return nil
}
