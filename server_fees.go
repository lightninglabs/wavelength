package darepo

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightninglabs/darepo/ledger"
	"github.com/lightninglabs/darepo/lndbackend"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// setupFeesSubsystem wires the fee calculator, treasury tracker, shared
// fee estimator, and durable ledger actor onto the Server. It must run
// after the chain source actor (step 3) and the database (step 4), and
// before the rounds subsystem (step 5a) so the rounds actor picks up
// the shared fee estimator. Producers of ledger messages (rounds,
// batch sweeper, OOR) are wired in a follow-up series; this step just
// brings the consumer side online so admin/client fee RPCs can read
// live state.
func (s *Server) setupFeesSubsystem(ctx context.Context) error {
	feesLog := subLogger(s.cfg.Loggers, Subsystem)
	ledgerLog := subLogger(s.cfg.Loggers, ledger.Subsystem)

	// Construct the fee-schedule persistence adapter early: it
	// only needs s.db, and the calculator below needs it to
	// reload any schedule that was hot-applied before a prior
	// shutdown. Using the default clock here matches the
	// production timestamping convention for other accounting
	// writes.
	s.scheduleStore = db.NewFeeScheduleStoreDB(
		s.db, clock.NewDefaultClock(),
	)

	// Prefer the most recent persisted schedule (written by a
	// previous UpdateFeeSchedule call) over the config-file
	// schedule. This closes the gap where a runtime fee-schedule
	// change silently reverts on restart.
	//
	// If the history table is empty (fresh install, or an
	// operator has never called UpdateFeeSchedule), fall through
	// to scheduleFromConfig and let cfg.Fees drive boot.
	persisted, found, err := s.scheduleStore.LatestFeeSchedule(ctx)
	if err != nil {
		return fmt.Errorf("load persisted fee schedule: %w", err)
	}

	var schedule *fees.Schedule
	if found {
		schedule = persisted
		feesLog.InfoS(ctx, "Loaded persisted fee schedule "+
			"from fee_schedule_history",
			"annual_rate", schedule.AnnualRate,
			"base_margin_sat", schedule.BaseMarginSat,
		)
	} else {
		schedule = scheduleFromConfig(s.cfg.Fees)
		feesLog.InfoS(ctx, "No persisted fee schedule found; "+
			"using schedule derived from config",
			"annual_rate", schedule.AnnualRate,
			"base_margin_sat", schedule.BaseMarginSat,
		)
	}

	if err := schedule.Validate(); err != nil {
		return fmt.Errorf("invalid fee schedule: %w", err)
	}

	calc, err := fees.NewCalculator(schedule)
	if err != nil {
		return fmt.Errorf("create fee calculator: %w", err)
	}
	s.feeCalculator = calc

	// Install the fee estimator via pickFeeEstimator. The helper
	// is factored out so the selector logic can be unit-tested
	// without spinning up a real Server + LND.
	var walletKit lndclient.WalletKitClient
	if s.lnd != nil {
		walletKit = s.lnd.WalletKit
	}
	s.feeEstimator = pickFeeEstimator(s.cfg.Fees, walletKit, feesLog)

	// Surface which estimator the selector installed at boot so an
	// operator can confirm from the logs whether they are running
	// chain-backed (production default once LND is wired),
	// statically pinned (explicit override or itest), or the floor
	// fallback (no LND and no static override -- silent today
	// without this log). The floor-fallback branch in particular
	// would otherwise be invisible in a misconfigured deployment
	// until someone noticed boarding fees were stuck at 253 sat/kW.
	logEstimatorSelection(ctx, feesLog, s.cfg.Fees, walletKit)

	// Create the treasury tracker. Zero-initialized; the ledger
	// actor's Start reseeds it from persisted ledger totals before
	// the mailbox accepts messages.
	s.treasury = fees.NewTreasuryTracker()

	// DB-backed ledger store; used by the actor for writes and by
	// the admin RPC ListFeeEvents handler indirectly via s.db for
	// reads.
	s.ledgerStore = db.NewLedgerStoreDB(s.db)

	// The ledger actor needs its own durable delivery store keyed
	// by its actor ID so replay state does not alias with the
	// rounds or OOR actors.
	deliveryStore, err := db.NewActorDeliveryStoreFromDB(
		s.db, clock.NewDefaultClock(), ledgerLog,
	)
	if err != nil {
		return fmt.Errorf("create ledger delivery store: %w", err)
	}

	utxoAuditStore := db.NewUTXOAuditStoreDB(s.db)

	s.ledgerActor = ledger.NewLedgerActor(ledger.ActorConfig{
		Log:             fn.Some(ledgerLog),
		DeliveryStore:   deliveryStore,
		LedgerStore:     s.ledgerStore,
		TreasuryTracker: s.treasury,
		BalanceReader: fn.Some[ledger.LedgerBalanceReader](
			s.ledgerStore,
		),
		UTXOAuditStore: fn.Some[ledger.UTXOAuditStore](
			utxoAuditStore,
		),
		UTXOSnapshotReader: fn.Some[ledger.UTXOSnapshotReader](
			utxoAuditStore,
		),
		ChainSource: fn.Some(s.chainSourceRef),
	})

	// Register the actor with the system via its service key and
	// stash the returned TellOnlyRef on the Server so downstream
	// producers (rounds, batch sweeper, OOR) can send fire-and-
	// forget ledger messages without having to resolve through
	// the receptionist on every call site.
	s.ledgerRef = actor.RegisterWithSystem(
		s.actorSystem, "ledger-actor", ledger.NewServiceKey(),
		s.ledgerActor,
	)

	if err := s.ledgerActor.Start(ctx); err != nil {
		return fmt.Errorf("start ledger actor: %w", err)
	}

	// Belt-and-suspenders: every downstream producer (rounds,
	// batch sweeper, OOR) assumes the fees + ledger fields on
	// Server are non-nil in production. A future refactor could
	// accidentally leave one unset -- the whole subsystem would
	// then admit rounds whose accounting is never persisted,
	// silently drifting the ledger off on-chain reality. Fail
	// the boot instead.
	if s.feeCalculator == nil {
		return fmt.Errorf("fees subsystem: FeeCalculator unset")
	}
	if s.feeEstimator == nil {
		return fmt.Errorf("fees subsystem: FeeEstimator unset")
	}
	if s.treasury == nil {
		return fmt.Errorf("fees subsystem: TreasuryTracker unset")
	}
	if s.ledgerRef == nil {
		return fmt.Errorf("fees subsystem: LedgerRef unset")
	}

	feesLog.InfoS(ctx, "Fees subsystem ready",
		"annual_rate", schedule.AnnualRate,
		"base_margin_sat", schedule.BaseMarginSat,
	)

	return nil
}

// stopFeesSubsystem releases resources held by the fees/ledger
// subsystem. Safe to call even if setupFeesSubsystem failed
// partway through.
func (s *Server) stopFeesSubsystem(_ context.Context) {
	if s.ledgerActor == nil {
		return
	}

	s.ledgerActor.Stop()
}

// pickFeeEstimator is the setupFeesSubsystem selector factored out
// so the config-driven selection logic can be exercised by a unit
// test without spinning up a real Server + LND. The priority is:
//
//  1. cfg.StaticFeeRateSatKW > 0: install a static estimator at
//     that rate. Used by the itest + systest harnesses to keep
//     regtest rounds deterministic. An operator can also pin this
//     in production if they don't want to trust WalletKit.
//
//  2. walletKit != nil: install the chain-backed WalletKit
//     estimator. The production default once #267 lands, closing
//     the silent-absorption hole where the previous static floor
//     made the operator eat every mempool spike.
//
//  3. Fallback: static FeePerKwFloor estimator. Hit when the
//     operator is in a dev mode with no LND wired AND has not
//     explicitly set StaticFeeRateSatKW.
//
// A nil cfg is treated as "no config," which takes the fallback
// branch — defensive so a mis-initialized caller does not panic.
func pickFeeEstimator(cfg *FeesConfig, walletKit lndclient.WalletKitClient,
	log btclog.Logger) chainfee.Estimator {

	if cfg != nil && cfg.StaticFeeRateSatKW > 0 {
		return chainfee.NewStaticEstimator(
			chainfee.SatPerKWeight(cfg.StaticFeeRateSatKW), 0,
		)
	}

	if walletKit != nil {
		return lndbackend.NewWalletKitEstimator(walletKit, log)
	}

	return chainfee.NewStaticEstimator(chainfee.FeePerKwFloor, 0)
}

// logEstimatorSelection emits an InfoS line at boot describing
// which estimator branch pickFeeEstimator just took. Kept separate
// from pickFeeEstimator so the selector stays pure and unit-testable
// without a logger mock doing double-duty as a behavioral assertion.
func logEstimatorSelection(ctx context.Context, log btclog.Logger,
	cfg *FeesConfig, walletKit lndclient.WalletKitClient) {

	switch {
	case cfg != nil && cfg.StaticFeeRateSatKW > 0:
		log.InfoS(ctx, "Fee estimator: static override",
			"rate_sat_kw", cfg.StaticFeeRateSatKW,
		)

	case walletKit != nil:
		log.InfoS(ctx, "Fee estimator: WalletKit chain-backed")

	default:
		log.WarnS(ctx, "Fee estimator: falling back to "+
			"FeePerKwFloor static estimator; no LND WalletKit "+
			"wired and no StaticFeeRateSatKW override set",
			fmt.Errorf("no chain fee source configured"),
			"rate_sat_kw", int64(chainfee.FeePerKwFloor))
	}
}

// scheduleFromConfig converts the operator-facing FeesConfig into
// the immutable fees.Schedule consumed by the calculator. A nil
// FeesConfig yields an all-zero schedule, which is a valid "fees
// disabled" configuration (boarding and refresh flows both compute
// to zero).
func scheduleFromConfig(cfg *FeesConfig) *fees.Schedule {
	if cfg == nil {
		return &fees.Schedule{}
	}

	policy, err := fees.ParseDustPolicy(cfg.MinViableVTXOPolicy)
	if err != nil {
		// Fall back to the stricter reject policy on a
		// malformed config string. The Validate() call in
		// the caller will surface the underlying parse error
		// only if a non-empty unknown value was supplied.
		policy = fees.DustPolicyReject
	}

	return &fees.Schedule{
		AnnualRate:                 cfg.AnnualRate,
		BaseMarginSat:              cfg.BaseMarginSat,
		UtilizationThresholdBPS:    cfg.UtilizationThresholdBPS,
		UtilizationSpreadDelta0BPS: cfg.UtilizationSpreadDelta0BPS,
		UtilizationSpreadDelta1BPS: cfg.UtilizationSpreadDelta1BPS,
		MinViableVTXOPolicy:        policy,
		MinViableVTXOPct:           cfg.MinViableVTXOPct,
		MinRefreshDeltaBlocks:      cfg.MinRefreshDeltaBlocks,
	}
}
