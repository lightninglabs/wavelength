package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// indexedWaitDaemon supplies the optional query wait while retaining ordinary
// fake wallet operations for funding and claim validation.
type indexedWaitDaemon struct {
	*testDaemonConn
	wait func(context.Context, []byte, []byte, waverpc.VTXOStatus,
		time.Duration) (*VTXOInfo, bool, error)
}

// WaitIndexedVTXO delegates to the test's bounded query.
func (d *indexedWaitDaemon) WaitIndexedVTXO(ctx context.Context, script,
	policy []byte, state waverpc.VTXOStatus, wait time.Duration) (*VTXOInfo,
	bool, error) {

	return d.wait(ctx, script, policy, state, wait)
}

// TestFundingWaitCompatibility proves supported empty waits requery without
// a second sleep, while old servers and cancellation retain bounded behavior.
func TestFundingWaitCompatibility(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"supported", "old server", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			script, policy := []byte{1}, []byte{2}
			d := &indexedWaitDaemon{
				testDaemonConn: &testDaemonConn{},
			}
			d.wait = func(ctx context.Context, gotScript,
				gotPolicy []byte, state waverpc.VTXOStatus,
				wait time.Duration) (*VTXOInfo, bool, error) {

				calls++
				require.Equal(t, script, gotScript)
				require.Equal(t, policy, gotPolicy)
				require.Equal(
					t, waverpc.VTXOStatus_VTXO_STATUS_LIVE,
					state,
				)
				require.Equal(t, time.Hour, wait)
				if mode == "canceled" {
					<-ctx.Done()

					return nil, false, ctx.Err()
				}
				if calls == 1 {
					return nil, mode == "supported", nil
				}

				return &VTXOInfo{
					Outpoint: "funded:0", AmountSat: 42,
					PkScript: script,
				}, true, nil
			}
			c := NewSwapClient(nil, d, nil, nil)
			c.waitPollInterval = time.Hour
			ctx, cancel := context.WithTimeout(
				t.Context(), 50*time.Millisecond,
			)
			defer cancel()
			info, err := c.waitForVHTLC(
				ctx, script, policy, time.Time{}, nil,
			)
			if mode == "supported" {
				require.NoError(t, err)
				require.Equal(t, "funded:0", info.Outpoint)
				require.Equal(t, 2, calls)
			} else {
				require.ErrorIs(
					t, err, context.DeadlineExceeded,
				)
				require.Equal(t, 1, calls)
			}
		})
	}
}

// TestQueryElapsedPreservesExpiry verifies a consumed wait avoids a second
// delay but never bypasses the hard deadline that starts refund recovery.
func TestQueryElapsedPreservesExpiry(t *testing.T) {
	t.Parallel()
	c := NewSwapClient(nil, &testDaemonConn{}, nil, nil)
	c.waitPollInterval = time.Hour
	s := &paySession{
		client: c,
		cfg: &InSwapConfig{
			Expiry: time.Now().Add(time.Hour),
		},
	}
	require.NoError(t, s.waitForNextPollAfter(t.Context(), time.Hour))
	s.cfg.Expiry = time.Now().Add(-time.Second)
	require.ErrorIs(
		t,
		s.waitForNextPollAfter(
			t.Context(), time.Hour,
		),
		errSwapExpired,
	)
}

// TestPayClaimWaitUsesVerifiedPreimage exercises the payment state transition
// with a remotely woken row. A notification alone cannot complete the payment.
func TestPayClaimWaitUsesVerifiedPreimage(t *testing.T) {
	t.Parallel()
	preimage, err := NewPreimage()
	require.NoError(t, err)
	script, policy := []byte{1}, []byte{2}
	calls := 0
	d := &indexedWaitDaemon{testDaemonConn: &testDaemonConn{}}
	d.wait = func(_ context.Context, gotScript, gotPolicy []byte,
		state waverpc.VTXOStatus, wait time.Duration) (*VTXOInfo, bool,
		error) {

		calls++
		require.Equal(t, script, gotScript)
		require.Equal(t, policy, gotPolicy)
		require.Equal(t, waverpc.VTXOStatus_VTXO_STATUS_SPENT, state)
		require.Positive(t, wait)
		if calls == 1 {
			return nil, true, nil
		}

		return &VTXOInfo{PkScript: script,
			FinalCheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(t, preimage[:]),
			}}, true, nil
	}
	c := NewSwapClient(nil, d, nil, nil)
	c.waitPollInterval = time.Hour
	s := &paySession{
		client: c, state: PayStateWaitingForClaim,
		vhtlcPkScript: script, vhtlcPolicyTemplate: policy,
		cfg: &InSwapConfig{PaymentHash: preimage.Hash(),
			SettlementType: SettlementTypeInArk,
			Expiry:         time.Now().Add(time.Hour),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime: 100,
			}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.NoError(t, s.waitForClaimPreimage(ctx))
	require.Equal(t, PayStateCompleted, s.state)
	require.Equal(t, &preimage, s.preimage)
	require.Equal(t, 2, calls)
	require.Zero(t, d.spentLookupCalls)
}

// TestIndexedWaitAuthorizationFallback preserves older peers' independently
// authorized legacy reads. Transient and integrity errors do not downgrade the
// query, and a legacy rejection remains an error rather than an empty success.
func TestIndexedWaitAuthorizationFallback(t *testing.T) {
	t.Parallel()
	for _, code := range []codes.Code{codes.Unimplemented,
		codes.Unauthenticated, codes.PermissionDenied,
		codes.Unavailable, codes.DataLoss} {

		t.Run(code.String(), func(t *testing.T) {
			legacy := &VTXOInfo{AmountSat: 42}
			d := &indexedWaitDaemon{testDaemonConn: &testDaemonConn{
				vhtlc: legacy,
			}}
			d.wait = func(context.Context, []byte, []byte,
				waverpc.VTXOStatus, time.Duration) (*VTXOInfo,
				bool, error) {

				return nil, false, status.Error(
					code, "wait rejected",
				)
			}
			c := NewSwapClient(nil, d, nil, nil)
			info, waited, err := c.findVTXOWithWait(
				t.Context(), []byte{1}, []byte{2},
				waverpc.VTXOStatus_VTXO_STATUS_LIVE,
				time.Second,
			)
			require.False(t, waited)
			if code == codes.Unavailable || code == codes.DataLoss {
				require.Error(t, err)
				require.Nil(t, info)
				require.Zero(t, d.liveLookupCalls)

				return
			}
			require.NoError(t, err)
			require.Equal(t, legacy, info)
			require.Equal(t, 1, d.liveLookupCalls)
			d.liveLookupErr = status.Error(
				codes.PermissionDenied, "legacy scope rejected",
			)
			_, _, err = c.findVTXOWithWait(
				t.Context(), []byte{1}, []byte{2},
				waverpc.VTXOStatus_VTXO_STATUS_LIVE,
				time.Second,
			)
			require.ErrorContains(t, err, "legacy scope rejected")

			d.spentVTXO = legacy
			info, waited, err = c.findVTXOWithWait(
				t.Context(), []byte{1}, []byte{2},
				waverpc.VTXOStatus_VTXO_STATUS_SPENT,
				time.Second,
			)
			require.NoError(t, err)
			require.False(t, waited)
			require.Equal(t, legacy, info)
			require.Equal(t, 1, d.spentLookupCalls)
		})
	}
}
