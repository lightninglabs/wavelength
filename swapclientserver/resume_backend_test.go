//go:build swapruntime

package swapclientserver

import (
	"encoding/hex"
	"testing"

	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
)

// TestServiceImplementsSwapBackend confirms that *swapClientService satisfies
// the waved.SwapBackend interface so the wavewalletrpc registrar can hold a
// handle to it via cfg.Swap.Backend without depending on swapclientserver
// internals.
func TestServiceImplementsSwapBackend(t *testing.T) {
	t.Parallel()

	service := newTestSwapClientService(newFakeSwapRuntime())
	defer service.cancel()

	var backend waved.SwapBackend = service
	require.NotNil(t, backend)
}

// TestExportedResumePendingDrivesSameSweep confirms that the exported
// ResumePending method behaves identically to the private resumePending
// implementation used by the existing in-package Register path.
func TestExportedResumePendingDrivesSameSweep(t *testing.T) {
	t.Parallel()

	payHash := testHash(21)
	receiveHash := testHash(22)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: payHash,
			State:       "funding",
			Pending:     true,
		},
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionReceive,
			PaymentHash: receiveHash,
			State:       "invoice_created",
			Pending:     true,
		},
	)
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	// Drive the sweep through the public Backend surface that wavewalletrpc
	// will call.
	var backend waved.SwapBackend = service
	backend.ResumePending(t.Context())

	fakeClient.awaitPayResume(t, payHash)
	fakeClient.awaitReceiveResume(t, receiveHash)

	require.Equal(
		t, 1, fakeClient.payResumeCount(payHash),
		"pay worker should resume exactly once for hash %s",
		hex.EncodeToString(payHash[:]),
	)
	require.Equal(
		t, 1, fakeClient.receiveResumeCount(receiveHash),
		"receive worker should resume exactly once for hash %s",
		hex.EncodeToString(receiveHash[:]),
	)

	// Calling again must be a no-op because the active set is gated by
	// the per-payment-hash admission map.
	backend.ResumePending(t.Context())

	require.Equal(
		t, 1, fakeClient.payResumeCount(payHash),
		"resume must be idempotent for the same payment hash",
	)
	require.Equal(t, 1, fakeClient.receiveResumeCount(receiveHash))
}
