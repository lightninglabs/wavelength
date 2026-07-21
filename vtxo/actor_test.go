package vtxo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/internal/testutils"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type noopChainSourceRef struct{}

func (n noopChainSourceRef) ID() string { return "noop-chainsource" }

func (n noopChainSourceRef) Tell(_ context.Context,
	_ chainsource.ChainSourceMsg) error {

	return nil
}

func (n noopChainSourceRef) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	switch msg.(type) {
	case *chainsource.SubscribeBlocksRequest:
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.SubscribeBlocksResponse{},
			),
		)

	case *chainsource.UnsubscribeBlocksRequest:
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.UnsubscribeBlocksResponse{},
			),
		)

	default:
		promise.Complete(
			fn.Err[chainsource.ChainSourceResp](
				errors.New("unexpected chainsource message"),
			),
		)
	}

	return promise.Future()
}

type cancelSensitiveChainResolverRef struct {
	seen chan error
}

func (c *cancelSensitiveChainResolverRef) ID() string {
	return "cancel-sensitive-chain-resolver"
}

func (c *cancelSensitiveChainResolverRef) Tell(ctx context.Context,
	_ ExpiringNotification) error {

	err := ctx.Err()
	c.seen <- err

	return err
}

var _ actor.ActorRef[
	chainsource.ChainSourceMsg,
	chainsource.ChainSourceResp,
] = noopChainSourceRef{}

// TestProcessOutboxForfeitSignature verifies that ForfeitSignatureSubmission
// messages are relayed through the manager to the round actor.
func TestProcessOutboxForfeitSignature(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	manager := newMockManagerRef(t)
	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
			Manager:     manager,
		},
		state: &LiveState{
			VTXO: vtxo,
		},
		env: h.env,
	}

	forfeitTx := wire.NewMsgTx(2)
	forfeitTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: vtxo.Outpoint,
	})
	forfeitTx.AddTxOut(&wire.TxOut{
		Value:    int64(vtxo.Amount),
		PkScript: []byte{0x51, 0x20},
	})

	testSig := testutils.TestSchnorrSignature(t, "forfeit")
	outbox := []VTXOOutMsg{
		&ForfeitSignatureSubmission{
			VTXOOutpoint: vtxo.Outpoint,
			RoundID:      "round-123",
			ForfeitTx:    forfeitTx,
			Signature:    testSig,
		},
	}

	require.NoError(t, actor.processOutbox(h.ctx, outbox))

	msgs := manager.getMessages()
	require.Len(t, msgs, 1)

	relayMsg, ok := msgs[0].(*RelayToRoundMsg)
	require.True(t, ok, "expected RelayToRoundMsg, got %T", msgs[0])

	resp, ok := relayMsg.Payload.(*round.ForfeitSignatureResponse)
	require.True(
		t, ok, "expected ForfeitSignatureResponse, got %T",
		relayMsg.Payload,
	)
	require.Equal(t, vtxo.Outpoint, resp.VTXOOutpoint)
	require.Equal(t, "round-123", resp.RoundID)
	require.NotNil(t, resp.ForfeitTx)
	require.Equal(t, testSig, resp.Signature)
}

// TestProcessOutboxMarkForfeiting verifies that VTXOStatusUpdate with
// forfeiting status and a forfeit tx calls MarkForfeiting on the store.
func TestProcessOutboxMarkForfeiting(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	forfeitTx := wire.NewMsgTx(2)
	forfeitTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: vtxo.Outpoint,
	})

	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			ChainParams: &chaincfg.RegressionNetParams,
		},
		state: &LiveState{
			VTXO: vtxo,
		},
		env: h.env,
	}

	h.store.On(
		"MarkForfeiting", h.ctx, vtxo.Outpoint, "round-456", forfeitTx,
	).Return(nil)

	outbox := []VTXOOutMsg{
		&VTXOStatusUpdate{
			Outpoint:  vtxo.Outpoint,
			NewStatus: VTXOStatusForfeiting,
			RoundID:   "round-456",
			ForfeitTx: forfeitTx,
		},
	}

	require.NoError(t, actor.processOutbox(h.ctx, outbox))

	h.store.AssertCalled(
		t, "MarkForfeiting", h.ctx, vtxo.Outpoint, "round-456",
		forfeitTx,
	)
}

// TestProcessOutboxStatusUpdate verifies that VTXOStatusUpdate without a
// forfeit tx calls UpdateVTXOStatus on the store.
func TestProcessOutboxStatusUpdate(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			ChainParams: &chaincfg.RegressionNetParams,
		},
		state: &LiveState{
			VTXO: vtxo,
		},
		env: h.env,
	}

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil)

	outbox := []VTXOOutMsg{
		&VTXOStatusUpdate{
			Outpoint:  vtxo.Outpoint,
			NewStatus: VTXOStatusPendingForfeit,
		},
	}

	require.NoError(t, actor.processOutbox(h.ctx, outbox))

	h.store.AssertCalled(
		t, "UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusPendingForfeit,
	)
}

// TestReceiveStatusUpdateFailurePreservesStateForRetry verifies that a failed
// durable VTXO status write prevents the actor from publishing the in-memory
// state transition. A later retry should re-drive the same SpendingState event
// and complete normally once the store accepts the write.
func TestReceiveStatusUpdateFailurePreservesStateForRetry(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			ChainSource: noopChainSourceRef{},
			ChainParams: &chaincfg.RegressionNetParams,
		},
		state: &SpendingState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		},
		env: h.env,
	}

	// Completing a spend leaves SpendingState, so persistence routes
	// through the reservation-releasing store method.
	h.store.On(
		"UpdateVTXOStatusReleasingReservation", h.ctx, vtxo.Outpoint,
		VTXOStatusSpent,
	).Return(errors.New("db down")).Once()

	result := actor.Receive(h.ctx, &SpendCompletedEvent{})
	_, err := result.Unpack()
	require.Error(t, err)
	require.ErrorContains(t, err, "persist vtxo status")
	_, ok := actor.state.(*SpendingState)
	require.True(
		t, ok, "expected retryable SpendingState, got %T", actor.state,
	)

	h.store.On(
		"UpdateVTXOStatusReleasingReservation", h.ctx, vtxo.Outpoint,
		VTXOStatusSpent,
	).Return(nil).Once()

	result = actor.Receive(h.ctx, &SpendCompletedEvent{})
	_, err = result.Unpack()
	require.NoError(t, err)
	_, ok = actor.state.(*SpentState)
	require.True(t, ok, "expected SpentState, got %T", actor.state)

	h.store.AssertExpectations(t)
}

// TestSpendReleasedPersistsLiveAndReservationRelease proves a known-safe OOR
// setup failure leaves Spending through the actor and selects the atomic store
// path that also deletes the durable preparation reservation.
func TestSpendReleasedPersistsLiveAndReservationRelease(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			ChainSource: noopChainSourceRef{},
			ChainParams: &chaincfg.RegressionNetParams,
		},
		state: &SpendingState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		},
		env: h.env,
	}

	h.store.On(
		"UpdateVTXOStatusReleasingReservation", h.ctx, vtxo.Outpoint,
		VTXOStatusLive,
	).Return(nil).Once()

	result := actor.Receive(h.ctx, &SpendReleasedEvent{})
	_, err := result.Unpack()
	require.NoError(t, err)
	_, ok := actor.state.(*LiveState)
	require.True(t, ok, "expected LiveState, got %T", actor.state)
	h.store.AssertExpectations(t)
}

// TestProcessOutboxForfeitRequest verifies that ForfeitRequest messages are
// relayed through the manager as a RelayToRoundMsg containing a
// RefreshVTXORequest with the correct fields.
func TestProcessOutboxForfeitRequest(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	manager := newMockManagerRef(t)
	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
			Manager:     manager,
		},
		state: &LiveState{
			VTXO: vtxo,
		},
		env: h.env,
	}

	outbox := []VTXOOutMsg{
		&ForfeitRequest{
			VTXOOutpoint: vtxo.Outpoint,
		},
	}

	require.NoError(t, actor.processOutbox(h.ctx, outbox))

	msgs := manager.getMessages()
	require.Len(t, msgs, 1)

	relayMsg, ok := msgs[0].(*RelayToRoundMsg)
	require.True(t, ok, "expected RelayToRoundMsg, got %T", msgs[0])

	refreshReq, ok := relayMsg.Payload.(*round.RefreshVTXORequest)
	require.True(
		t, ok, "expected RefreshVTXORequest, got %T", relayMsg.Payload,
	)
	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		vtxo.ClientKey.PubKey, vtxo.OperatorKey, vtxo.RelativeExpiry,
	)
	require.NoError(t, err)

	require.Equal(t, vtxo.Outpoint, refreshReq.VTXOOutpoint)
	require.Equal(t, int64(vtxo.Amount), refreshReq.Amount)
	require.Equal(t, policyTemplate, refreshReq.PolicyTemplate)
	require.Equal(t, vtxo.ClientKey, refreshReq.SigningKey)

	// With no RefreshFeeQuoter configured, OperatorFee stays zero
	// (pre-#269 behavior). Under a non-zero fee schedule the server's
	// validateOperatorFee will reject the resulting round, but that
	// is the caller-of-NewManager's responsibility to wire.
	require.Equal(
		t, int64(0), refreshReq.OperatorFee,
		"unconfigured quoter emits zero OperatorFee",
	)
}

// TestProcessOutboxForfeitRequestQuotesFee verifies that when a
// RefreshFeeQuoter is configured on VTXOActorConfig, the auto-refresh
// emission threads the quoted fee onto the relayed RefreshVTXORequest
// so the server-side validateOperatorFee (#269) sees a non-zero
// implicit operator fee on the refresh input. Drives the full
// Receive path so the FSM's LastCheckedHeight stamp on the outbox
// ForfeitRequest is exercised end-to-end — a previous version read
// the height from a.state inside processOutbox, which by that point
// had already transitioned out of LiveState, causing remainingBlocks
// to silently collapse to zero.
func TestProcessOutboxForfeitRequestQuotesFee(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	// Pick a block height in the refresh window: batch expires
	// 200 blocks out (above criticalThreshold for testExitDelay=144
	// and TreeDepth=2 → safeExit=156, critical=156, refresh=
	// max(144, 156+72)=228), so 156 < 200 <= 228 lands in
	// ExpiryStatusNeedsRefresh.
	currentHeight := vtxo.BatchExpiry - 200
	expectedRemaining := uint32(200)

	// Record the amount + remaining-blocks the quoter was called
	// with so we can assert them independently of its return.
	var (
		gotAmount    btcutil.Amount
		gotRemaining uint32
	)
	const quotedFee = btcutil.Amount(1_234)
	quoter := func(_ context.Context, amt btcutil.Amount,
		remaining uint32) btcutil.Amount {

		gotAmount = amt
		gotRemaining = remaining

		return quotedFee
	}

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil)

	manager := newMockManagerRef(t)
	a := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:             vtxo,
			Store:            h.store,
			Wallet:           h.wallet,
			ChainParams:      &chaincfg.RegressionNetParams,
			Manager:          manager,
			RefreshFeeQuoter: quoter,
		},
		state: &LiveState{
			VTXO: vtxo,
		},
		env: h.env,
	}

	// Drive the FSM through Receive so the state assignment and
	// outbox dispatch run in the production order. The regression
	// we are guarding against is that a.state is reassigned to
	// PendingForfeitState before processOutbox runs; if the quoter
	// reads a.state.(*LiveState), remainingBlocks collapses to
	// zero. With the fix the height is stamped on the ForfeitRequest
	// outbox message by the FSM transition itself.
	epoch := h.newBlockEpochEvent(currentHeight)
	result := a.Receive(h.ctx, epoch)
	_, err := result.Unpack()
	require.NoError(t, err)

	// Confirm the transition actually advanced out of LiveState;
	// otherwise the outbox would have emitted nothing and the
	// assertions below would falsely pass.
	_, inPending := a.state.(*PendingForfeitState)
	require.True(
		t, inPending, "Receive should have transitioned to "+
			"PendingForfeitState, got %T", a.state,
	)

	msgs := manager.getMessages()
	require.Len(t, msgs, 1)

	relayMsg, ok := msgs[0].(*RelayToRoundMsg)
	require.True(t, ok, "expected RelayToRoundMsg, got %T", msgs[0])

	refreshReq, ok := relayMsg.Payload.(*round.RefreshVTXORequest)
	require.True(
		t, ok, "expected RefreshVTXORequest, got %T", relayMsg.Payload,
	)

	require.Equal(
		t, int64(quotedFee), refreshReq.OperatorFee,
		"OperatorFee carries the quoter's return value",
	)
	require.Equal(t, vtxo.Amount, gotAmount,
		"quoter sees the VTXO amount")
	require.Equal(
		t, expectedRemaining, gotRemaining, "quoter sees "+
			"BatchExpiry - LastCheckedHeight from the "+
			"FSM-stamped outbox message, not a re-read of a.state",
	)

	// Forfeit input's Amount stays at the full VTXO value so the
	// implicit operator fee = sum(forfeits) - sum(new_vtxos) =
	// OperatorFee on this input.
	require.Equal(
		t, int64(vtxo.Amount), refreshReq.Amount,
		"forfeit input Amount is unchanged by the quote",
	)
}

// TestAutoRefreshTermsLookupFailureRetries verifies a transient GetInfo
// failure leaves the VTXO live and retries on the next block without writing a
// PendingForfeit reservation.
func TestAutoRefreshTermsLookupFailureRetries(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	currentHeight := desc.BatchExpiry - 200
	fetchErr := errors.New("operator unavailable")

	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil).Once()

	var attempts int
	manager := newMockManagerRef(t)
	actor := newRefreshTestActor(
		h, desc, manager,
		func(_ context.Context) (*btcec.PublicKey, error) {
			attempts++
			if attempts == 1 {
				return nil, fetchErr
			}

			return desc.OperatorKey, nil
		},
	)

	result := actor.Receive(
		h.ctx, h.newBlockEpochEvent(currentHeight),
	)
	_, err := result.Unpack()
	require.ErrorIs(t, err, fetchErr)
	require.IsType(t, &LiveState{}, actor.state)
	require.Empty(t, manager.getMessages())

	result = actor.Receive(
		h.ctx, h.newBlockEpochEvent(currentHeight+1),
	)
	_, err = result.Unpack()
	require.NoError(t, err)
	require.IsType(t, &PendingForfeitState{}, actor.state)
	require.Len(t, manager.getMessages(), 1)
	require.Equal(t, 2, attempts)
	h.store.AssertExpectations(t)
}

// TestAutoRefreshRechecksFreeWindow verifies the actor confirms the current
// operator window before reserving an automatic refresh input.
func TestAutoRefreshRechecksFreeWindow(t *testing.T) {
	t.Parallel()

	t.Run("later safe boundary defers", func(t *testing.T) {
		t.Parallel()

		h := newVTXOTestHarness(t)
		desc := h.newTestDescriptor()
		desc.RelativeExpiry = 24

		cachedWindow := uint32(120)
		freshWindow := uint32(110)
		cfg := DefaultExpiryConfig()
		cfg.FreeRefreshWindow = func() uint32 {
			return cachedWindow
		}
		h.withExpiryConfig(cfg)

		h.store.On(
			"UpdateVTXOStatus", h.ctx, desc.Outpoint,
			VTXOStatusPendingForfeit,
		).Return(nil).Once()

		var fetches int
		manager := newMockManagerRef(t)
		actor := newRefreshTestActor(
			h, desc, manager,
			func(_ context.Context) (*btcec.PublicKey, error) {
				fetches++
				cachedWindow = freshWindow

				return desc.OperatorKey, nil
			},
		)

		result := actor.Receive(
			h.ctx, h.newBlockEpochEvent(
				desc.BatchExpiry-int32(120),
			),
		)
		_, err := result.Unpack()
		require.NoError(t, err)
		require.IsType(t, &LiveState{}, actor.state)
		require.Empty(t, manager.getMessages())
		require.Equal(t, 1, fetches)

		result = actor.Receive(
			h.ctx, h.newBlockEpochEvent(
				desc.BatchExpiry-int32(freshWindow),
			),
		)
		_, err = result.Unpack()
		require.NoError(t, err)
		require.IsType(t, &PendingForfeitState{}, actor.state)
		require.Len(t, manager.getMessages(), 1)
		require.Equal(t, 2, fetches)
		h.store.AssertExpectations(t)
	})

	t.Run("disabled window preserves paid refresh", func(t *testing.T) {
		t.Parallel()

		h := newVTXOTestHarness(t)
		desc := h.newTestDescriptor()
		desc.RelativeExpiry = 24

		cachedWindow := uint32(120)
		cfg := DefaultExpiryConfig()
		cfg.FreeRefreshWindow = func() uint32 {
			return cachedWindow
		}
		h.withExpiryConfig(cfg)

		h.store.On(
			"UpdateVTXOStatus", h.ctx, desc.Outpoint,
			VTXOStatusPendingForfeit,
		).Return(nil).Once()

		var fetches int
		manager := newMockManagerRef(t)
		actor := newRefreshTestActor(
			h, desc, manager,
			func(_ context.Context) (*btcec.PublicKey, error) {
				fetches++
				cachedWindow = 0

				return desc.OperatorKey, nil
			},
		)

		result := actor.Receive(
			h.ctx, h.newBlockEpochEvent(
				desc.BatchExpiry-int32(120),
			),
		)
		_, err := result.Unpack()
		require.NoError(t, err)
		require.IsType(t, &PendingForfeitState{}, actor.state)
		require.Len(t, manager.getMessages(), 1)
		require.Equal(t, 1, fetches)
		h.store.AssertExpectations(t)
	})
}

// TestProcessOutboxTerminatedNotification verifies that
// VTXOTerminatedNotification messages are routed to the manager.
func TestProcessOutboxTerminatedNotification(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	manager := newMockManagerRef(t)
	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			ChainParams: &chaincfg.RegressionNetParams,
			Manager:     manager,
		},
		state: &ForfeitedState{
			VTXO: vtxo,
		},
		env: h.env,
	}

	outbox := []VTXOOutMsg{
		&VTXOTerminatedNotification{
			VTXOOutpoint: vtxo.Outpoint,
			FinalState:   "forfeited",
			Reason:       "forfeit confirmed",
		},
	}

	require.NoError(t, actor.processOutbox(h.ctx, outbox))

	msgs := manager.getMessages()
	require.Len(t, msgs, 1)

	termMsg, ok := msgs[0].(*VTXOTerminatedMsg)
	require.True(t, ok, "expected VTXOTerminatedMsg, got %T", msgs[0])
	require.Equal(t, vtxo.Outpoint, termMsg.Outpoint)
	require.Equal(t, "forfeited", termMsg.FinalState)
	require.Equal(t, "forfeit confirmed", termMsg.Reason)
}

// TestProcessOutboxExpiringNotification verifies that ExpiringNotification
// messages are routed directly to the chain resolver.
func TestProcessOutboxExpiringNotification(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	chainResolver := newMockChainResolverRef(t)
	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:          vtxo,
			Store:         h.store,
			ChainParams:   &chaincfg.RegressionNetParams,
			ChainResolver: chainResolver,
		},
		state: &UnilateralExitState{
			VTXO: vtxo,
		},
		env: h.env,
	}

	outbox := []VTXOOutMsg{
		&ExpiringNotification{
			VTXO:            vtxo,
			BlocksRemaining: 10,
			Reason:          "approaching expiry",
		},
	}

	require.NoError(t, actor.processOutbox(h.ctx, outbox))

	msgs := chainResolver.getMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, vtxo, msgs[0].VTXO)
	require.Equal(t, int32(10), msgs[0].BlocksRemaining)
}

// TestProcessOutboxExpiringNotificationDetachesCallerCancel verifies that the
// unilateral-exit handoff survives cancellation of the triggering actor call.
func TestProcessOutboxExpiringNotificationDetachesCallerCancel(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	chainResolver := &cancelSensitiveChainResolverRef{
		seen: make(chan error, 1),
	}
	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:          vtxo,
			Store:         h.store,
			ChainParams:   &chaincfg.RegressionNetParams,
			ChainResolver: chainResolver,
		},
		state: &UnilateralExitState{
			VTXO: vtxo,
		},
		env: h.env,
	}

	ctx, cancel := context.WithCancel(h.ctx)
	cancel()

	outbox := []VTXOOutMsg{
		&ExpiringNotification{
			VTXO:            vtxo,
			BlocksRemaining: 10,
			Reason:          "manual unroll",
		},
	}

	require.NoError(t, actor.processOutbox(ctx, outbox))

	select {
	case err := <-chainResolver.seen:
		require.NoError(t, err)

	case <-time.After(time.Second):
		t.Fatal("timed out waiting for chain resolver notification")
	}
}

// TestProcessOutboxExpiringNotificationNoLedgerEmission is a regression
// test for the ExitCostMsg poison-pill bug. The ledger handler rejects
// ExitCostMsg with ExitCostSat=0, and the VTXO actor cannot determine
// the miner fee. Unroll emits the ledger event after the final sweep
// confirms.
// Emitting anything at the ExpiringNotification dispatch site would
// create a permanent durable-mailbox retry loop. This test wires a
// capturing ledger sink and verifies no message is ever Tell'd.
func TestProcessOutboxExpiringNotificationNoLedgerEmission(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	chainResolver := newMockChainResolverRef(t)
	ledgerSink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)

	vtxoActor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:          vtxo,
			Store:         h.store,
			ChainParams:   &chaincfg.RegressionNetParams,
			ChainResolver: chainResolver,
			LedgerSink:    fn.Some[ledger.Sink](ledgerSink),
		},
		state: &UnilateralExitState{
			VTXO: vtxo,
		},
		env: h.env,
	}

	outbox := []VTXOOutMsg{
		&ExpiringNotification{
			VTXO:            vtxo,
			BlocksRemaining: 5,
			Reason:          "approaching expiry",
		},
	}

	require.NoError(t, vtxoActor.processOutbox(h.ctx, outbox))

	// Chain resolver still receives the notification.
	require.Len(t, chainResolver.getMessages(), 1)

	// The ledger sink must be empty -- a zero-fee ExitCostMsg
	// would be rejected by the handler and replay forever.
	select {
	case msg := <-ledgerSink.Messages():
		t.Fatalf("unexpected ledger emission: %T", msg)

	default:
	}
}

// TestActorRecoveryFromForfeiting verifies that statusToState correctly
// recovers a ForfeitingState with the persisted forfeit tx.
func TestActorRecoveryFromForfeiting(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Status = VTXOStatusForfeiting
	vtxo.RoundID = "round-789"

	forfeitTx := wire.NewMsgTx(2)
	forfeitTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: vtxo.Outpoint,
	})

	h.store.On("GetForfeitTx", mock.Anything, vtxo.Outpoint).Return(
		forfeitTx, nil,
	)

	state := statusToState(h.ctx, vtxo, h.store, btclog.Disabled)

	forfeitingState, ok := state.(*ForfeitingState)
	require.True(t, ok, "expected ForfeitingState, got %T", state)
	require.Equal(t, vtxo, forfeitingState.VTXO)
	require.Equal(t, "round-789", forfeitingState.NewRoundID)
	require.NotNil(t, forfeitingState.ForfeitTx)
	require.Equal(t, forfeitTx.TxHash(), forfeitingState.ForfeitTx.TxHash())
}

// TestActorRecoveryFromForfeitingNoTx verifies statusToState handles the case
// where no forfeit tx is persisted (crash before MarkForfeiting completed).
func TestActorRecoveryFromForfeitingNoTx(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Status = VTXOStatusForfeiting
	vtxo.RoundID = "round-999"

	h.store.On("GetForfeitTx", mock.Anything, vtxo.Outpoint).Return(
		nil, nil,
	)

	state := statusToState(h.ctx, vtxo, h.store, btclog.Disabled)

	forfeitingState, ok := state.(*ForfeitingState)
	require.True(t, ok, "expected ForfeitingState, got %T", state)
	require.Equal(t, vtxo, forfeitingState.VTXO)
	require.Nil(t, forfeitingState.ForfeitTx)
}

// TestStatusToStateLive verifies statusToState returns LiveState for live
// VTXOs.
func TestStatusToStateLive(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Status = VTXOStatusLive
	vtxo.CreatedHeight = 500

	state := statusToState(h.ctx, vtxo, h.store, btclog.Disabled)

	liveState, ok := state.(*LiveState)
	require.True(t, ok, "expected LiveState, got %T", state)
	require.Equal(t, vtxo, liveState.VTXO)
	require.Equal(t, int32(500), liveState.LastCheckedHeight)
}

// TestStatusToStateForfeited verifies statusToState returns ForfeitedState for
// forfeited VTXOs.
func TestStatusToStateForfeited(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Status = VTXOStatusForfeited
	vtxo.RoundID = "round-final"

	state := statusToState(h.ctx, vtxo, h.store, btclog.Disabled)

	forfeitedState, ok := state.(*ForfeitedState)
	require.True(t, ok, "expected ForfeitedState, got %T", state)
	require.Equal(t, vtxo, forfeitedState.VTXO)
	require.Equal(t, "round-final", forfeitedState.NewRoundID)
}

// TestStatusToStateSpending verifies statusToState returns SpendingState for
// VTXOs persisted as VTXOStatusSpending. This validates that spend claims
// survive restarts without being silently downgraded to LiveState.
func TestStatusToStateSpending(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Status = VTXOStatusSpending
	vtxo.CreatedHeight = 750

	state := statusToState(h.ctx, vtxo, h.store, btclog.Disabled)

	spendingState, ok := state.(*SpendingState)
	require.True(t, ok, "expected SpendingState, got %T", state)
	require.Equal(t, vtxo, spendingState.VTXO)
	require.Equal(t, int32(750), spendingState.LastCheckedHeight)
}

// TestStatusToStateSpent verifies statusToState returns SpentState for VTXOs
// persisted as VTXOStatusSpent. SpentState is terminal so the actor should
// be cleaned up after recovery.
func TestStatusToStateSpent(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Status = VTXOStatusSpent

	state := statusToState(h.ctx, vtxo, h.store, btclog.Disabled)

	spentState, ok := state.(*SpentState)
	require.True(t, ok, "expected SpentState, got %T", state)
	require.Equal(t, vtxo, spentState.VTXO)
	require.True(t, spentState.IsTerminal())
}

// TestStatusToStatePendingForfeit verifies statusToState returns
// PendingForfeitState for VTXOs persisted as VTXOStatusPendingForfeit.
// This validates that cooperative-claim reservations survive restarts.
func TestStatusToStatePendingForfeit(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Status = VTXOStatusPendingForfeit

	state := statusToState(h.ctx, vtxo, h.store, btclog.Disabled)

	pendingState, ok := state.(*PendingForfeitState)
	require.True(t, ok, "expected PendingForfeitState, got %T", state)
	require.Equal(t, vtxo, pendingState.VTXO)
}

// TestStatusToStateUnilateralExit verifies statusToState returns
// UnilateralExitState for VTXOs persisted as VTXOStatusUnilateralExit.
func TestStatusToStateUnilateralExit(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Status = VTXOStatusUnilateralExit

	state := statusToState(h.ctx, vtxo, h.store, btclog.Disabled)

	exitState, ok := state.(*UnilateralExitState)
	require.True(t, ok,
		"expected UnilateralExitState, got %T", state)
	require.Equal(t, vtxo, exitState.VTXO)
	require.Equal(t, "recovered from storage", exitState.Reason)
}

// TestStatusToStateFailed verifies statusToState returns FailedState for
// VTXOs persisted as VTXOStatusFailed.
func TestStatusToStateFailed(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Status = VTXOStatusFailed

	state := statusToState(h.ctx, vtxo, h.store, btclog.Disabled)

	failedState, ok := state.(*FailedState)
	require.True(t, ok, "expected FailedState, got %T", state)
	require.Equal(t, vtxo, failedState.VTXO)
	require.Equal(t, "recovered from storage", failedState.Reason)
}

// TestManagerGetActiveVTXOCount verifies the Manager returns the correct count
// of active VTXO actors when queried via GetActiveVTXOCountRequest.
func TestManagerGetActiveVTXOCount(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Create manager with empty actors map.
	manager := &Manager{
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	// Test empty state returns zero.
	result := manager.Receive(ctx, &GetActiveVTXOCountRequest{})
	resp, err := result.Unpack()
	require.NoError(t, err)

	countResp, ok := resp.(*GetActiveVTXOCountResponse)
	require.True(t, ok, "expected GetActiveVTXOCountResponse, got %T", resp)
	require.Equal(t, 0, countResp.Count)

	// Add some fake entries to simulate active actors. We use nil refs
	// since we only care about the count.
	manager.actors[wire.OutPoint{Index: 0}] = nil
	manager.actors[wire.OutPoint{Index: 1}] = nil
	manager.actors[wire.OutPoint{Index: 2}] = nil

	// Test returns correct count.
	result = manager.Receive(ctx, &GetActiveVTXOCountRequest{})
	resp, err = result.Unpack()
	require.NoError(t, err)

	countResp, ok = resp.(*GetActiveVTXOCountResponse)
	require.True(t, ok, "expected GetActiveVTXOCountResponse, got %T", resp)
	require.Equal(t, 3, countResp.Count)
}

// TestManagerListLiveDescriptors verifies the manager returns a snapshot
// of the live descriptors recovered at Start. This is the path daemon-
// local subsystems (recipient fraud watcher) use to re-arm per-VTXO
// state on restart.
func TestManagerListLiveDescriptors(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Empty manager: response carries an empty descriptor slice.
	manager := &Manager{
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}
	result := manager.Receive(ctx, &ListLiveDescriptorsRequest{})
	resp, err := result.Unpack()
	require.NoError(t, err)
	listResp, ok := resp.(*ListLiveDescriptorsResponse)
	require.True(
		t, ok, "expected ListLiveDescriptorsResponse, got %T", resp,
	)
	require.Empty(t, listResp.Descriptors)

	// Populate liveDescriptors as Start would have.
	a := &Descriptor{Outpoint: wire.OutPoint{Hash: [32]byte{0xa}}}
	b := &Descriptor{Outpoint: wire.OutPoint{Hash: [32]byte{0xb}}}
	manager.liveDescriptors = []*Descriptor{a, b}

	result = manager.Receive(ctx, &ListLiveDescriptorsRequest{})
	resp, err = result.Unpack()
	require.NoError(t, err)
	listResp, ok = resp.(*ListLiveDescriptorsResponse)
	require.True(
		t, ok, "expected ListLiveDescriptorsResponse, got %T", resp,
	)
	require.Equal(t, []*Descriptor{a, b}, listResp.Descriptors)

	// Response is a defensive copy: mutating it must not affect the
	// manager's internal slice.
	listResp.Descriptors[0] = nil
	require.Equal(t, a, manager.liveDescriptors[0])
}

// TestManagerRelayToRound verifies the manager forwards RelayToRoundMsg
// payloads to the round actor. This is the liveness path: when a VTXO
// actor detects approaching expiry and emits a ForfeitRequest, the
// manager must relay it promptly without requiring wallet intervention.
func TestManagerRelayToRound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	roundActor := newMockRoundActorRef(t)
	mgr := &Manager{
		cfg: &ManagerConfig{
			RoundActor: roundActor,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	// Simulate the payload a VTXO actor would build when
	// ExpiryStatusNeedsRefresh is detected.
	refreshReq := &round.RefreshVTXORequest{
		VTXOOutpoint: wire.OutPoint{
			Index: 42,
		},
		Amount: 50000,
	}

	result := mgr.Receive(ctx, &RelayToRoundMsg{Payload: refreshReq})
	_, err := result.Unpack()
	require.NoError(t, err)

	msgs := roundActor.getMessages()
	require.Len(t, msgs, 1)

	relayed, ok := msgs[0].(*round.RefreshVTXORequest)
	require.True(
		t, ok, "expected RefreshVTXORequest, got %T", msgs[0],
	)
	require.Equal(t, wire.OutPoint{Index: 42}, relayed.VTXOOutpoint)
	require.Equal(t, int64(50000), relayed.Amount)
}

// TestManagerRelayForfeitSig verifies the manager forwards forfeit
// signature submissions to the round actor.
func TestManagerRelayForfeitSig(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	roundActor := newMockRoundActorRef(t)
	mgr := &Manager{
		cfg: &ManagerConfig{
			RoundActor: roundActor,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	forfeitResp := &round.ForfeitSignatureResponse{
		VTXOOutpoint: wire.OutPoint{
			Index: 7,
		},
		RoundID: "round-abc",
	}

	result := mgr.Receive(ctx, &RelayToRoundMsg{Payload: forfeitResp})
	_, err := result.Unpack()
	require.NoError(t, err)

	msgs := roundActor.getMessages()
	require.Len(t, msgs, 1)

	relayed, ok := msgs[0].(*round.ForfeitSignatureResponse)
	require.True(
		t, ok, "expected ForfeitSignatureResponse, got %T", msgs[0],
	)
	require.Equal(t, wire.OutPoint{Index: 7}, relayed.VTXOOutpoint)
	require.Equal(t, "round-abc", relayed.RoundID)
}
