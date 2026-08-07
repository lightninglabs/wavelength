package actor

import (
	"context"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// defaultDeadLetterScanInterval is how often the monitor scans for
	// newly parked dead letters when the config leaves the interval
	// unset.
	defaultDeadLetterScanInterval = 30 * time.Second

	// defaultDeadLetterSweepInterval is how often the retention sweep and
	// expired-entry cleanup run when the config leaves the interval
	// unset. Retention is a slow-moving policy, so sweeping far less
	// often than the scan keeps the write load negligible.
	defaultDeadLetterSweepInterval = time.Hour

	// deadLetterScanBatchLimit bounds how many dead letters a single
	// store query returns during a scan. A scan drains the backlog in
	// batches of this size.
	deadLetterScanBatchLimit = 100

	// deadLetterScanMaxBatches bounds how many batches one scan drains,
	// as a backstop against a pathological flood keeping the monitor in
	// a single scan forever. Anything left over is picked up by the next
	// tick.
	deadLetterScanMaxBatches = 100

	// deadLetterMaxDetailLogs bounds how many per-entry log lines a
	// single scan emits. A poison storm parks many messages at once, and
	// one summary line carries the same alerting weight as a hundred
	// detail lines.
	deadLetterMaxDetailLogs = 10
)

// MaintenanceStore is the optional janitorial surface the dead-letter
// monitor drives alongside its retention sweep. CleanupExpired removes
// expired deduplication entries and ask results; nothing else in the daemon
// calls it periodically, so the monitor is its natural owner.
type MaintenanceStore interface {
	// CleanupExpired removes expired deduplication entries and ask
	// results.
	CleanupExpired(ctx context.Context) error
}

// DeadLetterMonitorConfig configures the dead-letter monitor.
type DeadLetterMonitorConfig struct {
	// Store is the dead-letter surface the monitor scans, purges, and
	// counts against.
	Store DeadLetterStore

	// Maintenance optionally carries the store's janitorial surface.
	// When set, the monitor runs CleanupExpired on every retention
	// sweep, so expired dedup entries and ask results are garbage
	// collected on the same cadence as old dead letters.
	Maintenance MaintenanceStore

	// ScanInterval is how often the monitor looks for newly parked dead
	// letters. Zero applies the default.
	ScanInterval time.Duration

	// SweepInterval is how often the retention sweep (and CleanupExpired)
	// runs. Zero applies the default.
	SweepInterval time.Duration

	// Retention is how long dead letters are kept before the sweep
	// deletes them. Zero disables the purge entirely: dead letters can
	// carry value-bearing messages, so aging them out is an explicit
	// operator opt-in, never a default.
	Retention time.Duration

	// Clock is the time source, injectable for tests.
	Clock clock.Clock

	// Log is the logger attached to the monitor's runtime context. When
	// unset, the monitor falls back to btclog.Disabled.
	Log fn.Option[btclog.Logger]

	// OnDeadLetter is an optional hook invoked exactly once per newly
	// observed dead letter, after it is logged. The daemon uses it to
	// feed metrics. It runs on the monitor goroutine, so it must not
	// block.
	OnDeadLetter func(DeadLetter)
}

// DeadLetterMonitor is a background service that gives dead letters an
// owner. Until it existed, a message that exhausted its retry policy was
// written to the dead-letter store and never looked at again by anything:
// no log, no metric, no recovery path (#705). The monitor closes the
// observability half of that gap: it periodically scans for newly parked
// dead letters, surfaces each one exactly once (log line plus the
// OnDeadLetter hook), and applies the retention policy. The recovery half
// is the operator surface built on DeadLetterStore.RequeueDeadLetter.
type DeadLetterMonitor struct {
	cfg DeadLetterMonitorConfig

	// ctx is the monitor's lifecycle context.
	ctx context.Context

	// cancel cancels the monitor's context.
	cancel context.CancelFunc

	// wg tracks the background goroutine.
	wg sync.WaitGroup

	// startOnce ensures the run loop is only started once.
	startOnce sync.Once

	// stopOnce ensures Stop is only executed once.
	stopOnce sync.Once

	// watermark and afterID form the strict (created_at, id) keyset
	// cursor of the incremental scan: the store returns only rows
	// strictly after the cursor, so every row surfaces exactly once and
	// a flood of same-second entries can never stall the scan. Only the
	// monitor goroutine touches them after Start.
	//
	// The cursor keys on the WRITER's created_at stamp, so on a
	// multi-writer store a writer whose clock lags the cursor can park
	// entries the incremental scan never returns; those still show in
	// the scrape-driven count gauges (which are cursor-free) and in the
	// next restart's backlog summary.
	watermark int64
	afterID   string
}

// NewDeadLetterMonitor creates a new dead-letter monitor.
func NewDeadLetterMonitor(cfg DeadLetterMonitorConfig) *DeadLetterMonitor {
	if cfg.ScanInterval == 0 {
		cfg.ScanInterval = defaultDeadLetterScanInterval
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = defaultDeadLetterSweepInterval
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}

	baseCtx := build.ContextWithLogger(
		context.Background(), cfg.Log.UnwrapOr(btclog.Disabled),
	)
	ctx, cancel := context.WithCancel(baseCtx)

	return &DeadLetterMonitor{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start begins the background monitoring loop.
func (m *DeadLetterMonitor) Start() {
	m.startOnce.Do(func() {
		logger(m.ctx).InfoS(m.ctx, "Starting dead-letter monitor",
			"scan_interval", m.cfg.ScanInterval,
			"sweep_interval", m.cfg.SweepInterval,
			"retention", m.cfg.Retention,
		)

		m.watermark = m.cfg.Clock.Now().Unix()

		m.wg.Add(1)
		go m.run()
	})
}

// Stop signals the monitor to terminate and waits for it to finish.
func (m *DeadLetterMonitor) Stop() {
	m.stopOnce.Do(func() {
		m.cancel()
		m.wg.Wait()

		logger(m.ctx).DebugS(m.ctx, "Dead-letter monitor stopped")
	})
}

// run is the main monitoring loop.
func (m *DeadLetterMonitor) run() {
	defer m.wg.Done()

	// Surface the backlog parked by previous runs once at startup. The
	// incremental scan only reports entries created from now on, so
	// without this line a restart would silently absorb a populated
	// queue.
	m.reportBacklog()

	// Run one sweep immediately so a freshly configured retention takes
	// effect at startup rather than after the first interval.
	m.sweep()

	scanTicker := time.NewTicker(m.cfg.ScanInterval)
	defer scanTicker.Stop()

	sweepTicker := time.NewTicker(m.cfg.SweepInterval)
	defer sweepTicker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return

		case <-scanTicker.C:
			m.scan()

		case <-sweepTicker.C:
			m.sweep()
		}
	}
}

// reportBacklog logs a summary of dead letters that were already parked
// before the monitor started.
func (m *DeadLetterMonitor) reportBacklog() {
	count, err := m.cfg.Store.CountDeadLetters(m.ctx)
	if err != nil {
		if !isExpectedShutdownErr(err) {
			logger(m.ctx).WarnS(
				m.ctx,
				"Failed to count dead-letter backlog",
				err,
			)
		}

		return
	}

	if count == 0 {
		return
	}

	logger(m.ctx).WarnS(m.ctx,
		"Dead letters parked from previous runs await operator action",
		nil, "count", count)
}

// scan surfaces dead letters that appeared since the previous scan. The
// strict (created_at, id) keyset cursor means every returned row is new by
// construction: the cursor advances to the last row of each batch, so a
// flood of entries sharing one created_at second pages through cleanly
// instead of re-reading the same first page. Anything left when the batch
// cap trips is picked up by the next tick, since the cursor is persistent
// monitor state.
func (m *DeadLetterMonitor) scan() {
	var newEntries []DeadLetter

	// Rows stamped in the still-open wall-clock second are deferred to
	// the next scan. A dead letter's id is the original message id,
	// minted at enqueue time rather than at dead-letter time, so within
	// one second a later write can carry a SMALLER id than the cursor;
	// advancing the cursor into an open second would skip such a row
	// forever. Once a second has closed it can receive no further writes
	// from this process's clock, and the keyset over it is exact.
	openSecond := m.cfg.Clock.Now().Unix()

	for batches := 0; batches < deadLetterScanMaxBatches; batches++ {
		entries, err := m.cfg.Store.ListDeadLettersSince(
			m.ctx, time.Unix(m.watermark, 0), m.afterID,
			deadLetterScanBatchLimit,
		)
		if err != nil {
			if !isExpectedShutdownErr(err) {
				logger(m.ctx).WarnS(
					m.ctx,
					"Dead-letter scan failed",
					err,
				)
			}

			return
		}

		// Truncate at the first row in the open second; the cursor
		// must not advance past it.
		full := len(entries) == deadLetterScanBatchLimit
		for i, entry := range entries {
			if entry.CreatedAt.Unix() >= openSecond {
				entries = entries[:i]
				full = false

				break
			}
		}

		if len(entries) > 0 {
			last := entries[len(entries)-1]
			m.watermark = last.CreatedAt.Unix()
			m.afterID = last.ID

			newEntries = append(newEntries, entries...)
		}

		if !full {
			break
		}
	}

	m.reportNewEntries(newEntries)
}

// reportNewEntries surfaces newly observed dead letters: a bounded number of
// detail lines, a summary when the scan found any, and the OnDeadLetter hook
// for each.
//
// The detail lines log at error level deliberately. The daemon durably
// accepted these messages and has now abandoned them after exhausting
// retries; whatever triggered the individual failures, dropping an accepted
// message is a breach of the delivery contract that requires operator
// action, exactly like the ingress deferral episodes.
func (m *DeadLetterMonitor) reportNewEntries(entries []DeadLetter) {
	if len(entries) == 0 {
		return
	}

	for i, entry := range entries {
		if i >= deadLetterMaxDetailLogs {
			break
		}

		logger(m.ctx).ErrorS(m.ctx, "Message dead-lettered", nil,
			"dead_letter_id", entry.ID,
			"actor_id", entry.ActorID,
			"msg_type", entry.MessageType,
			"source", entry.Source,
			"attempts", entry.Attempts,
			"reason", entry.FailureReason)
	}

	if len(entries) > deadLetterMaxDetailLogs {
		logger(m.ctx).ErrorS(m.ctx,
			"Further dead letters suppressed from detail logging",
			nil,
			"total_new", len(entries),
			"logged", deadLetterMaxDetailLogs)
	}

	if m.cfg.OnDeadLetter != nil {
		for _, entry := range entries {
			m.cfg.OnDeadLetter(entry)
		}
	}
}

// sweep applies the retention policy and runs the expired-entry cleanup.
func (m *DeadLetterMonitor) sweep() {
	if m.cfg.Retention > 0 {
		cutoff := m.cfg.Clock.Now().Add(-m.cfg.Retention)

		removed, err := m.cfg.Store.PurgeDeadLetters(m.ctx, cutoff)
		switch {
		case err != nil:
			if !isExpectedShutdownErr(err) {
				logger(m.ctx).WarnS(
					m.ctx,
					"Dead-letter retention sweep failed",
					err,
				)
			}

		case removed > 0:
			logger(m.ctx).InfoS(
				m.ctx,
				"Dead-letter retention sweep removed entries",
				"removed", removed,
				"cutoff", cutoff,
			)
		}
	}

	if m.cfg.Maintenance != nil {
		if err := m.cfg.Maintenance.CleanupExpired(m.ctx); err != nil {
			if !isExpectedShutdownErr(err) {
				logger(m.ctx).WarnS(
					m.ctx,
					"Expired-entry cleanup failed",
					err,
				)
			}
		}
	}
}
