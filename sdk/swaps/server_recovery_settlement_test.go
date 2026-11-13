package swaps

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// restoredRefundServer authorizes only the exact cooperative refund requested
// by a restored pay session; the embedded server supplies discovery metadata.
type restoredRefundServer struct {
	*testSwapServerConn
	authorization *InSwapRefundAuthorization
	calls         int
	bounded       bool
}

// AuthorizeInSwapRefund simulates a safely failed payment or unavailable peer.
func (s *restoredRefundServer) AuthorizeInSwapRefund(ctx context.Context,
	_ lntypes.Hash, _ string, _ int64, _ []byte, _ []byte, _ []byte) (
	*InSwapRefundAuthorization, error) {

	s.calls++
	deadline, ok := ctx.Deadline()
	s.bounded = ok &&
		time.Until(deadline) <= cooperativeRefundAuthTimeout
	if s.authorization == nil {
		return nil, status.Error(
			codes.Unavailable, "refund signer unavailable",
		)
	}

	return s.authorization, nil
}

// TestRestoredRefundBoundsCooperation keeps an unavailable wait-for-ready peer
// from preventing the mature sender-only refund under a daemon-lifetime
// context.
func TestRestoredRefundBoundsCooperation(t *testing.T) {
	t.Parallel()
	client, daemon, server, row := newRefundRecoveryFixture(t)
	remote := &restoredRefundServer{testSwapServerConn: server}
	client.server = remote
	client.waitPollInterval = time.Millisecond
	daemon.blockHeight = row.GetRefundLocktime()
	daemon.sendSessionID = "timeout-refund"
	result, err := client.RecoverSwapserverVHTLCs(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, result.RecoveredVHTLCRefunds)
	hash, err := lntypes.MakeHash(row.GetPaymentHash())
	require.NoError(t, err)
	session, err := client.ResumePayViaLightning(t.Context(), hash)
	require.NoError(t, err)
	require.NoError(t, session.completeRefund(t.Context()))
	require.True(t, remote.bounded)
	require.Equal(t, 1, daemon.sendCustomCalls)
}

// TestRestoredLateReceiveStaysPending prevents both cooperative disclosure and
// automatic exit admission once the sender's refund path is already open.
func TestRestoredLateReceiveStaysPending(t *testing.T) {
	t.Parallel()
	direction := swaprpc.
		RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT
	for _, auto := range []bool{true, false} {
		name := "held without consent"
		if auto {
			name = "late exit refused despite consent"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, d, server, inRow := newRefundRecoveryFixture(
				t,
			)
			preimage, params, err := newOutSwapPreimage(
				t.Context(), d,
			)
			require.NoError(t, err)
			remote, err := btcec.ParsePubKey(
				inRow.GetReceiverPubkey(),
			)
			require.NoError(t, err)
			row := recoverableSwapForTest(
				t, direction, preimage.Hash(), remote,
				d.identityKey, remote, params,
			)
			server.recoverableRows = []*swaprpc.RecoverableSwap{row}
			live := &VTXOInfo{Outpoint: "late:0", AmountSat: 42_000}
			d.liveLookupHook = func(int) (*VTXOInfo, error) {
				return live, nil
			}
			d.blockHeight = row.GetRefundLocktime()
			d.sendSessionID = "late-claim"
			d.oorSession = completedClaimOOR()
			c.waitPollInterval = time.Millisecond
			c.claimRetryDelay = time.Millisecond
			c.claimMaxAttempts = 1
			policy := DefaultRecoveryPolicy()
			policy.AutoEscalate = auto
			policy.CooperativeFailureGracePeriod = time.Hour
			c.SetRecoveryPolicy(policy)
			if auto {
				c.daemon = &completingRestoreDaemon{d}
			}
			result, err := c.RecoverSwapserverVHTLCs(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 1, result.RecoveredVHTLCClaims)
			session, err := c.ResumeReceiveViaLightning(
				t.Context(), preimage.Hash(),
			)
			require.NoError(t, err)
			// The short cancellation budget exercises pending Wait,
			// not SQLite setup under concurrent test/CI load.
			timeout := 100 * time.Millisecond
			ctx, cancel := context.WithTimeout(
				t.Context(), timeout,
			)
			defer cancel()
			_, err = session.Wait(ctx)
			require.Zero(t, d.sendCustomCalls)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Zero(t, d.escalateCalls)
			require.False(t, session.State().IsTerminal())
		})
	}
}

// completingRestoreDaemon simulates completion by the existing daemon executor
// after the SDK escalates, without making the restore scan itself execute it.
type completingRestoreDaemon struct {
	*testDaemonConn
}

// EscalateVHTLCRecovery records admission and exposes a completed daemon job.
func (d *completingRestoreDaemon) EscalateVHTLCRecovery(ctx context.Context,
	req *waverpc.EscalateVHTLCRecoveryRequest) (
	*waverpc.EscalateVHTLCRecoveryResponse, error) {

	response, err := d.testDaemonConn.EscalateVHTLCRecovery(ctx, req)
	if err == nil {
		d.statusResp = &waverpc.GetVHTLCRecoveryStatusResponse{
			Found: true,
			Status: &waverpc.VHTLCRecoveryStatus{
				RecoveryId: req.GetRecoveryId(),
				State:      recoveryStateCompleted,
			},
		}
	}

	return response, err
}

// TestRestoredRefundSessionSettles verifies discovery creates an executable,
// restartable refund-only session rather than merely a dormant recovery row.
func TestRestoredRefundSessionSettles(t *testing.T) {
	t.Parallel()
	for _, cooperative := range []bool{true, false} {
		name := "timeout exit fallback"
		if cooperative {
			name = "cooperative before maturity"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client, daemon, server, row := newRefundRecoveryFixture(
				t,
			)
			refundServer := &restoredRefundServer{
				testSwapServerConn: server,
			}
			client.server = refundServer
			client.waitPollInterval = time.Millisecond
			script, err := txscript.PayToTaprootScript(
				daemon.identityKey,
			)
			require.NoError(t, err)
			daemon.receiveInfo.PkScript = script
			daemon.sendSessionID = "restored-refund"
			signature := TaprootScriptSignature{
				PubKey:        row.GetReceiverPubkey(),
				WitnessScript: []byte("witness"),
				Signature:     []byte("authorized-refund"),
			}
			if cooperative {
				daemon.blockHeight = 50
				daemon.spendOnCustom = true
				authorization := &InSwapRefundAuthorization{
					Signature: signature,
				}
				refundServer.authorization = authorization
			} else {
				daemon.blockHeight = row.GetRefundLocktime()
				daemon.sendCustomErr = errors.New("operator " +
					"unavailable")
				client.daemon = &completingRestoreDaemon{daemon}
				client.SetRecoveryPolicy(
					RecoveryPolicy{
						AutoEscalate: true,
					},
				)
			}

			ctx, cancel := context.WithTimeout(
				t.Context(), 3*time.Second,
			)
			defer cancel()
			result, err := client.RecoverSwapserverVHTLCs(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, result.RecoveredVHTLCRefunds)
			require.Zero(t, daemon.sendCustomCalls)
			require.Zero(t, daemon.escalateCalls)

			// Reconstruct from SQL, as the daemon does after wallet
			// restore and again on restart. No pre-restore SDK
			// session is retained.
			hash, err := lntypes.MakeHash(row.GetPaymentHash())
			require.NoError(t, err)
			session, err := client.ResumePayViaLightning(ctx, hash)
			require.NoError(t, err)
			require.Equal(
				t, PayStateRefundInitiated, session.State(),
			)
			_, err = session.Wait(ctx)
			require.ErrorIs(t, err, ErrSwapRefunded)
			require.Equal(t, PayStateRefunded, session.State())
			require.Positive(t, refundServer.calls)
			require.Zero(t, daemon.sendPolicyCalls)
			if cooperative {
				require.Zero(t, daemon.escalateCalls)
				input := daemon.lastClaimInput[0]
				require.Len(t, input.ExternalSignatures, 1)
			} else {
				require.Equal(t, 1, daemon.escalateCalls)
			}
		})
	}
}

// TestRestoreRejectsTerminalRecovery avoids allocating a fresh destination
// against the deterministic request ID of an operator-cancelled or failed job.
func TestRestoreRejectsTerminalRecovery(t *testing.T) {
	t.Parallel()
	for _, state := range []waverpc.VHTLCRecoveryState{
		recoveryStateFailed,
		recoveryStateCancelled,
		recoveryStateCompleted,
	} {
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()
			client, daemon, _, row := newRefundRecoveryFixture(t)
			hash, err := lntypes.MakeHash(row.GetPaymentHash())
			require.NoError(t, err)
			stored := recoverableStatusForTest(
				row, hash, &VTXOInfo{
					Outpoint:  "owned:0",
					AmountSat: 42_000,
				}, "terminal-job",
				recoveryDirectionPay,
				recoveryActionRefundWithoutReceiver,
			)
			stored.State = state
			daemon.listRecoveriesResp = &waverpc.
				ListVHTLCRecoveriesResponse{
				Statuses: []*waverpc.VHTLCRecoveryStatus{
					stored,
				},
			}
			result, err := client.RecoverSwapserverVHTLCs(
				t.Context(),
			)
			require.ErrorContains(
				t, err, "inspect the existing job",
			)
			require.Zero(t, result.RecoveredVHTLCs)
			require.Zero(t, daemon.receiveAllocCalls)
			require.Zero(t, daemon.armRecoveryCalls)
		})
	}
}
