package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/vtxo"
)

// BatchRegistrar durably registers authenticated batch evidence and arms its
// reorg-aware watches before an incoming VTXO can be exposed.
type BatchRegistrar interface {
	RegisterBatch(context.Context, *batchcanon.RegisterBatchRequest) error
}

// BaseCoinInputs returns the committed base-VTXO outpoints the received coin
// ultimately descends from, across the full OOR checkpoint/ark chain (F-H1
// terminator). Each finalized checkpoint has exactly one input; a prevout that
// spends an ancestor package's ark output is an intermediate OOR hop, so it is
// skipped — the BASE checkpoints, whose prevouts are the actual commitment-tree
// leaves, spend no ancestor output. This makes the ancestry bind work uniformly
// for single-hop AND multi-hop receives: the returned outpoints must each be an
// authenticated ancestry leaf. Malformed checkpoints (no input) are skipped;
// upstream ValidateFinalizePackage guarantees well-formed, matched checkpoints,
// and a fully-empty result fails the bind closed.
func BaseCoinInputs(rootCheckpoints []*psbt.Packet,
	ancestors []PackageArtifact) []wire.OutPoint {

	ancestorSessions := make(map[SessionID]struct{}, len(ancestors))
	for _, a := range ancestors {
		ancestorSessions[a.SessionID] = struct{}{}
	}

	var base []wire.OutPoint
	collect := func(checkpoints []*psbt.Packet) {
		for _, cp := range checkpoints {
			if cp == nil || cp.UnsignedTx == nil ||
				len(cp.UnsignedTx.TxIn) == 0 {

				continue
			}
			prevOut := cp.UnsignedTx.TxIn[0].PreviousOutPoint
			if _, ok := ancestorSessions[SessionID(
				prevOut.Hash,
			)]; ok {

				continue
			}
			base = append(base, prevOut)
		}
	}

	collect(rootCheckpoints)
	for _, a := range ancestors {
		collect(a.FinalCheckpointPSBTs)
	}

	return base
}

// IncomingLineageVerifier cryptographically binds a received VTXO's ancestry to
// the commitments the client is about to watch: it proves each ancestry tree is
// a genuine, operator-signed descent from its authenticated commitment output
// (vtxo.VerifyOORAncestryLineage). It is injected rather than called directly
// so registration-logic tests, which use mock (unsigned) trees, can leave it
// nil; production wiring always supplies the real verifier. A nil verifier
// skips the binding.
type IncomingLineageVerifier func(ancestry []vtxo.Ancestry,
	evidence []batchcanon.BatchEvidence, chainDepth int,
	coinInputs []wire.OutPoint) error

// RegisterIncomingBatchEvidence registers every distinct commitment named by
// the resolved metadata. Replays merge the same dependent VTXOs idempotently.
// When verifyLineage is non-nil, each match's ancestry is cryptographically
// bound to its authenticated commitments before any watch is armed (F-H1); a
// failure fails closed, so no reorg watch is registered on an unverified
// commitment. coinInputs are the ark tx's real spent outpoints (the checkpoint
// prevouts), shared across matches: a single-hop receive's ancestry must
// account for them so a decoy commitment tree cannot be watched in place of the
// coin's true lineage.
func RegisterIncomingBatchEvidence(ctx context.Context,
	registrar BatchRegistrar, sessionID SessionID,
	matches []IncomingMetadataMatch, coinInputs []wire.OutPoint,
	verifyLineage IncomingLineageVerifier) error {

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

		// Bind the received VTXO's ancestry to its authenticated
		// commitments before any watch is armed: prove each ancestry
		// tree is a genuine, operator-signed descent from its
		// authenticated commitment output, so a malicious indexer
		// cannot name a real-but-decoy commitment for the client to
		// watch (F-H1). Only when evidence is present: an absent
		// evidence extension arms no watch (older-indexer / pre-gate
		// compatibility, handled above), so there is nothing to bind
		// and the receive degrades rather than failing. Fail-closed
		// otherwise.
		if verifyLineage != nil &&
			len(match.Metadata.BatchEvidence) > 0 {

			if err := verifyLineage(
				match.Metadata.Ancestry,
				match.Metadata.BatchEvidence,
				match.Metadata.ChainDepth, coinInputs,
			); err != nil {
				return fmt.Errorf("incoming metadata output "+
					"%d: bind lineage: %w",
					match.OutputIndex, err)
			}
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
