package round

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestTriggerBoardDoesNotParkOnStalledWallet pins that a board trigger whose
// wallet fetch is never answered ends in the deadline, delivered as the fetch
// reply, and not in a parked receive loop. The trigger turn itself only queues
// the request.
//
// The wallet actor Asks the round actor from its refresh, leave and send
// handlers, so a round turn that waits on the wallet closes a circular wait.
// TestBoardFetchDoesNotDeadlockWithWalletRegisterIntent covers the overlap on
// a real mailbox; this one covers the stalled-wallet failure.
func TestTriggerBoardDoesNotParkOnStalledWallet(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)

	// Shorten the production bound so the test does not have to wait it
	// out.
	h.actor.cfg.WalletAskTimeout = 50 * time.Millisecond

	release := h.walletActor.blockAsk()
	defer release()

	// Run the turn on its own goroutine: the regression is an unbounded
	// park, which would otherwise hang the package instead of failing.
	done := make(chan error, 1)
	go func() {
		done <- h.receiveBoard(&actormsg.TriggerBoardMsg{
			Amounts: []btcutil.Amount{49_000},
		}).Err()
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)

	case <-time.After(5 * time.Second):
		t.Fatal("round actor parked on the stalled wallet actor")
	}
}

// TestWalletAskRecoversAfterStall pins that the bound is a timeout and not a
// latch: once the wallet answers again, the same Ask succeeds.
func TestWalletAskRecoversAfterStall(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.actor.cfg.WalletAskTimeout = 50 * time.Millisecond

	release := h.walletActor.blockAsk()

	_, err := h.actor.askWallet(
		h.ctx, &wallet.GetConfirmedBoardingIntentsRequest{},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	release()

	resp, err := h.actor.askWallet(
		h.ctx, &wallet.GetConfirmedBoardingIntentsRequest{},
	)
	require.NoError(t, err)
	require.IsType(t, &wallet.GetConfirmedBoardingIntentsResponse{}, resp)
}

// mockBoardKeys lets the harness wallet derive any number of VTXO keys, which
// the board registration needs once per output.
func mockBoardKeys(h *actorTestHarness) {
	for _, family := range []keychain.KeyFamily{
		types.VTXOOwnerKeyFamily, types.VTXOSigningKeyFamily,
	} {
		h.wallet.On(
			"DeriveNextKey", mock.Anything, family,
		).Return(&keychain.KeyDescriptor{
			PubKey:     h.clientPubKey,
			KeyLocator: keychain.KeyLocator{Family: family},
		}, nil)
	}
}

// boardedAmounts returns the VTXO amounts of every round that has registered
// its intents, in no particular order.
func boardedAmounts(h *actorTestHarness) []btcutil.Amount {
	var amounts []btcutil.Amount
	for _, state := range h.queryState() {
		regState, ok := state.State.(*IntentSentState)
		if !ok {
			continue
		}

		for _, vtxo := range regState.Intents.VTXOs {
			amounts = append(amounts, vtxo.Amount)
		}
	}

	return amounts
}

// TestBoardFetchDoesNotDeadlockWithWalletRegisterIntent is the regression test
// for the circular wait between the round and wallet actors. The wallet
// handler Asks the round actor RegisterIntentMsg and waits for the reply
// before it answers the round's boarding query. Both actors run on real
// mailboxes with the production 30s wait bound. If the round actor parked in
// its turn on the boarding query, it could not serve the RegisterIntentMsg, and
// both would sit until the timeout fired.
func TestBoardFetchDoesNotDeadlockWithWalletRegisterIntent(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	mockBoardKeys(h)

	intent := h.newTestBoardingIntent()
	h.walletActor.setConfirmedIntents(*intent)
	h.actor.cfg.WalletAskTimeout = 30 * time.Second

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		_ = system.Shutdown(context.Background())
	})

	key := actor.NewServiceKey[
		actormsg.RoundReceivable, actormsg.RoundActorResp,
	](
		"test-round-board",
	)
	roundRef := actor.RegisterWithSystem(system, "test-round", key, h.actor)
	h.actor.cfg.SelfRef = roundRef

	// The wallet side of the cycle: before it answers the boarding query,
	// it Asks the round actor and waits for that reply.
	registerErr := make(chan error, 1)
	h.walletActor.boardingQueryHook = func(context.Context) error {
		// A fresh context stands in for the wallet actor's own turn;
		// the hook's argument carries the round actor's identity.
		askCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()

		_, err := roundRef.Ask( //nolint:contextcheck
			askCtx, &actormsg.RegisterIntentMsg{},
		).Await(askCtx).Unpack() //nolint:contextcheck
		registerErr <- err

		return nil
	}

	start := time.Now()
	require.NoError(
		t,
		roundRef.Tell(
			h.ctx, &actormsg.TriggerBoardMsg{
				Amounts: []btcutil.Amount{49_000},
			},
		),
	)

	// The empty package is rejected, but a rejection is still a reply: the
	// round actor served the request while its board fetch was pending.
	select {
	case err := <-registerErr:
		require.ErrorContains(t, err, "empty intent package")

	case <-time.After(5 * time.Second):
		t.Fatal(
			"RegisterIntentMsg unanswered while board fetch " +
				"pending",
		)
	}

	// The board registration completes once the wallet answers.
	require.Eventually(t, func() bool {
		resp, err := roundRef.Ask(
			h.ctx, &GetClientStateRequest{},
		).Await(h.ctx).Unpack()
		if err != nil {
			return false
		}

		stateResp, ok := resp.(*GetClientStateResponse)
		if !ok {
			return false
		}

		_, found := h.findTempState(stateResp.States)

		return found
	}, 5*time.Second, 5*time.Millisecond)

	require.Less(t, time.Since(start), 2*time.Second)
}

// TestBoardTriggersRunOneFetchAtATime pins the FIFO serialization: a second
// trigger waits for the first trigger's reply before its own fetch is sent, so
// two triggers cannot both see the same outpoint as confirmed.
func TestBoardTriggersRunOneFetchAtATime(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	mockBoardKeys(h)
	h.walletActor.setConfirmedIntents(*h.newTestBoardingIntent())

	var (
		queries  atomic.Int32
		inFlight atomic.Int32
		maxSeen  atomic.Int32
		release  = make(chan struct{}, 2)
	)
	h.walletActor.boardingQueryHook = func(context.Context) error {
		queries.Add(1)

		now := inFlight.Add(1)
		for {
			prev := maxSeen.Load()
			if now <= prev || maxSeen.CompareAndSwap(prev, now) {
				break
			}
		}

		<-release
		inFlight.Add(-1)

		return nil
	}

	for _, amount := range []btcutil.Amount{10_000, 20_000} {
		res := h.receive(&actormsg.TriggerBoardMsg{
			Amounts: []btcutil.Amount{amount},
		})
		require.True(t, res.IsOk(), "trigger: %v", res.Err())
	}

	// Both triggers are queued, but only the first has a fetch out.
	require.Eventually(t, func() bool {
		return queries.Load() == 1
	}, time.Second, time.Millisecond)

	release <- struct{}{}
	reply1, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)

	// The second fetch cannot have been sent: it follows the first reply's
	// turn, which has not run yet.
	require.EqualValues(t, 1, queries.Load())

	res := h.receive(reply1)
	require.True(t, res.IsOk(), "reply 1: %v", res.Err())
	require.Equal(t, []btcutil.Amount{10_000}, boardedAmounts(h))

	require.Eventually(t, func() bool {
		return queries.Load() == 2
	}, time.Second, time.Millisecond)

	release <- struct{}{}
	reply2, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)

	res = h.receive(reply2)
	require.True(t, res.IsOk(), "reply 2: %v", res.Err())
	require.ElementsMatch(
		t, []btcutil.Amount{10_000, 20_000}, boardedAmounts(h),
	)
	require.EqualValues(t, 1, maxSeen.Load())
}

// TestBoardFetchErrorLoggedAndNextProceeds pins that a failed wallet fetch is
// logged at error level, registers nothing, and does not stall the trigger
// queued behind it.
func TestBoardFetchErrorLoggedAndNextProceeds(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	var logs bytes.Buffer
	handler := btclog.NewDefaultHandler(&logs, btclog.WithNoTimestamp())
	handler.SetLevel(btclog.LevelError)
	h.actor.log = btclog.NewSLogger(handler)

	mockBoardKeys(h)
	h.walletActor.setConfirmedIntents(*h.newTestBoardingIntent())

	var queries atomic.Int32
	h.walletActor.boardingQueryHook = func(context.Context) error {
		if queries.Add(1) == 1 {
			return errors.New("wallet unavailable")
		}

		return nil
	}

	for _, amount := range []btcutil.Amount{10_000, 20_000} {
		res := h.receive(&actormsg.TriggerBoardMsg{
			Amounts: []btcutil.Amount{amount},
		})
		require.True(t, res.IsOk(), "trigger: %v", res.Err())
	}

	reply1, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)

	res := h.receive(reply1)
	require.ErrorContains(t, res.Err(), "wallet unavailable")
	require.Contains(t, logs.String(), "wallet unavailable")
	require.Empty(t, boardedAmounts(h))

	reply2, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)

	res = h.receive(reply2)
	require.True(t, res.IsOk(), "reply 2: %v", res.Err())
	require.Equal(t, []btcutil.Amount{20_000}, boardedAmounts(h))
}

// TestBoardQueueDropsExpiredHead pins that a head whose reply never arrives
// does not wedge the queue: the next trigger drops it, sends its own fetch,
// and the late reply of the dropped fetch is ignored.
func TestBoardQueueDropsExpiredHead(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	var logs bytes.Buffer
	handler := btclog.NewDefaultHandler(&logs, btclog.WithNoTimestamp())
	handler.SetLevel(btclog.LevelError)
	h.actor.log = btclog.NewSLogger(handler)

	mockBoardKeys(h)
	h.walletActor.setConfirmedIntents(*h.newTestBoardingIntent())

	const askTimeout = 50 * time.Millisecond
	h.actor.cfg.WalletAskTimeout = askTimeout

	// The first query is never answered, and the test never forwards its
	// timeout reply to the actor, which models a reply lost in delivery.
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	var queries atomic.Int32
	h.walletActor.boardingQueryHook = func(context.Context) error {
		if queries.Add(1) == 1 {
			<-stuck
		}

		return nil
	}

	res := h.receive(&actormsg.TriggerBoardMsg{
		Amounts: []btcutil.Amount{10_000},
	})
	require.True(t, res.IsOk(), "trigger 1: %v", res.Err())

	// The head expires at twice the ask timeout.
	time.Sleep(3 * askTimeout)

	res = h.receive(&actormsg.TriggerBoardMsg{
		Amounts: []btcutil.Amount{20_000},
	})
	require.True(t, res.IsOk(), "trigger 2: %v", res.Err())
	require.Contains(t, logs.String(), "Dropping board request")

	// The first reply is the stale timeout of the dropped head.
	stale, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)
	staleReply, ok := stale.(*boardingIntentsReply)
	require.True(t, ok)
	require.EqualValues(t, 1, staleReply.Seq)

	res = h.receive(stale)
	require.True(t, res.IsOk(), "stale reply: %v", res.Err())
	require.Empty(t, boardedAmounts(h))

	reply2, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)

	res = h.receive(reply2)
	require.True(t, res.IsOk(), "reply 2: %v", res.Err())
	require.Equal(t, []btcutil.Amount{20_000}, boardedAmounts(h))
}
