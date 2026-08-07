package actor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// A durable mailbox has no in-memory capacity, so the classic "mailbox full"
// backpressure signal never fires for it: every Tell lands as a database row
// and the backlog grows without bound while the consumer falls behind. The
// watermark check in this file is the missing bound. It reads the persistent
// backlog depth (with a TTL-cached probe so the common path costs nothing)
// and refuses new sends with ErrMailboxSaturated once the depth crosses a
// configured hard high watermark, with a soft watermark below it that only
// logs. Control-priority messages (restore/resume/restart) and
// outbox-propagated CDC deliveries are exempt so recovery and committed
// hand-offs always land, and a mailbox with no watermarks configured behaves
// exactly as before.

const (
	// DefaultSoftHighWatermark is the backlog depth at which a durable
	// mailbox that opts into watermarks starts logging that its consumer
	// is falling behind. Sends still succeed; the soft watermark is purely
	// an early-warning signal.
	DefaultSoftHighWatermark = 1_000

	// DefaultHardHighWatermark is the backlog depth at which a durable
	// mailbox that opts into watermarks refuses new sends with
	// ErrMailboxSaturated. At this depth the consumer is more than an
	// order of magnitude behind the soft warning, so accepting more work
	// only deepens the hole: shedding load at the producer is the only
	// move that helps.
	DefaultHardHighWatermark = 10_000

	// depthProbeTTL bounds how often the watermark check issues a real
	// COUNT query against the store. Between probes the check works off
	// the cached depth plus a local count of sends accepted since the
	// probe: within the window, LOCAL sends can only push the estimate up
	// (acks that shrink the backlog surface at the next probe), which is
	// the safe direction for an admission check. Sends from other
	// processes or replicas are invisible for up to one window, so the
	// bound is approximate, not exact. A one-second window keeps the
	// probe cost negligible against the write each send already performs.
	depthProbeTTL = time.Second
)

// MailboxDepthCount reports the pending backlog of one durable mailbox.
type MailboxDepthCount struct {
	// MailboxID identifies the mailbox.
	MailboxID string

	// Depth is the number of rows currently parked in the mailbox,
	// leased or not. Rows are deleted on ack, so this is exactly the
	// undelivered backlog.
	Depth int64
}

// MailboxDepthStore is the read-side surface for observing durable mailbox
// backlog depth. It is deliberately separate from DeliveryStore: the delivery
// pipeline never needs these reads, and folding them into DeliveryStore would
// force every test double through the depth surface. The watermark check and
// the metrics scrape both discover the capability with a type assertion, and
// a store that does not implement it simply runs without watermarks.
type MailboxDepthStore interface {
	// MailboxDepth returns the number of messages currently parked in the
	// given mailbox, including leased (in-flight) messages. Rows are
	// deleted on ack, so the count is the undelivered backlog.
	MailboxDepth(ctx context.Context, mailboxID string) (int64, error)

	// MailboxDepths returns the backlog of every mailbox that currently
	// holds at least one message. Mailboxes with an empty backlog are
	// absent from the result, which keeps a scrape cheap and bounded.
	MailboxDepths(ctx context.Context) ([]MailboxDepthCount, error)
}

// depthProbe caches the mailbox's probed backlog depth so the watermark check
// on the send path does not issue a COUNT query per send. Between probes the
// estimate is the probed depth plus the sends this mailbox accepted since,
// which is deliberately one-sided for LOCAL traffic: local sends push the
// estimate up immediately, while acks only surface at the next probe.
// Overshooting is the safe direction for an admission check; remote sends
// remain invisible for up to one probe window, so the bound is approximate.
type depthProbe struct {
	mu sync.Mutex

	// probedAt is when the cached depth was last read from the store. The
	// zero value forces a probe on the first checked send.
	probedAt time.Time

	// probing is true while one sender runs the COUNT query outside the
	// mutex. It single-flights the probe: concurrent senders keep using
	// the cached estimate instead of stacking up behind the query or
	// issuing duplicates.
	probing bool

	// depth is the backlog depth reported by the last probe.
	depth int64

	// sentSinceProbe counts sends accepted since the last probe.
	sentSinceProbe int64

	// softBreached tracks whether the estimate was at or above the soft
	// watermark after the last check, so the breach and the recovery are
	// each logged once per episode instead of once per send.
	softBreached bool
}

// checkWatermarks admits or refuses a send against the mailbox's configured
// backlog watermarks. It returns nil when watermarks are disabled, the store
// cannot report depth, the message is exempt (control priority or an
// outbox-propagated delivery), or the estimated depth is below the hard
// watermark. It returns an error wrapping ErrMailboxSaturated when the
// estimate is at or above the hard watermark.
//
// A probe failure fails OPEN: refusing delivery because a monitoring read
// broke would convert an observability fault into message loss, which is
// strictly worse than a temporarily unenforced bound.
func (m *DurableMailbox[M, R]) checkWatermarks(ctx context.Context,
	priority int) error {

	soft, hard := m.cfg.SoftHighWatermark, m.cfg.HardHighWatermark
	if m.depthStore == nil || (soft <= 0 && hard <= 0) {
		return nil
	}

	// Control and restart messages always land: a saturated mailbox
	// usually means the actor is wedged or gone, and the restore/resume
	// message that would un-wedge it must not be refused by the very
	// backlog it exists to drain.
	if priority >= ControlPriority {
		return nil
	}

	// Outbox-propagated deliveries are exempt: the message was already
	// accepted at its true producer and durably committed to the outbox,
	// so refusing the CDC hand-off here sheds nothing — it only strands a
	// committed message. Worse, the publisher's claim path bumps delivery
	// attempts in its own transaction, so repeated refusals would
	// dead-letter the outbox row (and any DurableAsk response it carries)
	// instead of exerting backpressure. The publisher stamps the outbox ID
	// into the context on every folded delivery, which is exactly the
	// marker keyed off here.
	if _, fromOutbox := OutboxIDFromContext(ctx); fromOutbox {
		return nil
	}

	m.depth.mu.Lock()
	defer m.depth.mu.Unlock()

	now := m.clock.Now()
	stale := m.depth.probedAt.IsZero() ||
		now.Sub(m.depth.probedAt) >= depthProbeTTL

	// Refresh the cached depth, single-flighted: only one sender runs the
	// COUNT while concurrent senders keep using the cached estimate. The
	// query runs OUTSIDE the mutex (so senders never park behind a slow
	// read) and OUTSIDE the caller's ambient transaction: joining a
	// sender's SERIALIZABLE write transaction would take predicate locks
	// over the whole mailbox partition and manufacture rw-conflicts with
	// the consumer's concurrent acks, exactly when the system is already
	// contended. The local delta already accounts for this sender's own
	// uncommitted enqueues, so the probe only needs committed state.
	if stale && !m.depth.probing {
		m.depth.probing = true

		// Snapshot the delta before releasing the mutex: sends that
		// race the probe increment it while the COUNT runs, and their
		// rows are enqueued after their check returns, so the COUNT
		// cannot see them. Subtracting the snapshot (rather than
		// zeroing) keeps those racing increments in the estimate,
		// preserving the local no-undershoot property across the
		// probe window.
		preProbeSent := m.depth.sentSinceProbe
		m.depth.mu.Unlock()

		depth, err := m.depthStore.MailboxDepth(
			WithoutTx(ctx), m.cfg.MailboxID,
		)

		m.depth.mu.Lock()
		m.depth.probing = false

		if err != nil {
			log := logger(m.actorCtx)
			log.WarnS(ctx, "Mailbox depth probe failed, "+
				"falling back to cached estimate", err,
				slog.String("mailbox_id", m.cfg.MailboxID),
			)
		} else {
			m.depth.probedAt = m.clock.Now()
			m.depth.depth = depth
			m.depth.sentSinceProbe -= preProbeSent
		}
	}

	// No baseline at all -- the first probe failed or is still in flight
	// on another sender -- fails open.
	if m.depth.probedAt.IsZero() {
		return nil
	}

	estimate := m.depth.depth + m.depth.sentSinceProbe

	// Evaluate the soft-watermark episode BEFORE the hard refusal, so a
	// backlog that enters saturation within one probe window still opens
	// the episode: the soft warning must fire at or before the first
	// refused send, or failing sends would be the operator's first signal.
	// The episode logs once per breach: the first check that finds the
	// estimate over the soft mark opens it, and the first check after it
	// falls back under closes it.
	if soft > 0 {
		breached := estimate >= int64(soft)
		switch {
		case breached && !m.depth.softBreached:
			log := logger(m.actorCtx)
			log.WarnS(ctx, "Mailbox backlog crossed soft "+
				"watermark: consumer is falling behind", nil,
				slog.String("mailbox_id", m.cfg.MailboxID),
				slog.Int64("depth", estimate),
				slog.Int("soft_watermark", soft),
				slog.Int("hard_watermark", hard),
			)

		case !breached && m.depth.softBreached:
			log := logger(m.actorCtx)
			log.InfoS(ctx, "Mailbox backlog fell below soft "+
				"watermark",
				slog.String("mailbox_id", m.cfg.MailboxID),
				slog.Int64("depth", estimate),
				slog.Int("soft_watermark", soft),
			)
		}
		m.depth.softBreached = breached
	}

	if hard > 0 && estimate >= int64(hard) {
		return fmt.Errorf("mailbox %s backlog %d at hard watermark "+
			"%d: %w", m.cfg.MailboxID, estimate, hard,
			ErrMailboxSaturated)
	}

	// Count this send into the estimate now, before the enqueue runs: a
	// failed enqueue leaves the estimate one high until the next probe,
	// which is the safe direction.
	m.depth.sentSinceProbe++

	return nil
}

// normalizeWatermarks resolves the soft/hard watermark pair from raw config
// values. Negative values are treated as disabled, and a soft watermark above
// the hard one is lowered to it: the soft warning must fire at or before the
// hard refusal, or the operator's first signal of trouble would be failing
// sends.
func normalizeWatermarks(soft, hard int) (int, int) {
	if soft < 0 {
		soft = 0
	}
	if hard < 0 {
		hard = 0
	}
	if hard > 0 && soft > hard {
		soft = hard
	}

	return soft, hard
}
