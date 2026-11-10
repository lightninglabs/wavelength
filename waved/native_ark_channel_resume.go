package waved

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
)

// nativeArkChannelResumeIDs validates the complete inventory synchronously.
// Replay must reload records in owned workers because a recovery watch can
// make even an initially active channel require future-block materialization.
func nativeArkChannelResumeIDs(ctx context.Context,
	service *arkchannel.Service) ([]arkchannel.ID, error) {

	records, err := service.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]arkchannel.ID, 0, len(records))
	for _, record := range records {
		_, err := arkchannel.PendingAction(record.Snapshot)
		if err != nil {
			return nil, fmt.Errorf("validate channel recovery "+
				"%x: %w", record.Snapshot.Terms.ID[:4], err)
		}
		ids = append(ids, record.Snapshot.Terms.ID)
	}

	return ids, nil
}

// startNativeArkChannelResume isolates durable replay in the existing
// controller lifecycle. One channel waiting for blocks cannot hold up startup
// or another channel; Stop cancels and joins these workers before lnd closes.
func (c *NativeArkChannelController) startNativeArkChannelResume(
	service *arkchannel.Service, node arkChannelForceCloseResumer,
	ids []arkchannel.ID) {

	for _, id := range ids {
		c.startLifecycleWorker(func(ctx context.Context) {
			c.resumeNativeArkChannel(ctx, service, node, id)
		})
	}
}

// resumeNativeArkChannel retries the action from fresh durable state instead
// of retaining a stale action or treating an operational failure as terminal.
func (c *NativeArkChannelController) resumeNativeArkChannel(ctx context.Context,
	service *arkchannel.Service, node arkChannelForceCloseResumer,
	id arkchannel.ID) {

	for ctx.Err() == nil {
		record, err := service.ResumeChannelAction(ctx, id)
		if err == nil &&
			shouldResumeOnchainArkChannel(record.Snapshot) {

			err = resumeOnchainArkChannelRecords(
				node, []arkchannel.Record{record},
			)
		}
		if err == nil || ctx.Err() != nil {
			return
		}
		if c.cfg.Log != nil {
			c.cfg.Log.DebugS(
				ctx,
				"Ark channel recovery deferred; retrying",
				btclog.Hex("channel_id", id[:]),
				btclog.Fmt("error", "%v", err),
			)
		}

		select {
		case <-ctx.Done():
			return

		case <-c.cfg.Clock.TickAfter(
			defaultArkChannelClientRetryInterval,
		):
		}
	}
}
