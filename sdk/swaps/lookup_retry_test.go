package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newLookupPaySession prepares a stored pay session before daemon key lookups.
func newLookupPaySession(t *testing.T) (*paySession, *testDaemonConn) {
	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemon := &testDaemonConn{
		identityKey:   clientKey.PubKey(),
		operatorKey:   operatorKey.PubKey(),
		blockHeight:   100,
		sendSessionID: "funding-session",
		sendOutpoint:  "funding:0",
	}
	server := &testInSwapServerConn{
		cfg: testInSwapConfig(
			serverKey.PubKey(), preimage,
			time.Now().Add(time.Minute),
		),
	}
	client := configureTestPayClient(
		NewSwapClientWithStore(
			server, daemon, nil, nil, newTestSwapStore(t),
		),
	)
	client.waitPollInterval = time.Millisecond

	return &paySession{
		client:    client,
		state:     PayStateCreated,
		invoice:   testValidPayInvoice(t, preimage),
		maxFeeSat: testInSwapFeeSat,
	}, daemon
}

// TestPaySetupRetriesDaemonKeyLookup verifies GetInfo-backed key failures do
// not terminally fail setup, and the same session succeeds after recovery.
func TestPaySetupRetriesDaemonKeyLookup(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"identity", "operator"} {
		t.Run(key, func(t *testing.T) {
			session, daemon := newLookupPaySession(t)
			lookupErr := status.Error(
				codes.Unavailable, "height unavailable",
			)
			if key == "identity" {
				daemon.identityKeyErr = lookupErr
			} else {
				daemon.operatorKeyErr = lookupErr
			}

			err := session.runUntil(
				t.Context(), PayStateSwapCreated,
			)
			var retryable *retryableActionError
			require.ErrorAs(t, err, &retryable)
			require.ErrorIs(t, err, lookupErr)
			require.Equal(t, PayStateCreated, session.State())
			require.Zero(t, daemon.sendPolicyCalls)

			daemon.identityKeyErr = nil
			daemon.operatorKeyErr = nil
			require.NoError(
				t,
				session.runUntil(
					t.Context(), PayStateSwapCreated,
				),
			)
			restored, err := session.client.ResumePayViaLightning(
				t.Context(), session.PaymentHash(),
			)
			require.NoError(t, err)
			require.Equal(t, PayStateSwapCreated, restored.State())
		})
	}
}

// TestReceiveSetupRetriesDaemonKeyLookup verifies receive setup also survives
// a GetInfo-backed key failure before publishing its invoice.
func TestReceiveSetupRetriesDaemonKeyLookup(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"identity", "operator"} {
		t.Run(key, func(t *testing.T) {
			pay, daemon := newLookupPaySession(t)
			operatorKey := daemon.operatorKey.SerializeCompressed()
			server := &testSwapServerConn{
				hint: &RouteHint{
					NodeID:    operatorKey,
					ChannelID: 1,
				},
			}
			creator := &testInvoiceCreator{
				invoice: &invoices.Invoice{
					PaymentRequest: []byte("lnrtest1swap"),
				},
			}
			session := &ReceiveSession{
				client: NewSwapClientWithStore(
					server, daemon, nil, creator,
					pay.client.store,
				),
				state:     ReceiveStateCreated,
				amountSat: testInSwapAmountSat,
			}
			lookupErr := status.Error(
				codes.Unavailable, "height unavailable",
			)
			if key == "identity" {
				daemon.identityKeyErr = lookupErr
			} else {
				daemon.operatorKeyErr = lookupErr
			}

			err := session.runUntil(
				t.Context(), ReceiveStateInvoiceCreated,
			)
			var retryable *retryableActionError
			require.ErrorAs(t, err, &retryable)
			require.ErrorIs(t, err, lookupErr)
			require.Equal(t, ReceiveStateCreated, session.State())
			require.Empty(t, session.Invoice)
			require.Zero(t, daemon.receiveAllocCalls)

			daemon.identityKeyErr = nil
			daemon.operatorKeyErr = nil
			require.NoError(
				t,
				session.runUntil(
					t.Context(), ReceiveStateInvoiceCreated,
				),
			)
			client := session.client
			restored, err := client.ResumeReceiveViaLightning(
				t.Context(), session.PaymentHash,
			)
			require.NoError(t, err)
			require.Equal(
				t, ReceiveStateInvoiceCreated, restored.State(),
			)
		})
	}
}

// TestPayClaimWaitHeightFailureHonorsDeadline verifies a funded swap reaches
// refund intent on wall-clock expiry even while its height is unreadable.
func TestPayClaimWaitHeightFailureHonorsDeadline(t *testing.T) {
	t.Parallel()

	session, daemon := newLookupPaySession(t)
	require.NoError(
		t,
		session.runUntil(
			t.Context(), PayStateWaitingForClaim,
		),
	)
	session.client.now = func() time.Time {
		return session.cfg.Expiry
	}
	daemon.blockHeightErr = status.Error(
		codes.Unavailable, "height unavailable",
	)

	require.NoError(
		t,
		session.runUntil(
			t.Context(), PayStateRefundInitiated,
		),
	)
	require.Equal(t, "claim deadline elapsed", session.TerminalReason())
	require.Zero(t, daemon.sendCustomCalls)
	restored, err := session.client.ResumePayViaLightning(
		t.Context(), session.PaymentHash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefundInitiated, restored.State())
}

// TestPayClaimWaitPollsThroughHeightFailure verifies failed height results
// cannot trigger a refund or end claim monitoring before the deadline.
func TestPayClaimWaitPollsThroughHeightFailure(t *testing.T) {
	t.Parallel()

	session, daemon := newLookupPaySession(t)
	require.NoError(
		t,
		session.runUntil(
			t.Context(), PayStateWaitingForClaim,
		),
	)
	daemon.blockHeight = session.cfg.VHTLCConfig.RefundLocktime
	daemon.blockHeightErr = status.Error(
		codes.Unavailable, "height unavailable",
	)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	daemon.liveLookupHook = func(int) (*VTXOInfo, error) {
		calls++
		if calls == 3 {
			cancel()
		}

		return nil, nil
	}

	err := session.runUntil(ctx, PayStateCompleted)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 3, calls)
	require.Equal(t, PayStateWaitingForClaim, session.State())
	require.Zero(t, daemon.sendCustomCalls)
}
