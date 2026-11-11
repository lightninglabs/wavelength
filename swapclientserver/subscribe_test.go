//go:build swapruntime

package swapclientserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// snapshotSwapStream publishes a terminal transition while the existing-row
// snapshot is being sent, before the handler enters its live loop.
type snapshotSwapStream struct {
	grpc.ServerStream
	ctx  context.Context //nolint:containedctx
	send func(*swapclientrpc.SubscribeSwapsResponse) error
}

// Context supplies the bounded test lifetime to the subscription handler.
func (s *snapshotSwapStream) Context() context.Context {
	return s.ctx
}

// Send delegates snapshot and live delivery to the controlled test callback.
func (s *snapshotSwapStream) Send(
	r *swapclientrpc.SubscribeSwapsResponse) error {

	return s.send(r)
}

// TestSubscribeSwapsBuffersFailureDuringSnapshot proves a pay failure emitted
// while the existing snapshot is in flight survives the handoff to live mode.
// The old ordering registered the subscriber after the snapshot and lost it.
func TestSubscribeSwapsBuffersFailureDuringSnapshot(t *testing.T) {
	t.Parallel()

	hash := testHash(51)
	client := newFakeSwapRuntime(swaps.SwapSummary{
		Direction:   swaps.SwapDirectionPay,
		PaymentHash: hash,
		State:       "SwapCreated",
		Pending:     true,
	})
	service := newTestSwapClientService(client)
	defer service.cancel()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	var states []swapclientrpc.SwapState
	done := errors.New("received terminal failure")
	stream := &snapshotSwapStream{ctx: ctx}
	stream.send = func(r *swapclientrpc.SubscribeSwapsResponse) error {
		states = append(states, r.GetSwap().GetState())
		if len(states) == 1 {
			client.mu.Lock()
			client.summaries[0].State = "Failed"
			client.summaries[0].Pending = false
			client.mu.Unlock()
			service.publishHash(hash)

			return nil
		}

		return done
	}
	err := service.SubscribeSwaps(
		&swapclientrpc.SubscribeSwapsRequest{
			IncludeExisting: true,
		},
		stream,
	)
	require.ErrorIs(t, err, done)
	require.Equal(t, []swapclientrpc.SwapState{
		swapclientrpc.SwapState_SWAP_STATE_SWAP_CREATED,
		swapclientrpc.SwapState_SWAP_STATE_FAILED,
	}, states)
	service.mu.Lock()
	defer service.mu.Unlock()
	require.Empty(t, service.subscribers)
}
