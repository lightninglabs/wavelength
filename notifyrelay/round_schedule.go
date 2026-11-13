package notifyrelay

import (
	"bytes"
	"context"
	"strconv"
	"time"

	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// roundSchedule observes the authenticated producer's published timetable. The
// source loop owns it; no timer, goroutine or wallet identity is retained. Each
// binding durably records the last scheduled cutoff in its job transaction.
type roundSchedule struct {
	store     *SQLStore
	source    *feedSource
	client    arkrpc.ArkServiceClient
	published fn.Option[batchschedule.Published]
	refreshAt time.Time
}

// step refreshes discovery at most twice a minute and queues at most 64 local
// jobs. Discovery failure cannot hold up source ingestion or a pending ACK.
func (r *roundSchedule) step(ctx context.Context, now time.Time) error {
	if r.source.config.EventClasses&notification.FeedClassRound == 0 {
		return nil
	}
	if !now.Before(r.refreshAt) {
		r.refreshAt = now.Add(30 * time.Second)
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		sent := time.Now()
		info, err := r.client.GetInfo(
			probeCtx, &arkrpc.GetInfoRequest{},
		)
		received := time.Now()
		cancel()
		if err != nil || info == nil || !bytes.Equal(
			info.Pubkey, r.source.public.SerializeCompressed(),
		) {

			// Do not extrapolate stale discovery through an outage.
			r.published = fn.None[batchschedule.Published]()

			return ErrFeedSource
		}
		published, err := arkrpc.ParseBatchSchedule(info.BatchSchedule)
		if err != nil {
			r.published = fn.None[batchschedule.Published]()

			return ErrFeedSource
		}
		r.published = published
		if published.IsSome() {
			p := published.UnsafeFromSome()
			r.published = fn.Some(
				p.WithClockOffset(
					batchschedule.EstimateClockOffset(
						p.ServerTime(), sent, received,
					),
				),
			)
		}
	}
	if r.published.IsNone() {
		return nil
	}
	published := r.published.UnsafeFromSome()
	slot, err := published.Next(
		now.Add(published.ClockOffset()).Add(
			batchschedule.MaxWakeMargin,
		),
	)
	if err != nil {
		return err
	}
	// Wake inside the operator's opening margin. Local expiry also leaves
	// that margin before cutoff; APNs may drop the hint after that instant.
	opens := slot.Opens.Add(batchschedule.MaxWakeMargin).Add(
		-published.ClockOffset(),
	)
	deadline := slot.Cutoff.Add(-batchschedule.MaxWakeMargin).Add(
		-published.ClockOffset(),
	)

	return r.store.scheduleRoundBindings(
		ctx, r.source.config.ID, slot.Cutoff.Unix(), opens, deadline,
		now,
	)
}

// scheduleRoundBindings commits a bounded page and its per-route cutoff marker
// together. Restart and competing replicas cannot queue the same slot twice.
func (s *SQLStore) scheduleRoundBindings(ctx context.Context, sourceID string,
	cutoff int64, opens, deadline, now time.Time) error {

	if !deadline.After(now) || !deadline.After(opens) {
		return nil
	}

	return s.database.Write(ctx, func(q *sqlc.Queries) error {
		bindings, err := q.ListScheduledBindings(
			ctx, sqlc.ListScheduledBindingsParams{
				SourceID: sourceID,
				ExpiresAt: max(
					now.Unix(),
					opens.Unix(),
				),
				ScheduledCutoff: cutoff,
			},
		)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			sub, err := q.GetSubscription(
				ctx, binding.SubscriptionID,
			)
			if err != nil {
				return err
			}
			sub.ExpiresAt = min(
				sub.ExpiresAt, binding.ExpiresAt,
				deadline.Unix(),
			)
			if sub.ExpiresAt <= max(now.Unix(), opens.Unix()) {
				continue
			}
			if err := enqueueSQLJobTx(
				ctx, q, sub, ScopeSchedule,
				sourceID+"/"+strconv.FormatInt(cutoff, 10), now,
			); err != nil {
				return err
			}
			if err := q.SetRoundJobWindow(
				ctx, sqlc.SetRoundJobWindowParams{
					ID: string(ScopeSchedule) + "/" +
						sub.ID,
					NextAt: max(
						now.UnixNano(),
						opens.UnixNano(),
					),
					ExpiresAt: time.
						Unix(sub.ExpiresAt, 0).
						UnixNano(),
				},
			); err != nil {
				return err
			}
			if _, err := q.MarkScheduledBinding(
				ctx, sqlc.MarkScheduledBindingParams{
					SourceID:        sourceID,
					SubscriptionID:  sub.ID,
					ScheduledCutoff: cutoff,
				},
			); err != nil {
				return err
			}
		}

		return nil
	})
}
