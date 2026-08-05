package round

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/stretchr/testify/require"
)

// TestTriggerBoardDoesNotParkOnStalledWallet is the regression test for the
// round-actor half of a circular wait between two single-goroutine actors. The
// wallet actor Asks the round actor from its refresh, leave and send handlers,
// and the round actor Asked the wallet back with no deadline on either the
// enqueue or the reply. Interleave a board trigger with any of those and both
// receive loops sit on the other's promise with no timer anywhere in the cycle.
//
// A board trigger arrives by Tell, so there is no caller deadline to fall back
// on: the bound has to come from the round actor itself. The test asserts that
// the handler returns rather than parks, and that it returns the deadline so
// the failure is attributable.
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
		done <- h.receive(&actormsg.TriggerBoardMsg{
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
