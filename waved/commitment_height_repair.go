package waved

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/wavelength/vtxo"
)

const (
	// legacyCommitmentHeightRepairTimeout bounds one maintenance pass so
	// an unavailable indexer cannot hold the worker indefinitely.
	legacyCommitmentHeightRepairTimeout = 30 * time.Second

	// legacyCommitmentHeightConfirmationTimeout leaves the rest of a pass
	// available to other targets when one local confirmation is
	// unavailable.
	legacyCommitmentHeightConfirmationTimeout = 10 * time.Second

	// legacyCommitmentHeightRepairInterval leaves room for live indexer
	// traffic between passes through the remaining legacy inventory.
	legacyCommitmentHeightRepairInterval = time.Minute

	// legacyCommitmentHeightRepairMaxInterval limits polling pressure from
	// targets that remain unavailable or fail validation across passes.
	legacyCommitmentHeightRepairMaxInterval = time.Hour
)

// startLegacyCommitmentHeightRepair starts daemon-owned maintenance after the
// startup publication barrier. Its cleanup cancels and joins the worker before
// wallet, transport, or database teardown, including on startup failure.
func (s *Server) startLegacyCommitmentHeightRepair(runCtx context.Context,
	issuePage indexerPageCall) func() {

	ctx, cancel := context.WithCancel(runCtx)
	done := make(chan struct{})
	go func() {
		defer close(done)

		s.runLegacyCommitmentHeightRepair(ctx, issuePage)
	}()

	return func() {
		cancel()
		<-done
	}
}

// runLegacyCommitmentHeightRepair waits for readiness and runs bounded passes
// until the durable legacy inventory is repaired or daemon shutdown begins.
func (s *Server) runLegacyCommitmentHeightRepair(ctx context.Context,
	issuePage indexerPageCall) {

	select {
	case <-s.DaemonReady():
	case <-ctx.Done():
		return
	}

	retryDelay := legacyCommitmentHeightRepairInterval
	sawCandidates := false
	for ctx.Err() == nil {
		repairCtx, repairCancel := context.WithTimeout(
			ctx, legacyCommitmentHeightRepairTimeout,
		)
		result, err := s.repairLegacyCommitmentHeights(
			repairCtx, issuePage,
		)
		sawCandidates = sawCandidates || result.candidates > 0
		repairCancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if sawCandidates {
				s.log.InfoS(
					ctx, "Legacy VTXO "+
						"commitment-height repair "+
						"complete",
				)
			}

			return
		}

		// Durable progress keeps migration moving promptly. Repeated
		// failures without progress back off, including setup errors.
		if result.completed > 0 {
			retryDelay = legacyCommitmentHeightRepairInterval
		}

		s.log.InfoS(ctx, "Legacy VTXO commitment-height repair "+
			"incomplete; retry scheduled",
			slog.String("error", err.Error()),
			slog.Duration("retry_in", retryDelay),
		)

		select {
		case <-s.clk.TickAfter(retryDelay):
		case <-ctx.Done():
			return
		}
		retryDelay = min(
			2*retryDelay, legacyCommitmentHeightRepairMaxInterval,
		)
	}
}

// legacyCommitmentHeightRepairResult distinguishes idle wallets from actual
// migration work and records durable progress for the worker cooldown.
type legacyCommitmentHeightRepairResult struct {
	candidates int
	completed  int
}

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
func (s *Server) repairLegacyCommitmentHeights(ctx context.Context,
	issuePage indexerPageCall) (legacyCommitmentHeightRepairResult, error) {

	return s.repairLegacyCommitmentHeightsWithTimeout(
		ctx, issuePage, legacyCommitmentHeightConfirmationTimeout,
	)
}

// repairLegacyCommitmentHeightsWithTimeout keeps the per-target wait separate
// from the caller's pass budget. Tests can shorten it without global state or
// changing the production deadline.
func (s *Server) repairLegacyCommitmentHeightsWithTimeout(ctx context.Context,
	issuePage indexerPageCall, confirmationTimeout time.Duration) (
	legacyCommitmentHeightRepairResult, error) {

	var result legacyCommitmentHeightRepairResult
	if s.vtxoStore == nil {
		return result, fmt.Errorf("vtxo store not initialized")
	}

	recoverable, err := s.vtxoStore.ListRecoverableVTXOs(ctx)
	if err != nil {
		return result, fmt.Errorf("list recoverable VTXOs: %w", err)
	}
	exiting, err := s.vtxoStore.ListVTXOsByStatus(
		ctx, vtxo.VTXOStatusUnilateralExit,
	)
	if err != nil {
		return result, fmt.Errorf("list exiting VTXOs: %w", err)
	}

	targets := make([]*vtxo.Descriptor, 0, len(recoverable)+len(exiting))
	targets = append(targets, recoverable...)
	targets = append(targets, exiting...)

	for _, desc := range targets {
		if hasUnknownCommitmentHeight(desc) {
			result.candidates++
		}
	}
	if result.candidates == 0 {
		return result, nil
	}
	if s.chainBackend == nil {
		return result, fmt.Errorf("chain backend not initialized")
	}

	bestHeight, _, err := s.chainBackend.BestBlock(ctx)
	if err != nil {
		return result, fmt.Errorf("get local best height: %w", err)
	}
	if bestHeight <= 0 {
		return result, fmt.Errorf("invalid local best height %d",
			bestHeight)
	}

	signerFactory, err := s.indexerProofSignerFactory()
	if err != nil {
		return result, fmt.Errorf("build indexer proof signer: %w", err)
	}
	fetcher, err := incomingAncestryOnlyFetcher(
		s.indexer, signerFactory, issuePage,
	)
	if err != nil {
		return result, fmt.Errorf("build ancestry fetcher: %w", err)
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
		if err := ctx.Err(); err != nil {
			failedTargets = result.candidates - result.completed
			if firstErr == nil {
				firstErr = err
			}

			break
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

		// A legacy creation height can precede a later confirmation of
		// the same transaction. Require local chain evidence before
		// letting an indexed height cross that historical bound.
		repairErr := s.verifyLaterCommitmentHeight(
			ctx, desc, extras.Ancestry, bestHeight,
			confirmationTimeout,
		)
		var repaired int
		if repairErr == nil {
			repaired, repairErr = s.vtxoStore.
				BackfillVTXOCommitmentHeights(
					ctx, desc.Outpoint, extras.Ancestry,
					bestHeight,
				)
		}
		if repairErr != nil {
			failedTargets++
			if firstErr == nil {
				firstErr = fmt.Errorf("repair %s: %w",
					desc.Outpoint, repairErr)
			}

			continue
		}

		// A successful atomic backfill guarantees all local heights are
		// known. Zero updates means another writer repaired this target
		// after our inventory snapshot, not an incomplete repair.
		if repaired > 0 {
			repairedTargets++
			repairedFragments += repaired
		}
		result.completed++
	}

	if repairedTargets > 0 {
		s.log.InfoS(ctx, "Repaired legacy VTXO commitment heights",
			slog.Int("target_count", repairedTargets),
			slog.Int("fragment_count", repairedFragments),
		)
	}

	if failedTargets > 0 {
		return result, fmt.Errorf("%d of %d legacy VTXO "+
			"commitment-height repairs failed; first failure: %w",
			failedTargets, result.candidates, firstErr)
	}

	return result, nil
}

// verifyLaterCommitmentHeight replaces the former single-fragment creation
// ceiling with local confirmation evidence when an indexed height exceeds it.
// The exact transaction must confirm at the claimed height; a signed tree alone
// cannot authenticate that height. Lookup failure leaves the safe fallback in
// place for the next maintenance pass. Above-tip claims are rejected without a
// watch. A per-target deadline bounds registration and confirmation waiting so
// one unavailable confirmation does not consume the whole pass. The watch is
// always cancelled before returning. LND's notifier retains its historical
// scan and result independently of that subscription, so a later pass can
// consume a result that arrived after this wait expired.
func (s *Server) verifyLaterCommitmentHeight(ctx context.Context,
	desc *vtxo.Descriptor, indexed []vtxo.Ancestry, bestHeight int32,
	confirmationTimeout time.Duration) error {

	if len(desc.Ancestry) != 1 || desc.CreatedHeight <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, confirmationTimeout)
	defer cancel()

	local := desc.Ancestry[0]
	txid := local.CommitmentTxID
	for _, candidate := range indexed {
		if candidate.CommitmentTxID != txid ||
			candidate.CommitmentHeight <= desc.CreatedHeight {

			continue
		}
		if candidate.CommitmentHeight > bestHeight {
			return fmt.Errorf("indexed commitment height %d is "+
				"above local best height %d",
				candidate.CommitmentHeight, bestHeight)
		}
		if local.TreePath == nil || local.TreePath.BatchOutput == nil {
			return fmt.Errorf("local commitment has no batch " +
				"output")
		}

		// Start at the old local bound, not the disputed indexed
		// height. This covers later confirmations without a genesis
		// rescan. An earlier or unavailable confirmation cannot
		// authorize the new height and leaves the repair pending.
		registration, err := s.chainBackend.RegisterConf(
			ctx, &txid, local.TreePath.BatchOutput.PkScript, 1,
			uint32(desc.CreatedHeight), false,
		)
		if err != nil {
			return fmt.Errorf("verify later commitment height: %w",
				err)
		}
		defer registration.Cancel()

		select {
		case confirmation := <-registration.Confirmed:
			if confirmation == nil || confirmation.Tx == nil ||
				confirmation.Tx.TxHash() != txid {
				return fmt.Errorf("confirmation does not " +
					"match local commitment")
			}
			if confirmation.BlockHeight !=
				uint32(candidate.CommitmentHeight) {
				return fmt.Errorf("indexed commitment height "+
					"%d differs from local confirmation %d",
					candidate.CommitmentHeight,
					confirmation.BlockHeight)
			}

		case <-ctx.Done():
			return ctx.Err()
		}
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
