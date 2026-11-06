package round

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestRuntimeSigningFence covers the first signer call, before there are any
// private sessions in the saved state, and ignores unrelated or stale pushes.
func TestRuntimeSigningFence(t *testing.T) {
	id := testRoundID("first-signing-call")
	for _, tc := range []struct {
		name  string
		state ClientState
		event ClientEvent
		owned bool
		want  bool
	}{
		{
			"first-call",
			&RoundJoinedState{},
			&CommitmentTxBuilt{
				RoundID: id,
			},
			true,
			true,
		},
		{
			"partial-sign",
			&NoncesSentState{},
			&NoncesAggregated{
				RoundID: id,
			},
			true,
			true,
		},
		{
			"unknown-round",
			&RoundJoinedState{},
			&CommitmentTxBuilt{
				RoundID: id,
			},
			false,
			false,
		},
		{
			"stale-batch",
			&InputSigSentState{},
			&CommitmentTxBuilt{
				RoundID: id,
			},
			true,
			false,
		},
		{
			"stale-nonces",
			&RoundJoinedState{},
			&NoncesAggregated{
				RoundID: id,
			},
			true,
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := &ClientEnvironment{Log: btclog.Disabled}
			machine := protofsm.NewInlineStateMachine(
				ClientStateMachineCfg{
					Logger:       btclog.Disabled,
					InitialState: tc.state,
					Env:          env,
				},
			)
			machine.Start(t.Context())
			t.Cleanup(machine.Stop)
			a := &RoundClientActor{
				cfg:    &RoundClientConfig{},
				rounds: make(map[RoundKeyStr]*RoundFSM),
			}
			key := RoundKeyStr(id.KeyString())
			if tc.owned {
				a.rounds[key] = &RoundFSM{
					FSM: machine,
				}
			}
			raw, err := a.interruptedSigningRound(
				t.Context(), &ServerMessageNotification{
					Message: tc.event,
				},
			)
			require.NoError(t, err)
			if !tc.want {
				require.Empty(t, raw)

				return
			}
			require.Equal(t, id[:], raw)
			snapshot := &clientActorSnapshot{
				rounds: map[RoundKeyStr]*restoredClientRound{
					key: {
						state: tc.state,
					},
				},
			}
			require.NoError(
				t, markInterruptedSigning(snapshot, raw),
			)
			require.True(
				t, snapshot.rounds[key].lostSignerSessions,
			)
		})
	}
}

// TestRuntimeColdSigningDoesNotReplay proves redelivery after a staged first
// signer call cannot enter the ordinary FSM or create another signer session.
func TestRuntimeColdSigningDoesNotReplay(t *testing.T) {
	h := newActorTestHarness(t)
	id := testRoundID("cold-first-call")
	env := *h.actor.env
	restored := &RoundFSM{
		Key:         id,
		RoundID:     id,
		env:         &env,
		coldSigning: true,
	}
	h.actor.installRoundState(
		t.Context(), restored, &RoundJoinedState{
			RoundID: id,
		},
	)
	t.Cleanup(restored.FSM.Stop)
	h.actor.rounds[RoundKeyStr(id.KeyString())] = restored
	// This deliberately incomplete batch would fail validation if the
	// ordinary FSM saw it. Cold recovery must consume neither its tree nor
	// any signer. A local cancellation must also leave ownership intact.
	require.NoError(
		t,
		h.actor.askEventAndProcessOutbox(
			t.Context(), restored, &CommitmentTxBuilt{
				RoundID: id,
			},
		),
	)
	push := h.actor.Receive(t.Context(), &ServerMessageNotification{
		Message: &BoardingFailed{RoundID: fn.Some(id)},
	})
	require.NoError(t, push.Err(), "expected failure push must be consumed")
	cancel, err := h.actor.handleCancelRound(
		t.Context(), &CancelRoundRequest{
			RoundKey: fn.Some(RoundKeyStr(id.KeyString())),
		},
	).Unpack()
	require.NoError(t, err)
	response, ok := cancel.(*CancelRoundResponse)
	require.True(t, ok)
	require.False(t, response.Success)
	state, err := restored.FSM.CurrentStateWithContext(t.Context())
	require.NoError(t, err)
	require.IsType(t, &RoundJoinedState{}, state)
	require.True(t, restored.coldSigning)
	require.Contains(t, h.actor.rounds, RoundKeyStr(id.KeyString()))
	require.Empty(t, h.serverMessages())
}

// TestRuntimeAttemptTiming proves a restart cannot renew an accepted deadline
// or move a selected registration into a later published window.
func TestRuntimeAttemptTiming(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	budget := &admissionBudget{
		roundID: testRoundID("expiry"),
		deadline: AdmissionDeadline{
			ExpiresAt: now.Add(time.Minute),
			Closed:    true,
		},
	}
	raw, err := encodeAdmissionBudget(budget)
	require.NoError(t, err)
	later := now.Add(2 * time.Minute)
	restored, err := decodeAdmissionBudget(raw, later)
	require.NoError(t, err)
	require.Equal(t, budget.deadline, restored.deadline)
	require.True(t, restored.cutoff.Before(later))
	identity := make([]byte, 32)
	identity[0] = 1
	id, err := batchschedule.IDFromBytes(identity)
	require.NoError(t, err)
	slot := batchschedule.Slot{Opens: now, Cutoff: now.Add(time.Minute)}
	attempt := &scheduledAttempt{
		selection: batchschedule.Selection{
			ScheduleID: id,
			Cutoff:     slot.Cutoff,
		},
		slot: slot, clockOffset: -3 * time.Second, wake: now.Add(
			-time.Second,
		), margin: 2 * time.Second,
	}
	raw, err = encodeScheduledAttempt(attempt)
	require.NoError(t, err)
	decoded, err := decodeScheduledAttempt(raw)
	require.NoError(t, err)
	require.Equal(t, attempt, decoded)
	published, err := batchschedule.NewPublished(
		id, []batchschedule.Slot{slot},
	)
	require.NoError(t, err)
	published = published.WithServerTime(now).WithClockOffset(
		attempt.clockOffset,
	)
	raw, err = encodePublishedSchedule(fn.Some(published))
	require.NoError(t, err)
	again, err := decodePublishedSchedule(raw)
	require.NoError(t, err)
	require.Equal(t, fn.Some(published), again)
}

// TestRuntimeTimerReplacement checks both delayed effects and already queued
// callbacks. Neither may expire a replacement with the same logical timer ID.
func TestRuntimeTimerReplacement(t *testing.T) {
	a := &RoundClientActor{env: &ClientEnvironment{}}
	key := RoundKeyStr(testRoundID("timer").KeyString())
	first, err := a.captureDurableEffect(&StartTimeoutReq{
		RoundKey: key, Phase: TimeoutPhaseRegistration,
		Duration: time.Minute,
	})
	require.NoError(t, err)
	_, _, deadline, err := decodeDurableTimer(first.Payload)
	require.NoError(t, err)
	second, err := a.captureDurableEffect(&StartTimeoutReq{
		RoundKey: key, Phase: TimeoutPhaseRegistration, Duration: 2 *
			time.Minute,
	})
	require.NoError(t, err)
	current, err := a.currentTimerEffect(first)
	require.NoError(t, err)
	require.False(t, current)
	current, err = a.currentTimerEffect(second)
	require.NoError(t, err)
	require.True(t, current)
	callback := &TimeoutMsg{
		TimeoutID: makeTimeoutID(key, TimeoutPhaseRegistration),
		deadline:  deadline,
	}
	command, err := newDurableClientCommand(callback)
	require.NoError(t, err)
	message, err := command.message(nil)
	require.NoError(t, err)
	callback, ok := message.(*TimeoutMsg)
	require.True(t, ok)
	current, err = a.currentTimerCallback(callback)
	require.NoError(t, err)
	require.False(t, current)
}

// TestRuntimeTimerRetirement proves fired callbacks are absent from persisted
// timer state and removing a round cannot resurrect its timer on recovery.
func TestRuntimeTimerRetirement(t *testing.T) {
	h := newActorTestHarness(t)
	h.actor.durable = true
	live := testRoundID("live-timer-owner")
	gone := testRoundID("retired-timer-owner")
	round := &RoundFSM{Key: live, RoundID: live, env: h.actor.env}
	h.actor.installRoundState(t.Context(), round, &Idle{})
	t.Cleanup(round.FSM.Stop)
	h.actor.rounds[RoundKeyStr(live.KeyString())] = round
	for _, id := range []RoundID{live, gone} {
		_, err := h.actor.captureDurableEffect(&StartTimeoutReq{
			RoundKey: RoundKeyStr(id.KeyString()),
			Phase:    TimeoutPhaseRegistration,
			Duration: time.Minute,
		})
		require.NoError(t, err)
	}
	require.NoError(t, h.actor.pruneDurableTimers())
	require.Len(t, h.actor.pendingTimers, 1)
	id := makeTimeoutID(
		RoundKeyStr(
			live.KeyString(),
		),
		TimeoutPhaseRegistration,
	)
	_, _, deadline, err := decodeDurableTimer(
		h.actor.pendingTimers[id].Payload,
	)
	require.NoError(t, err)
	result := h.actor.Receive(
		t.Context(), &TimeoutMsg{
			TimeoutID: id,
			deadline:  deadline,
		},
	)
	require.NoError(t, result.Err())
	require.Empty(t, h.actor.pendingTimers)
	_, err = h.actor.captureDurableEffect(&StartTimeoutReq{
		RoundKey: RoundKeyStr(live.KeyString()),
		Phase:    TimeoutPhaseRegistration,
		Duration: time.Minute,
	})
	require.NoError(t, err)
	delete(h.actor.rounds, RoundKeyStr(live.KeyString()))
	require.NoError(t, h.actor.pruneDurableTimers())
	raw, err := h.actor.encodeActorSnapshot(t.Context())
	require.NoError(t, err)
	restored, err := decodeActorSnapshot(raw, h.actor.env)
	require.NoError(t, err)
	require.Empty(t, restored.pendingTimers)
}

// TestRuntimeEffectMetadata retains round-scoped release authority and the
// accounting distinction between an external payment and a wallet-owned exit.
func TestRuntimeEffectMetadata(t *testing.T) {
	point := wire.OutPoint{Hash: chainhash.Hash{7}, Index: 4}
	release := &ReleaseForfeitReservation{
		Outpoints: []wire.OutPoint{
			point,
		},
		RoundID: "owner-round",
	}
	effect, err := newDurableClientEffect(release, time.Now())
	require.NoError(t, err)
	decoded, err := effect.localMessage(time.Now())
	require.NoError(t, err)
	require.Equal(t, release, decoded)
	outflow := RoundLedgerOutflow{
		AmountSat:         100_000,
		IdempotencyKey:    []byte("exit"),
		ProceedsOwnWallet: true,
		ProceedsVout:      3,
	}
	raw, err := encodeDurableOutflow(outflow)
	require.NoError(t, err)
	again, err := decodeDurableOutflow(raw)
	require.NoError(t, err)
	require.Equal(t, outflow, again)
	boarding := &releaseBoardingEffect{outpoints: []wire.OutPoint{point}}
	effect, err = newDurableClientEffect(boarding, time.Now())
	require.NoError(t, err)
	decoded, err = effect.localMessage(time.Now())
	require.NoError(t, err)
	require.Equal(t, boarding, decoded)
}

// TestRuntimeBoardingCorrelation retains the asynchronous read result while
// refusing an old response that arrives after the queue head has changed.
func TestRuntimeBoardingCorrelation(t *testing.T) {
	h := newActorTestHarness(t)
	intent := h.newTestBoardingIntent()
	// The older actor harness uses an output-only pseudo transaction.
	// A persisted wire transaction needs an input to decode unambiguously.
	intent.ChainInfo.ConfTx.AddTxIn(
		wire.NewTxIn(
			&wire.OutPoint{
				Hash: chainhash.Hash{1},
			},
			nil,
			nil,
		),
	)
	reply := &boardingIntentsReply{
		FetchID: "current",
		Res: fn.Ok[wallet.WalletResp](
			&wallet.GetConfirmedBoardingIntentsResponse{
				Intents: []wallet.BoardingIntent{*intent},
			},
		),
	}
	command, err := newDurableClientCommand(reply)
	require.NoError(t, err)
	message, err := command.message(h.actor.cfg.ChainParams)
	require.NoError(t, err)
	restored, ok := message.(*boardingIntentsReply)
	require.True(t, ok)
	require.Equal(t, reply.FetchID, restored.FetchID)
	response, err := restored.Res.Unpack()
	require.NoError(t, err)
	confirmed, ok := response.(*wallet.GetConfirmedBoardingIntentsResponse)
	require.True(t, ok)
	values := confirmed.Intents
	require.Len(t, values, 1)
	require.Equal(t, intent.Outpoint, values[0].Outpoint)
	require.Equal(
		t, intent.ChainInfo.ConfTx.WitnessHash(),
		values[0].ChainInfo.ConfTx.WitnessHash(),
	)
	again, err := newDurableClientCommand(restored)
	require.NoError(t, err)
	require.Equal(t, command.Payload, again.Payload)
	h.actor.durable = true
	h.actor.boardQueue = []*actormsg.TriggerBoardMsg{{}}
	h.actor.boardFetchInFlight = true
	h.actor.boardFetchID = "replacement"
	result := h.actor.handleBoardingIntentsReply(t.Context(), restored)
	require.NoError(t, result.Err())
	require.Len(t, h.actor.boardQueue, 1)
	require.Equal(t, "replacement", h.actor.boardFetchID)
}

// TestRuntimeDeferredLedger captures the accounting batch before completion
// removes it from the live owner. A transaction retry must see the same batch.
func TestRuntimeDeferredLedger(t *testing.T) {
	id := testRoundID("deferred-ledger")
	store := &MockRoundStore{}
	store.
		On(
			"FinalizeRound",
			mock.Anything,
			id,
			chainhash.Hash{},
			ConfInfo{},
		).
		Return(nil).
		Twice()
	writes := &clientTurnWrites{}
	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg]("ledger", 4)
	a := &RoundClientActor{
		cfg: &RoundClientConfig{
			RoundStore: &bufferedRoundStore{
				RoundStore: store,
				turn:       writes,
			},
			LedgerSink: fn.Some[ledger.Sink](sink),
		},
		log: btclog.Disabled,
	}
	a.emitVTXOsReceived(
		t.Context(), &VTXOCreatedNotification{
			RoundID: id.String(),
			VTXOs: []*ClientVTXO{{
				Outpoint: wire.OutPoint{Hash: chainhash.Hash{1}}, Amount: 100_000, Origin: types.VTXOOriginRoundBoarding,
			}},
		},
	)
	require.Len(t, a.stagedLedger[id.String()], 1)
	require.NoError(
		t,
		a.onRoundComplete(
			t.Context(), id, chainhash.Hash{}, ConfInfo{},
		),
	)
	require.Empty(t, a.stagedLedger)
	require.Empty(t, sink.Messages())
	require.NoError(t, writes.apply(context.Background()))
	first := sink.Messages()
	require.Len(t, first, 1)
	require.NoError(t, writes.apply(context.Background()))
	require.Equal(t, first, sink.Messages())
	store.AssertExpectations(t)
}

// TestRuntimeJoinDisablesRoundRetry checks the committed wire request and its
// replay. Durable owners must not admit lineage they cannot checkpoint, while
// ordinary clients continue advertising replacement support.
func TestRuntimeJoinDisablesRoundRetry(t *testing.T) {
	t.Parallel()

	request := &JoinRoundRequest{}
	ordinary := request.ToProto().UnwrapOrFail(t)
	join, ok := ordinary.(*roundpb.JoinRoundRequest)
	require.True(t, ok)
	require.True(t, join.SupportsRoundRetry)

	effect, err := newDurableClientEffect(request, time.Now())
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, effect.Encode(&encoded))
	var replay durableClientEffect
	require.NoError(t, replay.Decode(&encoded))
	for _, frozen := range []*durableClientEffect{effect, &replay} {
		wire, err := frozen.serverRequest()
		require.NoError(t, err)
		message := wire.Message.ToProto().UnwrapOrFail(t)
		join, ok := message.(*roundpb.JoinRoundRequest)
		require.True(t, ok)
		require.False(t, join.SupportsRoundRetry)
	}

	// Capturing an effect does not mutate the FSM's outbox object.
	ordinary = request.ToProto().UnwrapOrFail(t)
	join, ok = ordinary.(*roundpb.JoinRoundRequest)
	require.True(t, ok)
	require.True(t, join.SupportsRoundRetry)
}
