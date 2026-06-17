package oor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// pausedFinalizeHandler simulates a transport that drops the finalize response
// the first time finalize is sent, requiring an explicit resume/retry.
type pausedFinalizeHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	finalizePaused bool
}

// Handle processes the outbox request and returns follow-up events.
func (h *pausedFinalizeHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		err := SignArkPSBT(
			h.clientSigner, msg.ArkPSBT, msg.CheckpointPSBTs,
			msg.TransferInputs,
		)
		require.NoError(h.t, err)

		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner, msg.TransferInputs,
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
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		if !h.finalizePaused {
			h.finalizePaused = true

			return nil, nil
		}

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg

		return []Event{&InputsMarkedSpentEvent{}}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*pausedFinalizeHandler)(nil)

// pausedSubmitHandler simulates a transport that drops the submit response the
// first time submit is sent, requiring an explicit resume/retry.
type pausedSubmitHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	submitPaused bool
}

// Handle processes the outbox request and returns follow-up events.
func (h *pausedSubmitHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		err := SignArkPSBT(
			h.clientSigner, msg.ArkPSBT, msg.CheckpointPSBTs,
			msg.TransferInputs,
		)
		require.NoError(h.t, err)

		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner, msg.TransferInputs,
			msg.CheckpointPSBTs,
		)
		require.NoError(h.t, err)

		if !h.submitPaused {
			h.submitPaused = true

			return nil, nil
		}

		return []Event{
			&SubmitAcceptedEvent{
				SessionID:               sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
			},
		}, nil

	case *RequestCheckpointSignatures:
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		_ = msg

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg

		return []Event{&InputsMarkedSpentEvent{}}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*pausedSubmitHandler)(nil)

// pausedCoSignedHandler simulates a wallet/UI environment where checkpoint
// signing is not completed the first time it is requested (for example, the
// app is backgrounded), requiring resume.
type pausedCoSignedHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	signPaused bool
}

// Handle processes the outbox request and returns follow-up events.
func (h *pausedCoSignedHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		err := SignArkPSBT(
			h.clientSigner, msg.ArkPSBT, msg.CheckpointPSBTs,
			msg.TransferInputs,
		)
		require.NoError(h.t, err)

		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner, msg.TransferInputs,
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
		if !h.signPaused {
			h.signPaused = true

			return nil, nil
		}

		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		_ = msg

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg

		return []Event{&InputsMarkedSpentEvent{}}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*pausedCoSignedHandler)(nil)

// cosignedButDroppedHandler simulates the point-of-no-return edge case:
// the server accepted and co-signed the submit package, but the client did not
// receive the SubmitAccepted response.
//
// On retry, the client must re-send the exact same submit package, and the
// server must return the original co-signed artifacts (not new ones).
type cosignedButDroppedHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	firstSubmitDropped bool

	firstArkRaw         []byte
	firstCheckpointRaws [][]byte

	cosignedCheckpoints []*psbt.Packet
}

// Handle processes the outbox request and returns follow-up events.
func (h *cosignedButDroppedHandler) Handle(_ context.Context,
	sessionID SessionID, outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		err := SignArkPSBT(
			h.clientSigner, msg.ArkPSBT, msg.CheckpointPSBTs,
			msg.TransferInputs,
		)
		require.NoError(h.t, err)

		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner, msg.TransferInputs,
			msg.CheckpointPSBTs,
		)
		require.NoError(h.t, err)

		arkRaw, err := psbtutil.Serialize(msg.ArkPSBT)
		require.NoError(h.t, err)

		cpRaws, err := serializePSBTSlice(msg.CheckpointPSBTs)
		require.NoError(h.t, err)

		// First submit: simulate server co-signing and persisting, but
		// drop the response back to the client.
		if !h.firstSubmitDropped {
			h.firstSubmitDropped = true
			h.firstArkRaw = arkRaw
			h.firstCheckpointRaws = cpRaws

			// In v0 tests we don't need a real operator signature.
			// We only need stable "co-signed" artifacts for the
			// client to resume with.
			//
			// We model this by deep-copying the checkpoint PSBTs.
			// Then we hold them for the eventual retry response.
			h.cosignedCheckpoints, err = parsePSBTSlice(cpRaws)
			require.NoError(h.t, err)

			return nil, nil
		}

		// Retry: client must resend the exact same package.
		require.Equal(
			h.t, h.firstArkRaw, arkRaw,
			"ark psbt differs across submit retries",
		)
		require.Equal(
			h.t, h.firstCheckpointRaws, cpRaws,
			"checkpoint psbts differ across submit retries",
		)

		return []Event{
			&SubmitAcceptedEvent{
				SessionID: sessionID,
				ArkPSBT:   msg.ArkPSBT,
				CoSignedCheckpointPSBTs: h.
					cosignedCheckpoints,
			},
		}, nil

	case *RequestCheckpointSignatures:
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		_ = msg

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg

		return []Event{&InputsMarkedSpentEvent{}}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*cosignedButDroppedHandler)(nil)

// resumeViaSnapshotCase describes one export/restore/resume scenario. Each row
// drives a fresh transfer to an intermediate paused phase, exports a snapshot,
// restores it into a new actor, and asserts the resumed workflow completes.
type resumeViaSnapshotCase struct {
	// name is the subtest name.
	name string

	// actorID is the base actor ID; "-1" / "-2" suffixes distinguish the
	// original and restored actors.
	actorID string

	// newHandler builds the outbox handler that pauses at the phase under
	// test the first time, then completes on resume.
	newHandler func(t *testing.T, client, operator input.Signer) OutboxHandler //nolint:ll

	// wantIntermediate is the FSM state expected before export (a
	// zero-value pointer used only for require.IsType).
	wantIntermediate State

	// wantPhase is the snapshot phase expected at export time.
	wantPhase OutgoingPhase
}

// TestOORClientActorResumeFromSnapshot verifies the client actor can export a
// snapshot at each interruptible phase, restore it into a new actor, and resume
// the workflow to completion. The snapshot must carry enough information to
// re-emit the implied outbox work idempotently, including the
// point-of-no-return case where the server already co-signed but the client
// missed the response (it must re-send byte-identical submit bytes).
func TestOORClientActorResumeFromSnapshot(t *testing.T) {
	t.Parallel()

	cases := []resumeViaSnapshotCase{
		{
			name:    "paused finalize",
			actorID: "oor-resume-snapshot-actor",
			newHandler: func(t *testing.T,
				client, operator input.Signer) OutboxHandler {

				return &pausedFinalizeHandler{
					t:              t,
					clientSigner:   client,
					operatorSigner: operator,
				}
			},
			wantIntermediate: &AwaitingFinalizeAccepted{},
			wantPhase:        OutgoingPhaseFinalizeSent,
		},
		{
			name:    "cosigned but dropped",
			actorID: "oor-resume-cosigned-actor",
			newHandler: func(t *testing.T,
				client, operator input.Signer) OutboxHandler {

				return &cosignedButDroppedHandler{
					t:              t,
					clientSigner:   client,
					operatorSigner: operator,
				}
			},
			wantIntermediate: &AwaitingSubmitAccepted{},
			wantPhase:        OutgoingPhaseSubmitSent,
		},
		{
			name:    "paused submit",
			actorID: "oor-resume-submit-actor",
			newHandler: func(t *testing.T,
				client, operator input.Signer) OutboxHandler {

				return &pausedSubmitHandler{
					t:              t,
					clientSigner:   client,
					operatorSigner: operator,
				}
			},
			wantIntermediate: &AwaitingSubmitAccepted{},
			wantPhase:        OutgoingPhaseSubmitSent,
		},
		{
			name:    "paused checkpoint cosign",
			actorID: "oor-resume-cosigned-phase-actor",
			newHandler: func(t *testing.T,
				client, operator input.Signer) OutboxHandler {

				return &pausedCoSignedHandler{
					t:              t,
					clientSigner:   client,
					operatorSigner: operator,
				}
			},
			wantIntermediate: &AwaitingCheckpointSignatures{},
			wantPhase:        OutgoingPhaseCoSigned,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runResumeViaSnapshotCase(t, tc)
		})
	}
}

// runResumeViaSnapshotCase exercises one export/restore/resume scenario.
func runResumeViaSnapshotCase(t *testing.T, tc resumeViaSnapshotCase) {
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

	deliveryStore := newTestDeliveryStore(t)
	handler := tc.newHandler(t, clientSigner, operatorSigner)

	// Start a session and drive it until the handler pauses at the phase
	// under test.
	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       tc.actorID + "-1",
	})
	defer actor1.Stop()

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, tc.wantIntermediate, stateMsg.State)

	// Export a portable snapshot and restore it into a new actor to
	// simulate an app restart.
	exportResp := actor1.Receive(ctx, &ExportSnapshotRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, exportResp.IsOk())

	exportMsg, ok := exportResp.UnwrapOr(nil).(*ExportSnapshotResponse)
	require.True(t, ok)
	require.NotNil(t, exportMsg.Snapshot)
	require.Equal(t, tc.wantPhase, exportMsg.Snapshot.Phase)

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       tc.actorID + "-2",
	})
	defer actor2.Stop()

	restoreResp := actor2.Receive(ctx, &RestoreSessionRequest{
		Snapshot: exportMsg.Snapshot,
	})
	require.True(t, restoreResp.IsOk())

	restoreMsg, ok := restoreResp.UnwrapOr(nil).(*RestoreSessionResponse)
	require.True(t, ok)
	require.Equal(t, startMsg.SessionID, restoreMsg.SessionID)

	resumeResp := actor2.Receive(ctx, &ResumeSessionRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, resumeResp.IsOk())

	finalStateResp := actor2.Receive(ctx, &GetStateRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, finalStateResp.IsOk())

	finalStateMsg, ok := finalStateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, finalStateMsg.State)
}

// TestOORClientActorResumeAfterServerCoSignedFromStore verifies the client can
// resume after a crash using only the persisted snapshot (no explicit export)
// even if the server already co-signed but the submit response was dropped.
func TestOORClientActorResumeAfterServerCoSignedFromStore(t *testing.T) {
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

	deliveryStore := newTestDeliveryStore(t)
	handler := &cosignedButDroppedHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	const actorID = "oor-resume-cosigned-from-store-actor"

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})
	defer actor1.Stop()

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	// At this point the server has co-signed but the response was dropped,
	// so the client should still be waiting for submit acceptance.
	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingSubmitAccepted{}, stateMsg.State)

	actor1.Stop()

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})
	defer actor2.Stop()

	require.Eventually(t, func() bool {
		finalStateResp := actor2.Receive(ctx, &GetStateRequest{
			SessionID: startMsg.SessionID,
		})
		if finalStateResp.IsErr() {
			return false
		}

		respMsg := finalStateResp.UnwrapOr(nil)
		finalStateMsg, ok := respMsg.(*GetStateResponse)
		if !ok {
			return false
		}

		_, ok = finalStateMsg.State.(*Completed)

		return ok
	}, signingChainEventuallyTimeout, 50*time.Millisecond)
}

// TestOORClientActorDurableRestartAutoResume verifies the durable actor can
// restore checkpointed sessions and auto-resume pending outbox work after a
// process restart, without using ExportSnapshot/RestoreSession requests.
// TestOORClientActorDurableRestartWithRetryMetadata verifies that when a
// session has pending retry metadata at checkpoint time, a restart schedules a
// retry timer instead of immediately re-driving the outbox.
func TestOORClientActorDurableRestartWithRetryMetadata(t *testing.T) {
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

	deliveryStore := newTestDeliveryStore(t)

	const actorID = "oor-restart-retry-metadata"

	// retrySubmitOnceHandler fails the first submit with a retryable
	// error, then handles ScheduleRetryRequest as a no-op (no timeout
	// actor). On the second actor instance, it succeeds.
	handler1 := &retrySubmitOutboxHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler1,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	// After the retryable error, the session should remain in
	// AwaitingSubmitAccepted (not RetryBackoff, which no longer
	// exists).
	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingSubmitAccepted{}, stateMsg.State)

	// Verify retry metadata was persisted to the snapshot by exporting
	// it.
	exportResp := actor1.Receive(ctx, &ExportSnapshotRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, exportResp.IsOk())

	exportMsg, ok := exportResp.UnwrapOr(nil).(*ExportSnapshotResponse)
	require.True(t, ok)
	require.NotZero(
		t, exportMsg.Snapshot.RetryAfter,
		"retry metadata should be set on snapshot",
	)
	require.NotEmpty(
		t, exportMsg.Snapshot.FailReason,
		"retry reason should be set on snapshot",
	)

	// Stop actor1 — simulates a crash.
	actor1.Stop()

	// recordingHandler tracks which outbox events it receives so we can
	// verify that the restarted actor schedules a retry rather than
	// immediately re-driving the submit outbox.
	handler2 := &retryRecordingHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler2,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})
	defer actor2.Stop()

	// Wait for the restart to process the checkpoint.
	require.Eventually(t, func() bool {
		resp := actor2.Receive(ctx, &GetStateRequest{
			SessionID: startMsg.SessionID,
		})

		return resp.IsOk()
	}, signingChainEventuallyTimeout, 50*time.Millisecond)

	// The restarted actor should have emitted ScheduleRetryRequest
	// (not SendSubmitPackageRequest) because the checkpoint had
	// RetryAfter > 0.
	require.True(
		t, handler2.sawScheduleRetry.Load(),
		"restart should schedule retry, not drive outbox",
	)

	// Now explicitly resume so the transfer can complete.
	resumeResp := actor2.Receive(ctx, &ResumeSessionRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, resumeResp.IsOk())

	stateResp = actor2.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok = stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, stateMsg.State)
}

// retryRecordingHandler records whether it received a ScheduleRetryRequest
// and succeeds on all operations. Used to verify restart behavior.
type retryRecordingHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	sawScheduleRetry atomic.Bool
}

// Handle processes the outbox request and returns follow-up events.
func (h *retryRecordingHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		err := SignArkPSBT(
			h.clientSigner, msg.ArkPSBT, msg.CheckpointPSBTs,
			msg.TransferInputs,
		)
		require.NoError(h.t, err)

		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner, msg.TransferInputs,
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
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		return []Event{&FinalizeAcceptedEvent{}}, nil

	case *MarkInputsSpentRequest:
		return []Event{&InputsMarkedSpentEvent{}}, nil

	case *ScheduleRetryRequest:
		h.sawScheduleRetry.Store(true)

		return nil, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*retryRecordingHandler)(nil)

func TestOORClientActorDurableRestartAutoResume(t *testing.T) {
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

	deliveryStore := newTestDeliveryStore(t)
	handler := &pausedFinalizeHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	const actorID = "oor-durable-restart-actor"

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingFinalizeAccepted{}, stateMsg.State)

	actor1.Stop()

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})
	defer actor2.Stop()

	require.Eventually(t, func() bool {
		resp := actor2.Receive(ctx, &GetStateRequest{
			SessionID: startMsg.SessionID,
		})
		if resp.IsErr() {
			return false
		}

		got, ok := resp.UnwrapOr(nil).(*GetStateResponse)
		if !ok {
			return false
		}

		switch got.State.(type) {
		case *AwaitingLocalVTXOUpdate, *Completed:
			return true

		default:
			return false
		}
	}, signingChainEventuallyTimeout, 50*time.Millisecond)
}

// TestOORClientActorDurableRestartRetriesMarkInputsSpent asserts that a
// retryable local-spend completion failure survives restart without moving the
// session into Failed.
func TestOORClientActorDurableRestartRetriesMarkInputsSpent(t *testing.T) {
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
				Hash:  [32]byte{0x04},
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

	deliveryStore := newTestDeliveryStore(t)
	const actorID = "oor-restart-mark-inputs-spent-retry"

	handler1 := &retryRecordingHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	var completeSpendAttempts atomic.Int32
	completeSpend := func(_ context.Context, ops []wire.OutPoint) error {
		require.Equal(t, InputOutpoints(inputs), ops)

		// The first local-spend completion simulates the transient
		// manager-unavailable race we see during shutdown/restart
		// overlap.
		if completeSpendAttempts.Add(1) == 1 {
			return actor.ErrNoActorsAvailable
		}

		return nil
	}

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &LocalPersistenceOutboxHandler{
			Next:          handler1,
			CompleteSpend: completeSpend,
		},
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingLocalVTXOUpdate{}, stateMsg.State)

	// Export the checkpoint so we can assert the retry metadata is already
	// persisted before the first actor is stopped.
	exportResp := actor1.Receive(ctx, &ExportSnapshotRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, exportResp.IsOk())

	exportMsg, ok := exportResp.UnwrapOr(nil).(*ExportSnapshotResponse)
	require.True(t, ok)
	require.NotZero(t, exportMsg.Snapshot.RetryAfter)
	require.Contains(
		t, exportMsg.Snapshot.FailReason, "complete spend via manager",
	)

	actor1.Stop()

	handler2 := &retryRecordingHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &LocalPersistenceOutboxHandler{
			Next:          handler2,
			CompleteSpend: completeSpend,
		},
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})
	defer actor2.Stop()

	// Restart should honor the saved retry metadata by scheduling
	// retry work rather than immediately re-driving
	// MarkInputsSpentRequest.
	require.Eventually(t, func() bool {
		resp := actor2.Receive(ctx, &GetStateRequest{
			SessionID: startMsg.SessionID,
		})
		if resp.IsErr() {
			return false
		}

		got, ok := resp.UnwrapOr(nil).(*GetStateResponse)
		if !ok {
			return false
		}

		_, awaitingLocalUpdate := got.State.(*AwaitingLocalVTXOUpdate)

		return awaitingLocalUpdate
	}, signingChainEventuallyTimeout, 50*time.Millisecond)

	require.True(
		t, handler2.sawScheduleRetry.Load(),
		"restart should preserve retry intent for local spend "+
			"completion",
	)
	require.EqualValues(
		t, 1, completeSpendAttempts.Load(),
		"restart should not re-drive spend completion before "+
			"explicit resume",
	)

	// Once the user explicitly resumes, the same durable state
	// should replay the local spend completion and let the session
	// finish cleanly.
	resumeResp := actor2.Receive(ctx, &ResumeSessionRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, resumeResp.IsOk())

	require.Eventually(t, func() bool {
		resp := actor2.Receive(ctx, &GetStateRequest{
			SessionID: startMsg.SessionID,
		})
		if resp.IsErr() {
			return false
		}

		got, ok := resp.UnwrapOr(nil).(*GetStateResponse)
		if !ok {
			return false
		}

		_, completed := got.State.(*Completed)

		return completed
	}, signingChainEventuallyTimeout, 50*time.Millisecond)

	require.EqualValues(t, 2, completeSpendAttempts.Load())
}
