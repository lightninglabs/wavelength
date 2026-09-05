package swaps

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testRefreshServerConn struct {
	*testSwapServerConn

	config           *RefreshSwapConfig
	createErr        error
	createCalls      int
	lastPaymentHash  lntypes.Hash
	lastAmountSat    btcutil.Amount
	lastClientKey    *btcec.PublicKey
	lastMaxAgeBlocks uint32
}

// CreateRefreshSwap records the immutable client request and returns the
// configured first-leg terms.
func (s *testRefreshServerConn) CreateRefreshSwap(_ context.Context,
	paymentHash lntypes.Hash, amountSat btcutil.Amount,
	clientKey *btcec.PublicKey, maxAge uint32) (*RefreshSwapConfig, error) {

	s.createCalls++
	s.lastPaymentHash = paymentHash
	s.lastAmountSat = amountSat
	s.lastClientKey = clientKey
	s.lastMaxAgeBlocks = maxAge

	return s.config, s.createErr
}

type testRefreshEventReceiver struct {
	notification *IncomingVHTLCNotification
	waitCalls    int
	ackCalls     int
	lastCursor   uint64
}

// WaitOutSwapHtlc is unused because refresh swaps require the typed incoming
// vHTLC event surface.
func (r *testRefreshEventReceiver) WaitOutSwapHtlc(context.Context,
	lntypes.Hash, *btcec.PublicKey) (*OutSwapHtlcNotification, error) {

	return nil, fmt.Errorf("unexpected legacy out-swap wait")
}

// AckOutSwapHtlc records a resumed mailbox ACK when the in-memory callback is
// unavailable.
func (r *testRefreshEventReceiver) AckOutSwapHtlc(_ context.Context,
	_ lntypes.Hash, _ *btcec.PublicKey, cursor uint64) error {

	r.ackCalls++
	r.lastCursor = cursor

	return nil
}

// WaitIncomingVHTLC returns the configured refresh notification.
func (r *testRefreshEventReceiver) WaitIncomingVHTLC(context.Context,
	lntypes.Hash, *btcec.PublicKey) (*IncomingVHTLCNotification, error) {

	r.waitCalls++

	return r.notification, nil
}

// newRefreshHarness constructs valid input and output legs with enough timing
// margin for a deterministic happy-path test.
func newRefreshHarness(t *testing.T, maxAge uint32) (*SwapClient,
	*testRefreshServerConn, *testDaemonConn, *testRefreshEventReceiver,
	RefreshSwapRequest) {

	t.Helper()
	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage, err := NewPreimage()
	require.NoError(t, err)

	now := time.Unix(1_800_000_000, 0)
	serverKey := serverPriv.PubKey()
	serverKeyBytes := serverKey.SerializeCompressed()
	server := &testRefreshServerConn{
		testSwapServerConn: &testSwapServerConn{},
		config: &RefreshSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			ServerPubkey: serverKey,
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       320,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
				SwapServerPubkey: append(
					[]byte(nil), serverKeyBytes...,
				),
			},
			Expiry:         now.Add(time.Hour),
			SettlementType: SettlementTypeRefresh,
		},
	}
	daemon := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		blockHeight:   100,
		sendSessionID: "refresh-funding-session",
		sendOutpoint:  "input-vhtlc:0",
		liveVTXOs: []VTXOInfo{{
			Outpoint:  "old-vtxo:0",
			AmountSat: 42_000,
		}},
		receiveInfo: &ReceiveInfo{
			PubKeyXOnly: clientPriv.PubKey().
				SerializeCompressed()[1:],
			PkScript: []byte{
				0x51,
				0x20,
				1,
			},
		},
	}
	receiver := &testRefreshEventReceiver{}
	client := NewSwapClientWithStore(
		server, daemon, nil, nil, newTestSwapStore(t),
	)
	client.SetOutSwapEventReceiver(receiver)
	client.waitPollInterval = time.Millisecond
	client.claimRetryDelay = time.Millisecond
	client.claimMaxAttempts = 1
	client.now = func() time.Time {
		return now
	}

	return client, server, daemon, receiver, RefreshSwapRequest{
		Preimage:         preimage,
		PaymentHash:      preimage.Hash(),
		AmountSat:        42_000,
		SourceOutpoint:   "old-vtxo:0",
		MaxVTXOAgeBlocks: maxAge,
	}
}

// testRefreshOutputNotification builds production-shaped second-leg terms for
// restart and recovery-boundary tests.
func testRefreshOutputNotification(req RefreshSwapRequest,
	server *testRefreshServerConn, outpoint string,
	cursor uint64) *IncomingVHTLCNotification {

	return &IncomingVHTLCNotification{
		InArk: &InArkHtlcEvent{
			PaymentHash:  req.PaymentHash,
			AmountSat:    int64(req.AmountSat),
			SenderPubkey: server.config.ServerPubkey,
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       240,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
				SwapServerPubkey: server.config.ServerPubkey.
					SerializeCompressed(),
			},
			VHTLCOutpoint:      outpoint,
			VHTLCAmountSat:     int64(req.AmountSat),
			RequestedAmountSat: uint64(req.AmountSat),
			SettlementType:     SettlementTypeRefresh,
		},
		AckCursor: cursor,
	}
}

// TestRefreshSessionHappyPath verifies the composite FSM binds an exact input,
// durably accepts and ACKs the REFRESH event, enforces output age, claims with
// the shared preimage, then waits for the server's matching input claim.
func TestRefreshSessionHappyPath(t *testing.T) {
	t.Parallel()

	client, server, daemon, receiver, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, RefreshStateSwapCreated, session.State())
	require.Equal(t, 1, server.createCalls)
	require.Equal(t, req.PaymentHash, server.lastPaymentHash)
	require.Equal(t, req.AmountSat, server.lastAmountSat)
	require.Equal(t, req.MaxVTXOAgeBlocks, server.lastMaxAgeBlocks)
	resolved, err := client.store.ResolvePreimage(
		t.Context(), req.PaymentHash[:], req.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, req.Preimage, resolved)

	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)
	require.Len(t, daemon.sendPolicyOpts, 1)
	require.Equal(
		t, []string{req.SourceOutpoint},
		daemon.sendPolicyOpts[0].ExactInputOutpoints,
	)
	require.Zero(t, daemon.sendPolicyOpts[0].MaxVTXOAgeBlocks)
	require.Equal(t, 1, daemon.armRecoveryCalls)

	outputConfig := VHTLCConfig{
		RefundLocktime:                       240,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		SwapServerPubkey: server.config.ServerPubkey.
			SerializeCompressed(),
	}
	mailboxAckCalls := 0
	receiver.notification = &IncomingVHTLCNotification{
		InArk: &InArkHtlcEvent{
			PaymentHash:    req.PaymentHash,
			AmountSat:      int64(req.AmountSat),
			SenderPubkey:   server.config.ServerPubkey,
			VHTLCConfig:    outputConfig,
			VHTLCOutpoint:  "output-vhtlc:0",
			VHTLCAmountSat: int64(req.AmountSat),
			RequestedAmountSat: uint64(
				req.AmountSat,
			),
			SettlementType: SettlementTypeRefresh,
		},
		AckCursor: 11,
		Ack: func(context.Context) error {
			mailboxAckCalls++

			return nil
		},
	}
	output := &VTXOInfo{
		Outpoint:      "output-vhtlc:0",
		AmountSat:     int64(req.AmountSat),
		CreatedHeight: 95,
		BatchExpiry:   500,
	}

	// Stop after the durable event boundary and reconstruct the session
	// before output indexing catches up. The accepted event already names
	// the outpoint, but age evidence is intentionally still absent.
	err = session.runUntil(
		t.Context(), RefreshStateOutputHTLCEventAccepted,
	)
	require.NoError(t, err)
	resumedAtEvent, err := client.ResumeRefreshSwap(
		t.Context(), req.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(
		t, RefreshStateOutputHTLCEventAccepted, resumedAtEvent.State(),
	)
	require.Zero(t, resumedAtEvent.outputObservedHeight)
	session = resumedAtEvent
	daemon.liveByPkScript = map[string]*VTXOInfo{
		hex.EncodeToString(session.outputPkScript): output,
	}

	err = session.runUntil(t.Context(), RefreshStateOutputClaimed)
	require.NoError(t, err)
	require.Equal(t, 1, receiver.waitCalls)
	require.Zero(t, mailboxAckCalls)
	require.Equal(t, 1, receiver.ackCalls)
	require.EqualValues(t, 11, receiver.lastCursor)
	require.Equal(t, 1, server.serverAckCalls)
	require.Equal(t, 2, daemon.armRecoveryCalls)
	require.Equal(t, 1, daemon.sendCustomCalls)
	require.Equal(t, "output-vhtlc:0", daemon.lastClaimInput[0].Outpoint)
	require.Equal(t, req.Preimage.Hash(), daemon.lastAckSignHash)
	require.EqualValues(t, req.AmountSat, daemon.lastAckSignAmount)

	spentInput := VTXOInfo{
		Outpoint:    "input-vhtlc:0",
		AmountSat:   int64(req.AmountSat),
		PkScript:    append([]byte(nil), session.inputPkScript...),
		SpentByTxID: "server-claim-tx",
		FinalCheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, req.Preimage[:]),
		},
	}
	daemon.spentVTXOs = []VTXOInfo{spentInput}
	daemon.spentVTXO = &spentInput
	err = session.runUntil(t.Context(), RefreshStateCompleted)
	require.NoError(t, err)
	require.Equal(t, "server-claim-tx", session.inputClaimTxID)

	resumed, err := client.ResumeRefreshSwap(
		t.Context(), req.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, RefreshStateCompleted, resumed.State())
	require.Equal(t, req.SourceOutpoint, resumed.sourceOutpoint)
	require.Equal(t, req.MaxVTXOAgeBlocks, resumed.maxVTXOAgeBlocks)
	require.EqualValues(t, 100, resumed.outputObservedHeight)
	require.EqualValues(t, 95, resumed.outputCreatedHeight)
}

// TestRefreshAckFailureResumesFromAcceptedEvent verifies an ACK transport
// failure cannot leave the Loop FSM behind the durable business state.
func TestRefreshAckFailureResumesFromAcceptedEvent(t *testing.T) {
	t.Parallel()

	client, server, daemon, receiver, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)
	receiver.notification = testRefreshOutputNotification(
		req, server, "ack-output:0", 21,
	)
	err = session.runUntil(
		t.Context(), RefreshStateOutputHTLCEventAccepted,
	)
	require.NoError(t, err)

	ackErr := errors.New("temporary refresh ACK failure")
	server.serverAckErrs = []error{ackErr}
	err = session.runUntil(t.Context(), RefreshStateOutputVHTLCFunded)
	require.ErrorIs(t, err, ackErr)
	require.Equal(
		t, RefreshStateOutputHTLCEventAccepted, session.State(),
	)

	resumed, err := client.ResumeRefreshSwap(
		t.Context(), req.PaymentHash,
	)
	require.NoError(t, err)
	daemon.liveByPkScript = map[string]*VTXOInfo{
		hex.EncodeToString(resumed.outputPkScript): {
			Outpoint:      "ack-output:0",
			AmountSat:     int64(req.AmountSat),
			CreatedHeight: 95,
			BatchExpiry:   500,
		},
	}
	err = resumed.runUntil(t.Context(), RefreshStateOutputVHTLCFunded)
	require.NoError(t, err)
	require.Zero(t, resumed.pendingAckCursor)
	require.NotEmpty(t, resumed.claimRecoveryID)
}

// TestRefreshOutputRecoveryArmFailureResumesBeforeFundingState verifies the
// recovery side effect is armed before the output-funded transition and is
// safely idempotent after a crash or transient daemon failure.
func TestRefreshOutputRecoveryArmFailureResumesBeforeFundingState(
	t *testing.T) {

	t.Parallel()

	client, server, daemon, receiver, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)
	receiver.notification = testRefreshOutputNotification(
		req, server, "arm-output:0", 22,
	)
	err = session.runUntil(
		t.Context(), RefreshStateOutputHTLCEventAccepted,
	)
	require.NoError(t, err)
	daemon.liveByPkScript = map[string]*VTXOInfo{
		hex.EncodeToString(session.outputPkScript): {
			Outpoint:      "arm-output:0",
			AmountSat:     int64(req.AmountSat),
			CreatedHeight: 95,
			BatchExpiry:   500,
		},
	}
	armErr := errors.New("temporary recovery arm failure")
	daemon.armRecoveryErr = armErr
	err = session.runUntil(t.Context(), RefreshStateOutputVHTLCFunded)
	require.ErrorIs(t, err, armErr)
	require.Equal(
		t, RefreshStateOutputHTLCEventAccepted, session.State(),
	)
	require.Zero(t, session.outputObservedHeight)
	require.Empty(t, session.claimRecoveryID)

	daemon.armRecoveryErr = nil
	resumed, err := client.ResumeRefreshSwap(
		t.Context(), req.PaymentHash,
	)
	require.NoError(t, err)
	err = resumed.runUntil(t.Context(), RefreshStateOutputVHTLCFunded)
	require.NoError(t, err)
	require.NotEmpty(t, resumed.claimRecoveryID)
}

// TestRefreshSessionRejectsOldServerOutput verifies an actual output whose
// backing batch exceeds the caller's cap is never claimed and instead moves
// the first leg onto its already-armed refund path.
func TestRefreshSessionRejectsOldServerOutput(t *testing.T) {
	t.Parallel()

	client, server, daemon, receiver, req := newRefreshHarness(t, 4)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)

	receiver.notification = &IncomingVHTLCNotification{
		InArk: &InArkHtlcEvent{
			PaymentHash:  req.PaymentHash,
			AmountSat:    int64(req.AmountSat),
			SenderPubkey: server.config.ServerPubkey,
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       240,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
				SwapServerPubkey: server.config.ServerPubkey.
					SerializeCompressed(),
			},
			VHTLCOutpoint:  "old-output:0",
			VHTLCAmountSat: int64(req.AmountSat),
			RequestedAmountSat: uint64(
				req.AmountSat,
			),
			SettlementType: SettlementTypeRefresh,
		},
		AckCursor: 12,
		Ack: func(context.Context) error {
			return nil
		},
	}
	daemon.liveLookupHook = func(int) (*VTXOInfo, error) {
		return &VTXOInfo{
			Outpoint:      "old-output:0",
			AmountSat:     int64(req.AmountSat),
			CreatedHeight: 95,
			BatchExpiry:   500,
		}, nil
	}

	err = session.runUntil(t.Context(), RefreshStateRefundInitiated)
	require.NoError(t, err)
	require.Equal(t, RefreshStateRefundInitiated, session.State())
	require.Zero(t, daemon.sendCustomCalls)
	require.Equal(t, 1, daemon.armRecoveryCalls)
	require.Contains(t, session.TerminalReason(), "age 5 exceeds maximum 4")
}

// TestStartRefreshSwapRejectsDurableTermDrift verifies an idempotent local
// replay cannot silently substitute a different source or maximum age.
func TestStartRefreshSwapRejectsDurableTermDrift(t *testing.T) {
	t.Parallel()

	client, _, _, _, req := newRefreshHarness(t, 10)
	_, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)

	conflict := req
	conflict.MaxVTXOAgeBlocks++
	_, err = client.StartRefreshSwap(t.Context(), conflict)
	require.ErrorContains(t, err, "conflicts with durable terms")
}

// TestStartRefreshSwapRequiresMaximumAge mirrors the server-side refresh
// admission rule before any durable local or remote intent is created.
func TestStartRefreshSwapRequiresMaximumAge(t *testing.T) {
	t.Parallel()

	client, server, _, _, req := newRefreshHarness(t, 10)
	req.MaxVTXOAgeBlocks = 0
	_, err := client.StartRefreshSwap(t.Context(), req)
	require.ErrorContains(t, err, "maximum VTXO age must be positive")
	require.Zero(t, server.createCalls)
}

// TestRefreshSessionRejectsStaleSnapshot verifies separately resumed workers
// cannot overwrite a newer durable state with an older in-memory snapshot.
func TestRefreshSessionRejectsStaleSnapshot(t *testing.T) {
	t.Parallel()

	client, _, daemon, _, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	stale, err := client.ResumeRefreshSwap(t.Context(), req.PaymentHash)
	require.NoError(t, err)

	err = session.runUntil(t.Context(), RefreshStateFundingInitiated)
	require.NoError(t, err)
	_, err = stale.Wait(t.Context())
	require.ErrorIs(t, err, ErrRefreshSessionStale)
	require.Zero(t, daemon.sendPolicyCalls)
	err = stale.mutateAndPersist(t.Context(), func() error {
		return stale.transition(refreshEventFundingInitiated)
	})
	require.ErrorIs(t, err, ErrRefreshSessionStale)
	require.Equal(t, RefreshStateSwapCreated, stale.State())

	latest, err := client.ResumeRefreshSwap(t.Context(), req.PaymentHash)
	require.NoError(t, err)
	require.Equal(t, RefreshStateFundingInitiated, latest.State())
}

// TestRefreshSessionAppearsInGenericSummaries verifies daemon-facing
// inspection and pending-session discovery include the composite FSM.
func TestRefreshSessionAppearsInGenericSummaries(t *testing.T) {
	t.Parallel()

	client, _, _, _, req := newRefreshHarness(t, 10)
	_, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)

	summary, err := client.GetSwapSummary(t.Context(), req.PaymentHash)
	require.NoError(t, err)
	require.Equal(t, SwapDirectionRefresh, summary.Direction)
	require.Equal(t, SettlementTypeRefresh, summary.SettlementType)
	require.Equal(t, RefreshStateSwapCreated.String(), summary.State)
	require.True(t, summary.Pending)

	pending, err := client.ListSwapSummaries(t.Context(), true)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, SwapDirectionRefresh, pending[0].Direction)

	require.NoError(
		t,
		client.claimPaymentHashOwner(
			t.Context(), req.PaymentHash, SwapDirectionRefresh,
		),
	)
	require.ErrorIs(
		t,
		client.claimPaymentHashOwner(
			t.Context(), req.PaymentHash, SwapDirectionPay,
		),
		ErrSwapPaymentHashOwned,
	)
	require.Error(
		t,
		client.ensurePaymentHashOwnerAvailable(
			t.Context(), req.PaymentHash, SwapDirectionPay,
		),
	)
	require.Error(
		t,
		client.ensurePaymentHashOwnerAvailable(
			t.Context(), req.PaymentHash, SwapDirectionReceive,
		),
	)

	standardPreimage, err := NewPreimage()
	require.NoError(t, err)
	standardHash := standardPreimage.Hash()
	require.NoError(
		t,
		client.claimPaymentHashOwner(
			t.Context(), standardHash, SwapDirectionPay,
		),
	)
	require.NoError(
		t,
		client.claimPaymentHashOwner(
			t.Context(), standardHash, SwapDirectionReceive,
		),
	)
}

// TestRefreshFundingSafetyWindowUsesReadOnlyReplay verifies a resumed funding
// intent never submits a new old-input transfer after its block-height safety
// margin has closed.
func TestRefreshFundingSafetyWindowUsesReadOnlyReplay(t *testing.T) {
	t.Parallel()

	client, _, daemon, _, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateFundingInitiated)
	require.NoError(t, err)

	daemon.blockHeight = 319
	daemon.sendPolicyErr = status.Error(codes.NotFound, "not found")
	err = session.fundInputVHTLC(t.Context())
	require.ErrorIs(t, err, ErrSwapExpired)
	require.Equal(t, RefreshStateExpired, session.State())
	require.Len(t, daemon.sendPolicyOpts, 1)
	require.True(t, daemon.sendPolicyOpts[0].ExistingOnly)
	require.Empty(t, session.fundingSessionID)
}

// TestRefreshPreFundingFailureCanResume verifies invalid server terms persist
// a terminal diagnostic row without requiring terms that were never accepted.
func TestRefreshPreFundingFailureCanResume(t *testing.T) {
	t.Parallel()

	client, server, _, _, req := newRefreshHarness(t, 10)
	server.config.SettlementType = SettlementTypeLightning
	_, err := client.StartRefreshSwap(t.Context(), req)
	require.Error(t, err)

	resumed, err := client.ResumeRefreshSwap(t.Context(), req.PaymentHash)
	require.NoError(t, err)
	require.Equal(t, RefreshStateFailed, resumed.State())
}

// TestRefreshCreateCollisionFailsDurably verifies a server-side hash collision
// cannot leave the startup worker retrying an immutable request forever.
func TestRefreshCreateCollisionFailsDurably(t *testing.T) {
	t.Parallel()

	client, server, _, _, req := newRefreshHarness(t, 10)
	server.createErr = status.Error(
		codes.AlreadyExists, "payment hash belongs to another swap",
	)
	_, err := client.StartRefreshSwap(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, 1, server.createCalls)

	resumed, err := client.ResumeRefreshSwap(
		t.Context(), req.PaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, RefreshStateFailed, resumed.State())
	_, err = resumed.Wait(t.Context())
	require.Error(t, err)
	require.Equal(t, 1, server.createCalls)
}

// TestRefreshFailedFundingSessionRemainsRecoverable verifies daemon FAILED is
// not proof that the first-leg transfer never finalized at the operator.
func TestRefreshFailedFundingSessionRemainsRecoverable(t *testing.T) {
	t.Parallel()

	client, _, daemon, _, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)
	daemon.oorSession = &waverpc.OORSessionInfo{
		SessionId: session.fundingSessionID,
		Status: waverpc.
			OORSessionStatus_OOR_SESSION_STATUS_FAILED,
		FailureReason: "local bookkeeping failed after finalization",
	}

	err = session.checkInputFundingRejected(t.Context())
	require.NoError(t, err)
	require.Equal(t, RefreshStateInputVHTLCFunded, session.State())
	require.Zero(t, daemon.cancelCalls)
	require.NotEmpty(t, session.refundRecoveryID)
}

// TestRefreshFailedRefundSessionIsNotResubmitted verifies a FAILED local OOR
// row is not treated as proof that the operator left the exact input unspent.
func TestRefreshFailedRefundSessionIsNotResubmitted(t *testing.T) {
	t.Parallel()

	client, _, daemon, _, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)
	err = session.initiateRefund(t.Context(), "test refund")
	require.NoError(t, err)
	err = session.mutateAndPersist(t.Context(), func() error {
		session.refundSessionID = "ambiguous-refund-session"

		return nil
	})
	require.NoError(t, err)
	daemon.oorSession = &waverpc.OORSessionInfo{
		SessionId: session.refundSessionID,
		Status: waverpc.
			OORSessionStatus_OOR_SESSION_STATUS_FAILED,
		FailureReason: "bookkeeping failed after submit",
	}

	err = session.completeRefund(t.Context())
	require.NoError(t, err)
	require.Equal(
		t, "ambiguous-refund-session", session.refundSessionID,
	)
	require.Equal(t, RefreshStateRefundInitiated, session.State())
	require.Zero(t, daemon.sendCustomCalls)
}

// TestRefreshUnclassifiedFundedFailureNeedsIntervention verifies an unknown
// error cannot turn a possibly funded exact-input transfer into a safe Failed
// terminal state.
func TestRefreshUnclassifiedFundedFailureNeedsIntervention(t *testing.T) {
	t.Parallel()

	client, _, _, _, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateFundingInitiated)
	require.NoError(t, err)

	machine := newRefreshLoopFSM(
		session, RefreshStateInputVHTLCFunded,
	)
	event := machine.fail(t.Context(), errors.New("unexpected failure"))
	require.Equal(t, refreshEventNeedsIntervention, event)
	require.Equal(t, RefreshStateNeedsIntervention, session.State())
	require.True(
		t,
		refreshFailureNeedsIntervention(
			RefreshStateFundingInitiated,
			newFailureError("explicit but unsafe failure", nil),
		),
	)
}

// TestRefreshUnknownInputSpendNeedsIntervention verifies a spend of the exact
// first leg is not called a successful refund without positive evidence that
// either the cooperative refund or daemon recovery produced it.
func TestRefreshUnknownInputSpendNeedsIntervention(t *testing.T) {
	t.Parallel()

	client, _, daemon, _, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)
	err = session.initiateRefund(t.Context(), "test refund")
	require.NoError(t, err)

	otherPreimage, err := NewPreimage()
	require.NoError(t, err)
	spent := VTXOInfo{
		Outpoint:    session.inputOutpoint,
		AmountSat:   session.inputAmount,
		PkScript:    append([]byte(nil), session.inputPkScript...),
		SpentByTxID: "unknown-spend",
		FinalCheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, otherPreimage[:]),
		},
	}
	daemon.spentVTXOs = []VTXOInfo{spent}
	daemon.spentVTXO = &spent
	err = session.runUntil(t.Context(), RefreshStateRefunded)
	require.Error(t, err)
	require.Equal(t, RefreshStateNeedsIntervention, session.State())
	require.Contains(t, session.TerminalReason(), "confirmed client refund")
}

// TestRefreshClaimRecoveryCompletionBeatsRollover verifies a terminal claim
// recovery response cannot be mistaken for successful cancellation while the
// same-script cooperative rollover is being followed.
func TestRefreshClaimRecoveryCompletionBeatsRollover(t *testing.T) {
	t.Parallel()

	session, daemon := refreshSessionAtClaimIntent(t)
	claimRecoveryID := session.claimRecoveryID
	initialSendCalls := daemon.sendCustomCalls
	daemon.liveByPkScript[hex.EncodeToString(session.outputPkScript)] =
		&VTXOInfo{
			Outpoint:      "claim-race-replacement:0",
			AmountSat:     session.outputAmount,
			CreatedHeight: 96,
			BatchExpiry:   500,
		}
	daemon.cancelHook = func(req *waverpc.CancelVHTLCRecoveryRequest) (
		*waverpc.CancelVHTLCRecoveryResponse, error) {

		state := recoveryStateCancelled
		if req.GetRecoveryId() == claimRecoveryID {
			state = recoveryStateCompleted
		}

		return refreshCancelResponse(req.GetRecoveryId(), state), nil
	}

	err := session.claimOutputVHTLC(t.Context())
	require.NoError(t, err)
	require.Equal(t, RefreshStateOutputClaimed, session.State())
	require.Equal(t, "claim-race-output:0", session.outputOutpoint)
	require.Equal(t, initialSendCalls, daemon.sendCustomCalls)
}

// TestRefreshRefundRecoveryTerminalBlocksOutputClaim verifies the opposite
// recovery branch is quarantined when its terminal status is incompatible with
// recording a preimage-revealing output claim.
func TestRefreshRefundRecoveryTerminalBlocksOutputClaim(t *testing.T) {
	t.Parallel()

	states := []waverpc.VHTLCRecoveryState{
		recoveryStateCompleted,
		recoveryStateFailed,
	}
	for _, state := range states {
		state := state
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()

			session, daemon := refreshSessionAtClaimIntent(t)
			claimSessionID := "accepted-output-claim"
			err := session.mutateAndPersist(
				t.Context(), func() error {
					session.claimSessionID = claimSessionID

					return nil
				},
			)
			require.NoError(t, err)
			refundRecoveryID := session.refundRecoveryID
			daemon.cancelHook = func(
				req *waverpc.CancelVHTLCRecoveryRequest) (
				*waverpc.CancelVHTLCRecoveryResponse, error) {

				cancelState := recoveryStateCancelled
				if req.GetRecoveryId() == refundRecoveryID {
					cancelState = state
				}

				return refreshCancelResponse(
					req.GetRecoveryId(), cancelState,
				), nil
			}

			err = session.runUntil(
				t.Context(), RefreshStateCompleted,
			)
			require.Error(t, err)
			require.Equal(
				t, RefreshStateNeedsIntervention,
				session.State(),
			)
			require.Contains(
				t, session.TerminalReason(),
				"input refund",
			)
		})
	}
}

// TestCancelRefreshVHTLCRecoveryRejectsAmbiguousStatus covers daemon responses
// that do not prove a pre-escalation cancellation won.
func TestCancelRefreshVHTLCRecoveryRejectsAmbiguousStatus(t *testing.T) {
	t.Parallel()

	t.Run("missing status", func(t *testing.T) {
		daemon := &testDaemonConn{
			cancelHook: func(*waverpc.CancelVHTLCRecoveryRequest) (
				*waverpc.CancelVHTLCRecoveryResponse, error) {

				return nil, nil
			},
		}

		_, _, err := cancelRefreshVHTLCRecovery(
			t.Context(), daemon, "recovery", "test", "",
		)
		require.Error(t, err)
	})

	for _, state := range []waverpc.VHTLCRecoveryState{
		recoveryStateArmed,
		recoveryStateUnspecified,
	} {
		state := state
		t.Run("non-cancelled "+state.String(), func(t *testing.T) {
			cancelResp := refreshCancelResponse("recovery", state)
			daemon := &testDaemonConn{
				cancelResp: cancelResp,
			}

			_, _, err := cancelRefreshVHTLCRecovery(
				t.Context(), daemon, "recovery", "test", "",
			)
			require.Error(t, err)
		})
	}

	t.Run("escalated before registry visibility", func(t *testing.T) {
		daemon := &testDaemonConn{
			cancelResp: &waverpc.CancelVHTLCRecoveryResponse{
				Status: &waverpc.VHTLCRecoveryStatus{
					RecoveryId:      "recovery",
					State:           recoveryStateCancelled,
					EscalatedAtUnix: 1,
				},
			},
		}

		state, _, err := cancelRefreshVHTLCRecovery(
			t.Context(), daemon, "recovery", "test", "",
		)
		require.NoError(t, err)
		require.Equal(t, recoveryStateUnrollStarted, state)
	})

	t.Run("unknown joined unroll", func(t *testing.T) {
		daemon := &testDaemonConn{
			cancelResp: &waverpc.CancelVHTLCRecoveryResponse{
				Status: &waverpc.VHTLCRecoveryStatus{
					RecoveryId:  "recovery",
					State:       recoveryStateCancelled,
					UnrollFound: true,
				},
			},
		}

		state, _, err := cancelRefreshVHTLCRecovery(
			t.Context(), daemon, "recovery", "test", "",
		)
		require.NoError(t, err)
		require.Equal(t, recoveryStateUnrollStarted, state)
	})
}

// refreshSessionAtClaimIntent advances a valid refresh through both funded
// legs without submitting the preimage-bearing output claim.
func refreshSessionAtClaimIntent(t *testing.T) (*RefreshSession,
	*testDaemonConn) {

	t.Helper()
	client, server, daemon, receiver, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)
	receiver.notification = testRefreshOutputNotification(
		req, server, "claim-race-output:0", 37,
	)
	err = session.runUntil(
		t.Context(), RefreshStateOutputHTLCEventAccepted,
	)
	require.NoError(t, err)
	daemon.liveByPkScript = map[string]*VTXOInfo{
		hex.EncodeToString(session.outputPkScript): {
			Outpoint:      "claim-race-output:0",
			AmountSat:     int64(req.AmountSat),
			CreatedHeight: 95,
			BatchExpiry:   500,
		},
	}
	err = session.runUntil(t.Context(), RefreshStateOutputVHTLCFunded)
	require.NoError(t, err)
	err = session.initiateOutputClaim(t.Context())
	require.NoError(t, err)
	require.Equal(t, RefreshStateClaimInitiated, session.State())
	require.Empty(t, session.claimSessionID)

	return session, daemon
}

// refreshCancelResponse builds the daemon's post-cancellation status for one
// deterministic recovery race.
func refreshCancelResponse(recoveryID string,
	state waverpc.VHTLCRecoveryState) *waverpc.CancelVHTLCRecoveryResponse {

	return &waverpc.CancelVHTLCRecoveryResponse{
		Status: &waverpc.VHTLCRecoveryStatus{
			RecoveryId: recoveryID,
			State:      state,
		},
	}
}

// TestRefreshOutputRolloverUsesExactOutpoint verifies a spent predecessor with
// the same policy script cannot make the client abandon a live replacement.
func TestRefreshOutputRolloverUsesExactOutpoint(t *testing.T) {
	t.Parallel()

	client, server, daemon, receiver, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)

	outputConfig := VHTLCConfig{
		RefundLocktime:                       240,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		SwapServerPubkey: server.config.ServerPubkey.
			SerializeCompressed(),
	}
	receiver.notification = &IncomingVHTLCNotification{
		InArk: &InArkHtlcEvent{
			PaymentHash:        req.PaymentHash,
			AmountSat:          int64(req.AmountSat),
			SenderPubkey:       server.config.ServerPubkey,
			VHTLCConfig:        outputConfig,
			VHTLCOutpoint:      "old-output:0",
			VHTLCAmountSat:     int64(req.AmountSat),
			RequestedAmountSat: uint64(req.AmountSat),
			SettlementType:     SettlementTypeRefresh,
		},
		AckCursor: 19,
		Ack: func(context.Context) error {
			return nil
		},
	}
	err = session.runUntil(
		t.Context(), RefreshStateOutputHTLCEventAccepted,
	)
	require.NoError(t, err)
	scriptKey := hex.EncodeToString(session.outputPkScript)
	daemon.liveByPkScript = map[string]*VTXOInfo{
		scriptKey: {
			Outpoint:      "old-output:0",
			AmountSat:     int64(req.AmountSat),
			CreatedHeight: 95,
			BatchExpiry:   500,
		},
	}
	err = session.runUntil(t.Context(), RefreshStateOutputVHTLCFunded)
	require.NoError(t, err)
	oldRecoveryID := session.claimRecoveryID

	otherPreimage, err := NewPreimage()
	require.NoError(t, err)
	daemon.spentVTXOs = []VTXOInfo{{
		Outpoint:    "old-output:0",
		AmountSat:   int64(req.AmountSat),
		PkScript:    append([]byte(nil), session.outputPkScript...),
		SpentByTxID: "rollover-spend",
		FinalCheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, otherPreimage[:]),
		},
	}}
	daemon.liveByPkScript[scriptKey] = &VTXOInfo{
		Outpoint:      "replacement-output:0",
		AmountSat:     int64(req.AmountSat),
		CreatedHeight: 96,
		BatchExpiry:   500,
	}
	err = session.runUntil(t.Context(), RefreshStateOutputClaimed)
	require.NoError(t, err)
	require.Equal(t, "replacement-output:0", session.outputOutpoint)
	require.Equal(
		t, "replacement-output:0", daemon.lastClaimInput[0].Outpoint,
	)
	require.NotEqual(t, oldRecoveryID, session.claimRecoveryID)
}

// TestRefreshInputRolloverUsesExactOutpoint verifies the server claim is tied
// to the current first-leg outpoint after a same-script rollover, not its
// spent predecessor or Alice's second-leg claim.
func TestRefreshInputRolloverUsesExactOutpoint(t *testing.T) {
	t.Parallel()

	client, server, daemon, receiver, req := newRefreshHarness(t, 10)
	session, err := client.StartRefreshSwap(t.Context(), req)
	require.NoError(t, err)
	err = session.runUntil(t.Context(), RefreshStateInputVHTLCFunded)
	require.NoError(t, err)
	oldInputOutpoint := session.inputOutpoint
	receiver.notification = testRefreshOutputNotification(
		req, server, "claimable-output:0", 23,
	)
	err = session.runUntil(
		t.Context(), RefreshStateOutputHTLCEventAccepted,
	)
	require.NoError(t, err)
	daemon.liveByPkScript = map[string]*VTXOInfo{
		hex.EncodeToString(session.outputPkScript): {
			Outpoint:      "claimable-output:0",
			AmountSat:     int64(req.AmountSat),
			CreatedHeight: 95,
			BatchExpiry:   500,
		},
	}
	err = session.runUntil(t.Context(), RefreshStateOutputClaimed)
	require.NoError(t, err)

	inputScriptKey := hex.EncodeToString(session.inputPkScript)
	daemon.liveByPkScript[inputScriptKey] = &VTXOInfo{
		Outpoint:  "replacement-input:0",
		AmountSat: int64(req.AmountSat),
	}
	otherPreimage, err := NewPreimage()
	require.NoError(t, err)
	oldInput := VTXOInfo{
		Outpoint:    oldInputOutpoint,
		AmountSat:   int64(req.AmountSat),
		PkScript:    append([]byte(nil), session.inputPkScript...),
		SpentByTxID: "input-rollover-spend",
		FinalCheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, otherPreimage[:]),
		},
	}
	daemon.spentVTXOs = []VTXOInfo{oldInput}
	daemon.spentVTXO = &oldInput
	err = session.reconcileLiveInput(t.Context())
	require.NoError(t, err)
	require.Equal(t, "replacement-input:0", session.inputOutpoint)
	preimage, spent, err := client.waitForRefreshInputClaimObservation(
		t.Context(), req.PaymentHash, session.inputPkScript,
		session.inputOutpoint,
	)
	require.NoError(t, err)
	require.Nil(t, preimage)
	require.Nil(t, spent)

	serverClaim := VTXOInfo{
		Outpoint:    "replacement-input:0",
		AmountSat:   int64(req.AmountSat),
		PkScript:    append([]byte(nil), session.inputPkScript...),
		SpentByTxID: "server-replacement-claim",
		FinalCheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, req.Preimage[:]),
		},
	}
	daemon.spentVTXOs = append(daemon.spentVTXOs, serverClaim)
	daemon.spentVTXO = &serverClaim
	err = session.runUntil(t.Context(), RefreshStateCompleted)
	require.NoError(t, err)
	require.Equal(t, "server-replacement-claim", session.inputClaimTxID)
}

// TestRefreshInputClaimObservationIsRoleScoped verifies Alice's second-leg
// preimage cannot masquerade as the server spending the exact first leg.
func TestRefreshInputClaimObservationIsRoleScoped(t *testing.T) {
	t.Parallel()

	client, _, daemon, _, req := newRefreshHarness(t, 10)
	inputScript := []byte{0x51, 0x20, 2}
	daemon.liveVTXOs = []VTXOInfo{{
		Outpoint: "claimed-output:0",
		PkScript: []byte{
			0x51,
			0x20,
			3,
		},
		FinalCheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, req.Preimage[:]),
		},
	}}

	preimage, spent, err := client.waitForRefreshInputClaimObservation(
		t.Context(), req.PaymentHash, inputScript, "input-vhtlc:0",
	)
	require.NoError(t, err)
	require.Nil(t, preimage)
	require.Nil(t, spent)

	daemon.spentVTXOs = []VTXOInfo{{
		Outpoint:    "input-vhtlc:0",
		PkScript:    inputScript,
		SpentByTxID: "server-claim-tx",
		FinalCheckpointPSBTs: [][]byte{
			testCheckpointPSBTWithPreimage(t, req.Preimage[:]),
		},
	}}
	preimage, spent, err = client.waitForRefreshInputClaimObservation(
		t.Context(), req.PaymentHash, inputScript, "input-vhtlc:0",
	)
	require.NoError(t, err)
	require.NotNil(t, preimage)
	require.Equal(t, req.Preimage, *preimage)
	require.Equal(t, "server-claim-tx", spent.SpentByTxID)
}

// TestRefreshActiveRecoveriesSuppressCooperativeSpends verifies a daemon-owned
// unroll and an SDK custom-input spend are never submitted concurrently.
func TestRefreshActiveRecoveriesSuppressCooperativeSpends(t *testing.T) {
	t.Parallel()

	t.Run("output claim", func(t *testing.T) {
		client, _, daemon, _, req := newRefreshHarness(t, 10)
		daemon.statusResp = activeRefreshRecovery("claim-recovery")
		session := &RefreshSession{
			client:      client,
			paymentHash: req.PaymentHash,
			state:       RefreshStateClaimInitiated,
			outputPkScript: []byte{
				0x51,
				0x20,
				4,
			},
			claimRecoveryID:      "claim-recovery",
			claimIntentInProcess: true,
		}

		err := session.claimOutputVHTLC(t.Context())
		require.NoError(t, err)
		require.Zero(t, daemon.sendCustomCalls)
		require.Equal(t, 1, daemon.statusCalls)
	})

	t.Run("input refund", func(t *testing.T) {
		client, _, daemon, _, req := newRefreshHarness(t, 10)
		daemon.statusResp = activeRefreshRecovery("refund-recovery")
		session := &RefreshSession{
			client:      client,
			paymentHash: req.PaymentHash,
			state:       RefreshStateRefundInitiated,
			inputPkScript: []byte{
				0x51,
				0x20,
				5,
			},
			inputOutpoint: "input-vhtlc:0",
			refundReceiveScript: []byte{
				0x51,
				0x20,
				6,
			},
			refundRecoveryID: "refund-recovery",
		}

		err := session.completeRefund(t.Context())
		require.NoError(t, err)
		require.Zero(t, daemon.sendCustomCalls)
		require.Equal(t, 1, daemon.statusCalls)
	})
}

// activeRefreshRecovery returns a daemon status after unilateral recovery has
// taken ownership of one refresh leg.
func activeRefreshRecovery(
	recoveryID string) *waverpc.GetVHTLCRecoveryStatusResponse {

	return &waverpc.GetVHTLCRecoveryStatusResponse{
		Found: true,
		Status: &waverpc.VHTLCRecoveryStatus{
			RecoveryId: recoveryID,
			State:      recoveryStateUnrollStarted,
		},
	}
}

// TestValidateRefreshOutputAge covers equality acceptance and the fail-closed
// metadata rules used before the client reveals its preimage.
func TestValidateRefreshOutputAge(t *testing.T) {
	t.Parallel()

	vtxo := &VTXOInfo{
		CreatedHeight: 95,
		BatchExpiry:   120,
	}
	require.NoError(t, validateRefreshOutputAge(vtxo, 100, 5))
	require.Error(t, validateRefreshOutputAge(vtxo, 100, 4))
	require.Error(
		t,
		validateRefreshOutputAge(
			&VTXOInfo{
				BatchExpiry: 120,
			},
			100,
			5,
		),
	)
	require.Error(
		t,
		validateRefreshOutputAge(
			&VTXOInfo{
				CreatedHeight: 101,
				BatchExpiry:   120,
			},
			100,
			5,
		),
	)
	require.Error(
		t,
		validateRefreshOutputAge(
			&VTXOInfo{
				CreatedHeight: 95,
				BatchExpiry:   100,
			},
			100,
			5,
		),
	)
	require.Error(t, validateRefreshOutputAge(nil, 100, 0))
}
