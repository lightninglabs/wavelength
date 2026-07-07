package wallet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// sendReplayDestScript is a fixed P2TR-shaped destination script for the
// send replay tests.
var sendReplayDestScript = append(
	[]byte{0x51, 0x20}, make([]byte, 32)...,
)

// newSendReplayTestWallet wires a wallet with mock VTXO manager, mock round
// actor, mock boarding store, stub backend (for change-key derivation), and
// a static VTXO reader.
func newSendReplayTestWallet(t *testing.T, mgr *mockVTXOManagerBehavior,
	roundActor *mockRoundActorBehavior, store *MockBoardingStore,
	descs map[wire.OutPoint]*VTXODescriptor) *Ark {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		// Use Background context because t.Context() is already
		// cancelled by the time cleanup runs.
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	})

	mgrKey := actormsg.VTXOManagerServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.VTXOManagerServiceKeyName, mgrKey, mgr,
	)

	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName, roundKey, roundActor,
	)

	return NewArk(
		&stubBackend{}, store, testVTXOReader(descs), nil, system,
		fn.None[ledger.Sink](), btclog.Disabled,
		WithClock(
			clock.NewTestClock(
				time.Unix(1_700_000_000, 0),
			),
		),
	)
}

// boundedSendIntent builds a persisted kind='send_onchain' pending intent
// in bounded mode over the given anchors, mirroring what handleSendOnChain
// writes.
func boundedSendIntent(t *testing.T, operatorKey *btcec.PublicKey,
	target btcutil.Amount, anchors ...wire.OutPoint) PendingIntent {

	t.Helper()

	payload := &SendOnChainIntentPayload{
		DestinationPkScript: sendReplayDestScript,
		TargetAmountSat:     target,
		OperatorKey:         operatorKey,
		VTXOExitDelay:       144,
		DustLimit:           330,
	}

	return PendingIntent{
		ID:          NewPendingIntentID(payload, anchors),
		Payload:     payload,
		RequestedAt: 1_700_000_000,
		Anchors:     anchors,
	}
}

// TestSendOnChainIntentIDSensitivity verifies the send payload's canonical
// digest feeds the intent ID: every field participates, so flipping any one
// (the SweepAll bit in particular, the #634 vector) changes the ID. This is
// what makes the replay-time tamper guard work now that fields are stored as
// columns rather than an opaque blob.
func TestSendOnChainIntentIDSensitivity(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	op := testOutpoint(0)
	base := &SendOnChainIntentPayload{
		DestinationPkScript: sendReplayDestScript,
		TargetAmountSat:     30_000,
		OperatorKey:         priv.PubKey(),
		VTXOExitDelay:       144,
		DustLimit:           330,
	}
	baseID := NewPendingIntentID(base, []wire.OutPoint{op})

	// Re-deriving from an identical payload is stable.
	require.Equal(
		t, baseID,
		NewPendingIntentID(
			&SendOnChainIntentPayload{
				DestinationPkScript: sendReplayDestScript,
				TargetAmountSat:     30_000,
				OperatorKey:         priv.PubKey(),
				VTXOExitDelay:       144,
				DustLimit:           330,
			},
			[]wire.OutPoint{op},
		),
	)

	// Flipping the sweep-all bit (the #634 overpay vector) must change
	// the ID so a tampered detail row fails the replay integrity check.
	mutated := *base
	mutated.SweepAll = true
	require.NotEqual(
		t, baseID,
		NewPendingIntentID(
			&mutated, []wire.OutPoint{op},
		),
	)

	// Each remaining field also participates in the digest.
	for _, mutate := range []func(p *SendOnChainIntentPayload){
		func(p *SendOnChainIntentPayload) {
			p.TargetAmountSat = 30_001
		},
		func(p *SendOnChainIntentPayload) {
			p.DestinationPkScript = append(
				[]byte{0x51, 0x20}, make([]byte, 31)...,
			)
		},
		func(p *SendOnChainIntentPayload) {
			p.OperatorKey = nil
		},
		func(p *SendOnChainIntentPayload) {
			p.VTXOExitDelay = 145
		},
		func(p *SendOnChainIntentPayload) {
			p.DustLimit = 331
		},
	} {
		m := *base
		mutate(&m)
		require.NotEqual(
			t, baseID,
			NewPendingIntentID(
				&m, []wire.OutPoint{op},
			),
			"every payload field must affect the intent ID",
		)
	}
}

// TestHandleSendOnChainPersistsIntentBeforeRegistration verifies the
// persist-before-publish invariant for sends: a successful bounded send
// writes one pending intent anchored to the reserved forfeit outpoints
// BEFORE the round actor sees RegisterIntentMsg, and the row survives a
// successful registration (it is cleared only by round adoption via
// CommitState).
func TestHandleSendOnChainPersistsIntentBeforeRegistration(t *testing.T) {
	t.Parallel()

	opA := testOutpoint(1)
	opB := testOutpoint(2)

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: opA,
					Amount:   40_000,
				},
				{
					Outpoint: opB,
					Amount:   25_000,
				},
			},
			TotalSelected: 65_000,
		},
	}
	roundActor := &mockRoundActorBehavior{}

	store := &MockBoardingStore{}
	var persisted PendingIntent
	store.On(
		"UpsertPendingIntent", mock.Anything, mock.Anything,
	).Run(func(args mock.Arguments) {
		//nolint:forcetypeassert
		persisted = args.Get(1).(PendingIntent)

		// The round actor must not have been Asked yet when the
		// persist lands: persist-before-publish.
		require.Equal(
			t, 0, roundActor.registerCalls,
			"outbox persist must happen before the round Ask",
		)
	}).Return(nil)

	w := newSendReplayTestWallet(t, mgr, roundActor, store, nil)

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	result := w.Receive(t.Context(), &SendOnChainRequest{
		DestinationPkScript: sendReplayDestScript,
		TargetAmountSat:     30_000,
		OperatorFee:         1_000,
		DustLimit:           330,
		OperatorKey:         priv.PubKey(),
		VTXOExitDelay:       144,
	})
	require.True(
		t, result.IsOk(),
		"send must succeed; got %v", result.Err(),
	)

	store.AssertExpectations(t)
	store.AssertNotCalled(
		t, "DeletePendingIntent", mock.Anything, mock.Anything,
	)

	require.Equal(t, PendingIntentKindSendOnChain, persisted.Kind())
	require.ElementsMatch(
		t, []wire.OutPoint{opA, opB}, persisted.Anchors,
		"anchors must be exactly the reserved forfeit outpoints",
	)

	payload, ok := persisted.Payload.(*SendOnChainIntentPayload)
	require.True(t, ok, "persisted payload must be a send payload")
	require.EqualValues(t, 30_000, payload.TargetAmountSat)
	require.False(t, payload.SweepAll)
	require.Equal(t, sendReplayDestScript, payload.DestinationPkScript)

	require.Equal(t, 1, roundActor.registerCalls)

	// The submitted response surfaces the persisted intent id so callers
	// can use it as the operation's stable handle (issue #610).
	walletResp, err := result.Unpack()
	require.NoError(t, err)
	sendResp, ok := walletResp.(*SendOnChainResponse)
	require.True(t, ok, "response must be a SendOnChainResponse")
	require.NotEqual(t, PendingIntentID{}, sendResp.IntentID)
	require.Equal(t, persisted.ID, sendResp.IntentID)
}

// TestHandleSendOnChainRoundRejectionDeletesIntent verifies that a round
// rejection deletes the freshly persisted outbox row alongside the
// reservation release: the caller sees an error, so the send must not
// silently resurrect on the next start.
func TestHandleSendOnChainRoundRejectionDeletesIntent(t *testing.T) {
	t.Parallel()

	opA := testOutpoint(3)

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: opA,
					Amount:   40_000,
				},
			},
			TotalSelected: 40_000,
		},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{
		registerErr: errors.New("simulated round rejection"),
	}

	store := &MockBoardingStore{}
	store.On(
		"UpsertPendingIntent", mock.Anything, mock.Anything,
	).Return(nil)
	store.On(
		"DeletePendingIntent", mock.Anything, mock.Anything,
	).Return(nil)

	w := newSendReplayTestWallet(t, mgr, roundActor, store, nil)

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	result := w.Receive(t.Context(), &SendOnChainRequest{
		DestinationPkScript: sendReplayDestScript,
		TargetAmountSat:     30_000,
		OperatorFee:         1_000,
		DustLimit:           330,
		OperatorKey:         priv.PubKey(),
		VTXOExitDelay:       144,
	})
	require.True(
		t, result.IsErr(),
		"send must fail when the round rejects the intent",
	)

	store.AssertExpectations(t)
	require.Equal(
		t, 1, mgr.forfeitReleaseCalls,
		"reservation must be released on rejection",
	)
}

// TestHandleSendOnChainCallerCancelKeepsIntent pins the P1 fix: when the
// caller's RPC ctx is canceled after the outbox row is persisted but before
// the round Ask returns, the row and the forfeit reservation must survive —
// the detached Ask may still land the intent, so deleting here would reopen
// the #660 window. We force the race deterministically by canceling the ctx
// from inside the UpsertPendingIntent hook, so the subsequent Await(ctx)
// observes cancellation regardless of the (detached) round outcome.
func TestHandleSendOnChainCallerCancelKeepsIntent(t *testing.T) {
	t.Parallel()

	opA := testOutpoint(9)

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: opA,
					Amount:   40_000,
				},
			},
			TotalSelected: 40_000,
		},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	registerGate := make(chan struct{})
	roundActor := &mockRoundActorBehavior{
		registerGate: registerGate,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &MockBoardingStore{}
	store.On(
		"UpsertPendingIntent", mock.Anything, mock.Anything,
	).Run(func(_ mock.Arguments) {
		// Cancel the caller ctx during the persist so the round
		// Await(ctx) below sees a canceled caller, exercising the
		// caller-cancel branch deterministically.
		cancel()
	}).Return(nil)

	w := newSendReplayTestWallet(t, mgr, roundActor, store, nil)
	t.Cleanup(func() {
		close(registerGate)
	})

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	result := w.Receive(ctx, &SendOnChainRequest{
		DestinationPkScript: sendReplayDestScript,
		TargetAmountSat:     30_000,
		OperatorFee:         1_000,
		DustLimit:           330,
		OperatorKey:         priv.PubKey(),
		VTXOExitDelay:       144,
	})
	require.True(
		t, result.IsErr(),
		"caller-cancel must surface as an error",
	)
	require.ErrorContains(t, result.Err(), "await canceled")

	// The row must NOT be deleted and the reservation must NOT be
	// released: replay (or the round checkpoint) reconciles the
	// in-flight send.
	store.AssertNotCalled(
		t, "DeletePendingIntent", mock.Anything, mock.Anything,
	)
	require.Equal(
		t, 0, mgr.forfeitReleaseCalls,
		"reservation must survive caller cancellation",
	)
}

// TestReplaySendOnChainStaleAnchorsDropIntent verifies the staleness
// branch: when the exact-anchor reservation fails (some anchor is no
// longer a live VTXO), the intent is deleted rather than left to fail on
// every future start, and the round actor is never Asked.
func TestReplaySendOnChainStaleAnchorsDropIntent(t *testing.T) {
	t.Parallel()

	opA := testOutpoint(4)

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveErr: errors.New("vtxo not in live state"),
	}
	roundActor := &mockRoundActorBehavior{}

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	intent := boundedSendIntent(t, priv.PubKey(), 30_000, opA)

	store := &MockBoardingStore{}
	store.On(
		"DeletePendingIntent", mock.Anything, intent.ID,
	).Return(nil)

	w := newSendReplayTestWallet(t, mgr, roundActor, store, nil)

	result := w.Receive(t.Context(), &ReplaySendOnChainIntent{
		Intent: intent,
	})
	require.True(
		t, result.IsOk(),
		"stale replay must resolve cleanly; got %v", result.Err(),
	)

	store.AssertExpectations(t)
	require.Equal(
		t, 0, roundActor.registerCalls,
		"stale intent must not reach the round actor",
	)
}

// TestReplaySendOnChainReRegistersExactAnchors verifies the live replay
// path end-to-end inside the wallet: the handler re-reserves exactly the
// persisted anchors, rebuilds the bounded intent (fixed leave + change
// VTXO) from the TLV payload with canonical amounts from the VTXO store,
// and registers with TriggerRegistration=true since no RPC caller exists
// to fire the second step.
func TestReplaySendOnChainReRegistersExactAnchors(t *testing.T) {
	t.Parallel()

	opA := testOutpoint(5)
	opB := testOutpoint(6)

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	intent := boundedSendIntent(
		t, priv.PubKey(), 30_000, opA, opB,
	)

	descs := map[wire.OutPoint]*VTXODescriptor{
		opA: {
			Outpoint: opA,
			Amount:   40_000,
		},
		opB: {
			Outpoint: opB,
			Amount:   25_000,
		},
	}

	store := &MockBoardingStore{}

	w := newSendReplayTestWallet(t, mgr, roundActor, store, descs)

	result := w.Receive(t.Context(), &ReplaySendOnChainIntent{
		Intent: intent,
	})
	require.True(
		t, result.IsOk(),
		"live replay must succeed; got %v", result.Err(),
	)

	// The row must NOT be deleted: only round adoption (CommitState)
	// clears a successfully re-registered intent.
	store.AssertNotCalled(
		t, "DeletePendingIntent", mock.Anything, mock.Anything,
	)

	require.Equal(t, 1, roundActor.registerCalls)
	captured := roundActor.capturedIntent
	require.NotNil(t, captured)
	require.True(
		t, captured.TriggerRegistration,
		"replay must trigger registration itself",
	)

	gotForfeits := make([]wire.OutPoint, 0, len(captured.Forfeits))
	for _, f := range captured.Forfeits {
		gotForfeits = append(gotForfeits, *f.VTXOOutpoint)
	}
	require.ElementsMatch(
		t, []wire.OutPoint{opA, opB}, gotForfeits,
		"forfeits must be exactly the persisted anchors",
	)

	require.Len(t, captured.Leaves, 1)
	require.EqualValues(t, 30_000, captured.Leaves[0].Output.Value)
	require.False(t, captured.Leaves[0].IsChange)

	require.Len(t, captured.VTXOs, 1)
	require.EqualValues(t, 65_000-30_000, captured.VTXOs[0].Amount)
	require.True(t, captured.VTXOs[0].IsChange)
}

// TestReplayPendingIntentsDispatchesSendKind verifies the registry wiring:
// a persisted send_onchain intent surfaces from the generic replay Ask as
// a ReplaySendOnChainIntent self-Tell into the wallet's own mailbox.
func TestReplayPendingIntentsDispatchesSendKind(t *testing.T) {
	t.Parallel()

	opA := testOutpoint(7)

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	intent := boundedSendIntent(t, priv.PubKey(), 30_000, opA)

	store := &MockBoardingStore{}
	store.On(
		"ListPendingIntents", mock.Anything, PendingIntentKindBoard,
	).Return([]PendingIntent(nil), nil)
	store.On(
		"ListPendingIntents", mock.Anything,
		PendingIntentKindSendOnChain,
	).Return([]PendingIntent{intent}, nil)

	mgr := &mockVTXOManagerBehavior{}
	roundActor := &mockRoundActorBehavior{}
	w := newSendReplayTestWallet(t, mgr, roundActor, store, nil)

	selfRef := actor.NewChannelTellOnlyRef[WalletMsg](
		"test-wallet-self", 4,
	)
	w.selfRef = selfRef

	result := w.Receive(t.Context(), &ReplayPendingIntentsRequest{})
	require.True(
		t, result.IsOk(),
		"replay must succeed; got %v", result.Err(),
	)

	resp, ok := result.UnwrapOrFail(t).(*ReplayPendingIntentsResponse)
	require.True(t, ok)
	require.True(t, resp.Replayed)

	msg, ok := selfRef.AwaitMessage(2 * time.Second)
	require.True(
		t, ok, "expected replay to self-Tell a ReplaySendOnChainIntent",
	)
	replayMsg, ok := msg.(*ReplaySendOnChainIntent)
	require.True(t, ok, "expected *ReplaySendOnChainIntent, got %T", msg)
	require.Equal(t, intent.ID, replayMsg.Intent.ID)

	store.AssertExpectations(t)
}

// TestReplaySendOnChainTamperedPayloadDropsIntent pins the integrity gate
// against a #634-style regression through the outbox: a persisted bounded
// send whose payload was mutated after the fact (here, the SweepAll bit
// flipped, which would drain the full forfeit set to the destination) must
// fail the intent-ID hash check and be dropped before any reservation or
// round registration happens.
func TestReplaySendOnChainTamperedPayloadDropsIntent(t *testing.T) {
	t.Parallel()

	opA := testOutpoint(8)

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Build a legitimate bounded intent, then swap in a tampered
	// payload (sweep-all mode flipped) while keeping the original ID, as
	// if the persisted detail row had been mutated under the daemon.
	intent := boundedSendIntent(t, priv.PubKey(), 25_000, opA)
	intent.Payload = &SendOnChainIntentPayload{
		DestinationPkScript: sendReplayDestScript,
		SweepAll:            true,
	}

	mgr := &mockVTXOManagerBehavior{}
	roundActor := &mockRoundActorBehavior{}

	store := &MockBoardingStore{}
	store.On(
		"DeletePendingIntent", mock.Anything, intent.ID,
	).Return(nil)

	w := newSendReplayTestWallet(t, mgr, roundActor, store, nil)

	result := w.Receive(t.Context(), &ReplaySendOnChainIntent{
		Intent: intent,
	})
	require.True(
		t, result.IsOk(),
		"tampered replay must resolve cleanly; got %v", result.Err(),
	)

	store.AssertExpectations(t)
	require.Equal(
		t, 0, mgr.forfeitReserveCalls,
		"tampered intent must never reach the VTXO manager",
	)
	require.Equal(
		t, 0, roundActor.registerCalls,
		"tampered intent must never reach the round actor",
	)
}
