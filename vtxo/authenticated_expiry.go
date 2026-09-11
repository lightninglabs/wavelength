package vtxo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
)

// ErrInvalidBatchExpiryEvidence identifies malformed or conflicting expiry
// evidence. Unlike a confirmation lookup failure, retrying the same evidence
// cannot repair this error.
var ErrInvalidBatchExpiryEvidence = errors.New("invalid batch expiry evidence")

// CommitmentConfirmation is the chain-authenticated transaction and height
// used to derive one commitment's absolute sweep expiry.
type CommitmentConfirmation struct {
	Tx          *wire.MsgTx
	BlockHeight int32
}

// CommitmentConfirmationResolver resolves a commitment transaction's current
// confirmation snapshot. PkScript and heightHint select and optimize the
// chain lookup; callers must not treat either as authentication by itself.
type CommitmentConfirmationResolver func(ctx context.Context,
	txid chainhash.Hash, pkScript []byte,
	heightHint uint32) (CommitmentConfirmation, error)

// AuthenticateBatchExpiry derives the earliest absolute expiry across all
// ancestry fragments. Each delay is authenticated against its tree root, and
// each tree's claimed batch output is checked against the transaction returned
// by the local chain source before the confirmation height is used.
func AuthenticateBatchExpiry(ctx context.Context, ancestry []Ancestry,
	resolve CommitmentConfirmationResolver) (int32, error) {

	if len(ancestry) == 0 {
		return 0, invalidBatchExpiry("ancestry must be provided")
	}
	if resolve == nil {
		return 0, fmt.Errorf("confirmation resolver must be provided")
	}

	type commitmentGroup struct {
		fragments  []Ancestry
		pkScript   []byte
		heightHint uint32
	}

	groups := make(map[chainhash.Hash]*commitmentGroup, len(ancestry))
	for i := range ancestry {
		fragment := ancestry[i]
		if err := validateExpiryFragment(fragment); err != nil {
			return 0, fmt.Errorf("ancestry fragment %d: %w", i, err)
		}

		group := groups[fragment.CommitmentTxID]
		if group == nil {
			group = &commitmentGroup{
				pkScript: bytes.Clone(
					fragment.TreePath.BatchOutput.PkScript,
				),
			}
			groups[fragment.CommitmentTxID] = group
		}
		group.fragments = append(group.fragments, fragment)
		if fragment.CommitmentHeight > 0 &&
			(group.heightHint == 0 ||
				uint32(fragment.CommitmentHeight) <
					group.heightHint) {

			group.heightHint = uint32(fragment.CommitmentHeight)
		}
	}

	var earliest int64
	for txid, group := range groups {
		confirmation, err := resolve(
			ctx, txid, group.pkScript, group.heightHint,
		)
		if err != nil {
			return 0, fmt.Errorf("resolve commitment %s: %w", txid,
				err)
		}
		if confirmation.Tx == nil {
			return 0, fmt.Errorf("commitment %s confirmation has "+
				"no transaction", txid)
		}
		if confirmation.BlockHeight <= 0 {
			return 0, fmt.Errorf("commitment %s has invalid "+
				"confirmation height %d", txid,
				confirmation.BlockHeight)
		}
		if confirmation.Tx.TxHash() != txid {
			return 0, invalidBatchExpiry(
				"confirmation transaction does not match "+
					"commitment %s", txid,
			)
		}

		for _, fragment := range group.fragments {
			idx := fragment.TreePath.BatchOutpoint.Index
			if idx >= uint32(len(confirmation.Tx.TxOut)) {
				return 0, invalidBatchExpiry(
					"commitment %s output %d is out of "+
						"range", txid, idx,
				)
			}

			claimed := fragment.TreePath.BatchOutput
			actual := confirmation.Tx.TxOut[idx]
			if claimed.Value != actual.Value || !bytes.Equal(
				claimed.PkScript, actual.PkScript,
			) {
				return 0, invalidBatchExpiry(
					"commitment %s output %d does not "+
						"match ancestry", txid, idx,
				)
			}

			expiry := int64(confirmation.BlockHeight) +
				int64(fragment.CommitmentSweepDelay)
			if expiry > math.MaxInt32 {
				return 0, invalidBatchExpiry(
					"commitment %s expiry overflows int32",
					txid,
				)
			}
			if earliest == 0 || expiry < earliest {
				earliest = expiry
			}
		}
	}

	if earliest == 0 {
		return 0, invalidBatchExpiry("no expiry was derived")
	}

	return int32(earliest), nil
}

// validateExpiryFragment binds one ancestry tree to its sweep policy and
// recomputed root output.
func validateExpiryFragment(fragment Ancestry) error {
	if fragment.TreePath == nil || fragment.TreePath.BatchOutput == nil {
		return invalidBatchExpiry(
			"tree path and batch output are required",
		)
	}
	if fragment.TreePath.Root == nil {
		return invalidBatchExpiry("tree root is required")
	}
	if fragment.TreePath.BatchOutpoint.Hash != fragment.CommitmentTxID {
		return invalidBatchExpiry(
			"tree batch outpoint does not match commitment txid",
		)
	}
	if fragment.CommitmentSweepDelay == 0 {
		return invalidBatchExpiry("sweep delay must be positive")
	}
	if fragment.CommitmentSweepKey == nil {
		return invalidBatchExpiry("sweep key must be provided")
	}

	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		fragment.CommitmentSweepKey, fragment.CommitmentSweepDelay,
	)
	if err != nil {
		return invalidBatchExpiry("build sweep leaf: %v", err)
	}
	sweepRoot := sweepLeaf.TapHash()
	if !bytes.Equal(
		sweepRoot[:], fragment.TreePath.SweepTapscriptRoot,
	) {
		return invalidBatchExpiry(
			"sweep key and delay do not match committed sweep root",
		)
	}

	finalKey, err := tree.ComputeFinalKey(
		fragment.TreePath.Root.CoSigners,
		fragment.TreePath.SweepTapscriptRoot,
	)
	if err != nil {
		return invalidBatchExpiry("recompute tree root key: %v", err)
	}
	expectedScript, err := txscript.PayToTaprootScript(finalKey)
	if err != nil {
		return invalidBatchExpiry("recompute tree root script: %v", err)
	}
	if !bytes.Equal(
		expectedScript, fragment.TreePath.BatchOutput.PkScript,
	) {
		return invalidBatchExpiry(
			"batch output does not match tree root",
		)
	}

	return nil
}

// invalidBatchExpiry wraps a permanent evidence failure with the public
// classification sentinel.
func invalidBatchExpiry(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidBatchExpiryEvidence, fmt.Sprintf(
		format, args...))
}
