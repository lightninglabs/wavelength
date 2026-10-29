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

// ErrCommitmentUnconfirmed identifies a chain lookup that could not return a
// confirmed commitment transaction (not visible to the chain source yet, not
// yet at the required depth, or the lookup itself timed out). It is distinct
// from ErrInvalidBatchExpiryEvidence: the evidence may be perfectly good and
// simply not confirmed yet, so a durable caller can keep waiting and can label
// the wait as unconfirmed rather than malformed.
var ErrCommitmentUnconfirmed = errors.New("commitment confirmation unavailable")

// CommitmentConfirmation is the chain-authenticated transaction and height
// used to derive one commitment's absolute sweep expiry.
type CommitmentConfirmation struct {
	Tx          *wire.MsgTx
	BlockHeight int32
}

// AuthenticatedBatchEvidence contains the expiry and earliest commitment
// confirmation height proven by the same local chain lookups. The height is a
// conservative watch floor for VTXOs backed by multiple commitments.
type AuthenticatedBatchEvidence struct {
	// BatchExpiry is the earliest verified sweep maturity height.
	BatchExpiry int32

	// EarliestCommitmentHeight is a conservative scan floor across all
	// confirmed commitments backing the VTXO.
	EarliestCommitmentHeight int32
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

	evidence, err := AuthenticateBatchEvidence(ctx, ancestry, resolve)
	if err != nil {
		return 0, err
	}

	return evidence.BatchExpiry, nil
}

// AuthenticateBatchEvidence derives the earliest sweep expiry and a safe
// confirmation-height floor from the same verified commitment transactions.
// Indexer height hints select the lookup but never enter the returned values.
func AuthenticateBatchEvidence(ctx context.Context, ancestry []Ancestry,
	resolve CommitmentConfirmationResolver) (AuthenticatedBatchEvidence,
	error) {

	var empty AuthenticatedBatchEvidence

	if len(ancestry) == 0 {
		return empty, invalidBatchExpiry("ancestry must be provided")
	}
	if resolve == nil {
		return empty, fmt.Errorf("confirmation resolver must be " +
			"provided")
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
			return empty, fmt.Errorf("ancestry fragment %d: %w", i,
				err)
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
	var earliestHeight int32
	for txid, group := range groups {
		confirmation, err := resolve(
			ctx, txid, group.pkScript, group.heightHint,
		)
		if err != nil {
			return empty, fmt.Errorf("resolve commitment %s: "+
				"%w: %w", txid, ErrCommitmentUnconfirmed, err)
		}
		if confirmation.Tx == nil {
			return empty, fmt.Errorf("commitment %s confirmation "+
				"has no transaction: %w", txid,
				ErrCommitmentUnconfirmed)
		}
		if confirmation.BlockHeight <= 0 {
			return empty, fmt.Errorf("commitment %s has invalid "+
				"confirmation height %d: %w", txid,
				confirmation.BlockHeight,
				ErrCommitmentUnconfirmed)
		}
		if confirmation.Tx.TxHash() != txid {
			return empty, invalidBatchExpiry(
				"confirmation transaction does not match "+
					"commitment %s", txid,
			)
		}

		for _, fragment := range group.fragments {
			idx := fragment.TreePath.BatchOutpoint.Index
			if idx >= uint32(len(confirmation.Tx.TxOut)) {
				return empty, invalidBatchExpiry(
					"commitment %s output %d is out of "+
						"range", txid, idx,
				)
			}

			claimed := fragment.TreePath.BatchOutput
			actual := confirmation.Tx.TxOut[idx]
			if claimed.Value != actual.Value || !bytes.Equal(
				claimed.PkScript, actual.PkScript,
			) {
				return empty, invalidBatchExpiry(
					"commitment %s output %d does not "+
						"match ancestry", txid, idx,
				)
			}

			expiry := int64(confirmation.BlockHeight) +
				int64(fragment.CommitmentSweepDelay)
			if expiry > math.MaxInt32 {
				return empty, invalidBatchExpiry(
					"commitment %s expiry overflows int32",
					txid,
				)
			}
			if earliest == 0 || expiry < earliest {
				earliest = expiry
			}
		}
		if earliestHeight == 0 ||
			confirmation.BlockHeight < earliestHeight {

			earliestHeight = confirmation.BlockHeight
		}
	}

	if earliest == 0 {
		return empty, invalidBatchExpiry("no expiry was derived")
	}

	return AuthenticatedBatchEvidence{
		BatchExpiry:              int32(earliest),
		EarliestCommitmentHeight: earliestHeight,
	}, nil
}

// ValidateIncomingBatchExpiry checks a locally authenticated deadline against
// the incoming creation metadata before it can be persisted. Accepting a
// creation height above the deadline would leave the new coin permanently
// unmonitored by the legacy consistency guard. Zero creation heights remain
// permitted for recovery compatibility. This check does not authenticate the
// creation height or the supplied deadline itself.
func ValidateIncomingBatchExpiry(batchExpiry, createdHeight int32) error {
	if batchExpiry <= 0 {
		return invalidBatchExpiry(
			"derived batch expiry must be positive",
		)
	}
	if createdHeight > batchExpiry {
		return invalidBatchExpiry(
			"creation height %d exceeds authenticated batch "+
				"expiry %d", createdHeight, batchExpiry,
		)
	}

	return nil
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
