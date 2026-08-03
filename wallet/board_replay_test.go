package wallet

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// boardReplayTestOutpoint returns a deterministic, unique outpoint for a
// per-test seed so multiple confirmed-intent fixtures in one test do not
// collide on their primary key.
func boardReplayTestOutpoint(seed byte) wire.OutPoint {
	return wire.OutPoint{
		Hash: chainhash.Hash{
			seed,
		},
		Index: 0,
	}
}

// boardReplayTestIntent builds a minimal confirmed BoardingIntent fixture
// suitable for FetchBoardingIntentsByStatus expectations in the replay
// tests.
func boardReplayTestIntent(seed byte, amount btcutil.Amount) BoardingIntent {
	op := boardReplayTestOutpoint(seed)

	return BoardingIntent{
		Outpoint: op,
		ChainInfo: BoardingChainInfo{
			OutPoint: op,
			Amount:   amount,
		},
		Status: BoardingStatusConfirmed,
	}
}

// pendingBoardIntent builds a persisted kind='board' pending intent over the
// given anchors with the given replay target, mirroring what handleBoard
// writes.
func pendingBoardIntent(t *testing.T, target uint32, requestedAt int64,
	anchors ...wire.OutPoint) PendingIntent {

	t.Helper()

	payload := &BoardIntentPayload{
		TargetVTXOCount: target,
	}

	return PendingIntent{
		ID:          NewPendingIntentID(payload, anchors),
		Payload:     payload,
		RequestedAt: requestedAt,
		Anchors:     anchors,
	}
}

// newBoardReplayTestWallet wires a wallet with a registered mock round actor
// so handleBoard's Tell to the round-service-key path can be observed. The
// MockBoardingStore is left to the caller to configure.
func newBoardReplayTestWallet(t *testing.T, store *MockBoardingStore,
	clk clock.Clock) (*Ark, *mockRoundActorBehavior) {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		// Use Background ctx because t.Context is cancelled before
		// Cleanup runs.
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	})

	roundActor := &mockRoundActorBehavior{}
	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName, roundKey, roundActor,
	)

	w := NewArk(
		nil, store, nil, nil, system, fn.None[ledger.Sink](),
		btclog.Disabled, WithClock(clk),
	)

	return w, roundActor
}

// TestHandleBoardPersistsIntentWithConfirmedAnchors pins down the H-1 fix in
// its generalized form: handleBoard writes one pending intent anchored to
// every BoardingStatusConfirmed outpoint the call admits, with the target
// VTXO count carried in the TLV payload.
func TestHandleBoardPersistsIntentWithConfirmedAnchors(t *testing.T) {
	t.Parallel()

	intentA := boardReplayTestIntent(0x01, 25_000)
	intentB := boardReplayTestIntent(0x02, 75_000)

	store := &MockBoardingStore{}
	store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent{intentA, intentB}, nil)

	fixedTime := time.Unix(1_700_000_000, 0)
	clk := clock.NewTestClock(fixedTime)

	want := pendingBoardIntent(
		t, 3, fixedTime.Unix(), intentA.Outpoint, intentB.Outpoint,
	)
	store.On(
		"UpsertPendingIntent", mock.Anything, want,
	).Return(nil)

	w, roundActor := newBoardReplayTestWallet(t, store, clk)

	result := w.Receive(t.Context(), &BoardRequest{TargetVTXOCount: 3})
	require.True(
		t, result.IsOk(),
		"Board must succeed when round Tell succeeds; got %v",
		result.Err(),
	)

	store.AssertExpectations(t)

	// The Tell to the round actor is asynchronous via the service-key
	// router, so the actor system needs a moment to dispatch it. Poll
	// until the mock observes the call instead of sleeping for a fixed
	// duration.
	require.Eventually(t, func() bool {
		return roundActor.TriggerBoardCalls() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"round actor must receive exactly one TriggerBoardMsg")
}

// TestHandleBoardPersistFailureSkipsRoundTell verifies the persist-before-Tell
// invariant: if the wallet cannot persist the pending intent, the round actor
// MUST NOT receive a TriggerBoardMsg. Otherwise a crash between Tell and
// persist would leave the round holding an in-memory intent with no on-disk
// marker — the exact failure mode #416 reported.
func TestHandleBoardPersistFailureSkipsRoundTell(t *testing.T) {
	t.Parallel()

	intent := boardReplayTestIntent(0x11, 50_000)

	store := &MockBoardingStore{}
	store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent{intent}, nil)

	wantErr := errors.New("simulated persist failure")
	store.On(
		"UpsertPendingIntent", mock.Anything,
		mock.Anything,
	).Return(wantErr)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, roundActor := newBoardReplayTestWallet(t, store, clk)

	result := w.Receive(t.Context(), &BoardRequest{TargetVTXOCount: 0})
	require.True(
		t, result.IsErr(),
		"Board must fail when persist fails so caller retries",
	)
	require.ErrorContains(t, result.Err(), "persist pending board intent")
	require.ErrorIs(t, result.Err(), wantErr)

	require.Equal(
		t, 0, roundActor.TriggerBoardCalls(),
		"round actor must NOT be Told when persist failed",
	)
}

// TestReplayClearsAllIntentsWhenNoAnchorIsStillConfirmed verifies the H-1
// stale-row sweep: when every persisted intent anchors only outpoints that
// have already moved out of Confirmed (e.g. the round adopted, swept, or
// failed), the wallet wipes the board kind and skips the self-Tell. This is
// the "round already completed" branch that prevents a stale
// target_vtxo_count from re-firing against a fresh, unrelated deposit.
func TestReplayClearsAllIntentsWhenNoAnchorIsStillConfirmed(t *testing.T) {
	t.Parallel()

	op := boardReplayTestOutpoint(0x21)
	pending := []PendingIntent{
		pendingBoardIntent(t, 5, 1_700_000_000, op),
	}

	store := &MockBoardingStore{}
	store.On(
		"ListPendingIntents", mock.Anything, PendingIntentKindBoard,
	).Return(pending, nil)
	store.On(
		"ListPendingIntents", mock.Anything,
		PendingIntentKindSendOnChain,
	).Return([]PendingIntent(nil), nil)

	// Confirmed set excludes the pending intent's anchor, so every
	// pending intent is stale.
	store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent(nil), nil)

	store.On(
		"ClearPendingIntentsByKind", mock.Anything,
		PendingIntentKindBoard,
	).Return(nil)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, roundActor := newBoardReplayTestWallet(t, store, clk)

	// Run the replay through the Ask handler. Inject a TellOnly
	// self-ref that records whether the wallet self-Tells anything;
	// for the all-stale branch it must not.
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
	require.False(t, resp.Replayed, "all-stale replay must report false")

	store.AssertExpectations(t)
	require.Equal(
		t, 0, roundActor.TriggerBoardCalls(),
		"no round Tell when every pending intent is stale",
	)
}

// TestReplaySelfTellsBoardWhenAnchorIsStillConfirmed verifies the H-2 fix:
// when at least one persisted anchor points at a still-Confirmed outpoint,
// the wallet self-Tells a BoardRequest carrying the persisted target VTXO
// count decoded from the intent payload. FIFO ordering of the wallet's own
// mailbox guarantees this replay is dispatched before any gRPC-issued
// BoardRequest, closing the startup race.
func TestReplaySelfTellsBoardWhenAnchorIsStillConfirmed(t *testing.T) {
	t.Parallel()

	liveOp := boardReplayTestOutpoint(0x31)
	staleOp := boardReplayTestOutpoint(0x32)

	pending := []PendingIntent{
		pendingBoardIntent(t, 4, 1_700_000_000, liveOp, staleOp),
	}

	store := &MockBoardingStore{}
	store.On(
		"ListPendingIntents", mock.Anything, PendingIntentKindBoard,
	).Return(pending, nil)
	store.On(
		"ListPendingIntents", mock.Anything,
		PendingIntentKindSendOnChain,
	).Return([]PendingIntent(nil), nil)

	// Only liveOp is in the Confirmed set; staleOp is stale.
	store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent{
		{
			Outpoint: liveOp,
			ChainInfo: BoardingChainInfo{
				OutPoint: liveOp,
				Amount:   25_000,
			},
			Status: BoardingStatusConfirmed,
		},
	}, nil)

	// In the replay path the wallet must NOT bulk-clear: at least one
	// anchor is live. Stale-anchor cleanup happens via the
	// round-adoption transition (see
	// db.RoundPersistenceStore.CommitState).
	store.AssertNotCalled(
		t, "ClearPendingIntentsByKind", mock.Anything, mock.Anything,
	)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, _ := newBoardReplayTestWallet(t, store, clk)

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
	require.True(t, resp.Replayed, "live replay must report true")

	// One BoardRequest must be enqueued on the self-ref carrying the
	// persisted target_vtxo_count. AwaitMessage drains the channel
	// helper with a bounded timeout so the test fails fast rather than
	// hanging if replay silently skipped the Tell.
	msg, ok := selfRef.AwaitMessage(2 * time.Second)
	require.True(t, ok, "expected replay to self-Tell a BoardRequest")
	boardReq, ok := msg.(*BoardRequest)
	require.True(t, ok, "expected *BoardRequest, got %T", msg)
	require.Equal(t, uint32(4), boardReq.TargetVTXOCount)

	store.AssertExpectations(t)
}

// TestBoardIntentIDDistinguishesPolicy verifies the intent-ID digest is
// policy-sensitive: two board intents over the same anchors and target that
// differ only by their custom policy (or by an empty vs a set policy) hash to
// distinct IDs. Without this, a custom-policy board and a standard board over
// the same outpoints would collide and upsert over each other in the outbox.
func TestBoardIntentIDDistinguishesPolicy(t *testing.T) {
	t.Parallel()

	anchors := []wire.OutPoint{boardReplayTestOutpoint(0x71)}

	standard := &BoardIntentPayload{TargetVTXOCount: 1}
	customA := &BoardIntentPayload{
		TargetVTXOCount: 1,
		PolicyTemplate: []byte{
			0x01,
			0xaa,
		},
	}
	customB := &BoardIntentPayload{
		TargetVTXOCount: 1,
		PolicyTemplate: []byte{
			0x01,
			0xbb,
		},
	}
	customBWithScript := &BoardIntentPayload{
		TargetVTXOCount: 1,
		PolicyTemplate: []byte{
			0x01,
			0xbb,
		},
		PkScript: []byte{
			0x02,
		},
	}

	standardID := NewPendingIntentID(standard, anchors)
	customAID := NewPendingIntentID(customA, anchors)
	customBID := NewPendingIntentID(customB, anchors)
	customBScriptID := NewPendingIntentID(customBWithScript, anchors)

	require.NotEqual(
		t, standardID, customAID,
		"standard and custom policy must not collide",
	)
	require.NotEqual(
		t, customAID, customBID,
		"distinct policy templates must not collide",
	)
	require.NotEqual(
		t, customBID, customBScriptID,
		"pinned pk_script must affect the intent ID",
	)

	// The digest is deterministic for identical payloads.
	require.Equal(t, customAID, NewPendingIntentID(customA, anchors))
}

// TestReplayCarriesCustomPolicyToBoardRequest verifies that a persisted board
// intent's custom VTXO policy survives restart replay: the self-Telled
// BoardRequest carries the same PolicyTemplate and PkScript, so replay
// recreates the same custom-owned output instead of silently re-boarding into
// the standard collaborative shape.
func TestReplayCarriesCustomPolicyToBoardRequest(t *testing.T) {
	t.Parallel()

	liveOp := boardReplayTestOutpoint(0x61)

	policyTemplate := []byte{0x01, 0xaa, 0xbb, 0xcc}
	pkScript := bytes.Repeat([]byte{0x02}, 34)

	payload := &BoardIntentPayload{
		TargetVTXOCount: 3,
		PolicyTemplate:  policyTemplate,
		PkScript:        pkScript,
	}
	anchors := []wire.OutPoint{liveOp}
	pending := []PendingIntent{{
		ID:          NewPendingIntentID(payload, anchors),
		Payload:     payload,
		RequestedAt: 1_700_000_000,
		Anchors:     anchors,
	}}

	store := &MockBoardingStore{}
	store.On(
		"ListPendingIntents", mock.Anything, PendingIntentKindBoard,
	).Return(pending, nil)
	store.On(
		"ListPendingIntents", mock.Anything,
		PendingIntentKindSendOnChain,
	).Return([]PendingIntent(nil), nil)
	store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent{
		{
			Outpoint: liveOp,
			ChainInfo: BoardingChainInfo{
				OutPoint: liveOp,
				Amount:   25_000,
			},
			Status: BoardingStatusConfirmed,
		},
	}, nil)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, _ := newBoardReplayTestWallet(t, store, clk)

	selfRef := actor.NewChannelTellOnlyRef[WalletMsg](
		"test-wallet-self", 4,
	)
	w.selfRef = selfRef

	result := w.Receive(t.Context(), &ReplayPendingIntentsRequest{})
	require.True(
		t, result.IsOk(),
		"replay must succeed; got %v", result.Err(),
	)

	msg, ok := selfRef.AwaitMessage(2 * time.Second)
	require.True(t, ok, "expected replay to self-Tell a BoardRequest")
	boardReq, ok := msg.(*BoardRequest)
	require.True(t, ok, "expected *BoardRequest, got %T", msg)
	require.Equal(t, uint32(3), boardReq.TargetVTXOCount)
	require.Equal(t, policyTemplate, boardReq.PolicyTemplate)
	require.Equal(t, pkScript, boardReq.PkScript)

	store.AssertExpectations(t)
}

// TestReplayDeletesStaleBoardIntentAlongsideLiveOne verifies that a fully
// stale board intent (all anchors no longer Confirmed) is deleted by ID
// during replay even when another intent is still live, rather than
// lingering until a later all-stale start. The live intent still drives a
// self-Tell.
func TestReplayDeletesStaleBoardIntentAlongsideLiveOne(t *testing.T) {
	t.Parallel()

	liveOp := boardReplayTestOutpoint(0x51)
	staleOp := boardReplayTestOutpoint(0x52)

	liveIntent := pendingBoardIntent(t, 2, 1_700_000_000, liveOp)
	staleIntent := pendingBoardIntent(t, 7, 1_700_000_500, staleOp)

	store := &MockBoardingStore{}
	store.On(
		"ListPendingIntents", mock.Anything, PendingIntentKindBoard,
	).Return([]PendingIntent{liveIntent, staleIntent}, nil)
	store.On(
		"ListPendingIntents", mock.Anything,
		PendingIntentKindSendOnChain,
	).Return([]PendingIntent(nil), nil)

	// Only liveOp remains Confirmed; staleOp's intent is fully stale.
	store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent{
		{
			Outpoint: liveOp,
			ChainInfo: BoardingChainInfo{
				OutPoint: liveOp,
				Amount:   25_000,
			},
			Status: BoardingStatusConfirmed,
		},
	}, nil)

	// The stale intent is deleted by its own ID; the live one is not.
	store.On(
		"DeletePendingIntent", mock.Anything, staleIntent.ID,
	).Return(nil)

	// At least one intent is live, so the wallet must NOT bulk-clear.
	store.AssertNotCalled(
		t, "ClearPendingIntentsByKind", mock.Anything, mock.Anything,
	)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, _ := newBoardReplayTestWallet(t, store, clk)

	selfRef := actor.NewChannelTellOnlyRef[WalletMsg](
		"test-wallet-self", 4,
	)
	w.selfRef = selfRef

	result := w.Receive(t.Context(), &ReplayPendingIntentsRequest{})
	require.True(
		t, result.IsOk(),
		"replay must succeed; got %v", result.Err(),
	)

	// The live intent still drives a self-Tell with its target.
	msg, ok := selfRef.AwaitMessage(2 * time.Second)
	require.True(t, ok, "expected replay to self-Tell a BoardRequest")
	boardReq, ok := msg.(*BoardRequest)
	require.True(t, ok, "expected *BoardRequest, got %T", msg)
	require.Equal(t, uint32(2), boardReq.TargetVTXOCount)

	store.AssertExpectations(t)
	store.AssertCalled(
		t, "DeletePendingIntent", mock.Anything, staleIntent.ID,
	)
}

// TestHandleBoardNoPersistSkipsUpsert verifies the CLI/RPC opt-out: when
// BoardRequest.NoPersist is true, the wallet does NOT write to the
// pending-intents outbox. The round actor still receives TriggerBoardMsg so
// the boarding flow proceeds; a daemon restart between admission and round
// seal will silently drop the request — the user's explicit choice.
func TestHandleBoardNoPersistSkipsUpsert(t *testing.T) {
	t.Parallel()

	intent := boardReplayTestIntent(0x41, 60_000)

	store := &MockBoardingStore{}
	store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent{intent}, nil)

	// Explicitly refuse any UpsertPendingIntent call; if the wallet
	// writes despite NoPersist, the mock's strict assertion fails this
	// test.
	store.AssertNotCalled(
		t, "UpsertPendingIntent", mock.Anything, mock.Anything,
	)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, roundActor := newBoardReplayTestWallet(t, store, clk)

	result := w.Receive(t.Context(), &BoardRequest{
		TargetVTXOCount: 2,
		NoPersist:       true,
	})
	require.True(
		t, result.IsOk(),
		"Board with NoPersist must still succeed; got %v", result.Err(),
	)

	store.AssertExpectations(t)
	store.AssertNotCalled(
		t, "UpsertPendingIntent", mock.Anything, mock.Anything,
	)

	require.Eventually(t, func() bool {
		return roundActor.TriggerBoardCalls() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"round actor must still receive TriggerBoardMsg with "+
			"NoPersist set")
}

// TestReplayEmptyStoreIsNoOp verifies that a wallet with no persisted
// intents skips the replay path entirely — no clear, no self-Tell, no
// FetchBoardingIntentsByStatus. This is the common happy path on a fresh
// daemon start.
func TestReplayEmptyStoreIsNoOp(t *testing.T) {
	t.Parallel()

	store := &MockBoardingStore{}
	store.On(
		"ListPendingIntents", mock.Anything, PendingIntentKindBoard,
	).Return([]PendingIntent(nil), nil)
	store.On(
		"ListPendingIntents", mock.Anything,
		PendingIntentKindSendOnChain,
	).Return([]PendingIntent(nil), nil)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, _ := newBoardReplayTestWallet(t, store, clk)

	selfRef := actor.NewChannelTellOnlyRef[WalletMsg](
		"test-wallet-self", 1,
	)
	w.selfRef = selfRef

	result := w.Receive(t.Context(), &ReplayPendingIntentsRequest{})
	require.True(
		t, result.IsOk(),
		"replay must succeed; got %v", result.Err(),
	)

	resp, ok := result.UnwrapOrFail(t).(*ReplayPendingIntentsResponse)
	require.True(t, ok)
	require.False(t, resp.Replayed)

	store.AssertExpectations(t)
	store.AssertNotCalled(
		t, "FetchBoardingIntentsByStatus", mock.Anything, mock.Anything,
	)
	store.AssertNotCalled(
		t, "ClearPendingIntentsByKind", mock.Anything, mock.Anything,
	)

	_, gotMsg := selfRef.AwaitMessage(50 * time.Millisecond)
	require.False(t, gotMsg, "empty store must not self-Tell")
}
