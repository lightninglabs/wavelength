//go:build swapruntime

package swapclientserver

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestCreditBridge builds a credit bridge over a fake swap runtime whose
// StartPay fails with startPayErr. The service decodes invoices against
// regtest, as the daemon does for its configured network.
func newTestCreditBridge(t *testing.T, startPayErr error,
	summaries ...swaps.SwapSummary) (*creditServerBridge,
	*fakeSwapRuntime) {

	t.Helper()

	fakeClient := newFakeSwapRuntime(summaries...)
	fakeClient.startPayErr = startPayErr
	service := newTestSwapClientService(fakeClient)
	service.chainParams = &chaincfg.RegressionNetParams
	t.Cleanup(service.cancel)

	return &creditServerBridge{svc: service}, fakeClient
}

// TestCreditBridgeStartPayRejection asserts that each refusal the swap client
// returns before it creates a pay swap reaches the credit operation as
// ErrPayRejected, so the operation fails instead of being redelivered.
func TestCreditBridgeStartPayRejection(t *testing.T) {
	t.Parallel()

	invoice := testStartPayInvoice(t, testHash(41), 10_000)

	tests := []struct {
		name     string
		startErr error
		invoice  string
	}{
		{
			name: "malformed invoice",
			startErr: status.Error(
				codes.InvalidArgument, "decode invoice",
			),
			invoice: invoice,
		},
		{
			name: "max fee exceeded",
			startErr: status.Error(
				codes.InvalidArgument,
				"in-swap fee 9 exceeds max fee 1",
			),
			invoice: invoice,
		},
		{
			name: "unimplemented",
			startErr: status.Error(
				codes.Unimplemented, "unknown method",
			),
			invoice: invoice,
		},
		{
			// The handler itself rejects an empty invoice before it
			// calls the swap client.
			name:    "empty invoice",
			invoice: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			bridge, _ := newTestCreditBridge(t, test.startErr)

			err := bridge.StartPay(
				context.Background(), test.invoice, 1, 0,
			)
			require.ErrorIs(t, err, credit.ErrPayRejected)
		})
	}
}

// TestCreditBridgeStartPayStaysRetryable asserts that errors which do not prove
// nothing was started are returned without ErrPayRejected, so the credit
// operation keeps retrying them.
func TestCreditBridgeStartPayStaysRetryable(t *testing.T) {
	t.Parallel()

	hash := testHash(42)
	invoice := testStartPayInvoice(t, hash, 10_000)
	rejection := status.Error(codes.InvalidArgument, "rejected")

	tests := []struct {
		name      string
		startErr  error
		summaries []swaps.SwapSummary
		noParams  bool
	}{
		{
			name: "unavailable",
			startErr: status.Error(
				codes.Unavailable, "connection lost",
			),
		},
		{
			name: "deadline exceeded",
			startErr: status.Error(
				codes.DeadlineExceeded, "timed out",
			),
		},
		{
			name:     "canceled",
			startErr: context.Canceled,
		},
		{
			// A failure after the server accepted the swap, such as
			// the client-side quote check, carries no status.
			name:     "no status",
			startErr: errors.New("validate in-swap quote"),
		},
		{
			name: "ambiguous code",
			startErr: status.Error(
				codes.FailedPrecondition, "credit unavailable",
			),
		},
		{
			// An earlier StartPay of this operation left a record
			// the swap server now refuses to create again.
			name:     "swap already recorded",
			startErr: rejection,
			summaries: []swaps.SwapSummary{{
				Direction:   swaps.SwapDirectionPay,
				PaymentHash: hash,
				State:       "Completed",
			}},
		},
		{
			name:     "swap record unknowable",
			startErr: rejection,
			noParams: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			bridge, _ := newTestCreditBridge(
				t, test.startErr, test.summaries...,
			)
			if test.noParams {
				bridge.svc.chainParams = nil
			}

			err := bridge.StartPay(
				context.Background(), invoice, 1, 0,
			)
			require.Error(t, err)
			require.NotErrorIs(t, err, credit.ErrPayRejected)
		})
	}
}
