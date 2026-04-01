package metrics

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// InstrumentedLocker wraps a vtxo.Locker with metrics instrumentation.
// Lock duration and failure counts are sent to the metrics actor.
type InstrumentedLocker struct {
	// inner is the underlying locker being instrumented.
	inner vtxo.Locker

	// metricsRef is the metrics actor to send lock results to.
	// Initially None; set via SetMetricsRef after the metrics
	// actor is spawned.
	metricsRef fn.Option[actor.TellOnlyRef[Msg]]
}

// NewInstrumentedLocker creates a new locker that instruments the
// given inner locker. Call SetMetricsRef after the metrics actor is
// spawned to enable metric emission.
func NewInstrumentedLocker(
	inner vtxo.Locker) *InstrumentedLocker {

	return &InstrumentedLocker{
		inner: inner,
	}
}

// SetMetricsRef sets the metrics actor reference. This must be
// called after the metrics actor is spawned.
func (l *InstrumentedLocker) SetMetricsRef(
	ref actor.TellOnlyRef[Msg]) {

	l.metricsRef = fn.Some(ref)
}

// LockMany implements vtxo.Locker.
func (l *InstrumentedLocker) LockMany(ctx context.Context,
	outpoints []wire.OutPoint, owner vtxo.LockOwner) error {

	start := time.Now()
	err := l.inner.LockMany(ctx, outpoints, owner)
	d := time.Since(start)

	l.reportLockResult(ctx, owner, d, err)

	return err
}

// UnlockMany implements vtxo.Locker.
func (l *InstrumentedLocker) UnlockMany(ctx context.Context,
	outpoints []wire.OutPoint, owner vtxo.LockOwner) error {

	return l.inner.UnlockMany(ctx, outpoints, owner)
}

// LockManyWithExpiry implements vtxo.LeaseLocker if the underlying
// locker supports it.
func (l *InstrumentedLocker) LockManyWithExpiry(ctx context.Context,
	outpoints []wire.OutPoint, owner vtxo.LockOwner,
	expiresAt time.Time) error {

	leaseLocker, ok := l.inner.(vtxo.LeaseLocker)
	if !ok {
		return l.LockMany(ctx, outpoints, owner)
	}

	start := time.Now()
	err := leaseLocker.LockManyWithExpiry(
		ctx, outpoints, owner, expiresAt,
	)
	d := time.Since(start)

	l.reportLockResult(ctx, owner, d, err)

	return err
}

// reportLockResult sends a VTXOLockResultMsg to the metrics actor.
func (l *InstrumentedLocker) reportLockResult(ctx context.Context,
	owner vtxo.LockOwner, d time.Duration, err error) {

	l.metricsRef.WhenSome(
		func(ref actor.TellOnlyRef[Msg]) {
			msg := &VTXOLockResultMsg{
				Owner:    ownerCategory(owner),
				Duration: d,
				Success:  err == nil,
			}
			if err != nil {
				switch {
				case errors.Is(err, context.Canceled):
					msg.Reason = "canceled"
				case errors.Is(err, context.DeadlineExceeded):
					msg.Reason = "timeout"
				default:
					msg.Reason = "conflict"
				}
			}

			_ = ref.Tell(ctx, msg)
		},
	)
}

// ownerCategory extracts the subsystem prefix from a LockOwner for
// use as a metric label (e.g. "round" or "oor").
func ownerCategory(owner vtxo.LockOwner) string {
	s := string(owner)
	if idx := strings.IndexByte(s, ':'); idx != -1 {
		return s[:idx]
	}

	return s
}
