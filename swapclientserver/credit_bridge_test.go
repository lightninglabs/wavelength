//go:build swapruntime

package swapclientserver

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
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

// failedPreconditionWithReason builds the swap server's FailedPrecondition
// reply carrying an ErrorInfo detail, wrapped the way the sdk wraps the RPC
// error on its way to the bridge.
func failedPreconditionWithReason(t *testing.T, domain, reason string) error {
	t.Helper()

	st, err := status.New(
		codes.FailedPrecondition, "existing credit pay is in state "+
			"released",
	).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain})
	require.NoError(t, err)

	return fmt.Errorf("create in-swap: CreateInSwap RPC: %w", st.Err())
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
			// The swap server released this pay's reservation, so
			// the payment hash can never be reserved again.
			name: "released reservation",
			startErr: failedPreconditionWithReason(
				t, creditErrorDomain, creditReasonPayReleased,
			),
			invoice: invoice,
		},
		{
			// The swap server refused the pay before reserving or
			// debiting anything.
			name: "credit shortfall",
			startErr: failedPreconditionWithReason(
				t, creditErrorDomain, creditReasonShortfall,
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
			// A server that predates the reason sends the same
			// message and code with no detail.
			name: "released without detail",
			startErr: status.Error(
				codes.FailedPrecondition,
				"existing credit pay is in state released",
			),
		},
		{
			// A reason this client does not know is not terminal.
			name: "unknown reason",
			startErr: failedPreconditionWithReason(
				t, creditErrorDomain, "CREDIT_PAY_DISPATCHED",
			),
		},
		{
			// A terminal reason from another service is not ours.
			name: "foreign domain",
			startErr: failedPreconditionWithReason(
				t, "other.example.com", creditReasonPayReleased,
			),
		},
		{
			// A reason on any other code is not a pay rejection.
			name: "reason on unavailable",
			startErr: func() error {
				st, err := status.New(
					codes.Unavailable, "backend down",
				).WithDetails(&errdetails.ErrorInfo{
					Reason: creditReasonShortfall,
					Domain: creditErrorDomain,
				})
				require.NoError(t, err)

				return st.Err()
			}(),
		},
		{
			// The daemon holds a swap for the invoice, so the
			// released reply may belong to a pay that is underway.
			name: "released with swap recorded",
			startErr: failedPreconditionWithReason(
				t, creditErrorDomain, creditReasonPayReleased,
			),
			summaries: []swaps.SwapSummary{{
				Direction:   swaps.SwapDirectionPay,
				PaymentHash: hash,
				State:       "Completed",
			}},
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

// newTestRedeemBridge builds a credit bridge over a fake swap runtime whose
// RedeemCredit fails with redeemErr.
func newTestRedeemBridge(t *testing.T, redeemErr error) *creditServerBridge {
	t.Helper()

	fakeClient := newFakeSwapRuntime()
	fakeClient.redeemCreditErr = redeemErr
	service := newTestSwapClientService(fakeClient)
	t.Cleanup(service.cancel)

	return &creditServerBridge{svc: service}
}

// TestCreditBridgeRedeemRejection asserts that an InvalidArgument refusal from
// the swap server, such as a redeem amount below the operator VTXO floor,
// reaches the credit operation as ErrRedeemRejected even through the SDK's
// error wrapping, so the operation fails instead of being redelivered.
func TestCreditBridgeRedeemRejection(t *testing.T) {
	t.Parallel()

	rejection := status.Error(
		codes.InvalidArgument,
		"redeem amount 100 sat is below operator VTXO floor 330 sat",
	)
	bridge := newTestRedeemBridge(
		t, fmt.Errorf("RedeemCredit RPC: %w", rejection),
	)

	_, err := bridge.RedeemCredit(
		context.Background(), nil, "redeem:abc", 100, []byte{0x02},
	)
	require.ErrorIs(t, err, credit.ErrRedeemRejected)
}

// TestCreditBridgeRedeemStaysRetryable asserts that every other RedeemCredit
// failure is returned without ErrRedeemRejected, so the credit operation keeps
// retrying it. FailedPrecondition and AlreadyExists are what the swap server
// returns for a replay of an admitted redemption, and ResourceExhausted is a
// transient admission cap.
func TestCreditBridgeRedeemStaysRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{
			name: "unavailable",
			err: status.Error(
				codes.Unavailable, "connection lost",
			),
		},
		{
			name: "deadline exceeded",
			err: status.Error(
				codes.DeadlineExceeded, "timed out",
			),
		},
		{
			name: "canceled",
			err:  context.Canceled,
		},
		{
			name: "no status",
			err:  errors.New("redeem failed"),
		},
		{
			name: "failed precondition",
			err: status.Error(
				codes.FailedPrecondition, "state changed",
			),
		},
		{
			name: "already exists",
			err: status.Error(
				codes.AlreadyExists, "idempotency mismatch",
			),
		},
		{
			name: "resource exhausted",
			err: status.Error(
				codes.ResourceExhausted, "admission capacity",
			),
		},
		{
			name: "unimplemented",
			err: status.Error(
				codes.Unimplemented, "credits are not enabled",
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			bridge := newTestRedeemBridge(t, test.err)

			_, err := bridge.RedeemCredit(
				context.Background(), nil, "redeem:abc", 100,
				[]byte{0x02},
			)
			require.Error(t, err)
			require.NotErrorIs(t, err, credit.ErrRedeemRejected)
		})
	}
}
