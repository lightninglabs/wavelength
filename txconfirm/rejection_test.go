package txconfirm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestBroadcastFailureClassification keeps transport ambiguity and structural
// invalidity outside the class that authorizes a fee replacement.
func TestBroadcastFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want BroadcastFailureClass
	}{
		{
			"minimum relay",
			errors.New("broadcast: min relay fee not met"),
			BroadcastFailureFee,
		},
		{
			"mempool floor",
			errors.New("RPC: mempool min fee not met"),
			BroadcastFailureFee,
		},
		{"replacement fee", errors.New("insufficient fee, rejecting " +
			"replacement"), BroadcastFailureFee},
		{
			"script",
			errors.New("mandatory-script-verify-flag-failed"),
			BroadcastFailurePermanent,
		},
		{
			"structure",
			errors.New("bad-txns-vout-negative"),
			BroadcastFailurePermanent,
		},
		{"version", fmt.Errorf(
			"broadcast: %w",
			ErrNonTRUCParent), BroadcastFailurePermanent},
		{
			"timeout",
			context.DeadlineExceeded,
			BroadcastFailureUnknown,
		},
		{
			"unclassified",
			errors.New("mempool reject"),
			BroadcastFailureUnknown,
		},
		{
			"none",
			nil,
			BroadcastFailureUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(
				t, tc.want, ClassifyBroadcastFailure(tc.err),
			)
		})
	}
}

// TestDirectBroadcastFailureCarriesClass exercises the actual anchorless
// submission and terminal notification path, including the invalid control.
func TestDirectBroadcastFailureCarriesClass(t *testing.T) {
	for _, tc := range []struct {
		reason string
		class  BroadcastFailureClass
		retry  bool
	}{
		{
			"min relay fee not met",
			BroadcastFailureFee, false,
		},
		{
			"mandatory-script-verify-flag-failed",
			BroadcastFailurePermanent, false,
		},
		{
			"connection reset by peer",
			BroadcastFailureUnknown, false,
		},
		{
			"min relay fee not met",
			BroadcastFailureFee,
			true,
		},
		{
			"mandatory-script-verify-flag-failed",
			BroadcastFailurePermanent,
			true,
		},
	} {
		name := tc.reason
		if tc.retry {
			name += "/retry"
		}
		t.Run(name, func(t *testing.T) {
			chain := newFakeChainSourceRef(100)
			chain.broadcastErr = errors.New(tc.reason)
			ref, _ := newTestActor(t, Config{
				ChainSource: chain,
				Wallet:      &fakeWallet{},
			})
			sub := actor.NewChannelTellOnlyRef[Notification](
				"fee-sub", 4,
			)
			resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
				Tx: makeTestTx(false), Subscriber: sub,
				RetryUntilAccepted: tc.retry,
			})
			require.Equal(t, TxStateFailed, resp.State)
			failed, ok := mustAwaitNotification(t, sub).(*TxFailed)
			require.True(t, ok)
			require.Equal(t, tc.class, failed.Class)
			require.Contains(t, failed.Reason, tc.reason)
			require.Equal(t, 1, chain.broadcastCallCount())
		})
	}
}
