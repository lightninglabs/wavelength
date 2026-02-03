package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestProcessOutboxForfeitSignature verifies that ForfeitSignatureSubmission
// messages in the outbox are routed to the round actor.
func TestProcessOutboxForfeitSignature(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	roundActor := newMockRoundActorRef(t)
	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
			RoundActor:  roundActor,
			Logger:      btclog.Disabled,
		},
		state: &LiveState{VTXO: vtxo},
		env:   h.env,
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

	actor.processOutbox(h.ctx, outbox)

	msgs := roundActor.getMessages()
	require.Len(t, msgs, 1)

	resp, ok := msgs[0].(*round.ForfeitSignatureResponse)
	require.True(
		t, ok, "expected ForfeitSignatureResponse, got %T", msgs[0],
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
			Logger:      btclog.Disabled,
		},
		state: &LiveState{VTXO: vtxo},
		env:   h.env,
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

	actor.processOutbox(h.ctx, outbox)

	h.store.AssertCalled(
		t, "MarkForfeiting", h.ctx, vtxo.Outpoint,
		"round-456", forfeitTx,
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
			Logger:      btclog.Disabled,
		},
		state: &LiveState{VTXO: vtxo},
		env:   h.env,
	}

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint, VTXOStatusRefreshRequested,
	).Return(nil)

	outbox := []VTXOOutMsg{
		&VTXOStatusUpdate{
			Outpoint:  vtxo.Outpoint,
			NewStatus: VTXOStatusRefreshRequested,
		},
	}

	actor.processOutbox(h.ctx, outbox)

	h.store.AssertCalled(
		t, "UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusRefreshRequested,
	)
}

// TestProcessOutboxForfeitRequest verifies that ForfeitRequest messages in the
// outbox are routed to the round actor with the correct fields populated. Since
// refresh is decoupled from VTXO creation, both a RefreshVTXORequest and a
// VTXORequestsReceived message should be sent.
func TestProcessOutboxForfeitRequest(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	roundActor := newMockRoundActorRef(t)
	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
			RoundActor:  roundActor,
			Logger:      btclog.Disabled,
		},
		state: &LiveState{VTXO: vtxo},
		env:   h.env,
	}

	outbox := []VTXOOutMsg{
		&ForfeitRequest{
			VTXOOutpoint: vtxo.Outpoint,
		},
	}

	actor.processOutbox(h.ctx, outbox)

	// Should send both a RefreshVTXORequest and a VTXORequestsReceived.
	msgs := roundActor.getMessages()
	require.Len(t, msgs, 2)

	// First message should be the refresh request.
	refreshReq, ok := msgs[0].(*round.RefreshVTXORequest)
	require.True(t, ok, "expected RefreshVTXORequest, got %T", msgs[0])
	require.Equal(t, vtxo.Outpoint, refreshReq.VTXOOutpoint)
	require.Equal(t, int64(vtxo.Amount), refreshReq.Amount)
	require.Equal(t, vtxo.ClientKey.PubKey, refreshReq.NewVTXOKey)
	require.Equal(t, vtxo.OperatorKey, refreshReq.OperatorKey)
	require.Equal(t, vtxo.RelativeExpiry, refreshReq.Expiry)

	// Second message should be the VTXO request for the new VTXO.
	vtxoReqMsg, ok := msgs[1].(*round.VTXORequestsReceived)
	require.True(t, ok, "expected VTXORequestsReceived, got %T", msgs[1])
	require.Len(t, vtxoReqMsg.Requests, 1)

	vtxoReq := vtxoReqMsg.Requests[0]
	require.Equal(t, vtxo.Amount, vtxoReq.Amount)
	require.Equal(t, vtxo.PkScript, vtxoReq.PkScript)
	require.Equal(t, vtxo.RelativeExpiry, vtxoReq.Expiry)
	require.Equal(t, vtxo.ClientKey.PubKey, vtxoReq.ClientKey)
	require.Equal(t, vtxo.OperatorKey, vtxoReq.OperatorKey)
	require.Equal(
		t, vtxo.ClientKey.KeyLocator, vtxoReq.SigningKey.KeyLocator,
	)
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
			Logger:      btclog.Disabled,
		},
		state: &ForfeitedState{VTXO: vtxo},
		env:   h.env,
	}

	outbox := []VTXOOutMsg{
		&VTXOTerminatedNotification{
			VTXOOutpoint: vtxo.Outpoint,
			FinalState:   "forfeited",
			Reason:       "forfeit confirmed",
		},
	}

	actor.processOutbox(h.ctx, outbox)

	msgs := manager.getMessages()
	require.Len(t, msgs, 1)

	termMsg, ok := msgs[0].(*VTXOTerminatedMsg)
	require.True(t, ok, "expected VTXOTerminatedMsg, got %T", msgs[0])
	require.Equal(t, vtxo.Outpoint, termMsg.Outpoint)
	require.Equal(t, "forfeited", termMsg.FinalState)
	require.Equal(t, "forfeit confirmed", termMsg.Reason)
}

// TestProcessOutboxExpiringNotification verifies that ExpiringNotification
// messages are routed to the chain resolver.
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
			Logger:        btclog.Disabled,
		},
		state: &ExpiringState{VTXO: vtxo},
		env:   h.env,
	}

	outbox := []VTXOOutMsg{
		&ExpiringNotification{
			VTXO:            vtxo,
			BlocksRemaining: 10,
			Reason:          "approaching expiry",
		},
	}

	actor.processOutbox(h.ctx, outbox)

	msgs := chainResolver.getMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, vtxo, msgs[0].VTXO)
	require.Equal(t, int32(10), msgs[0].BlocksRemaining)
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
