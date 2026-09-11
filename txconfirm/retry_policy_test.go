package txconfirm

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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

// TestAnchorlessRetryUntilAccepted proves that repeated transient failures keep
// the original signed transaction watched, use block pacing, and escalate
// before recovery. Backend acceptance with a lost response is safe to retry
// unchanged.
func TestAnchorlessRetryUntilAccepted(t *testing.T) {
	for _, recoveryErr := range []error{nil, fmt.Errorf(
		"txn-already-in-mempool")} {
		t.Run(fmt.Sprint(recoveryErr), func(t *testing.T) {
			var logs bytes.Buffer
			logger := btclog.NewSLogger(
				btclog.NewDefaultHandler(&logs),
			)
			logger.SetLevel(btclog.LevelTrace)
			chain := newFakeChainSourceRef(100)
			chain.broadcastErr = fmt.Errorf("backend temporarily " +
				"unavailable")
			ref, _ := newTestActor(t, Config{
				ChainSource: chain, FeeBumpIntervalBlocks: 2,
				BroadcastFailureAlertThreshold: 3,
				Log: fn.Some[btclog.Logger](
					logger,
				),
			})
			tx := makeTestTx(false)
			tx.TxIn[0].Witness = wire.TxWitness{[]byte{1, 2, 3}}
			original := tx.Copy()
			sub := actor.NewChannelTellOnlyRef[Notification](
				"retry", 4,
			)
			req := &EnsureConfirmedReq{
				Tx:                 tx,
				Subscriber:         sub,
				RetryUntilAccepted: true,
			}
			require.Equal(
				t, TxStateBroadcasting,
				mustEnsure(t, ref.Ref(), req).State,
			)
			// The caller cannot mutate the actor's retained
			// transaction after admission.
			tx.TxIn[0].Witness[0][0] = 9
			req.Tx = original
			for _, step := range []struct {
				height int32
				calls  int
			}{
				{
					101,
					1,
				}, {
					102,
					2,
				}, {
					102,
					2,
				}, {
					103,
					2,
				}, {
					104,
					3,
				}, {
					105,
					3,
				},
			} {
				chain.emitBlock(t, step.height)
				require.Equal(
					t, TxStateBroadcasting,
					mustEnsure(t, ref.Ref(), req).State,
				)
				require.Equal(
					t, step.calls,
					chain.broadcastCallCount(),
				)
			}
			mustHaveNoNotification(t, sub)
			require.Contains(
				t, logs.String(),
				"operator intervention",
			)
			chain.mu.Lock()
			chain.broadcastErr = recoveryErr
			chain.mu.Unlock()
			chain.emitBlock(t, 106)
			require.Equal(
				t, TxStateAwaitingConfirmation,
				mustEnsure(t, ref.Ref(), req).State,
			)
			require.Equal(t, 4, chain.broadcastCallCount())
			chain.mu.Lock()
			for _, call := range chain.broadcastCalls {
				require.Equal(t, original, call.Tx)
				require.Equal(
					t, original.TxHash(), call.Tx.TxHash(),
				)
			}
			chain.mu.Unlock()
			require.Equal(t, 0, chain.packageCallCount())
			chain.emitConfirmation(t, original.TxHash(), 107)
			notification := mustAwaitNotification(t, sub)
			confirmed, ok := notification.(*TxConfirmed)
			require.True(t, ok)
			require.Equal(t, original.TxHash(), confirmed.Txid)
		})
	}
}

// TestRetryPolicyCannotOverrideStructuralRejection keeps the broadcaster's
// non-TRUC ephemeral-anchor gate terminal even when a caller opts into retry.
func TestRetryPolicyCannotOverrideStructuralRejection(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{ChainSource: chain})
	tx := makeTestTx(true)
	tx.Version = 2
	sub := actor.NewChannelTellOnlyRef[Notification]("invalid", 4)
	req := &EnsureConfirmedReq{
		Tx:                 tx,
		Subscriber:         sub,
		RetryUntilAccepted: true,
	}
	require.Equal(t, TxStateFailed, mustEnsure(t, ref.Ref(), req).State)
	require.IsType(t, &TxFailed{}, mustAwaitNotification(t, sub))
	require.Zero(t, chain.broadcastCallCount())
	require.Zero(t, chain.packageCallCount())
}
