package wallet

import (
	"context"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// boardIdempotencyFixture wires a wallet against a mock store and an
// in-process round actor that captures every TriggerBoardMsg, so tests can
// drive handleBoard directly and inspect which boarding outpoints each
// successive trigger sized its amounts over.
type boardIdempotencyFixture struct {
	store      *MockBoardingStore
	roundActor *mockRoundActorBehavior
	wallet     *Ark
	intentA    BoardingIntent
	intentB    BoardingIntent
}

// newBoardIdempotencyFixture builds two confirmed boarding intents (A, B)
// paying the same boarding address. The caller sets the store's
// FetchBoardingIntentsByStatus expectation to model the confirmed set as it
// evolves across handleBoard calls.
func newBoardIdempotencyFixture(t *testing.T) *boardIdempotencyFixture {
	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rootHash := []byte{0xaa, 0xbb, 0xcc}
	taprootKey := txscript.ComputeTaprootOutputKey(
		clientKey.PubKey(), rootHash,
	)
	address, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	boardingAddr := &BoardingAddress{
		Address: address,
		Tapscript: &waddrmgr.Tapscript{
			Type:     waddrmgr.TapscriptTypeFullTree,
			RootHash: rootHash,
		},
		KeyDesc: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(BoardingKeyFamily),
				Index:  0,
			},
		},
		OperatorKey: operatorKey.PubKey(),
		ExitDelay:   144,
	}

	mkIntent := func(tag byte, amt btcutil.Amount) BoardingIntent {
		op := wire.OutPoint{Hash: chainhash.Hash{tag}, Index: 0}

		return BoardingIntent{
			Address:  *boardingAddr,
			Outpoint: op,
			ChainInfo: BoardingChainInfo{
				ConfHeight: 100,
				ConfHash: chainhash.Hash{
					0xbb,
				},
				ConfTx: &wire.MsgTx{
					TxOut: []*wire.TxOut{
						{
							Value: int64(amt),
						},
					},
				},
				OutPoint: op,
				Amount:   amt,
			},
			Status: BoardingStatusConfirmed,
		}
	}

	store := &MockBoardingStore{}
	store.On(
		"UpsertPendingIntent", mock.Anything, mock.Anything,
	).Return(nil)

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown(context.Background()))
	})

	roundActor := &mockRoundActorBehavior{}
	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName, roundKey, roundActor,
	)

	w := NewArk(
		nil, store, nil, nil, system, fn.None[ledger.Sink](),
		btclog.Disabled,
		WithClock(
			clock.NewDefaultClock(),
		),
	)

	return &boardIdempotencyFixture{
		store:      store,
		roundActor: roundActor,
		wallet:     w,
		intentA:    mkIntent(0xaa, 100_000),
		intentB:    mkIntent(0xbb, 60_000),
	}
}

// triggerOutpoints returns the Outpoints of the n-th (0-indexed) captured
// TriggerBoardMsg, failing if fewer were captured.
func triggerOutpoints(t *testing.T, fix *boardIdempotencyFixture,
	n int) []wire.OutPoint {

	t.Helper()

	calls := fix.roundActor.CapturedTriggerBoards()
	require.Greater(t, len(calls), n, "expected at least %d triggers", n+1)

	return calls[n].Outpoints
}

// TestHandleBoardExcludesInFlightOutpoints proves that once a boarding
// outpoint has been shipped into an in-flight round, a later trigger fired
// when a second deposit confirms boards only the new outpoint, rather than
// re-registering the in-flight one under a freshly derived owner key (the
// quote pkScript-echo mismatch on darepo-client#772).
func TestHandleBoardExcludesInFlightOutpoints(t *testing.T) {
	fix := newBoardIdempotencyFixture(t)
	ctx := context.Background()

	opA := fix.intentA.Outpoint
	opB := fix.intentB.Outpoint

	// First trigger: only A is confirmed. Second trigger: A is still
	// confirmed (its round has not yet adopted it) and B has now
	// confirmed too.
	fetch := fix.store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	)
	fetch.Return([]BoardingIntent{fix.intentA}, nil).Once()
	fix.store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent{fix.intentA, fix.intentB}, nil)

	res := fix.wallet.handleBoard(ctx, &BoardRequest{})
	_, err := res.Unpack()
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() == 1
	}, time.Second, 5*time.Millisecond)
	require.Equal(t, []wire.OutPoint{opA}, triggerOutpoints(t, fix, 0))

	res = fix.wallet.handleBoard(ctx, &BoardRequest{})
	_, err = res.Unpack()
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() == 2
	}, time.Second, 5*time.Millisecond)

	// The second trigger must board only B: A is already in flight.
	require.Equal(t, []wire.OutPoint{opB}, triggerOutpoints(t, fix, 1))
}

// TestHandleBoardSkipsFullyInFlightTrigger proves a redundant trigger whose
// every confirmed outpoint is already in flight does not re-dispatch to the
// round actor at all (it would otherwise mint a divergent registration).
func TestHandleBoardSkipsFullyInFlightTrigger(t *testing.T) {
	fix := newBoardIdempotencyFixture(t)
	ctx := context.Background()

	// Both triggers see the same single confirmed outpoint A.
	fix.store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent{fix.intentA}, nil)

	res := fix.wallet.handleBoard(ctx, &BoardRequest{})
	_, err := res.Unpack()
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() == 1
	}, time.Second, 5*time.Millisecond)

	// Second call: A is still the only confirmed outpoint and is already
	// in flight, so handleBoard must report the balance without Telling
	// the round actor again.
	res = fix.wallet.handleBoard(ctx, &BoardRequest{})
	resp, err := res.Unpack()
	require.NoError(t, err)

	boardResp, ok := resp.(*BoardResponse)
	require.True(t, ok)
	require.Equal(
		t, fix.intentA.ChainInfo.Amount, boardResp.BoardingBalance,
	)

	// No second Tell: the trigger count must stay at one.
	require.Never(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() > 1
	}, 100*time.Millisecond, 10*time.Millisecond)
}

// TestHandleBoardReshipsAfterOutpointLeavesConfirmed proves the in-flight
// guard releases an outpoint once it leaves the confirmed set (its round
// adopted it), so a brand-new deposit re-using nothing still boards and the
// guard never permanently strands funds.
func TestHandleBoardReshipsAfterOutpointLeavesConfirmed(t *testing.T) {
	fix := newBoardIdempotencyFixture(t)
	ctx := context.Background()

	opB := fix.intentB.Outpoint

	// First call sees A; second call sees only B (A has been adopted and
	// left the confirmed set, B is a fresh deposit).
	fetch := fix.store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	)
	fetch.Return([]BoardingIntent{fix.intentA}, nil).Once()
	fix.store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return([]BoardingIntent{fix.intentB}, nil)

	_, err := fix.wallet.handleBoard(ctx, &BoardRequest{}).Unpack()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() == 1
	}, time.Second, 5*time.Millisecond)

	_, err = fix.wallet.handleBoard(ctx, &BoardRequest{}).Unpack()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() == 2
	}, time.Second, 5*time.Millisecond)

	// B boards on the second trigger; A was pruned from the in-flight set
	// when it left the confirmed set.
	require.Equal(t, []wire.OutPoint{opB}, triggerOutpoints(t, fix, 1))
}
