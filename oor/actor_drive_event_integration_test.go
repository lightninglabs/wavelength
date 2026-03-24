package oor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

const (
	resumeTestEventuallyTimeout = 4 * time.Second
	resumeTestEventuallyPoll    = 20 * time.Millisecond
)

type resumeIntegrationHandler struct {
	sawFinalize atomic.Bool
}

func (h *resumeIntegrationHandler) Handle(_ context.Context,
	_ SessionID, outbox OutboxEvent) ([]Event, error) {

	switch outbox.(type) {
	case *SendFinalizePackageRequest:
		h.sawFinalize.Store(true)
		return nil, nil

	default:
		return nil, fmt.Errorf("unexpected outbox event: %T", outbox)
	}
}

var _ OutboxHandler = (*resumeIntegrationHandler)(nil)

type mockVTXOManagerRef struct {
	mu       sync.Mutex
	messages []vtxo.ManagerMsg
}

func (m *mockVTXOManagerRef) ID() string {
	return "mock-vtxo-manager"
}

func (m *mockVTXOManagerRef) Tell(_ context.Context,
	msg vtxo.ManagerMsg) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, msg)

	return nil
}

func (m *mockVTXOManagerRef) sent() []vtxo.ManagerMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]vtxo.ManagerMsg(nil), m.messages...)
}

var _ interface {
	ID() string
	Tell(context.Context, vtxo.ManagerMsg) error
} = (*mockVTXOManagerRef)(nil)

type mockOORDurableRef struct {
	ch chan OORDurableMsg
}

// ID returns the stable ref identifier for mockOORDurableRef.
func (m *mockOORDurableRef) ID() string {
	return "mock-oor-self"
}

// Tell records an OOR durable message for assertions.
func (m *mockOORDurableRef) Tell(_ context.Context, msg OORDurableMsg) error {
	m.ch <- msg
	return nil
}

var _ interface {
	ID() string
	Tell(context.Context, OORDurableMsg) error
} = (*mockOORDurableRef)(nil)

// buildIncomingResolveResponse creates an indexer response carrying the full
// Ark/checkpoint package for a lightweight incoming-transfer hint.
func buildIncomingResolveResponse(t *testing.T) (
	*arkrpc.ListOORRecipientEventsByScriptResponse,
	SessionID, []byte, uint64,
) {

	t.Helper()

	arkPSBT, finalCheckpoints, recipients, _, _, _ :=
		buildTestIncomingMaterialization(t)

	arkRaw, err := psbtutil.Serialize(arkPSBT)
	require.NoError(t, err)

	checkpointRaws := make([][]byte, 0, len(finalCheckpoints))
	for _, checkpoint := range finalCheckpoints {
		checkpointRaw, checkpointErr := psbtutil.Serialize(
			checkpoint,
		)
		require.NoError(t, checkpointErr)

		checkpointRaws = append(checkpointRaws, checkpointRaw)
	}

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	recipient := recipients[0]

	return &arkrpc.ListOORRecipientEventsByScriptResponse{
		Events: []*arkrpc.OORRecipientEvent{
			{
				RecipientPkScript: recipient.PkScript,
				EventId:           7,
				SessionId:         sessionID[:],
				OutputIndex:       recipient.OutputIndex,
				Value:             uint64(recipient.Value),
				ArkPsbt:           arkRaw,
				CheckpointPsbts:   checkpointRaws,
			},
		},
		NextCursor: 8,
	}, sessionID, recipient.PkScript, 7
}

func testRetryTransferInputs(t *testing.T) []TransferInput {
	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return []TransferInput{
		newTestTransferInput(
			t,
			clientKey,
			operatorKey.PubKey(),
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			btcutil.Amount(10_000),
		),
	}
}

// TestOORClientActorResumeSessionDurablePath verifies ResumeSessionRequest can
// cross the durable actor boundary and re-drive the resumed outbox work.
func TestOORClientActorResumeSessionDurablePath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	snapshot, err := NewOutgoingSnapshot(
		sessionID,
		&AwaitingFinalizeAccepted{
			SessionID:            sessionID,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
			TransferInputs:       testRetryTransferInputs(t),
		},
	)
	require.NoError(t, err)

	handler := &resumeIntegrationHandler{}
	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-resume-session-durable-path",
	})
	defer actor.Stop()

	restoreResp := actor.Receive(ctx, &RestoreSessionRequest{
		Snapshot: snapshot,
	})
	require.True(t, restoreResp.IsOk())

	resumeResp := actor.Receive(ctx, &ResumeSessionRequest{
		SessionID: sessionID,
	})
	require.True(t, resumeResp.IsOk())

	require.Eventually(
		t, func() bool {
			return handler.sawFinalize.Load()
		}, resumeTestEventuallyTimeout, resumeTestEventuallyPoll,
	)

	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: sessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingFinalizeAccepted{}, stateMsg.State)
}

// TestOORDurableBehaviorDriveIncomingHandledNotifiesVTXOManager verifies the
// DriveEventRequest path forwards materialized incoming VTXOs to the manager
// after the receive-state transition is durably checkpointed.
func TestOORDurableBehaviorDriveIncomingHandledNotifiesVTXOManager(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, finalCheckpoints, recipients, parentCommitment, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	session, _, err := DriveIncomingTransferWithCheckpoints(
		ctx, sessionID, arkPSBT, finalCheckpoints,
	)
	require.NoError(t, err)

	desc, err := BuildIncomingVTXODescriptor(
		arkPSBT, IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			OwnerKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "round-incoming",
				CommitmentTxID: parentCommitment,
				BatchExpiry:    1000,
				TreeDepth:      1,
				ChainDepth:     len(finalCheckpoints),
				CreatedHeight:  700,
			},
		},
	)
	require.NoError(t, err)

	managerRef := &mockVTXOManagerRef{}
	behavior := &oorDurableBehavior{
		cfg: ClientActorCfg{
			DeliveryStore: newTestDeliveryStore(t),
			ActorID:       "oor-drive-incoming-handled-notify",
			OutboxHandler: &noopOutboxHandler{},
			VTXOManager:   managerRef,
		},
		sessions: map[SessionID]*sessionHandle{
			sessionID: {
				FSM:  session.FSM,
				kind: sessionKindIncoming,
			},
		},
	}

	resp := behavior.handleDriveEvent(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event: &IncomingHandledEvent{
			MaterializedVTXOs: []*vtxo.Descriptor{desc},
		},
	})
	require.True(t, resp.IsOk())

	sent := managerRef.sent()
	require.Len(t, sent, 1)

	notification, ok := sent[0].(*vtxo.VTXOsMaterializedNotification)
	require.True(t, ok)
	require.Len(t, notification.VTXOs, 1)
	require.Equal(t, desc.Outpoint, notification.VTXOs[0].Outpoint)

	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &ReceiveAwaitingAck{}, state)
}

// TestOORDurableBehaviorDriveIncomingHandledReloadsFromStore verifies the
// durable callback path can reload incoming VTXOs by outpoint after the event
// round-trips through the mailbox without attached descriptors.
func TestOORDurableBehaviorDriveIncomingHandledReloadsFromStore(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, finalCheckpoints, recipients, parentCommitment, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	session, _, err := DriveIncomingTransferWithCheckpoints(
		ctx, sessionID, arkPSBT, finalCheckpoints,
	)
	require.NoError(t, err)

	desc, err := BuildIncomingVTXODescriptor(
		arkPSBT, IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			OwnerKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "round-incoming",
				CommitmentTxID: parentCommitment,
				BatchExpiry:    1000,
				TreeDepth:      1,
				ChainDepth:     len(finalCheckpoints),
				CreatedHeight:  700,
			},
		},
	)
	require.NoError(t, err)

	store := newTestVTXOStore()
	require.NoError(t, store.SaveVTXO(ctx, desc))

	managerRef := &mockVTXOManagerRef{}
	behavior := &oorDurableBehavior{
		cfg: ClientActorCfg{
			DeliveryStore: newTestDeliveryStore(t),
			ActorID:       "oor-drive-incoming-handled-reload",
			OutboxHandler: &noopOutboxHandler{},
			VTXOManager:   managerRef,
			VTXOStore:     store,
		},
		sessions: map[SessionID]*sessionHandle{
			sessionID: {
				FSM:  session.FSM,
				kind: sessionKindIncoming,
			},
		},
	}

	resp := behavior.handleDriveEvent(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event: &IncomingHandledEvent{
			MaterializedOutpoints: []wire.OutPoint{desc.Outpoint},
		},
	})
	require.True(t, resp.IsOk())

	sent := managerRef.sent()
	require.Len(t, sent, 1)

	notification, ok := sent[0].(*vtxo.VTXOsMaterializedNotification)
	require.True(t, ok)
	require.Len(t, notification.VTXOs, 1)
	require.Equal(t, desc.Outpoint, notification.VTXOs[0].Outpoint)
}

// TestOORDurableBehaviorHandleResolveIncomingTransferAsync verifies the actor
// checkpoints an incoming resolve-pending state and returns before the
// follow-up indexer fetch completes.
func TestOORDurableBehaviorHandleResolveIncomingTransferCreatesSession(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	_, sessionID, recipientPkScript, recipientEventID :=
		buildIncomingResolveResponse(t)

	behavior := &oorDurableBehavior{
		cfg: ClientActorCfg{
			DeliveryStore: newTestDeliveryStore(t),
			ActorID:       "oor-resolve-incoming",
			OutboxHandler: &noopOutboxHandler{},
		},
		sessions: make(map[SessionID]*sessionHandle),
	}

	resp := behavior.handleResolveIncomingTransfer(
		ctx, &ResolveIncomingTransferRequest{
			SessionID:         sessionID,
			RecipientPkScript: recipientPkScript,
			RecipientEventID:  recipientEventID,
		},
	)
	require.NoError(t, resp.Err())

	// The session should be created in ReceiveResolving state
	// with the correct hint fields. The durable transport path
	// handles the actual query emission post-commit.
	handle := behavior.sessions[sessionID]
	require.NotNil(t, handle)
	require.Equal(t, sessionKindIncoming, handle.kind)

	state, err := handle.currentSessionState()
	require.NoError(t, err)

	resolving, ok := state.(*ReceiveResolving)
	require.True(t, ok)
	require.Equal(t, recipientPkScript, resolving.RecipientPkScript)
	require.Equal(t, recipientEventID, resolving.RecipientEventID)
}

// TestOORDurableBehaviorResumeRestoredSessionsResolvePending
// verifies that restored ReceiveResolving sessions survive restart
// without error. The durable transport path handles re-emission of
// the query post-commit.
func TestOORDurableBehaviorResumeRestoredSessionsResolvePending(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	_, sessionID, recipientPkScript, recipientEventID :=
		buildIncomingResolveResponse(t)

	session, err := newReceiveSessionWithState(
		ctx, sessionID, &ReceiveResolving{
			SessionID:         sessionID,
			RecipientPkScript: recipientPkScript,
			RecipientEventID:  recipientEventID,
		},
	)
	require.NoError(t, err)

	behavior := &oorDurableBehavior{
		cfg: ClientActorCfg{
			DeliveryStore: newTestDeliveryStore(t),
			ActorID:       "oor-resume-incoming-resolve",
			OutboxHandler: &noopOutboxHandler{},
		},
		sessions: map[SessionID]*sessionHandle{
			sessionID: {
				FSM:  session.FSM,
				kind: sessionKindIncoming,
			},
		},
	}

	err = behavior.resumeRestoredSessions(ctx)
	require.NoError(t, err)

	// The session should still be in ReceiveResolving after
	// resume. The durable transport will re-emit the indexer
	// query on the next outbox drain.
	handle := behavior.sessions[sessionID]
	require.NotNil(t, handle)

	state, stateErr := handle.currentSessionState()
	require.NoError(t, stateErr)
	require.IsType(t, &ReceiveResolving{}, state)
}
