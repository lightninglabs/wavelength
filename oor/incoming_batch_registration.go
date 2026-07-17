package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
)

// BatchRegistrar durably registers authenticated batch evidence and arms its
// reorg-aware watches before an incoming VTXO can be exposed.
type BatchRegistrar interface {
	RegisterBatch(context.Context, *batchcanon.RegisterBatchRequest) error
}

// RegisterIncomingBatchEvidence registers every distinct commitment named by
// the resolved metadata. Replays merge the same dependent VTXOs idempotently.
func RegisterIncomingBatchEvidence(ctx context.Context,
	registrar BatchRegistrar, sessionID SessionID,
	matches []IncomingMetadataMatch) error {

	type pendingRegistration struct {
		evidence   batchcanon.BatchEvidence
		dependents []wire.OutPoint
	}

	registrations := make(map[chainhash.Hash]*pendingRegistration)
	order := make([]chainhash.Hash, 0)
	outputs := make(map[uint32]struct{}, len(matches))
	for i := range matches {
		match := matches[i]
		if _, duplicate := outputs[match.OutputIndex]; duplicate {
			return fmt.Errorf("incoming metadata output %d is "+
				"duplicated", match.OutputIndex)
		}
		outputs[match.OutputIndex] = struct{}{}

		if err := validateIncomingEvidenceCoverage(
			match.Metadata,
		); err != nil {
			return fmt.Errorf("incoming metadata output %d: %w",
				match.OutputIndex, err)
		}

		dependent := wire.OutPoint{
			Hash:  chainhash.Hash(sessionID),
			Index: match.OutputIndex,
		}
		for _, evidence := range match.Metadata.BatchEvidence {
			existing, ok := registrations[evidence.BatchTxID]
			if !ok {
				registrations[evidence.BatchTxID] =
					&pendingRegistration{
						evidence: evidence,
						dependents: []wire.OutPoint{
							dependent,
						},
					}
				order = append(order, evidence.BatchTxID)

				continue
			}

			if !existing.evidence.Equal(evidence) {
				return fmt.Errorf("conflicting evidence for "+
					"commitment %s", evidence.BatchTxID)
			}
			existing.dependents = append(
				existing.dependents, dependent,
			)
		}
	}

	if len(registrations) == 0 {
		return nil
	}
	if registrar == nil {
		return fmt.Errorf("batch registrar must be provided")
	}

	for _, txid := range order {
		registration := registrations[txid]
		request := registration.evidence.RegisterRequest(
			registration.dependents,
		)
		if err := registrar.RegisterBatch(ctx, request); err != nil {
			return fmt.Errorf("register commitment %s: %w", txid,
				err)
		}
	}

	return nil
}

// validateIncomingEvidenceCoverage binds optional evidence to every distinct
// commitment in the metadata. A completely absent extension remains compatible
// with older indexers and naturally stays blocked when the fail-closed gate is
// active.
func validateIncomingEvidenceCoverage(meta IncomingVTXOMetadata) error {
	if len(meta.BatchEvidence) == 0 {
		return nil
	}

	ancestry := make(map[chainhash.Hash]struct{}, len(meta.Ancestry))
	for _, fragment := range meta.Ancestry {
		ancestry[fragment.CommitmentTxID] = struct{}{}
	}

	seen := make(map[chainhash.Hash]struct{}, len(meta.BatchEvidence))
	for i, evidence := range meta.BatchEvidence {
		if err := evidence.Validate(); err != nil {
			return fmt.Errorf("batch evidence %d: %w", i, err)
		}
		if evidence.CSVExpiryDelta <= 0 {
			return fmt.Errorf("batch evidence %d has non-positive "+
				"CSV expiry delta", i)
		}
		if _, ok := ancestry[evidence.BatchTxID]; !ok {
			return fmt.Errorf("batch evidence %d names commitment "+
				"%s outside ancestry", i, evidence.BatchTxID)
		}
		if _, duplicate := seen[evidence.BatchTxID]; duplicate {
			return fmt.Errorf("batch evidence %d duplicates "+
				"commitment %s", i, evidence.BatchTxID)
		}

		seen[evidence.BatchTxID] = struct{}{}
	}

	if len(seen) != len(ancestry) {
		return fmt.Errorf("batch evidence covers %d of %d ancestry "+
			"commitments", len(seen), len(ancestry))
	}

	return nil
}
