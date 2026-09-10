package waved

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/wavelength/vtxo"
)

// legacyCommitmentHeightRepairTimeout bounds the optional post-ready indexer
// maintenance so a slow or unavailable indexer cannot hold its startup worker
// indefinitely.
const legacyCommitmentHeightRepairTimeout = 30 * time.Second

// repairLegacyCommitmentHeights refreshes commitment confirmation heights for
// active VTXOs written before that field was persisted. It runs as bounded
// post-ready maintenance. Existing restored exits therefore retain the safe
// configured fallback floor for the current process, while repaired heights
// become available to later admissions and restarts.
//
// The authenticated indexer supplies only candidate heights. The database
// repair matches each candidate against the exact local commitment/tree
// fragment, bounds it by local chain state, and preserves all local proof
// material. Per-target failures do not stop later repairs; the caller receives
// one bounded summary and the unroller retains its locally configured safe
// fallback floor without depending on repair success.
func (s *Server) repairLegacyCommitmentHeights(ctx context.Context) error {
	if s.vtxoStore == nil {
		return fmt.Errorf("vtxo store not initialized")
	}

	recoverable, err := s.vtxoStore.ListRecoverableVTXOs(ctx)
	if err != nil {
		return fmt.Errorf("list recoverable VTXOs: %w", err)
	}
	exiting, err := s.vtxoStore.ListVTXOsByStatus(
		ctx, vtxo.VTXOStatusUnilateralExit,
	)
	if err != nil {
		return fmt.Errorf("list exiting VTXOs: %w", err)
	}

	targets := make([]*vtxo.Descriptor, 0, len(recoverable)+len(exiting))
	targets = append(targets, recoverable...)
	targets = append(targets, exiting...)
	candidateCount := 0
	for _, desc := range targets {
		if hasUnknownCommitmentHeight(desc) {
			candidateCount++
		}
	}
	if candidateCount == 0 {
		return nil
	}
	if s.chainBackend == nil {
		return fmt.Errorf("chain backend not initialized")
	}

	bestHeight, _, err := s.chainBackend.BestBlock(ctx)
	if err != nil {
		return fmt.Errorf("get local best height: %w", err)
	}
	if bestHeight <= 0 {
		return fmt.Errorf("invalid local best height %d", bestHeight)
	}

	signerFactory, err := s.indexerProofSignerFactory()
	if err != nil {
		return fmt.Errorf("build indexer proof signer: %w", err)
	}
	fetcher, err := incomingAncestryFetcher(s.indexer, signerFactory)
	if err != nil {
		return fmt.Errorf("build ancestry fetcher: %w", err)
	}

	var (
		repairedTargets   int
		repairedFragments int
		failedTargets     int
		firstErr          error
	)
	for _, desc := range targets {
		if !hasUnknownCommitmentHeight(desc) {
			continue
		}

		extras, fetchErr := fetcher(
			ctx, desc.Outpoint, desc.PkScript, desc.ClientKey,
		)
		if fetchErr != nil {
			failedTargets++
			if firstErr == nil {
				firstErr = fmt.Errorf("fetch %s: %w",
					desc.Outpoint, fetchErr)
			}

			continue
		}

		repaired, repairErr := s.vtxoStore.
			BackfillVTXOCommitmentHeights(
				ctx, desc.Outpoint, extras.Ancestry, bestHeight,
			)
		if repairErr != nil {
			failedTargets++
			if firstErr == nil {
				firstErr = fmt.Errorf("repair %s: %w",
					desc.Outpoint, repairErr)
			}

			continue
		}

		if repaired > 0 {
			repairedTargets++
			repairedFragments += repaired
		}
	}

	if repairedTargets > 0 {
		s.log.InfoS(ctx, "Repaired legacy VTXO commitment heights",
			slog.Int("target_count", repairedTargets),
			slog.Int("fragment_count", repairedFragments),
		)
	}

	if failedTargets > 0 {
		return fmt.Errorf("%d of %d legacy VTXO commitment-height "+
			"repairs failed; first failure: %w", failedTargets,
			candidateCount, firstErr)
	}

	return nil
}

// hasUnknownCommitmentHeight reports whether desc has usable local ancestry
// but at least one fragment still lacks its commitment confirmation height.
// Empty ancestry is a separate recovery-material defect and cannot be repaired
// safely by a height-only backfill.
func hasUnknownCommitmentHeight(desc *vtxo.Descriptor) bool {
	if desc == nil || len(desc.Ancestry) == 0 {
		return false
	}

	for _, ancestry := range desc.Ancestry {
		if ancestry.CommitmentHeight <= 0 {
			return true
		}
	}

	return false
}
