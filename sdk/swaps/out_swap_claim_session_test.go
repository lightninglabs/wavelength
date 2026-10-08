package swaps

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// Short names for the daemon OOR session statuses used below.
const (
	oorUnspecified = waverpc.OORSessionStatus_OOR_SESSION_STATUS_UNSPECIFIED
	oorPending     = waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING
	oorCompleted   = waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED
	oorFailed      = waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED
)

// claimSessionHarness is a funded receive session sitting in ClaimInitiated
// with a scripted daemon, so a test can drive claimFundedVHTLC one iteration at
// a time.
type claimSessionHarness struct {
	session *ReceiveSession
	daemon  *testDaemonConn
}

// newClaimSessionHarness builds the harness. With restart set, the session
// carries a persisted claim session id like a row restored after a crash;
// otherwise it is a fresh in-process claim that has yet to be submitted.
func newClaimSessionHarness(t *testing.T, restart bool) *claimSessionHarness {
	t.Helper()

	senderPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	receiverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:   senderPriv.PubKey(),
		Receiver: receiverPriv.PubKey(),
		Server:   operatorPriv.PubKey(),
		PreimageHash: lntypes.Hash(
			sha256.Sum256(preimage[:]),
		),
		RefundLocktime:                       300,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
	})
	require.NoError(t, err)

	policyTemplate, err := encodeVHTLCPolicyTemplate(policy)
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		blockHeight: 100,
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: schnorr.SerializePubKey(
				receiverPriv.PubKey(),
			),
		},
		sendSessionID: "claim-session",
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond
	client.claimResumeGracePeriod = 0

	session := &ReceiveSession{
		client:              client,
		state:               ReceiveStateClaimInitiated,
		Preimage:            preimage,
		PaymentHash:         preimage.Hash(),
		clientPubKey:        receiverPriv.PubKey(),
		swapServerPubKey:    senderPriv.PubKey(),
		operatorPubKey:      operatorPriv.PubKey(),
		vhtlcPolicy:         policy,
		vhtlcPolicyTemplate: policyTemplate,
		vhtlcPkScript:       pkScript,
		vhtlcConfig: VHTLCConfig{
			RefundLocktime: 300,
		},
		vhtlcOutpoint: "funding:0",
		vhtlcAmount:   42_000,
		claimReceivePubKey: schnorr.SerializePubKey(
			receiverPriv.PubKey(),
		),
		claimReceiveScript: []byte{
			0x51,
		},
		claimRecoveryID: "recovery-1",
	}
	if restart {
		session.claimSessionID = "claim-session"
	} else {
		session.claimIntentRecordedInProcess = true
	}

	return &claimSessionHarness{
		session: session,
		daemon:  daemonConn,
	}
}

// claimOOR returns a claim OOR status as the daemon reports it.
func claimOOR(status waverpc.OORSessionStatus,
	beforePONR bool) *waverpc.OORSessionInfo {

	return &waverpc.OORSessionInfo{
		SessionId:        "claim-session",
		Status:           status,
		FailedBeforePonr: beforePONR,
	}
}

// requireClaimUnresolved asserts a claim iteration neither completed the swap
// nor cancelled the armed recovery, and left the claim session id as is.
func requireClaimUnresolved(t *testing.T, h *claimSessionHarness,
	wantSessionID string) {

	t.Helper()

	require.Equal(t, ReceiveStateClaimInitiated, h.session.State())
	require.Zero(t, h.daemon.cancelCalls)
	require.Equal(t, wantSessionID, h.session.claimSessionID)
}

// TestReceiveClaimCompletesOnlyWhenClaimOORCompleted checks that an admitted
// claim OOR does not finish the swap or cancel claim recovery: only a
// completed status does, both right after the send and on the restart branch.
func TestReceiveClaimCompletesOnlyWhenClaimOORCompleted(t *testing.T) {
	t.Parallel()

	for _, restart := range []bool{false, true} {
		name := "after send"
		if restart {
			name = "restart"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newClaimSessionHarness(t, restart)
			h.daemon.oorSession = claimOOR(
				oorCompleted,
				false,
			)

			err := h.session.claimFundedVHTLC(t.Context())
			require.NoError(t, err)
			require.Equal(
				t, ReceiveStateCompleted, h.session.State(),
			)
			require.Equal(t, 1, h.daemon.cancelCalls)
			require.Equal(
				t, "claim-session", h.daemon.lastOORSessionID,
			)

			wantSends := 1
			if restart {
				wantSends = 0
			}
			require.Equal(t, wantSends, h.daemon.sendCustomCalls)
		})
	}
}

// TestReceiveClaimWaitsWhileClaimOORInFlight checks that a claim OOR the
// daemon has admitted but not finished keeps the recovery armed and the swap
// in ClaimInitiated, and that a later completed status then finishes it. A
// missing session reads the same as an in-flight one.
func TestReceiveClaimWaitsWhileClaimOORInFlight(t *testing.T) {
	t.Parallel()

	inFlight := map[string]*waverpc.OORSessionInfo{
		"pending": claimOOR(
			oorPending,
			false,
		),
		"unspecified": claimOOR(
			oorUnspecified,
			false,
		),
		"not found": nil,
	}

	for name, status := range inFlight {
		for _, restart := range []bool{false, true} {
			testName := name + " after send"
			if restart {
				testName = name + " restart"
			}

			t.Run(testName, func(t *testing.T) {
				t.Parallel()

				h := newClaimSessionHarness(t, restart)
				h.daemon.oorSession = status

				err := h.session.claimFundedVHTLC(t.Context())
				require.NoError(t, err)
				requireClaimUnresolved(t, h, "claim-session")

				// The next iteration polls the same session
				// and must not submit a second claim.
				err = h.session.claimFundedVHTLC(t.Context())
				require.NoError(t, err)
				requireClaimUnresolved(t, h, "claim-session")
				wantSends := 1
				if restart {
					wantSends = 0
				}
				require.Equal(
					t, wantSends, h.daemon.sendCustomCalls,
				)

				// Once the OOR completes, the swap completes.
				h.daemon.oorSession = claimOOR(
					oorCompleted, false,
				)
				err = h.session.claimFundedVHTLC(t.Context())
				require.NoError(t, err)
				require.Equal(
					t, ReceiveStateCompleted,
					h.session.State(),
				)
				require.Equal(t, 1, h.daemon.cancelCalls)
			})
		}
	}
}

// TestReceiveClaimOORStatusErrorIsRetryable checks that a failed status query
// is never read as a result: the swap stays in ClaimInitiated with recovery
// armed, and the error is the retryable kind the surrounding loop absorbs.
func TestReceiveClaimOORStatusErrorIsRetryable(t *testing.T) {
	t.Parallel()

	for _, restart := range []bool{false, true} {
		name := "after send"
		if restart {
			name = "restart"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newClaimSessionHarness(t, restart)
			h.daemon.oorSessionErr = errors.New("daemon down")

			err := h.session.claimFundedVHTLC(t.Context())

			var retryable *retryableActionError
			require.ErrorAs(t, err, &retryable)
			requireClaimUnresolved(t, h, "claim-session")
		})
	}
}

// TestReceiveClaimFailedBeforePONRResubmits checks that a claim OOR proven to
// have failed before the point of no return never completes the swap. No spend
// exists, so the dead session id is dropped, recovery stays armed, and the
// next iteration submits a fresh claim that can then complete.
func TestReceiveClaimFailedBeforePONRResubmits(t *testing.T) {
	t.Parallel()

	for _, restart := range []bool{false, true} {
		name := "after send"
		if restart {
			name = "restart"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newClaimSessionHarness(t, restart)
			h.daemon.oorSession = claimOOR(
				oorFailed,
				true,
			)

			err := h.session.claimFundedVHTLC(t.Context())
			require.NoError(t, err)
			requireClaimUnresolved(t, h, "")

			// The retry forgets the failed session and claims
			// again; this time the OOR completes.
			h.daemon.oorSession = claimOOR(
				oorCompleted,
				false,
			)
			err = h.session.claimFundedVHTLC(t.Context())
			require.NoError(t, err)
			require.Equal(
				t, ReceiveStateCompleted, h.session.State(),
			)
			require.Equal(t, 1, h.daemon.cancelCalls)

			wantSends := 2
			if restart {
				wantSends = 1
			}
			require.Equal(t, wantSends, h.daemon.sendCustomCalls)
		})
	}
}

// TestReceiveClaimFailedAfterPONRNeedsIntervention checks that a claim OOR
// failing after the point of no return, or without the daemon proving it
// failed before, is never completed or resubmitted. The operator may hold a
// co-signed spend of the vHTLC, so the swap stops for intervention with the
// claim recovery still armed.
func TestReceiveClaimFailedAfterPONRNeedsIntervention(t *testing.T) {
	t.Parallel()

	for _, restart := range []bool{false, true} {
		name := "after send"
		if restart {
			name = "restart"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// The daemon reports failedBeforePonr false both for a
			// post-PONR failure and for a failure it cannot
			// classify, so one status covers both.
			h := newClaimSessionHarness(t, restart)
			h.daemon.oorSession = claimOOR(
				oorFailed,
				false,
			)

			err := h.session.claimFundedVHTLC(t.Context())
			require.NotEmpty(t, interventionReason(err))
			requireClaimUnresolved(t, h, "claim-session")

			wantSends := 1
			if restart {
				wantSends = 0
			}
			require.Equal(t, wantSends, h.daemon.sendCustomCalls)
			require.Zero(t, h.daemon.escalateCalls)
		})
	}
}

// TestReceiveClaimRestartFailedAfterPONRStopsWait drives a restored session
// through the public Wait loop and checks it ends in NeedsIntervention rather
// than Completed.
func TestReceiveClaimRestartFailedAfterPONRStopsWait(t *testing.T) {
	t.Parallel()

	h := newClaimSessionHarness(t, true)
	h.daemon.oorSession = claimOOR(
		oorFailed, false,
	)

	_, err := h.session.Wait(t.Context())
	require.Error(t, err)
	require.NotEmpty(t, interventionReason(err))
	require.Equal(t, ReceiveStateNeedsIntervention, h.session.State())
	require.Zero(t, h.daemon.cancelCalls)
}

// TestReceiveClaimFailedAfterPONRIndexedCompletes checks that a claim OOR
// reported failed after the point of no return still completes the swap when
// the indexer shows the vHTLC spent with the receive preimage, which is what a
// finalized claim whose local mark-spent step failed looks like. Recovery is
// cancelled as indexed.
func TestReceiveClaimFailedAfterPONRIndexedCompletes(t *testing.T) {
	t.Parallel()

	h := newClaimSessionHarness(t, true)
	h.daemon.oorSession = claimOOR(oorFailed, false)

	preimage := h.session.Preimage
	h.daemon.spentVTXO = &VTXOInfo{
		Outpoint:    "funding:0",
		AmountSat:   42_000,
		SpentByTxID: "claim-session",
		FinalCheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, preimage[:]),
		},
	}

	err := h.session.claimFundedVHTLC(t.Context())
	require.NoError(t, err)
	require.Equal(t, ReceiveStateCompleted, h.session.State())
	require.Equal(t, 1, h.daemon.cancelCalls)
	require.Equal(
		t, recoveryReasonClaimIndexed, h.daemon.lastCancel.GetReason(),
	)
	require.Zero(t, h.daemon.sendCustomCalls)
}

// TestReceiveClaimFailedAfterPONRIndexerErrorIsRetryable checks that an
// indexer failure while classifying a post-PONR claim failure is neither
// completion nor intervention.
func TestReceiveClaimFailedAfterPONRIndexerErrorIsRetryable(t *testing.T) {
	t.Parallel()

	h := newClaimSessionHarness(t, true)
	h.daemon.oorSession = claimOOR(oorFailed, false)
	h.daemon.spentLookupErr = errors.New("indexer down")

	err := h.session.claimFundedVHTLC(t.Context())

	var retryable *retryableActionError
	require.ErrorAs(t, err, &retryable)
	require.Empty(t, interventionReason(err))
	requireClaimUnresolved(t, h, "claim-session")
}

// TestReceiveClaimFailedAfterPONREscalatesNearDeadline checks that a post-PONR
// claim failure still runs the escalation policy before the swap parks for
// intervention, so a receiver racing the sender's refund locktime can unroll.
func TestReceiveClaimFailedAfterPONREscalatesNearDeadline(t *testing.T) {
	t.Parallel()

	h := newClaimSessionHarness(t, true)
	h.session.client.SetRecoveryPolicy(RecoveryPolicy{
		AutoEscalate:                  true,
		CooperativeFailureGracePeriod: time.Hour,
		MinRecoveryMarginBlocks:       12,
	})

	// Within the margin of the refund locktime of 300.
	h.daemon.blockHeight = 295
	h.daemon.oorSession = claimOOR(oorFailed, false)

	err := h.session.claimFundedVHTLC(t.Context())
	require.NotEmpty(t, interventionReason(err))
	require.Equal(t, 1, h.daemon.escalateCalls)
	require.Zero(t, h.daemon.cancelCalls)
}
