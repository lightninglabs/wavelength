package swaps

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// admissionPaySession builds distinct normal and restore snapshots for the
// same policy so tests detect lost destinations, OOR IDs, and terminal state.
func admissionPaySession(t *testing.T, client *SwapClient,
	restored bool) *paySession {

	t.Helper()
	key, err := btcec.ParsePubKey(testPubKeyBytes(2))
	require.NoError(t, err)
	session := &paySession{
		client: client, state: PayStateSwapCreated,
		invoice: "normal-invoice", maxFeeSat: 123,
		cfg: &InSwapConfig{
			PaymentHash: testHash(201), AmountSat: 42_000,
			ServerPubkey:   key,
			SettlementType: SettlementTypeLightning,
			Expiry:         time.Unix(10_000, 0),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime: 144, UnilateralClaimDelay: 6,
				UnilateralRefundDelay:                12,
				UnilateralRefundWithoutReceiverDelay: 18,
			},
		},
		clientPubKey: key, operatorPubKey: key, serverPubKey: key,
		vhtlcPkScript:       []byte("policy"),
		vhtlcPolicyTemplate: []byte("template"),
		vhtlcOutpoint:       "funded:0", vhtlcAmount: 42_000,
		fundingSessionID:    "accepted-funding",
		refundReceivePubKey: []byte("normal-key"),
		refundReceiveScript: []byte("normal-script"),
		refundRecoveryID:    "normal-recovery",
		createdAt:           time.Unix(1_000, 0),
	}
	if restored {
		session.state = PayStateRefundInitiated
		session.invoice = ""
		session.fundingSessionID = ""
		session.refundReceivePubKey = []byte("restored-key")
		session.refundReceiveScript = []byte("restored-script")
		session.refundRecoveryID = "restored-recovery"
		// Restore constructors prefill timestamps; explicit admission
		// must remain insert-only even with a nonzero updatedAt.
		session.updatedAt = time.Unix(2_000, 0)
	}

	return session
}

// admissionReceiveSession builds receive snapshots with distinct accepted
// claim identity, destination, and invoice metadata for the same payment hash.
func admissionReceiveSession(t *testing.T, client *SwapClient,
	restored bool) *ReceiveSession {

	t.Helper()
	key, err := btcec.ParsePubKey(testPubKeyBytes(2))
	require.NoError(t, err)
	preimage := lntypes.Preimage{202}
	session := &ReceiveSession{
		client: client, state: ReceiveStateInvoiceCreated,
		PaymentHash: preimage.Hash(), Preimage: preimage,
		Invoice: "normal-invoice", amountSat: 42_000,
		requestedAmountSat: 42_000, expectedVHTLCSat: 42_000,
		clientPubKey: key, operatorPubKey: key, swapServerPubKey: key,
		vhtlcConfig: VHTLCConfig{
			RefundLocktime: 144, UnilateralClaimDelay: 6,
			UnilateralRefundDelay:                12,
			UnilateralRefundWithoutReceiverDelay: 18,
		},
		settlementType:      SettlementTypeLightning,
		vhtlcPkScript:       []byte("policy"),
		vhtlcPolicyTemplate: []byte("template"),
		vhtlcOutpoint:       "funded:0", vhtlcAmount: 42_000,
		claimReceivePubKey: []byte("normal-key"),
		claimReceiveScript: []byte("normal-script"),
		claimSessionID:     "accepted-claim",
		claimRecoveryID:    "normal-recovery",
		deadline:           time.Unix(10_000, 0), createdAt: time.Unix(
			1_000, 0,
		),
		forfeitBindingGate: newReceiveForfeitBindingGate(),
	}
	if restored {
		session.state = ReceiveStateVHTLCFunded
		session.Invoice = ""
		session.claimSessionID = ""
		session.claimReceivePubKey = []byte("restored-key")
		session.claimReceiveScript = []byte("restored-script")
		session.claimRecoveryID = "restored-recovery"
		session.updatedAt = time.Unix(2_000, 0)
	}

	return session
}

// TestRestoredAdmissionPreservesWinner covers stale restore admission after a
// normal worker has persisted accepted protocol work or a terminal outcome.
func TestRestoredAdmissionPreservesWinner(t *testing.T) {
	t.Parallel()
	for _, terminal := range []bool{false, true} {
		name := "pending"
		if terminal {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store := newTestSwapStore(t)
			client := NewSwapClientWithStore(
				nil, nil, nil, nil, store,
			)
			pay := admissionPaySession(t, client, false)
			receive := admissionReceiveSession(t, client, false)
			if terminal {
				pay.state = PayStateNeedsIntervention
				pay.interventionReason = "manual inspection"
				receive.state = ReceiveStateCompleted
			}
			require.NoError(t, pay.persist(ctx))
			require.NoError(t, receive.persist(ctx))
			payRow, err := store.queries.GetPaySwap(
				ctx, pay.cfg.PaymentHash[:],
			)
			require.NoError(t, err)
			receiveRow, err := store.queries.GetReceiveSwap(
				ctx, receive.PaymentHash[:],
			)
			require.NoError(t, err)

			restoredPay := admissionPaySession(t, client, true)
			restoredReceive := admissionReceiveSession(
				t, client, true,
			)
			require.NoError(t, restoredPay.persistRestored(ctx))
			require.NoError(t, restoredReceive.persistRestored(ctx))
			afterPay, err := store.queries.GetPaySwap(
				ctx, pay.cfg.PaymentHash[:],
			)
			require.NoError(t, err)
			afterReceive, err := store.queries.GetReceiveSwap(
				ctx, receive.PaymentHash[:],
			)
			require.NoError(t, err)
			require.Equal(t, payRow, afterPay)
			require.Equal(t, receiveRow, afterReceive)
			require.Equal(t, pay.state, restoredPay.state)
			require.Equal(
				t, pay.fundingSessionID,
				restoredPay.fundingSessionID,
			)
			require.Equal(t, receive.state, restoredReceive.state)
			require.Equal(
				t, receive.claimSessionID,
				restoredReceive.claimSessionID,
			)
		})
	}
}

// TestNormalAdmissionReloadsRestoredWinner verifies a losing create is rolled
// back without failing the winner, and callers can return the winner without
// advancing its refund or claim FSM inside the original create request.
func TestNormalAdmissionReloadsRestoredWinner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestSwapStore(t)
	client := NewSwapClientWithStore(nil, nil, nil, nil, store)
	payWinner := admissionPaySession(t, client, true)
	receiveWinner := admissionReceiveSession(t, client, true)
	require.NoError(t, payWinner.persistRestored(ctx))
	require.NoError(t, receiveWinner.persistRestored(ctx))

	pay := &paySession{client: client, state: PayStateCreated}
	payErr := pay.mutateAndPersist(ctx, func() error {
		*pay = *admissionPaySession(t, client, false)

		return nil
	})
	var retryable *retryableActionError
	require.ErrorAs(t, payErr, &retryable)
	require.Equal(t, PayStateCreated, pay.state)
	machine := newPayLoopFSM(pay, PayStateSwapCreated)
	machine.fail(ctx, payErr)
	require.ErrorIs(t, machine.runErr, payErr)
	resumedPay, err := client.resumePayAdmission(ctx, machine.runErr)
	require.NoError(t, err)
	require.Equal(t, PayStateRefundInitiated, resumedPay.state)
	require.Equal(
		t, payWinner.refundReceiveScript,
		resumedPay.refundReceiveScript,
	)
	require.Empty(t, resumedPay.fundingSessionID)

	receive := &ReceiveSession{client: client, state: ReceiveStateCreated}
	receiveErr := receive.mutateAndPersist(ctx, func() error {
		*receive = *admissionReceiveSession(t, client, false)

		return nil
	})
	require.ErrorAs(t, receiveErr, &retryable)
	require.Equal(t, ReceiveStateCreated, receive.state)
	require.Zero(t, receive.PaymentHash)
	receiveMachine := newReceiveLoopFSM(receive, ReceiveStateInvoiceCreated)
	receiveMachine.fail(ctx, receiveErr)
	resumedReceive, err := client.resumeReceiveAdmission(
		ctx, receiveMachine.runErr,
	)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateVHTLCFunded, resumedReceive.state)
	require.Equal(
		t, receiveWinner.claimReceiveScript,
		resumedReceive.claimReceiveScript,
	)
	require.Empty(t, resumedReceive.claimSessionID)

	// Once admitted, ordinary follow-up writes still update the same row.
	resumedPay.refundSessionID = "new-refund"
	require.NoError(t, resumedPay.persist(ctx))
	resumedReceive.claimSessionID = "new-claim"
	require.NoError(t, resumedReceive.persist(ctx))
	resumedPay, err = client.ResumePayViaLightning(
		ctx, payWinner.cfg.PaymentHash,
	)
	require.NoError(t, err)
	resumedReceive, err = client.ResumeReceiveViaLightning(
		ctx, receiveWinner.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, "new-refund", resumedPay.refundSessionID)
	require.Equal(t, "new-claim", resumedReceive.claimSessionID)
}

// TestConcurrentSessionAdmission uses independent connections to prove SQL,
// rather than a process-local restore mutex, selects exactly one snapshot.
func TestConcurrentSessionAdmission(t *testing.T) {
	t.Parallel()
	for _, paySide := range []bool{true, false} {
		name := "receive"
		if paySide {
			name = "pay"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "admission.db")
			store, err := NewSqliteStore(&SqliteStoreConfig{
				DatabaseFileName: path,
			}, btclog.Disabled)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			other, err := NewSqliteStore(&SqliteStoreConfig{
				DatabaseFileName: path, SkipMigrations: true,
			}, btclog.Disabled)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			client := NewSwapClientWithStore(
				nil, nil, nil, nil, store,
			)
			otherClient := NewSwapClientWithStore(
				nil, nil, nil, nil, other,
			)
			var normal, restored func(context.Context) error
			if paySide {
				session := admissionPaySession(t, client, false)
				normal = session.persist
				restored = admissionPaySession(
					t, otherClient, true,
				).persistRestored
			} else {
				normal = admissionReceiveSession(
					t, client, false,
				).persist
				restored = admissionReceiveSession(
					t, otherClient, true,
				).persistRestored
			}
			start := make(chan struct{})
			normalResult := make(chan error, 1)
			restoredResult := make(chan error, 1)
			go func() {
				<-start
				normalResult <- normal(t.Context())
			}()
			go func() {
				<-start
				restoredResult <- restored(t.Context())
			}()
			close(start)
			normalErr := <-normalResult
			require.NoError(t, <-restoredResult)
			var conflict *swapAdmissionConflict
			if normalErr != nil {
				require.ErrorAs(t, normalErr, &conflict)
			}
			if paySide {
				winner, err := client.ResumePayViaLightning(
					t.Context(), testHash(201),
				)
				require.NoError(t, err)
				if normalErr == nil {
					require.Equal(
						t, "accepted-funding",
						winner.fundingSessionID,
					)
					require.Equal(
						t, PayStateSwapCreated,
						winner.state,
					)
				} else {
					require.Empty(
						t, winner.fundingSessionID,
					)
					require.Equal(
						t, PayStateRefundInitiated,
						winner.state,
					)
				}
			} else {
				preimage := lntypes.Preimage{202}
				hash := preimage.Hash()
				winner, err := client.ResumeReceiveViaLightning(
					t.Context(), hash,
				)
				require.NoError(t, err)
				if normalErr == nil {
					require.Equal(
						t, "accepted-claim",
						winner.claimSessionID,
					)
					require.Equal(
						t, ReceiveStateInvoiceCreated,
						winner.state,
					)
				} else {
					require.Empty(t, winner.claimSessionID)
					require.Equal(
						t, ReceiveStateVHTLCFunded,
						winner.state,
					)
				}
			}
		})
	}
}

// TestAdmissionFailurePreservesFirstWrite verifies failed persistence cannot
// turn a retry into an unconditional overwrite, and unrelated errors do not
// cause the admission helpers to resume a different session.
func TestAdmissionFailurePreservesFirstWrite(t *testing.T) {
	t.Parallel()
	store := newTestSwapStore(t)
	client := NewSwapClientWithStore(nil, nil, nil, nil, store)
	pay := admissionPaySession(t, client, false)
	receive := admissionReceiveSession(t, client, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, pay.persist(ctx), context.Canceled)
	require.ErrorIs(t, receive.persist(ctx), context.Canceled)
	require.True(t, pay.updatedAt.IsZero())
	require.True(t, receive.updatedAt.IsZero())
	require.NoError(
		t,
		admissionPaySession(
			t, client, true).persistRestored(t.Context()),
	)
	require.NoError(
		t,
		admissionReceiveSession(
			t, client, true).persistRestored(t.Context()),
	)
	var conflict *swapAdmissionConflict
	require.ErrorAs(t, pay.persist(t.Context()), &conflict)
	require.ErrorAs(t, receive.persist(t.Context()), &conflict)
	unrelated := errors.New("create failed")
	_, err := client.resumePayAdmission(t.Context(), unrelated)
	require.ErrorIs(t, err, unrelated)
	_, err = client.resumeReceiveAdmission(t.Context(), unrelated)
	require.ErrorIs(t, err, unrelated)
}
