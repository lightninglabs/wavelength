package swaps

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

type testInSwapServerConn struct {
	cfg *InSwapConfig
}

// RequestChannelID is unused in these tests.
func (c *testInSwapServerConn) RequestChannelID(
	_ context.Context, _ *btcec.PublicKey,
	_ uint32) (*RouteHint, *VHTLCConfig, error) {

	return nil, nil, nil
}

// CreateInSwap returns the preconfigured in-swap config.
func (c *testInSwapServerConn) CreateInSwap(
	context.Context, string, uint64,
	*btcec.PublicKey) (*InSwapConfig, error) {

	return c.cfg, nil
}

// Close closes the server connection.
func (c *testInSwapServerConn) Close() error {
	return nil
}

// TestPayViaLightningReturnsClaimPreimage asserts the SDK recovers the
// preimage from the spending OOR package after the vHTLC is claimed.
func TestPayViaLightningReturnsClaimPreimage(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
		spentVTXO: &VTXOInfo{
			SpentByTxID: "0123456789abcdef0123456789abcdef" +
				"0123456789abcdef0123456789abcdef",
		},
		indexedPackage: &OORPackageInfo{
			CheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		},
	}

	client := NewSwapClient(serverConn, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond
	client.fundingExpiryBuffer = 0

	result, err := client.PayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), result.PaymentHash)
	require.Equal(t, preimage, result.Preimage)
	require.Equal(t, "funding-session", result.FundingSessionID)
	require.EqualValues(t, 123, result.FeeSat)
	require.NotEmpty(t, daemonConn.lastSendPolicy)
}

// TestPayViaLightningRequiresClaimPreimage asserts the pay FSM never treats an
// absent live vHTLC as completion unless the claim preimage is actually
// indexed.
func TestPayViaLightningRequiresClaimPreimage(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(200 * time.Millisecond),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
	}

	client := NewSwapClient(serverConn, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.PayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.ErrorIs(t, err, errSwapExpired)
	require.Nil(t, result)
}

// TestPaySessionRefundsFundedVHTLCOnTimeout asserts a pay session
// automatically sweeps its funded vHTLC back through the sender refund path
// once the server claim deadline elapses.
func TestPaySessionRefundsFundedVHTLCOnTimeout(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	now := time.Unix(1_700_000_000, 0)
	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: now.Add(time.Millisecond),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   100,
		sendSessionID: "refund-session",
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: 42_000,
		},
		receiveInfo: &OORReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		spendOnCustom: true,
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0
	client.now = func() time.Time { return now }

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)

	client.now = func() time.Time { return now.Add(2 * time.Millisecond) }

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)
	require.Equal(t, 1, daemonConn.sendCustomCalls)
	require.NotEmpty(t, daemonConn.lastClaimInput[0].SpendPath)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, resumed.State())
	require.Equal(t, "refund-session", resumed.refundSessionID)
}

// TestPaySessionRefundsWhenRefundLocktimePassesBeforeClaim asserts that a
// funded pay session does not wait for the wall-clock swap deadline after the
// Ark refund locktime matures. The client should durably enter the refund path
// and sweep the vHTLC back as soon as the timeout branch is spendable.
func TestPaySessionRefundsWhenRefundLocktimePassesBeforeClaim(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   99,
		sendSessionID: "refund-session",
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: 42_000,
		},
		receiveInfo: &OORReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		spendOnCustom: true,
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.waitPollInterval = time.Millisecond
	client.refundLocktimeBuffer = 0

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)

	err = session.runUntil(t.Context(), PayStateWaitingForClaim)
	require.NoError(t, err)
	require.Equal(t, PayStateWaitingForClaim, session.State())

	daemonConn.blockHeight = 100

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)
	require.Equal(t, 1, daemonConn.sendCustomCalls)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, resumed.State())
	require.Equal(t, "refund-session", resumed.refundSessionID)
}

// TestPaySessionResumeFromStore asserts the SDK can reload a persisted pay
// session from the isolated swap database and finish once the claim preimage is
// indexed.
func TestPaySessionResumeFromStore(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)
	require.Equal(t, PayStateSwapCreated, session.State())

	daemonConn.spentVTXO = &VTXOInfo{
		SpentByTxID: "0123456789abcdef0123456789abcdef" +
			"0123456789abcdef0123456789abcdef",
	}
	daemonConn.indexedPackage = &OORPackageInfo{
		CheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, preimage[:]),
		},
	}

	resumedClient := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	resumedClient.waitPollInterval = time.Millisecond

	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateSwapCreated, resumed.State())

	result, err := resumed.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), result.PaymentHash)
	require.Equal(t, preimage, result.Preimage)
	require.Equal(t, "funding-session", result.FundingSessionID)
}

// TestPaySessionCancelDoesNotPersistFailed asserts caller cancellation does
// not durably mark a persisted pay session as Failed while it is waiting for
// funding or claim reconciliation.
func TestPaySessionCancelDoesNotPersistFailed(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(
		t.Context(), 5*time.Millisecond,
	)
	defer cancel()

	_, err = session.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	resumedClient := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.NotEqual(t, PayStateFailed, resumed.State())
	require.NotEqual(t, PayStateNeedsIntervention, resumed.State())
}

// TestPaySessionResumeFundingGraceSkipsImmediateResend asserts a resumed pay
// session in the accepted-but-not-yet-persisted funding window does not
// immediately resend funding while the ambiguity grace period is still active.
func TestPaySessionResumeFundingGraceSkipsImmediateResend(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	now := time.Unix(1_700_000_000, 0)
	grace := 50 * time.Millisecond

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: now.Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.waitPollInterval = time.Millisecond
	client.fundingResumeGracePeriod = grace
	client.now = func() time.Time { return now }

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)

	err = session.mutateAndPersist(t.Context(), func() error {
		return session.transition(payEventFundingInitiated)
	})
	require.NoError(t, err)

	resumedClient := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	resumedClient.waitPollInterval = time.Millisecond
	resumedClient.fundingResumeGracePeriod = grace
	resumedClient.now = func() time.Time { return now }

	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateFundingInitiated, resumed.State())

	waitCtx, cancel := context.WithTimeout(
		t.Context(), 5*time.Millisecond,
	)
	defer cancel()

	_, err = resumed.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 0, daemonConn.sendPolicyCalls)
	require.Equal(t, PayStateFundingInitiated, resumed.State())
}

// TestPaySessionResumeFundingGraceEventuallyRetries asserts a resumed pay
// session retries funding after the ambiguity grace period elapses without the
// vHTLC appearing.
func TestPaySessionResumeFundingGraceEventuallyRetries(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	start := time.Unix(1_700_000_000, 0)
	grace := 10 * time.Millisecond

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: start.Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.waitPollInterval = time.Millisecond
	client.fundingResumeGracePeriod = grace
	client.now = func() time.Time { return start }

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)

	err = session.mutateAndPersist(t.Context(), func() error {
		return session.transition(payEventFundingInitiated)
	})
	require.NoError(t, err)

	resumedClient := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	resumedClient.waitPollInterval = time.Millisecond
	resumedClient.fundingResumeGracePeriod = grace
	resumedClient.now = func() time.Time {
		return start.Add(2 * grace)
	}

	resumed, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(
		t.Context(), 5*time.Millisecond,
	)
	defer cancel()

	_, err = resumed.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, daemonConn.sendPolicyCalls)
	require.Equal(t, PayStateFundingInitiated, resumed.State())

	reloaded, err := resumedClient.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, "funding-session", reloaded.fundingSessionID)
	require.Equal(t, PayStateFundingInitiated, reloaded.State())
}

// TestPaySessionRefundsAmountMismatch asserts the client preserves mismatch
// context while still sweeping the funded vHTLC back once refund matures.
func TestPaySessionRefundsAmountMismatch(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refundScript, err := txscript.PayToTaprootScript(clientPriv.PubKey())
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 144,
		vhtlc: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: 41_999,
		},
		receiveInfo: &OORReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
			PkScript:    refundScript,
		},
		sendSessionID: "refund-session",
		spendOnCustom: true,
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, ErrSwapRefunded)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateRefunded, resumed.State())
	require.Contains(t, resumed.TerminalReason(), "does not match quote")
	require.Empty(t, resumed.InterventionReason())
	require.Equal(t, "funding:0", resumed.vhtlcOutpoint)
	require.EqualValues(t, 41_999, resumed.vhtlcAmount)
	require.Equal(t, "refund-session", resumed.refundSessionID)
}

// TestPaySessionFailsNearRefundLocktime asserts the client refuses to submit
// pay-side funding when the refund locktime is already imminent.
func TestPaySessionFailsNearRefundLocktime(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       100,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 99,
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorContains(t, err, "refund locktime")
	require.Equal(t, 0, daemonConn.sendPolicyCalls)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateFailed, resumed.State())
	require.Contains(t, resumed.TerminalReason(), "refund locktime")
	require.Empty(t, resumed.InterventionReason())
}

// TestPaySessionExpiresBeforeUnsafeLateFunding asserts the client refuses to
// start funding when the persisted funding deadline is already effectively
// exhausted.
func TestPaySessionExpiresBeforeUnsafeLateFunding(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	now := time.Unix(1_700_000_000, 0)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       200,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: now.Add(2 * time.Second),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey: clientPriv.PubKey(),
		operatorKey: operatorPriv.PubKey(),
		blockHeight: 100,
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.now = func() time.Time { return now }
	client.fundingExpiryBuffer = 5 * time.Second

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorIs(t, err, errSwapExpired)
	require.Equal(t, 0, daemonConn.sendPolicyCalls)

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateExpired, resumed.State())
}

// TestPaySessionNeedsInterventionOnSpentWithoutPreimage asserts the client
// preserves operator context when the funded vHTLC is authoritatively spent
// but no matching claim preimage can be recovered.
func TestPaySessionNeedsInterventionOnSpentWithoutPreimage(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       200,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   100,
		sendSessionID: "funding-session",
		spentVTXO: &VTXOInfo{
			Outpoint:    "funding:0",
			AmountSat:   42_000,
			SpentByTxID: "deadbeef",
		},
		indexedPackage: &OORPackageInfo{},
	}

	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, store,
	)
	client.waitPollInterval = time.Millisecond

	session, err := client.StartPayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)

	_, err = session.Wait(t.Context())
	require.ErrorContains(t, err, "spent without claim preimage")

	resumed, err := client.ResumePayViaLightning(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, PayStateNeedsIntervention, resumed.State())
	require.Contains(t, resumed.InterventionReason(),
		"spent without claim preimage")
	require.Equal(t, "funding:0", resumed.vhtlcOutpoint)
	require.EqualValues(t, 42_000, resumed.vhtlcAmount)
}

// TestWaitForInSwapClaimObservationToleratesPreimageLag asserts an indexed
// spend does not become NeedsIntervention before the preimage lookup retry
// window has a chance to catch up.
func TestWaitForInSwapClaimObservationToleratesPreimageLag(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		spentVTXO: &VTXOInfo{
			Outpoint:  "funding:0",
			AmountSat: 42_000,
			SpentByTxID: "0123456789abcdef0123456789abcdef" +
				"0123456789abcdef0123456789abcdef",
		},
		indexedPackages: []*OORPackageInfo{
			{},
			{
				CheckpointPSBTs: [][]byte{
					testCheckpointPSBTWithPreimage(
						t, preimage[:],
					),
				},
			},
		},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	foundPreimage, spentVTXO, err := client.waitForInSwapClaimObservation(
		t.Context(), preimage.Hash(), []byte{0x51},
	)
	require.NoError(t, err)
	require.Nil(t, spentVTXO)
	require.Equal(t, preimage, *foundPreimage)
}

// TestFindInSwapClaimObservationPropagatesListSpentError asserts reconciliation
// does not silently swallow local spent-VTXO query failures.
func TestFindInSwapClaimObservationPropagatesListSpentError(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		listSpentErr: errors.New("spent lookup failed"),
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)

	foundPreimage, spentVTXO, err := client.findInSwapClaimObservation(
		t.Context(), preimage.Hash(), []byte{0x51},
	)
	require.Nil(t, foundPreimage)
	require.Nil(t, spentVTXO)
	require.ErrorContains(t, err, "spent lookup failed")
}

// TestWaitForSpentVTXOPreimageUsesSpendingSession asserts the SDK fetches the
// checkpoints of the OOR session that spent the funded vHTLC when the spent
// vHTLC's own package does not carry the preimage.
func TestWaitForSpentVTXOPreimageUsesSpendingSession(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		spentVTXO: &VTXOInfo{
			SpentByTxID: "0123456789abcdef0123456789abcdef" +
				"0123456789abcdef0123456789abcdef",
		},
		indexedPackage: &OORPackageInfo{
			CheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.waitForSpentVTXOPreimage(
		t.Context(), preimage.Hash(), []byte{0x51},
		time.Now().Add(time.Second),
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *result)
}

// TestWaitForSpentVTXOPreimageUsesLocalSpentPackages asserts the SDK prefers
// locally persisted spent-VTXO checkpoints when they already carry the claim
// preimage.
func TestWaitForSpentVTXOPreimageUsesLocalSpentPackages(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		spentVTXOs: []VTXOInfo{{
			PkScript: []byte{0x51},
			FinalCheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		}},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.waitForSpentVTXOPreimage(
		t.Context(), preimage.Hash(), []byte{0x51},
		time.Now().Add(time.Second),
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *result)
}

// TestWaitForSpentVTXOPreimageFallsBackToLivePackages asserts the SDK can
// recover the claim preimage from a received live VTXO package when the spent
// vHTLC itself is not exposed as a local spent VTXO.
func TestWaitForSpentVTXOPreimageFallsBackToLivePackages(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		liveVTXOs: []VTXOInfo{{
			FinalCheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		}},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.waitForSpentVTXOPreimage(
		t.Context(), preimage.Hash(), []byte{0x51},
		time.Now().Add(time.Second),
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *result)
}

// testCheckpointPSBTWithPreimage encodes one finalized checkpoint PSBT that
// carries preimage in a taproot script-spend signature slot.
func testCheckpointPSBTWithPreimage(t *testing.T, preimage []byte) []byte {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{
		Value:    1,
		PkScript: []byte{0x51},
	})

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	var witness bytes.Buffer
	err = wire.WriteVarInt(&witness, 0, 1)
	require.NoError(t, err)

	err = wire.WriteVarBytes(&witness, 0, preimage)
	require.NoError(t, err)

	packet.Inputs[0].FinalScriptWitness = witness.Bytes()

	var buf bytes.Buffer
	err = packet.Serialize(&buf)
	require.NoError(t, err)

	return buf.Bytes()
}
