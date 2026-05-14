package wallet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
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

// TestHandleBoardPersistsOneRowPerConfirmedOutpoint pins down the H-1 fix:
// handleBoard writes one PendingBoardRequest row per BoardingStatusConfirmed
// intent the call admits, bound to the confirmed outpoint. Each row carries
// the same target_vtxo_count and requested_at_unix.
func TestHandleBoardPersistsOneRowPerConfirmedOutpoint(t *testing.T) {
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

	want := []PendingBoardRequest{
		{
			Outpoint:        intentA.Outpoint,
			TargetVTXOCount: 3,
			RequestedAt:     fixedTime.Unix(),
		},
		{
			Outpoint:        intentB.Outpoint,
			TargetVTXOCount: 3,
			RequestedAt:     fixedTime.Unix(),
		},
	}
	store.On(
		"UpsertPendingBoardRequests", mock.Anything, want,
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
// invariant: if the wallet cannot persist the pending rows, the round actor
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
		"UpsertPendingBoardRequests", mock.Anything,
		mock.Anything,
	).Return(wantErr)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, roundActor := newBoardReplayTestWallet(t, store, clk)

	result := w.Receive(t.Context(), &BoardRequest{TargetVTXOCount: 0})
	require.True(
		t, result.IsErr(),
		"Board must fail when persist fails so caller retries",
	)
	require.ErrorContains(t, result.Err(), "persist pending board request")
	require.ErrorIs(t, result.Err(), wantErr)

	require.Equal(
		t, 0, roundActor.TriggerBoardCalls(),
		"round actor must NOT be Told when persist failed",
	)
}

// TestReplayClearsAllRowsWhenNoOutpointIsStillConfirmed verifies the H-1
// stale-row sweep: when every persisted row points at an outpoint that has
// already moved out of Confirmed (e.g. the round adopted, swept, or
// failed), the wallet wipes the table and skips the self-Tell. This is the
// "round already completed" branch that prevents a stale target_vtxo_count
// from re-firing against a fresh, unrelated deposit.
func TestReplayClearsAllRowsWhenNoOutpointIsStillConfirmed(t *testing.T) {
	t.Parallel()

	op := boardReplayTestOutpoint(0x21)
	pending := []PendingBoardRequest{
		{
			Outpoint:        op,
			TargetVTXOCount: 5,
			RequestedAt:     1_700_000_000,
		},
	}

	store := &MockBoardingStore{}
	store.On(
		"ListPendingBoardRequests", mock.Anything,
	).Return(pending, nil)

	// Confirmed set excludes the pending row's outpoint, so every
	// pending row is stale.
	store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent(nil), nil)

	store.On(
		"ClearAllPendingBoardRequests", mock.Anything,
	).Return(nil)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, roundActor := newBoardReplayTestWallet(t, store, clk)

	// Run only the replay path. Pass a TellOnly self-ref that records
	// whether the wallet self-Tells anything; for the all-stale branch
	// it must not.
	selfRef := actor.NewChannelTellOnlyRef[WalletMsg](
		"test-wallet-self", 4,
	)

	err := w.replayPendingBoardOnStart(t.Context(), selfRef)
	require.NoError(t, err)

	store.AssertExpectations(t)
	require.Equal(
		t, 0, roundActor.TriggerBoardCalls(),
		"no round Tell when every pending row is stale",
	)
}

// TestReplaySelfTellsBoardWhenOutpointIsStillConfirmed verifies the H-2 fix:
// when at least one persisted row points at a still-Confirmed outpoint, the
// wallet self-Tells a BoardRequest carrying the persisted target_vtxo_count.
// FIFO ordering of the wallet's own mailbox guarantees this replay is
// dispatched before any gRPC-issued BoardRequest, closing the startup race.
func TestReplaySelfTellsBoardWhenOutpointIsStillConfirmed(t *testing.T) {
	t.Parallel()

	liveOp := boardReplayTestOutpoint(0x31)
	staleOp := boardReplayTestOutpoint(0x32)

	pending := []PendingBoardRequest{
		{
			Outpoint:        liveOp,
			TargetVTXOCount: 4,
			RequestedAt:     1_700_000_000,
		},
		{
			Outpoint:        staleOp,
			TargetVTXOCount: 4,
			RequestedAt:     1_700_000_000,
		},
	}

	store := &MockBoardingStore{}
	store.On(
		"ListPendingBoardRequests", mock.Anything,
	).Return(pending, nil)

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
	// row is live. Stale-row cleanup happens via the round-adoption
	// transition (see db.RoundPersistenceStore.CommitState).
	store.AssertNotCalled(t, "ClearAllPendingBoardRequests",
		mock.Anything)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, _ := newBoardReplayTestWallet(t, store, clk)

	selfRef := actor.NewChannelTellOnlyRef[WalletMsg](
		"test-wallet-self", 4,
	)

	err := w.replayPendingBoardOnStart(t.Context(), selfRef)
	require.NoError(t, err)

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

// TestHandleBoardNoPersistSkipsUpsert verifies the CLI/RPC opt-out: when
// BoardRequest.NoPersist is true, the wallet does NOT write to
// pending_board_requests. The round actor still receives TriggerBoardMsg so
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

	// Explicitly refuse any UpsertPendingBoardRequests call; if the
	// wallet writes despite NoPersist, the mock's strict assertion
	// fails this test.
	store.AssertNotCalled(
		t, "UpsertPendingBoardRequests", mock.Anything, mock.Anything,
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
		t, "UpsertPendingBoardRequests", mock.Anything, mock.Anything,
	)

	require.Eventually(t, func() bool {
		return roundActor.TriggerBoardCalls() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"round actor must still receive TriggerBoardMsg with "+
			"NoPersist set")
}

// TestReplayEmptyStoreIsNoOp verifies that a wallet with no persisted Board
// rows skips the replay path entirely — no ClearAll, no self-Tell, no
// FetchBoardingIntentsByStatus. This is the common happy path on a fresh
// daemon start.
func TestReplayEmptyStoreIsNoOp(t *testing.T) {
	t.Parallel()

	store := &MockBoardingStore{}
	store.On(
		"ListPendingBoardRequests", mock.Anything,
	).Return([]PendingBoardRequest(nil), nil)

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	w, _ := newBoardReplayTestWallet(t, store, clk)

	selfRef := actor.NewChannelTellOnlyRef[WalletMsg](
		"test-wallet-self", 1,
	)

	err := w.replayPendingBoardOnStart(t.Context(), selfRef)
	require.NoError(t, err)

	store.AssertExpectations(t)
	store.AssertNotCalled(
		t, "FetchBoardingIntentsByStatus", mock.Anything, mock.Anything,
	)
	store.AssertNotCalled(t, "ClearAllPendingBoardRequests",
		mock.Anything)

	_, gotMsg := selfRef.AwaitMessage(50 * time.Millisecond)
	require.False(t, gotMsg, "empty store must not self-Tell")
}
