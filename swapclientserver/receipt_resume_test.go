//go:build swapruntime

package swapclientserver

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestTargetedReceiveResume never scans or starts a pay/unknown source and
// shares duplicate-worker ownership with ordinary foreground resume.
func TestTargetedReceiveResume(t *testing.T) {
	receive, pay := testHash(71), testHash(72)
	client := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionReceive,
			PaymentHash: receive,
			Pending:     true,
		}, swaps.SwapSummary{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: pay,
			Pending:     true,
		},
	)
	service := newTestSwapClientService(client)
	t.Cleanup(service.cancel)
	require.NoError(
		t,
		service.ResumeReceive(
			t.Context(), hex.EncodeToString(receive[:]),
		),
	)
	client.awaitReceiveResume(t, receive)
	require.NoError(
		t,
		service.ResumeReceive(
			t.Context(), hex.EncodeToString(receive[:]),
		),
	)
	require.Equal(t, 1, client.receiveResumeCount(receive))
	require.ErrorContains(
		t,
		service.ResumeReceive(
			t.Context(), hex.EncodeToString(pay[:]),
		),
		"not a receive",
	)
	require.Zero(t, client.payResumeCount(pay))
	require.False(t, client.sawPendingOnlyList())
}

// TestTargetedReceiveRearmsExitedWarmWorker proves a pending source can be
// restarted after its prior worker returned, without a history scan or a pay.
func TestTargetedReceiveRearmsExitedWarmWorker(t *testing.T) {
	hash := testHash(73)
	client := newFakeSwapRuntime(swaps.SwapSummary{
		Direction: swaps.SwapDirectionReceive, PaymentHash: hash, Pending: true,
	})
	done := make(chan struct{})
	client.receiveDone = map[lntypes.Hash]chan struct{}{hash: done}
	service := newTestSwapClientService(client)
	t.Cleanup(service.cancel)
	id := hex.EncodeToString(hash[:])
	require.NoError(t, service.ResumeReceive(t.Context(), id))
	client.awaitReceiveResume(t, hash)
	close(done)
	require.Eventually(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()

		return service.activeReceives == 0
	}, time.Second, time.Millisecond)
	require.Equal(t, 1, client.receiveResumeCount(hash))
	client.mu.Lock()
	client.receiveDone[hash] = nil
	client.mu.Unlock()
	require.NoError(t, service.ResumeReceive(t.Context(), id))
	client.awaitReceiveResume(t, hash)
	require.Equal(t, 2, client.receiveResumeCount(hash))
	require.NoError(t, service.ResumeReceive(t.Context(), id))
	require.Equal(t, 2, client.receiveResumeCount(hash))
	require.False(t, client.sawPendingOnlyList())
}

// TestTargetedReceiveRetainsActiveAndQueuedOwners keeps a background re-arm
// from bypassing the receive FIFO or creating another active signer owner.
func TestTargetedReceiveRetainsActiveAndQueuedOwners(t *testing.T) {
	first, second := testHash(74), testHash(75)
	client := newFakeSwapRuntime(
		swaps.SwapSummary{Direction: swaps.SwapDirectionReceive,
			PaymentHash: first, Pending: true},
		swaps.SwapSummary{Direction: swaps.SwapDirectionReceive,
			PaymentHash: second, Pending: true},
	)
	service := newTestSwapClientService(client)
	service.maxActiveReceives = 1
	t.Cleanup(service.cancel)
	require.Equal(t, workerStarted, service.enqueuePendingReceive(first))
	client.awaitReceiveResume(t, first)
	require.Equal(t, workerQueued, service.enqueuePendingReceive(second))
	for _, hash := range []lntypes.Hash{first, second} {
		require.NoError(
			t,
			service.ResumeReceive(
				t.Context(), hex.EncodeToString(hash[:]),
			),
		)
	}
	service.mu.Lock()
	active := service.activeReceives
	queued := append([]lntypes.Hash(nil), service.pendingReceives...)
	service.mu.Unlock()
	require.Equal(t, 1, active)
	require.Equal(t, []lntypes.Hash{second}, queued)
	require.Equal(t, 1, client.receiveResumeCount(first))
	require.Zero(t, client.receiveResumeCount(second))
	require.False(t, client.sawPendingOnlyList())
}
