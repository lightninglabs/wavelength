package waved

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/vtxo"
)

// validateArkChannelRecoveryProof applies the ordinary wallet receive boundary
// before accepting a hub's recovery package. Structural resolvability alone
// cannot authorize releasing an invoice preimage.
func validateArkChannelRecoveryProof(ctx context.Context, desc *vtxo.Descriptor,
	recovery arkchannel.RecoveryPackage,
	backend chainsource.ChainBackend) error {

	candidate := &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: desc.Outpoint.Hash[:], Vout: desc.Outpoint.Index,
		},
		ValueSat: uint64(desc.Amount), PkScript: desc.PkScript,
	}
	for _, fragment := range desc.Ancestry {
		path, err := arkrpc.AncestryPathFromTree(
			fragment.TreePath, fragment.CommitmentTxID,
			fragment.InputIndices,
		)
		if err != nil {
			return err
		}
		path.TreeDepth = fragment.TreeDepth
		path.CommitmentHeight = fragment.CommitmentHeight
		path.CommitmentSweepDelay = fragment.CommitmentSweepDelay
		if fragment.CommitmentSweepKey != nil {
			path.CommitmentSweepKey = fragment.CommitmentSweepKey.
				SerializeCompressed()
		}
		candidate.AncestryPaths = append(candidate.AncestryPaths, path)
	}
	for _, pkg := range recovery.Packages {
		candidate.AncestryPackages = append(
			candidate.AncestryPackages, &arkrpc.OORSessionPackage{
				ArkPsbt:         pkg.ArkPSBT,
				CheckpointPsbts: pkg.Checkpoints,
			},
		)
	}
	ancestry, err := vtxo.IndexedAncestryFromRPC(candidate)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, expiryAuthenticationTimeout)
	defer cancel()
	heights := make(map[chainhash.Hash]int32)
	evidence, err := vtxo.AuthenticateBatchEvidence(
		ctx, ancestry,
		func(ctx context.Context, txid chainhash.Hash, pkScript []byte,
			heightHint uint32) (vtxo.CommitmentConfirmation,
			error) {

			var empty vtxo.CommitmentConfirmation
			registration, err := backend.RegisterConf(
				ctx, &txid, pkScript, 1, heightHint, false,
			)
			if err != nil {
				return empty, err
			}
			defer registration.Cancel()
			select {
			case confirmation, ok := <-registration.Confirmed:
				if !ok || confirmation == nil {
					return empty, fmt.Errorf(
						"confirmation watch closed " +
							"without a transaction")
				}

				heights[txid] = int32(confirmation.BlockHeight)

				return vtxo.CommitmentConfirmation{
					Tx: confirmation.Tx,
					BlockHeight: int32(
						confirmation.BlockHeight,
					),
				}, nil

			case <-ctx.Done():
				return empty, ctx.Err()
			}
		},
	)
	if err != nil {
		return err
	}
	for i := range ancestry {
		fragment := &ancestry[i]
		fragment.CommitmentHeight = heights[fragment.CommitmentTxID]
	}
	desc.Ancestry = ancestry
	desc.BatchExpiry = evidence.BatchExpiry

	return nil
}

// validateArkChannelRecoveryWindow bounds the actual signed dependency graph,
// including relative and absolute height locks, by the allowance used for
// native broadcast timing. Fee-market delays remain the caller's reaction
// margin; accepting arbitrarily long or time-locked ancestry is not safe.
func validateArkChannelRecoveryWindow(proof *recovery.Proof, allowance uint32,
	height int32) error {

	if height < 0 || allowance == 0 {
		return fmt.Errorf("channel recovery window is unavailable")
	}
	ready := make(map[chainhash.Hash]uint64)
	for _, layer := range proof.Layers() {
		for _, txid := range layer {
			node, ok := proof.Node(txid)
			if !ok {
				return fmt.Errorf("missing recovery " +
					"transaction")
			}
			tx := node.Tx
			var earliest uint64
			lockActive := false
			for _, input := range tx.TxIn {
				sequence := input.Sequence
				lockActive = lockActive ||
					sequence != wire.MaxTxInSequenceNum
				delay, err := recoverySequenceBlocks(
					tx.Version, sequence,
				)
				if err != nil {
					return err
				}
				earliest = max(
					earliest,
					ready[input.PreviousOutPoint.Hash]+
						delay,
				)
			}
			if lockActive && tx.LockTime != 0 {
				if tx.LockTime >= txscript.LockTimeThreshold {
					return fmt.Errorf("time-based " +
						"recovery lock")
				}
				if tx.LockTime >= uint32(height) {
					earliest = max(
						earliest, uint64(tx.LockTime)-
							uint64(height)+
							1,
					)
				}
			}
			ready[txid] = earliest
		}
	}
	needed := ready[proof.TargetOutpoint().Hash] +
		uint64(proof.CSVDelay()) + 1
	if needed > uint64(allowance) {
		return fmt.Errorf("source recovery needs %d blocks, "+
			"allowance %d", needed, allowance)
	}

	return nil
}

// recoverySequenceBlocks reserves a confirmation per dependency even when no
// relative lock is active. Time-mode locks have no bounded block allowance.
func recoverySequenceBlocks(version int32, sequence uint32) (uint64, error) {
	if version < 2 || sequence&wire.SequenceLockTimeDisabled != 0 {
		return 1, nil
	}
	if sequence&wire.SequenceLockTimeIsSeconds != 0 {
		return 0, fmt.Errorf("time-based recovery lock")
	}

	return max(1, uint64(sequence&wire.SequenceLockTimeMask)), nil
}
