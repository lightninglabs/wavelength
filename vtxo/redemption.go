package vtxo

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// maxRedemptionClaimsPerRound mirrors the protocol admission bound for
	// one client claim bundle. Larger local recovery sets are intentionally
	// drained over successive rounds.
	maxRedemptionClaimsPerRound = 1024

	// defaultRedemptionPollInterval keeps offline clients converging after
	// an operator sweep without coupling correctness to a push
	// notification.
	defaultRedemptionPollInterval = 30 * time.Second

	// defaultRedemptionMaxPollInterval caps exponential retries while the
	// operator omits a still-pending claim or remains unavailable.
	defaultRedemptionMaxPollInterval = 5 * time.Minute

	// defaultRedemptionRequestTimeout prevents an unavailable operator from
	// wedging the single reconciliation loop.
	defaultRedemptionRequestTimeout = 30 * time.Second
)

// RedemptionResult is the protocol-independent server answer for one locally
// expired VTXO. An omitted result means the operator has not finalized a
// redeemable claim yet. Replacement is set when this or another authorized
// participant already completed the reissue.
type RedemptionResult struct {
	// Source is the expired VTXO whose claim was checked.
	Source wire.OutPoint

	// Redeemable indicates the finalized operator sweep made Source
	// eligible to join a zero-fee reissue round.
	Redeemable bool

	// ClaimRoundID is the exact operator round accepting this claim. It is
	// set only when Redeemable is true and is covered by the participant's
	// tagged Schnorr authorization.
	ClaimRoundID string

	// Replacement identifies an already-created replacement. It is not a
	// bearer credential; the resolver must still fetch and validate the
	// complete replacement descriptor before local finalization.
	Replacement *wire.OutPoint
}

// RedemptionChecker checks only the locally supplied expired descriptors.
// Results may be sparse: omission means "not redeemable yet" and is retried.
type RedemptionChecker func(context.Context,
	[]*Descriptor) ([]RedemptionResult, error)

// RedemptionSubmitter hands redeemable descriptors to the round protocol.
// It must preserve each descriptor's full amount and exact policy. The round
// integration calls MarkRedeeming immediately after durable round admission.
type RedemptionSubmitter func(context.Context, string, []*Descriptor) error

// ReplacementResolver fetches the complete descriptor for a replacement
// reported by the operator. Existing incoming-VTXO/indexer recovery logic can
// implement this seam without teaching the coordinator any wire protocol.
type ReplacementResolver func(context.Context,
	*Descriptor, wire.OutPoint) (*Descriptor, error)

// PendingRedemption identifies one source-to-replacement observer record that
// was atomically written with the terminal Redeemed link. The observer may be
// replayed until this exact tuple is acknowledged.
type PendingRedemption struct {
	Source      wire.OutPoint
	Replacement wire.OutPoint
	RoundID     string
}

// RedemptionStore is the narrow durable surface used by the redemption
// coordinator. It intentionally stores no server-side lifecycle mirror: only
// the locally-derived Expired fact, a claim-bearing round checkpoint, and the
// terminal source-to-replacement link are persisted.
type RedemptionStore interface {
	ListVTXOsByStatus(context.Context, VTXOStatus) ([]*Descriptor, error)

	GetVTXO(context.Context, wire.OutPoint) (*Descriptor, error)

	MarkVTXORedeeming(context.Context, string, wire.OutPoint) error

	RevertVTXORedeeming(context.Context, string, wire.OutPoint) error

	FinalizeVTXORedemption(context.Context, wire.OutPoint,
		*Descriptor) error

	ListPendingVTXORedemptions(context.Context) ([]PendingRedemption, error)

	AcknowledgeVTXORedemption(context.Context, PendingRedemption) error
}

// RedemptionCoordinatorConfig configures selective expired-VTXO
// reconciliation.
type RedemptionCoordinatorConfig struct {
	Store               RedemptionStore
	Checker             RedemptionChecker
	Submitter           RedemptionSubmitter
	ReplacementResolver ReplacementResolver
	FinalizedObserver   func(
		context.Context, wire.OutPoint, *Descriptor,
	) error
	Clock           clock.Clock
	PollInterval    time.Duration
	MaxPollInterval time.Duration
	RequestTimeout  time.Duration
	Jitter          func(time.Duration) time.Duration
	Log             fn.Option[btclog.Logger]
}

// RedemptionCoordinator polls the operator only for VTXOs the synchronized
// client chain view has already classified Expired. It coalesces block/startup
// triggers and suppresses duplicate local submissions until the round reports
// adoption, rollback, or replacement.
type RedemptionCoordinator struct {
	cfg RedemptionCoordinatorConfig

	startOnce sync.Once
	trigger   chan struct{}

	mu         sync.Mutex
	submitting map[wire.OutPoint]struct{}
}

// NewRedemptionCoordinator constructs a selective reconciliation coordinator.
func NewRedemptionCoordinator(
	cfg RedemptionCoordinatorConfig) *RedemptionCoordinator {

	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultRedemptionPollInterval
	}
	if cfg.MaxPollInterval <= 0 {
		cfg.MaxPollInterval = defaultRedemptionMaxPollInterval
	}
	if cfg.MaxPollInterval < cfg.PollInterval {
		cfg.MaxPollInterval = cfg.PollInterval
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRedemptionRequestTimeout
	}
	if cfg.Jitter == nil {
		cfg.Jitter = jitterRedemptionDelay
	}

	return &RedemptionCoordinator{
		cfg:        cfg,
		trigger:    make(chan struct{}, 1),
		submitting: make(map[wire.OutPoint]struct{}),
	}
}

// Start launches the non-blocking polling loop and schedules an immediate
// startup reconciliation. Checker failures are logged and retried; they never
// undo the required local expiry classification or block daemon readiness.
func (c *RedemptionCoordinator) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		go c.run(ctx)
		c.Trigger()
	})
}

// Trigger coalesces an immediate reconciliation request. The durable Expired
// status is the recovery queue, so dropping redundant trigger edges is safe.
func (c *RedemptionCoordinator) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// run serializes periodic and event-driven checks so one slow operator call
// cannot build an unbounded request backlog. Omitted claims and transport
// failures back off exponentially from 30 seconds to five minutes; a block,
// reconnect, or other explicit Trigger resets the delay and retries now.
func (c *RedemptionCoordinator) run(ctx context.Context) {
	nextDelay := c.cfg.PollInterval
	timer := c.cfg.Clock.TickAfter(c.cfg.Jitter(nextDelay))

	for {
		triggered := false
		select {
		case <-ctx.Done():
			return

		case <-c.trigger:
			triggered = true

		case <-timer:
		}
		if triggered {
			nextDelay = c.cfg.PollInterval
		}

		requestCtx, cancel := context.WithTimeout(
			ctx, c.cfg.RequestTimeout,
		)
		outcome, err := c.checkNow(requestCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			c.logger(ctx).WarnS(
				ctx,
				"Expired VTXO reconciliation failed",
				err,
			)
		}

		stalled := err != nil || (outcome.hasClaims &&
			(outcome.omitted || !outcome.progressed))
		delay := c.cfg.PollInterval
		if stalled {
			delay = nextDelay
			nextDelay = minRedemptionDelay(
				nextDelay*2, c.cfg.MaxPollInterval,
			)
		} else {
			nextDelay = c.cfg.PollInterval
		}
		timer = c.cfg.Clock.TickAfter(c.cfg.Jitter(delay))
	}
}

type redemptionCheckOutcome struct {
	hasClaims  bool
	omitted    bool
	progressed bool
}

// CheckNow performs one selective reconciliation pass. It is exported for
// deterministic startup/system tests; production callers normally use Trigger.
func (c *RedemptionCoordinator) CheckNow(ctx context.Context) error {
	_, err := c.checkNow(ctx)

	return err
}

// checkNow performs one pass and reports whether pending work made progress,
// allowing the background loop to distinguish success from a sparse omission.
//
//nolint:funlen // Keep reconciliation validation in one atomic pass.
func (c *RedemptionCoordinator) checkNow(ctx context.Context) (
	redemptionCheckOutcome, error) {

	var outcome redemptionCheckOutcome
	if c.cfg.Store == nil {
		return outcome, fmt.Errorf("redemption store is required")
	}

	pending, err := c.cfg.Store.ListPendingVTXORedemptions(ctx)
	if err != nil {
		return outcome, fmt.Errorf("list pending VTXO redemptions: %w",
			err)
	}
	if len(pending) > 0 {
		outcome.hasClaims = true
	}
	for _, redemption := range pending {
		if err := c.replayPendingRedemption(
			ctx, redemption,
		); err != nil {
			return outcome, err
		}
		outcome.progressed = true
	}

	expired, err := c.cfg.Store.ListVTXOsByStatus(
		ctx, VTXOStatusExpired,
	)
	if err != nil {
		return outcome, fmt.Errorf("list expired VTXOs: %w", err)
	}
	redeeming, err := c.cfg.Store.ListVTXOsByStatus(
		ctx, VTXOStatusRedeeming,
	)
	if err != nil {
		return outcome, fmt.Errorf("list redeeming VTXOs: %w", err)
	}

	// Redeeming remains part of the reconciliation set. A crash can happen
	// after the local round checkpoint but before confirmation, and a
	// failed operator round eventually unlocks the claim back to
	// Redeemable. Polling both durable local states lets that claim retry
	// without relying on an in-memory round-failure callback surviving the
	// crash.
	expired = append(expired, redeeming...)
	outcome.hasClaims = outcome.hasClaims || len(expired) > 0
	if len(expired) == 0 {
		return outcome, nil
	}
	if c.cfg.Checker == nil {
		outcome.omitted = true

		return outcome, nil
	}

	results, err := c.cfg.Checker(ctx, expired)
	if err != nil {
		return outcome, fmt.Errorf("check expired VTXOs: %w", err)
	}

	byOutpoint := make(map[wire.OutPoint]*Descriptor, len(expired))
	for _, desc := range expired {
		if desc == nil {
			return outcome, fmt.Errorf("expired descriptor is " +
				"required")
		}
		byOutpoint[desc.Outpoint] = desc
	}

	seen := make(map[wire.OutPoint]struct{}, len(results))
	for _, result := range results {
		_, ok := byOutpoint[result.Source]
		if !ok {
			return outcome, fmt.Errorf("operator returned "+
				"unrequested source %s", result.Source)
		}
		if _, ok := seen[result.Source]; ok {
			return outcome, fmt.Errorf("operator returned "+
				"duplicate source %s", result.Source)
		}
		seen[result.Source] = struct{}{}

		if result.Replacement != nil && result.Redeemable {
			return outcome, fmt.Errorf("source %s is both "+
				"redeemable and replaced", result.Source)
		}
		if result.Replacement == nil && !result.Redeemable {
			return outcome, fmt.Errorf("operator returned no "+
				"positive redemption fact for source %s",
				result.Source)
		}
		if result.Replacement != nil && result.ClaimRoundID != "" {
			return outcome, fmt.Errorf("replaced source %s "+
				"carries claim round %s", result.Source,
				result.ClaimRoundID)
		}
		if !result.Redeemable {
			continue
		}
		if result.ClaimRoundID == "" {
			return outcome, fmt.Errorf("redeemable source %s has "+
				"no claim round ID", result.Source)
		}
	}

	// A checker pass can span multiple bounded RPCs while the operator
	// rotates its open round. Preserve every positive fact, but submit at
	// most one concrete-round cohort and one protocol-sized bundle in this
	// pass. Other cohorts remain unreserved Expired rows for a fresh check.
	cohorts := make(map[string][]*Descriptor)
	cohortOrder := make([]string, 0)
	for _, result := range results {
		desc := byOutpoint[result.Source]

		if result.Replacement != nil {
			if err := c.recoverReplacement(
				ctx, desc, *result.Replacement,
			); err != nil {
				return outcome, err
			}
			c.clearSubmitting(result.Source)
			outcome.progressed = true

			continue
		}

		if result.Redeemable && desc.Status == VTXOStatusRedeeming {
			if err := c.revertOrphanedClaim(ctx, desc); err != nil {
				return outcome, err
			}
			outcome.progressed = true
		}

		if result.Redeemable {
			if _, ok := cohorts[result.ClaimRoundID]; !ok {
				cohortOrder = append(
					cohortOrder, result.ClaimRoundID,
				)
			}
			cohorts[result.ClaimRoundID] = append(
				cohorts[result.ClaimRoundID], desc,
			)
		}
	}
	outcome.omitted = len(seen) < len(expired)

	var (
		claimRoundID string
		redeemable   []*Descriptor
	)
	for _, roundID := range cohortOrder {
		for _, desc := range cohorts[roundID] {
			if len(redeemable) == maxRedemptionClaimsPerRound {
				break
			}
			if c.markSubmitting(desc.Outpoint) {
				redeemable = append(redeemable, desc)
			}
		}
		if len(redeemable) > 0 {
			claimRoundID = roundID

			break
		}
	}

	if len(redeemable) == 0 || c.cfg.Submitter == nil {
		if c.cfg.Submitter == nil {
			c.clearSubmittingDescriptors(redeemable)
		}

		return outcome, nil
	}

	if err := c.cfg.Submitter(ctx, claimRoundID, redeemable); err != nil {
		c.clearSubmittingDescriptors(redeemable)

		return outcome, fmt.Errorf("submit expired VTXOs: %w", err)
	}
	outcome.progressed = true

	return outcome, nil
}

// MarkRedeeming durably records round ownership immediately after the
// server-assigned round is admitted and re-keyed locally. This happens before
// quote processing so every server-side claim lock has a crash-recoverable
// local owner.
func (c *RedemptionCoordinator) MarkRedeeming(ctx context.Context,
	roundID string, outpoints []wire.OutPoint) error {

	if c.cfg.Store == nil {
		return fmt.Errorf("redemption store is required")
	}
	if roundID == "" {
		return fmt.Errorf("redemption round ID is required")
	}

	var errs []error
	for _, outpoint := range dedupRedemptionOutpoints(outpoints) {
		err := c.cfg.Store.MarkVTXORedeeming(ctx, roundID, outpoint)
		if err != nil {
			errs = append(
				errs, fmt.Errorf("mark %s redeeming: %w",
					outpoint, err),
			)

			continue
		}
		c.clearSubmitting(outpoint)
	}

	return errors.Join(errs...)
}

// RevertRedeeming returns claims from a terminally failed round to Expired so
// the next poll may retry them. It is idempotent across crash replay.
func (c *RedemptionCoordinator) RevertRedeeming(ctx context.Context,
	roundID string, outpoints []wire.OutPoint) error {

	if c.cfg.Store == nil {
		return fmt.Errorf("redemption store is required")
	}
	if roundID == "" {
		for _, outpoint := range dedupRedemptionOutpoints(outpoints) {
			c.clearSubmitting(outpoint)
		}
		c.Trigger()

		return nil
	}

	var errs []error
	for _, outpoint := range dedupRedemptionOutpoints(outpoints) {
		err := c.cfg.Store.RevertVTXORedeeming(ctx, roundID, outpoint)
		if err != nil {
			errs = append(
				errs, fmt.Errorf("revert %s redemption: %w",
					outpoint, err),
			)

			continue
		}
		c.clearSubmitting(outpoint)
	}
	c.Trigger()

	return errors.Join(errs...)
}

// Finalize validates and atomically links a complete replacement descriptor
// supplied by the local round confirmation path.
func (c *RedemptionCoordinator) Finalize(ctx context.Context,
	source wire.OutPoint, replacement *Descriptor) error {

	if c.cfg.Store == nil {
		return fmt.Errorf("redemption store is required")
	}

	original, err := c.cfg.Store.GetVTXO(ctx, source)
	if err != nil {
		return fmt.Errorf("get redemption source: %w", err)
	}
	if err := validateRedemptionReplacement(
		original, replacement,
	); err != nil {
		return err
	}

	if err := c.cfg.Store.FinalizeVTXORedemption(
		ctx, source, replacement,
	); err != nil {
		return err
	}
	pending := PendingRedemption{
		Source:      source,
		Replacement: replacement.Outpoint,
		RoundID:     replacement.RoundID,
	}
	if err := c.materializeFinalizedRedemption(
		ctx, pending, replacement,
	); err != nil {
		return err
	}
	c.clearSubmitting(source)

	return nil
}

// recoverReplacement resolves an operator-reported outpoint, then applies the
// same exact-policy validation and atomic finalization as a local round.
func (c *RedemptionCoordinator) recoverReplacement(ctx context.Context,
	source *Descriptor, replacementOutpoint wire.OutPoint) error {

	var (
		replacement *Descriptor
		err         error
	)
	if c.cfg.ReplacementResolver != nil {
		replacement, err = c.cfg.ReplacementResolver(
			ctx, source, replacementOutpoint,
		)
	} else {
		replacement, err = c.cfg.Store.GetVTXO(
			ctx, replacementOutpoint,
		)
	}
	if err != nil {
		return fmt.Errorf("resolve replacement %s: %w",
			replacementOutpoint, err)
	}
	if replacement == nil || replacement.Outpoint != replacementOutpoint {
		return fmt.Errorf("resolver returned wrong replacement for %s",
			replacementOutpoint)
	}
	if err := validateRedemptionReplacement(
		source, replacement,
	); err != nil {
		return err
	}
	if err := c.cfg.Store.FinalizeVTXORedemption(
		ctx, source.Outpoint, replacement,
	); err != nil {
		return err
	}
	pending := PendingRedemption{
		Source:      source.Outpoint,
		Replacement: replacement.Outpoint,
		RoundID:     replacement.RoundID,
	}
	if err := c.materializeFinalizedRedemption(
		ctx, pending, replacement,
	); err != nil {
		return err
	}

	return nil
}

// replayPendingRedemption completes observer work that was not acknowledged
// before a crash or prior observer error. It uses only the durable local
// source/replacement link and never expands the operator reconciliation set.
func (c *RedemptionCoordinator) replayPendingRedemption(ctx context.Context,
	pending PendingRedemption) error {

	if pending.RoundID == "" {
		return fmt.Errorf("pending redemption for %s has no round ID",
			pending.Source)
	}
	source, err := c.cfg.Store.GetVTXO(ctx, pending.Source)
	if err != nil {
		return fmt.Errorf("load pending redemption source %s: %w",
			pending.Source, err)
	}
	if source.Status != VTXOStatusRedeemed {
		return fmt.Errorf("pending redemption source %s has status %s",
			pending.Source, source.Status)
	}
	replacedBy, err := source.ReplacedBy.UnwrapOrErr(
		fmt.Errorf("pending redemption source %s has no replacement",
			pending.Source),
	)
	if err != nil {
		return err
	}
	if replacedBy != pending.Replacement {
		return fmt.Errorf("pending redemption source %s links to "+
			"%s, not %s", pending.Source, replacedBy,
			pending.Replacement)
	}
	roundID, err := source.RedemptionRoundID.UnwrapOrErr(
		fmt.Errorf("pending redemption source %s has no round ID",
			pending.Source),
	)
	if err != nil {
		return err
	}
	if roundID != pending.RoundID {
		return fmt.Errorf("pending redemption source %s belongs to "+
			"round %q, not %q", pending.Source, roundID,
			pending.RoundID)
	}

	replacement, err := c.cfg.Store.GetVTXO(ctx, pending.Replacement)
	if err != nil {
		return fmt.Errorf("load pending redemption replacement %s: %w",
			pending.Replacement, err)
	}
	if replacement.RoundID != pending.RoundID {
		return fmt.Errorf("pending redemption replacement %s belongs "+
			"to round %q, not %q", pending.Replacement,
			replacement.RoundID, pending.RoundID)
	}
	if err := validateRedemptionReplacement(
		source, replacement,
	); err != nil {
		return fmt.Errorf("validate pending redemption: %w", err)
	}

	return c.materializeFinalizedRedemption(ctx, pending, replacement)
}

// materializeFinalizedRedemption invokes the idempotent observer before
// acknowledging its exact durable outbox tuple. If either step fails, the row
// remains discoverable by the next startup, block, reconnect, or timer pass.
func (c *RedemptionCoordinator) materializeFinalizedRedemption(
	ctx context.Context, pending PendingRedemption,
	replacement *Descriptor) error {

	if c.cfg.FinalizedObserver != nil {
		if err := c.cfg.FinalizedObserver(
			ctx, pending.Source, replacement,
		); err != nil {
			return fmt.Errorf("observe finalized redemption: %w",
				err)
		}
	}
	if err := c.cfg.Store.AcknowledgeVTXORedemption(
		ctx, pending,
	); err != nil {
		return fmt.Errorf("acknowledge finalized redemption: %w", err)
	}

	return nil
}

// revertOrphanedClaim clears a stale local round lease only after the server
// reports the source redeemable again. That answer proves the old operator
// round no longer owns the claim; persisting Expired before resubmission keeps
// a crash from stranding the source behind an obsolete round ID.
func (c *RedemptionCoordinator) revertOrphanedClaim(ctx context.Context,
	desc *Descriptor) error {

	roundID, err := desc.RedemptionRoundID.UnwrapOrErr(
		fmt.Errorf("redeeming source %s has no round ID",
			desc.Outpoint),
	)
	if err != nil {
		return err
	}
	if err := c.cfg.Store.RevertVTXORedeeming(
		ctx, roundID, desc.Outpoint,
	); err != nil {
		return fmt.Errorf("release orphaned redemption round %q: %w",
			roundID, err)
	}

	desc.Status = VTXOStatusExpired
	desc.RedemptionRoundID = fn.None[string]()
	c.clearSubmitting(desc.Outpoint)

	return nil
}

// validateRedemptionReplacement enforces the zero-fee, exact-script reissue
// contract locally before a replacement can retire the source.
func validateRedemptionReplacement(source, replacement *Descriptor) error {
	if source == nil || replacement == nil {
		return fmt.Errorf("source and replacement are required")
	}
	if source.Outpoint == replacement.Outpoint {
		return fmt.Errorf("replacement must differ from source")
	}
	if replacement.RoundID == "" {
		return fmt.Errorf("replacement round ID is required")
	}
	if source.Amount != replacement.Amount {
		return fmt.Errorf("replacement amount changed: got %d, want %d",
			replacement.Amount, source.Amount)
	}
	if !bytes.Equal(source.PolicyTemplate, replacement.PolicyTemplate) {
		return fmt.Errorf("replacement policy changed")
	}
	if !bytes.Equal(source.PkScript, replacement.PkScript) {
		return fmt.Errorf("replacement pkScript changed")
	}
	if source.ClientKey.PubKey != nil &&
		(replacement.ClientKey.PubKey == nil ||
			!source.ClientKey.PubKey.IsEqual(
				replacement.ClientKey.PubKey,
			) ||
			source.ClientKey.KeyLocator != replacement.
				ClientKey.
				KeyLocator) {
		return fmt.Errorf("replacement participant key changed")
	}
	if (source.OperatorKey == nil) != (replacement.OperatorKey == nil) ||
		(source.OperatorKey != nil &&
			!source.OperatorKey.IsEqual(replacement.OperatorKey)) {
		return fmt.Errorf("replacement operator key changed")
	}
	if replacement.RelativeExpiry != source.RelativeExpiry {
		return fmt.Errorf("replacement relative expiry changed")
	}
	if replacement.ConstructionVersion != source.ConstructionVersion {
		return fmt.Errorf("replacement construction version changed")
	}
	if replacement.BatchExpiry <= source.BatchExpiry {
		return fmt.Errorf("replacement batch expiry %d must exceed "+
			"source %d", replacement.BatchExpiry,
			source.BatchExpiry)
	}

	return nil
}

// markSubmitting reserves one source in memory while its submission is in
// flight. The durable status remains Expired until round adoption.
func (c *RedemptionCoordinator) markSubmitting(outpoint wire.OutPoint) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.submitting[outpoint]; ok {
		return false
	}
	c.submitting[outpoint] = struct{}{}

	return true
}

// clearSubmitting releases an in-memory submission reservation.
func (c *RedemptionCoordinator) clearSubmitting(outpoint wire.OutPoint) {
	c.mu.Lock()
	delete(c.submitting, outpoint)
	c.mu.Unlock()
}

// clearSubmittingDescriptors releases a set of in-memory reservations.
func (c *RedemptionCoordinator) clearSubmittingDescriptors(
	descs []*Descriptor) {

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, desc := range descs {
		delete(c.submitting, desc.Outpoint)
	}
}

// logger returns the configured subsystem logger or the context fallback.
func (c *RedemptionCoordinator) logger(ctx context.Context) btclog.Logger {
	return c.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// dedupRedemptionOutpoints preserves first-seen order while removing repeats.
func dedupRedemptionOutpoints(outpoints []wire.OutPoint) []wire.OutPoint {
	result := make([]wire.OutPoint, 0, len(outpoints))
	seen := make(map[wire.OutPoint]struct{}, len(outpoints))
	for _, outpoint := range outpoints {
		if _, ok := seen[outpoint]; ok {
			continue
		}
		seen[outpoint] = struct{}{}
		result = append(result, outpoint)
	}

	return result
}

// jitterRedemptionDelay applies symmetric 20-percent jitter so many clients
// coming online after the same sweep do not retry in lockstep.
func jitterRedemptionDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}

	spread := delay / 5
	if spread <= 0 {
		return delay
	}
	width := big.NewInt(int64(spread)*2 + 1)
	random, err := rand.Int(rand.Reader, width)
	if err != nil {
		return delay
	}

	return delay - spread + time.Duration(random.Int64())
}

// minRedemptionDelay returns the smaller retry delay.
func minRedemptionDelay(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}

	return b
}
