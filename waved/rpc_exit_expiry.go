package waved

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vtxo"
)

// ExitCommitmentSweep proves the block-based operator timeout path belongs to
// an external commitment output. It contains no signature or private key.
type ExitCommitmentSweep struct {
	DelayBlocks  uint32
	LeafScript   []byte
	ControlBlock []byte
}

// exportCommitmentSweeps binds saved sweep policy to the actual exported
// prevouts. Legacy rows without policy remain exportable with absent evidence;
// conflicting or malformed policy must never advertise a later deadline.
func exportCommitmentSweeps(desc *vtxo.Descriptor,
	ancestry *unroll.SignedAncestry) (
	map[wire.OutPoint]*ExitCommitmentSweep, error) {

	inputs := make(map[wire.OutPoint]*wire.TxOut)
	for _, input := range ancestry.ExternalInputs {
		inputs[input.Outpoint] = input.Output
	}
	result := make(map[wire.OutPoint]*ExitCommitmentSweep)
	for _, fragment := range desc.Ancestry {
		path := fragment.TreePath
		output := inputs[path.BatchOutpoint]
		if output == nil {
			continue
		}
		if fragment.CommitmentSweepKey == nil &&
			fragment.CommitmentSweepDelay == 0 {

			continue
		}
		delay := fragment.CommitmentSweepDelay
		if fragment.CommitmentTxID != path.BatchOutpoint.Hash ||
			fragment.CommitmentSweepKey == nil ||
			fragment.CommitmentSweepDelay == 0 ||
			delay > wire.SequenceLockTimeMask {
			return nil, fmt.Errorf("invalid sweep policy for %s",
				path.BatchOutpoint)
		}
		internalKey, err := tree.ComputeInternalKey(path.Root.CoSigners)
		if err != nil {
			return nil, err
		}
		spend, err := tree.NewBranchSweepSpendInfo(
			internalKey, fragment.CommitmentSweepKey,
			fragment.CommitmentSweepDelay,
		)
		if err != nil {
			return nil, err
		}
		control, err := txscript.ParseControlBlock(spend.ControlBlock)
		if err != nil {
			return nil, err
		}
		if !txscript.IsPayToTaproot(output.PkScript) {
			return nil, fmt.Errorf("commitment %s is not taproot",
				path.BatchOutpoint)
		}
		if err := txscript.VerifyTaprootLeafCommitment(
			control, output.PkScript[2:], spend.WitnessScript,
		); err != nil {
			return nil, fmt.Errorf("commitment %s sweep proof: %w",
				path.BatchOutpoint, err)
		}
		evidence := &ExitCommitmentSweep{
			DelayBlocks:  fragment.CommitmentSweepDelay,
			LeafScript:   spend.WitnessScript,
			ControlBlock: spend.ControlBlock,
		}
		previous := result[path.BatchOutpoint]
		if previous != nil &&
			(previous.DelayBlocks != evidence.DelayBlocks ||
				!bytes.Equal(
					previous.LeafScript,
					evidence.LeafScript,
				) ||
				!bytes.Equal(
					previous.ControlBlock,
					evidence.ControlBlock,
				)) {
			return nil, fmt.Errorf("conflicting sweep "+
				"policy for %s", path.BatchOutpoint)
		}
		result[path.BatchOutpoint] = evidence
	}

	return result, nil
}
