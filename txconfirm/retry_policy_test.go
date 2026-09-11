package txconfirm

import (
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestEnsureRetryPolicyDedup rejects policy changes in either direction without
// attaching the rejected subscriber or disturbing the original confirmation.
func TestEnsureRetryPolicyDedup(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := map[bool]string{false: "terminal", true: "retry"}[retry]
		t.Run(name, func(t *testing.T) {
			chain := newFakeChainSourceRef(100)
			ref, _ := newTestActor(
				t, Config{
					ChainSource: chain,
				},
			)
			tx := makeTestTx(false)
			first := actor.NewChannelTellOnlyRef[Notification](
				"first", 4,
			)
			other := actor.NewChannelTellOnlyRef[Notification](
				"other", 4,
			)
			req := &EnsureConfirmedReq{
				Tx:                 tx,
				Subscriber:         first,
				RetryUntilAccepted: retry,
			}
			require.True(
				t, mustEnsure(t, ref.Ref(), req).Created,
			)
			require.False(
				t, mustEnsure(t, ref.Ref(), req).Created,
			)
			conflict := &EnsureConfirmedReq{
				Tx:                 tx,
				Subscriber:         other,
				RetryUntilAccepted: !retry,
			}
			_, err := ref.Ref().Ask(t.Context(), conflict).
				Await(t.Context()).Unpack()
			require.ErrorIs(t, err, ErrEnsureParamsMismatch)
			require.Equal(t, 1, chain.broadcastCallCount())
			require.Equal(t, 1, chain.registerConfCount())
			chain.emitConfirmation(t, tx.TxHash(), 101)
			require.IsType(
				t, &TxConfirmed{},
				mustAwaitNotification(t, first),
			)
			mustHaveNoNotification(t, other)
		})
	}
}
