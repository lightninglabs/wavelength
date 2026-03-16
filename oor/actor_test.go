package oor

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// testOutboxHandler is a minimal in-process outbox handler for client actor
// tests. It simulates a server and wallet by returning follow-up events that
// drive the FSM forward.
type testOutboxHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer
}

// Handle processes the outbox request and returns follow-up events.
func (h *testOutboxHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		err := SignArkPSBT(
			h.clientSigner, msg.ArkPSBT,
			msg.CheckpointPSBTs, msg.TransferInputs,
		)
		require.NoError(h.t, err)

		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		// The session ID is defined as the Ark txid, which means the
		// client can reconstruct it deterministically from PSBT bytes.
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner,
			msg.TransferInputs,
			msg.CheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&SubmitAcceptedEvent{
				SessionID:               sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
			},
		}, nil

	case *RequestCheckpointSignatures:
		// This simulates wallet-side signing.
		//
		// The FSM is expected to request that the application/wallet
		// layer attaches client signatures to the (server co-signed)
		// checkpoint PSBTs.
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		// After signing, we emit the event that drives the FSM into the
		// finalize step.
		finalCheckpoints := msg.CoSignedCheckpointPSBTs

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: finalCheckpoints,
			},
		}, nil

	case *SendFinalizePackageRequest:
		// Finalize is the last transport step: after this point, the
		// server is expected to persist the transfer's VTXO set update.
		//
		// In unit tests we model this as unconditional acceptance.
		_ = msg
		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		// Outgoing OOR transfers are off-chain.
		// Once finalize is accepted, the local wallet must record
		// that its inputs are spent.
		_ = msg
		return []Event{
			&InputsMarkedSpentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*testOutboxHandler)(nil)

// noopOutboxHandler acknowledges local outbox events without producing any
// follow-up events. Tests use this when they only need the actor plumbing,
// not full wallet/server side effects.
type noopOutboxHandler struct{}

// Handle implements OutboxHandler.
func (h *noopOutboxHandler) Handle(_ context.Context, _ SessionID,
	_ OutboxEvent) ([]Event, error) {

	return nil, nil
}

var _ OutboxHandler = (*noopOutboxHandler)(nil)

// testOutgoingPackageStore records package persistence calls for assertions.
type testOutgoingPackageStore struct {
	packageCalls int
	bindingCalls int

	lastDirection PackageDirection
	lastSessionID chainhash.Hash
}

// UpsertPackage records one outgoing package persistence invocation.
func (s *testOutgoingPackageStore) UpsertPackage(_ context.Context,
	direction PackageDirection, sessionID chainhash.Hash, _ *psbt.Packet,
	_ []*psbt.Packet) error {

	s.packageCalls++
	s.lastDirection = direction
	s.lastSessionID = sessionID

	return nil
}

// UpsertBinding records one outgoing input-binding persistence invocation.
func (s *testOutgoingPackageStore) UpsertBinding(_ context.Context,
	_ wire.OutPoint, _ chainhash.Hash, _ uint32, _ PackageLinkKind) error {

	s.bindingCalls++

	return nil
}

// TestOORClientActorHappyPath exercises the outgoing transfer flow end-to-end
// using the client actor wrapper and a stub outbox handler.
func TestOORClientActorHappyPath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// This is a pure unit test: we use mock keys and a mock signer so the
	// test is deterministic and does not require an external wallet.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)
	packageStore := &testOutgoingPackageStore{}

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	// The actor wrapper is responsible for:
	// - creating a per-session FSM instance
	// - delivering outbox work to an application-provided handler
	// - driving follow-up events back into the FSM
	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &testOutboxHandler{
			t:              t,
			clientSigner:   clientSigner,
			operatorSigner: operatorSigner,
		},
		PackageStore:  packageStore,
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-actor-test-happy",
	})
	defer actor.Stop()

	startResp := actor.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.NotEqual(t, SessionID{}, startMsg.SessionID)

	// Verify the session reached a terminal state without requiring any
	// explicit "drive" calls by the test: outbox ↔ event feedback should
	// be sufficient for the happy path.
	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, stateMsg.State)
	require.Equal(t, 1, packageStore.packageCalls)
	require.Equal(t, len(inputs), packageStore.bindingCalls)
	require.Equal(t, PackageDirectionOutgoing, packageStore.lastDirection)
	require.Equal(t, chainhash.Hash(startMsg.SessionID),
		packageStore.lastSessionID)
}

// TestOORClientActorHandlesIncomingTransferWithoutExistingSession asserts the
// actor can materialize a fresh incoming transfer before any session has been
// registered under that session ID.
func TestOORClientActorHandlesIncomingTransferWithoutExistingSession(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, finalCheckpoints, recipients, parentCommitment, recipientKey,
		operatorKey :=
		buildTestIncomingMaterialization(t)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()
	packageStore := &testPackageStore{}

	notifyCalls := 0
	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &LocalPersistenceOutboxHandler{
			Store:        store,
			PackageStore: packageStore,
			OperatorKey:  operatorKey,
			ExitDelay:    10,
			NotifyIncomingVTXOs: func(_ context.Context,
				_ []*vtxo.Descriptor) error {

				notifyCalls++
				return nil
			},
			ResolveIncomingClientKey: func(_ context.Context,
				_ ArkRecipientOutput) (
				keychain.KeyDescriptor, error) {

				return keychain.KeyDescriptor{
					PubKey: recipientKey.PubKey(),
				}, nil
			},
			ResolveIncomingMetadata: func(_ context.Context,
				_ SessionID, _ ArkRecipientOutput, _ *psbt.Packet, //nolint:ll
				_ []*psbt.Packet) (IncomingVTXOMetadata, error) { //nolint:ll

				return IncomingVTXOMetadata{
					RoundID:        "round-incoming",
					CommitmentTxID: parentCommitment,
					BatchExpiry:    1000,
					TreeDepth:      1,
					ChainDepth:     len(finalCheckpoints),
					CreatedHeight:  700,
				}, nil
			},
		},
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-actor-test-incoming",
	})
	defer actor.Stop()

	resp := actor.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event: &IncomingTransferEvent{
			SessionID:            sessionID,
			ArkPSBT:              arkPSBT,
			FinalCheckpointPSBTs: finalCheckpoints,
		},
	})
	require.True(t, resp.IsOk())

	liveVTXOs, err := store.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 1)
	require.Equal(t, 1, notifyCalls)
	require.Equal(t, 1, packageStore.packageCalls)
	require.Equal(t, 1, packageStore.bindingCalls)
	require.Equal(t, "round-incoming", liveVTXOs[0].RoundID)
	require.Equal(t, parentCommitment, liveVTXOs[0].CommitmentTxID)
	require.Equal(t, wire.OutPoint{
		Hash:  arkPSBT.UnsignedTx.TxHash(),
		Index: recipients[0].OutputIndex,
	}, liveVTXOs[0].Outpoint)
}

// retrySubmitOutboxHandler simulates a retryable transport error on the first
// submit attempt and verifies the FSM can back off and retry.
type retrySubmitOutboxHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	submitAttempts int
}

// Handle processes the outbox request and returns follow-up events.
func (h *retrySubmitOutboxHandler) Handle(
	_ context.Context,
	sessionID SessionID,
	outbox OutboxEvent,
) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		err := SignArkPSBT(
			h.clientSigner, msg.ArkPSBT,
			msg.CheckpointPSBTs, msg.TransferInputs,
		)
		require.NoError(h.t, err)

		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		h.submitAttempts++

		// First attempt fails with a retryable error.
		if h.submitAttempts == 1 {
			return nil, NewRetryableOutboxError(
				fmt.Errorf("temporary transport error"),
				0,
			)
		}

		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner,
			msg.TransferInputs,
			msg.CheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&SubmitAcceptedEvent{
				SessionID:               sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
			},
		}, nil

	case *ScheduleRetryRequest:
		_ = msg

		return nil, nil

	case *RequestCheckpointSignatures:
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		cps := msg.CoSignedCheckpointPSBTs

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: cps,
			},
		}, nil

	case *SendFinalizePackageRequest:
		_ = msg

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg
		return []Event{
			&InputsMarkedSpentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*retrySubmitOutboxHandler)(nil)

// TestOORClientActorRetryResume asserts the client actor can handle a
// retryable error, persist retry intent, and complete after explicit resume.
func TestOORClientActorRetryResume(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x02},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &retrySubmitOutboxHandler{
			t:              t,
			clientSigner:   clientSigner,
			operatorSigner: operatorSigner,
		},
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-actor-retry-backoff",
	})
	defer actor.Stop()

	startResp := actor.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingSubmitAccepted{}, stateMsg.State)

	resumeResp := actor.Receive(ctx, &ResumeSessionRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, resumeResp.IsOk())

	stateResp = actor.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok = stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, stateMsg.State)
}

// mockServerConnRef captures all messages Tell'd to the server connection
// actor for test verification.
type mockServerConnRef struct {
	t *testing.T

	id       string
	messages []serverconn.ServerConnMsg
	mu       sync.Mutex

	// tellErr, when non-nil, is returned by Tell instead of recording
	// the message.
	tellErr error
}

// newMockServerConnRef creates a new mock server connection reference.
func newMockServerConnRef(t *testing.T) *mockServerConnRef {
	return &mockServerConnRef{
		t:        t,
		id:       "mock-server-conn",
		messages: make([]serverconn.ServerConnMsg, 0),
	}
}

// ID returns the mock's stable identifier.
func (m *mockServerConnRef) ID() string {
	return m.id
}

// Tell records outgoing messages for assertion.
func (m *mockServerConnRef) Tell(
	_ context.Context, msg serverconn.ServerConnMsg,
) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.tellErr != nil {
		return m.tellErr
	}

	m.messages = append(m.messages, msg)

	return nil
}

// lastSent returns the most recent SendClientEventRequest captured by the
// mock. It fails the test if no messages have been captured.
func (m *mockServerConnRef) lastSent() *serverconn.SendClientEventRequest {
	m.t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	require.NotEmpty(m.t, m.messages, "no messages captured")

	last := m.messages[len(m.messages)-1]
	req, ok := last.(*serverconn.SendClientEventRequest)
	require.True(
		m.t, ok, "last message is not SendClientEventRequest",
	)

	return req
}

// lastRecipientQuery returns the most recent durable recipient-events query
// captured by the mock. It fails the test if no messages have been captured.
// localOnlyOutboxHandler handles only local outbox events (signing,
// persistence, timers). Transport events should be routed through serverconn
// and never reach this handler.
type localOnlyOutboxHandler struct {
	t *testing.T

	clientSigner input.Signer
}

// Handle processes only local outbox events and fails on transport events.
func (h *localOnlyOutboxHandler) Handle(_ context.Context,
	_ SessionID, outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		err := SignArkPSBT(
			h.clientSigner, msg.ArkPSBT,
			msg.CheckpointPSBTs, msg.TransferInputs,
		)
		require.NoError(h.t, err)

		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *RequestCheckpointSignatures:
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		cps := msg.CoSignedCheckpointPSBTs

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: cps,
			},
		}, nil

	case *MarkInputsSpentRequest:
		return []Event{
			&InputsMarkedSpentEvent{},
		}, nil

	case *SendSubmitPackageRequest, *SendFinalizePackageRequest,
		*SendIncomingAckRequest:

		h.t.Fatalf("transport event %T should not reach "+
			"local handler", outbox)

		return nil, nil

	default:
		h.t.Fatalf("unhandled local event %T", outbox)

		return nil, nil
	}
}

var _ OutboxHandler = (*localOnlyOutboxHandler)(nil)

// TestOORClientActorTransportViaServerConn verifies that transport outbox
// events (submit, finalize, ack) are Tell'd to the serverconn actor when
// configured, while local events (signing, persistence) continue through
// the OutboxHandler.
func TestOORClientActorTransportViaServerConn(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)
	packageStore := &testOutgoingPackageStore{}
	mockConn := newMockServerConnRef(t)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x03},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(
				t, clientKey.PubKey(),
			),
			Value: inputValue,
		},
	}

	// Wire the actor with both a local-only handler and a mock
	// serverconn. Transport events should go to the mock, local events
	// to the handler.
	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &localOnlyOutboxHandler{
			t:            t,
			clientSigner: clientSigner,
		},
		ServerConn:    mockConn,
		PackageStore:  packageStore,
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-actor-serverconn-test",
	})
	defer actor.Stop()

	// Start the transfer. The FSM will emit RequestArkSignatures
	// (local → handler → ArkSignedEvent) then SendSubmitPackageRequest
	// (transport → serverconn mock). The actor returns with the FSM in
	// AwaitingSubmitAccepted.
	startResp := actor.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	sessionID := startMsg.SessionID

	// Verify the submit request was captured by the mock, not the
	// handler.
	submitReq := mockConn.lastSent()
	submitMsg, ok := submitReq.Message.(*SendSubmitPackageRequest)
	require.True(t, ok)
	require.NotNil(t, submitMsg.ArkPSBT)

	// Verify the FSM is waiting for the server response.
	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: sessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingSubmitAccepted{}, stateMsg.State)

	// Simulate server response. Co-sign checkpoints with the operator
	// key, then inject SubmitAcceptedEvent via DriveEventRequest. The
	// FSM will emit RequestCheckpointSignatures (local → handler →
	// CheckpointsSignedEvent) then SendFinalizePackageRequest
	// (transport → serverconn mock).
	err = coSignCheckpointPSBTsForTest(
		operatorSigner,
		submitMsg.TransferInputs,
		submitMsg.CheckpointPSBTs,
	)
	require.NoError(t, err)

	driveResp := actor.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event: &SubmitAcceptedEvent{
			SessionID:               sessionID,
			ArkPSBT:                 submitMsg.ArkPSBT,
			CoSignedCheckpointPSBTs: submitMsg.CheckpointPSBTs,
		},
	})
	require.True(t, driveResp.IsOk())

	// Verify the finalize request was captured by the mock.
	finalizeReq := mockConn.lastSent()
	_, ok = finalizeReq.Message.(*SendFinalizePackageRequest)
	require.True(t, ok)

	// Verify the FSM is waiting for the finalize response.
	stateResp = actor.Receive(ctx, &GetStateRequest{
		SessionID: sessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok = stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingFinalizeAccepted{}, stateMsg.State)

	// Simulate finalize accepted. The FSM will emit
	// MarkInputsSpentRequest (local → handler →
	// InputsMarkedSpentEvent) and transition to Completed.
	driveResp = actor.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &FinalizeAcceptedEvent{},
	})
	require.True(t, driveResp.IsOk())

	// Verify terminal state.
	stateResp = actor.Receive(ctx, &GetStateRequest{
		SessionID: sessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok = stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, stateMsg.State)

	// Verify package was persisted.
	require.Equal(t, 1, packageStore.packageCalls)
	require.Equal(t, len(inputs), packageStore.bindingCalls)
}

// TestOORClientActorSubmitAcceptedNilArkPSBTEnrichment verifies that a
// SubmitAcceptedEvent with nil ArkPSBT is enriched from the session's
// AwaitingSubmitAccepted state. This is the production path for server-push
// events dispatched via the EventRouter, where the oorpb proto response
// does not echo the Ark PSBT back.
func TestOORClientActorSubmitAcceptedNilArkPSBTEnrichment(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)
	mockConn := newMockServerConnRef(t)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x05},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(
				t, clientKey.PubKey(),
			),
			Value: inputValue,
		},
	}

	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &localOnlyOutboxHandler{
			t:            t,
			clientSigner: clientSigner,
		},
		ServerConn:    mockConn,
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-actor-nil-ark-enrich",
	})
	defer actor.Stop()

	// Start the transfer to reach AwaitingSubmitAccepted.
	startResp := actor.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	sessionID := startMsg.SessionID

	// Capture the submit request so we can co-sign checkpoints.
	submitReq := mockConn.lastSent()
	submitMsg, ok := submitReq.Message.(*SendSubmitPackageRequest)
	require.True(t, ok)

	err = coSignCheckpointPSBTsForTest(
		operatorSigner,
		submitMsg.TransferInputs,
		submitMsg.CheckpointPSBTs,
	)
	require.NoError(t, err)

	// Drive with a SubmitAcceptedEvent that has nil ArkPSBT, simulating
	// a server-push event dispatched via the EventRouter. The actor
	// should enrich ArkPSBT from the AwaitingSubmitAccepted state.
	driveResp := actor.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event: &SubmitAcceptedEvent{
			SessionID:               sessionID,
			ArkPSBT:                 nil,
			CoSignedCheckpointPSBTs: submitMsg.CheckpointPSBTs,
		},
	})
	require.True(t, driveResp.IsOk(),
		"expected enrichment to succeed, got: %v",
		driveResp.Err())

	// The FSM should have advanced past AwaitingSubmitAccepted.
	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: sessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingFinalizeAccepted{}, stateMsg.State)
}

// TestIsTransportEventClassification verifies that isTransportEvent correctly
// classifies all outbox event types. Transport events (submit, finalize, ack)
// must be routed to serverconn, while local events (signing, persistence,
// timers) must remain on the local handler.
func TestIsTransportEventClassification(t *testing.T) {
	t.Parallel()

	mockConn := newMockServerConnRef(t)
	behavior := &oorDurableBehavior{
		cfg: ClientActorCfg{
			ServerConn: mockConn,
		},
	}

	// Transport events should be routed to serverconn.
	transportEvents := []OutboxEvent{
		&SendSubmitPackageRequest{},
		&SendFinalizePackageRequest{},
		&SendIncomingAckRequest{},
		&QueryIncomingTransferRequest{},
		&QueryIncomingMetadataRequest{},
	}
	for _, evt := range transportEvents {
		require.True(
			t, behavior.isTransportEvent(evt),
			"expected %T to be classified as transport", evt,
		)
	}

	// Local events must stay on the local handler.
	localEvents := []OutboxEvent{
		&RequestArkSignatures{},
		&RequestCheckpointSignatures{},
		&MarkInputsSpentRequest{},
		&IncomingTransferNotification{},
		&MaterializeIncomingVTXOsRequest{},
		&ScheduleRetryRequest{},
	}
	for _, evt := range localEvents {
		require.False(
			t, behavior.isTransportEvent(evt),
			"expected %T to be classified as local", evt,
		)
	}

	// When ServerConn is nil, all events are classified as local for
	// backward compatibility.
	nilBehavior := &oorDurableBehavior{
		cfg: ClientActorCfg{},
	}
	for _, evt := range transportEvents {
		require.False(
			t, nilBehavior.isTransportEvent(evt),
			"expected %T to be local when ServerConn is nil",
			evt,
		)
	}
}

// TestOORClientActorTellFailurePropagation verifies that when
// ServerConn.Tell() returns an error, driveOutbox propagates it and the
// Receive call returns an error result.
func TestOORClientActorTellFailurePropagation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)

	mockConn := newMockServerConnRef(t)
	mockConn.tellErr = fmt.Errorf("connection lost")

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x04},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(
				t, clientKey.PubKey(),
			),
			Value: inputValue,
		},
	}

	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &localOnlyOutboxHandler{
			t:            t,
			clientSigner: clientSigner,
		},
		ServerConn:    mockConn,
		PackageStore:  &testOutgoingPackageStore{},
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-actor-tell-fail-test",
	})
	defer actor.Stop()

	// Start a transfer. The local signing succeeds, but Tell() to
	// serverconn fails when dispatching SendSubmitPackageRequest.
	startResp := actor.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsErr())

	require.Contains(t, startResp.Err().Error(), "connection lost")
}
