//go:build swapruntime

package swapclientserver

import (
	"encoding/hex"
	"testing"

	"github.com/lightninglabs/wavelength/sdk/swaps"
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
