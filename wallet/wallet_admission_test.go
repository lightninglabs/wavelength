package wallet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// mockVTXOManagerBehavior implements actor.ActorBehavior for the VTXO manager
// service key type. It responds to admission messages with configurable
// results, enabling wallet handler tests without a real VTXO manager.
type mockVTXOManagerBehavior struct {
	// selectReq is the last SelectAndReserveSpendRequest received.
	selectReq *actormsg.SelectAndReserveSpendRequest

	// selectResp is returned for SelectAndReserveSpendRequest.
	selectResp *actormsg.SelectAndReserveSpendResponse

	// selectErr when set causes SelectAndReserveSpendRequest to fail.
	selectErr error

	// releaseResp is returned for ReleaseSpendRequest.
	releaseResp *actormsg.ReleaseSpendResponse

	// releaseErr when set causes ReleaseSpendRequest to fail.
	releaseErr error

	// completeResp is returned for CompleteSpendRequest.
	completeResp *actormsg.CompleteSpendResponse

	// completeErr when set causes CompleteSpendRequest to fail.
	completeErr error

	// forfeitReserveResp is returned for ReserveForfeitRequest.
	forfeitReserveResp *actormsg.ReserveForfeitResponse

	// forfeitReserveErr when set causes ReserveForfeitRequest to fail.
	forfeitReserveErr error

	// forfeitReleaseResp is returned for ReleaseForfeitRequest.
	forfeitReleaseResp *actormsg.ReleaseForfeitResponse

	// forfeitReleaseErr when set causes ReleaseForfeitRequest to fail.
	forfeitReleaseErr error

	// forfeitReleaseCalls tracks how many ReleaseForfeitRequest were
	// received.
	forfeitReleaseCalls int

	// selectForfeitResp is returned for
	// SelectAndReserveForfeitRequest.
	selectForfeitResp *actormsg.SelectAndReserveForfeitResponse

	// selectForfeitErr when set causes
	// SelectAndReserveForfeitRequest to fail.
	selectForfeitErr error
}

// Receive processes VTXO manager messages from the wallet.
func (m *mockVTXOManagerBehavior) Receive(_ context.Context,
	msg actormsg.VTXOManagerMsg) fn.Result[actormsg.VTXOManagerResp] {

	switch msg := msg.(type) {
	case *actormsg.SelectAndReserveSpendRequest:
		m.selectReq = msg

		if m.selectErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.selectErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](m.selectResp)

	case *actormsg.ReleaseSpendRequest:
		if m.releaseErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.releaseErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](m.releaseResp)

	case *actormsg.CompleteSpendRequest:
		if m.completeErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.completeErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](m.completeResp)

	case *actormsg.ReserveForfeitRequest:
		if m.forfeitReserveErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.forfeitReserveErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](
			m.forfeitReserveResp,
		)

	case *actormsg.ReleaseForfeitRequest:
		m.forfeitReleaseCalls++

		if m.forfeitReleaseErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.forfeitReleaseErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](
			m.forfeitReleaseResp,
		)

	case *actormsg.SelectAndReserveForfeitRequest:
		if m.selectForfeitErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.selectForfeitErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](
			m.selectForfeitResp,
		)

	default:
		return fn.Err[actormsg.VTXOManagerResp](
			fmt.Errorf("unexpected message: %T", msg),
		)
	}
}

// newTestWalletWithManager creates an Ark wallet backed by a mock VTXO
// manager registered in a real actor system. Returns the wallet, the mock
// manager behavior (for configuring responses), and a cleanup function.
func newTestWalletWithManager(t *testing.T, mgr *mockVTXOManagerBehavior) *Ark {
	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		// Use Background context because t.Context() is already
		// cancelled by the time cleanup runs.
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	})

	// Register the mock VTXO manager with the well-known service key.
	mgrKey := actormsg.VTXOManagerServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.VTXOManagerServiceKeyName, mgrKey, mgr,
	)

	// Chain source is not needed for admission tests so we pass nil.
	return NewArk(
		nil, nil, nil, nil, system, fn.None[ledger.Sink](),
		btclog.Disabled,
	)
}

// testOutpoint returns a deterministic outpoint for testing.
func testOutpoint(idx uint32) wire.OutPoint {
	return wire.OutPoint{
		Hash: chainhash.Hash{
			byte(idx),
			0xaa,
		},
		Index: idx,
	}
}

// TestSelectAndLockVTXOs verifies that the wallet forwards a spend selection
// request to the VTXO manager and translates the response correctly.
func TestSelectAndLockVTXOs(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectResp: &actormsg.SelectAndReserveSpendResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: testOutpoint(0),
					Amount:   50000,
					PkScript: []byte{
						0x51,
						0x20,
						0x01,
					},
				},
				{
					Outpoint: testOutpoint(1),
					Amount:   30000,
					PkScript: []byte{
						0x51,
						0x20,
						0x02,
					},
				},
			},
			TotalSelected: 80000,
		},
	}
	w := newTestWalletWithManager(t, mgr)

	req := &SelectAndLockVTXOsRequest{
		TargetAmount:    70000,
		MinChangeAmount: 1000,
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*SelectAndLockVTXOsResponse)
	require.Len(t, resp.SelectedVTXOs, 2)
	require.Equal(t, btcutil.Amount(80000), resp.TotalSelected)
	require.Equal(t, testOutpoint(0), resp.SelectedVTXOs[0].Outpoint)
	require.Equal(t, btcutil.Amount(50000),
		resp.SelectedVTXOs[0].Amount)
	require.Equal(t, btcutil.Amount(70000), mgr.selectReq.TargetAmount)
	require.Equal(t, btcutil.Amount(1000), mgr.selectReq.MinChangeAmount)
}

// TestSelectAndLockVTXOsInsufficientFunds verifies that the wallet surfaces
// manager errors when selection fails.
func TestSelectAndLockVTXOsInsufficientFunds(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectErr: fmt.Errorf("insufficient funds: need 100000"),
	}
	w := newTestWalletWithManager(t, mgr)

	req := &SelectAndLockVTXOsRequest{
		TargetAmount: 100000,
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "insufficient funds")
}

// TestUnlockVTXOs verifies that the wallet forwards a spend release request
// to the VTXO manager and translates the response.
func TestUnlockVTXOs(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		releaseResp: &actormsg.ReleaseSpendResponse{
			ReleasedCount: 2,
		},
	}
	w := newTestWalletWithManager(t, mgr)

	req := &UnlockVTXOsRequest{
		Outpoints: []wire.OutPoint{
			testOutpoint(0), testOutpoint(1),
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*UnlockVTXOsResponse)
	require.Equal(t, 2, resp.UnlockedCount)
}

// TestUnlockVTXOsError verifies that unlock surfaces manager errors.
func TestUnlockVTXOsError(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		releaseErr: fmt.Errorf("no actor for outpoint"),
	}
	w := newTestWalletWithManager(t, mgr)

	req := &UnlockVTXOsRequest{
		Outpoints: []wire.OutPoint{
			testOutpoint(99),
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "no actor for outpoint")
}

// TestCompleteSpendVTXOs verifies that the wallet forwards a spend completion
// request to the VTXO manager.
func TestCompleteSpendVTXOs(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		completeResp: &actormsg.CompleteSpendResponse{
			CompletedCount: 3,
		},
	}
	w := newTestWalletWithManager(t, mgr)

	req := &CompleteSpendVTXOsRequest{
		Outpoints: []wire.OutPoint{
			testOutpoint(0), testOutpoint(1), testOutpoint(2),
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*CompleteSpendVTXOsResponse)
	require.Equal(t, 3, resp.CompletedCount)
}

// TestCompleteSpendVTXOsError verifies that complete surfaces manager errors.
func TestCompleteSpendVTXOsError(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		completeErr: fmt.Errorf("no actor for outpoint"),
	}
	w := newTestWalletWithManager(t, mgr)

	req := &CompleteSpendVTXOsRequest{
		Outpoints: []wire.OutPoint{
			testOutpoint(7),
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "no actor for outpoint")
}

// TestSelectAndLockNoActorSystem verifies that admission handlers return an
// error when the actor system is not configured.
func TestSelectAndLockNoActorSystem(t *testing.T) {
	t.Parallel()

	w := NewArk(
		nil, nil, nil, nil, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	result := w.Receive(t.Context(), &SelectAndLockVTXOsRequest{
		TargetAmount: 10000,
	})
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(),
		"actor system not configured")
}

// =============================================================================
// Cooperative admission tests (refresh/leave gating)
// =============================================================================

// mockRoundActorBehavior implements actor.ActorBehavior for the round actor
// service key type. It responds to RegisterIntentMsg with a configurable
// error, enabling wallet handler tests without a real round actor.
//
// The TriggerBoardMsg counter is updated from the actor's goroutine and
// read from test goroutines via TriggerBoardCalls(), so it MUST be
// accessed under a mutex; the wallet's Tell-based dispatch to the round
// actor is asynchronous, so a poll-based test (require.Eventually) would
// otherwise race the actor's Receive loop. The same mutex covers the
// captured-message pointer because pointer assignment is not atomic on
// 64-bit reads/writes.
type mockRoundActorBehavior struct {
	// registerErr when set causes RegisterIntentMsg to fail.
	registerErr error

	// registerCalls tracks how many times RegisterIntentMsg was received.
	registerCalls int

	// capturedIntent holds the last RegisterIntentMsg received, so
	// tests can inspect the intent package contents.
	capturedIntent *actormsg.RegisterIntentMsg

	// mu guards the TriggerBoardMsg fields below. The RegisterIntentMsg
	// fields above are safe without locking because every existing
	// caller drives them through an Ask/Await that synchronously
	// publishes the actor-goroutine write before the test goroutine
	// reads. The TriggerBoardMsg path is Tell-based, so it needs an
	// explicit happens-before edge for the test to observe.
	mu sync.Mutex

	// triggerBoardCalls tracks how many times TriggerBoardMsg was
	// received, so Board / replay tests can assert the round actor was
	// (or was not) re-tickled. Read under mu.
	triggerBoardCalls int

	// capturedTriggerBoard holds the last TriggerBoardMsg received so
	// tests can inspect the VTXO amounts and target count handed
	// across. Read under mu.
	capturedTriggerBoard *actormsg.TriggerBoardMsg
}

// TriggerBoardCalls returns a snapshot of triggerBoardCalls under the
// mock's mutex so test goroutines can poll the counter without racing
// the actor's Receive loop.
func (m *mockRoundActorBehavior) TriggerBoardCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.triggerBoardCalls
}

// Receive processes round actor messages from the wallet.
func (m *mockRoundActorBehavior) Receive(_ context.Context,
	msg actormsg.RoundReceivable) fn.Result[actormsg.RoundActorResp] {

	switch typedMsg := msg.(type) {
	case *actormsg.RegisterIntentMsg:
		m.registerCalls++
		m.capturedIntent = typedMsg

		if m.registerErr != nil {
			return fn.Err[actormsg.RoundActorResp](
				m.registerErr,
			)
		}

		return fn.Ok[actormsg.RoundActorResp](nil)

	case *actormsg.TriggerBoardMsg:
		m.mu.Lock()
		m.triggerBoardCalls++
		m.capturedTriggerBoard = typedMsg
		m.mu.Unlock()

		return fn.Ok[actormsg.RoundActorResp](nil)

	default:
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("unexpected message: %T", msg),
		)
	}
}

// newTestWalletWithManagerAndRound creates a wallet with both mock VTXO
// manager and mock round actor registered. The vtxoReader provides VTXO
// descriptors for intent composition.
func newTestWalletWithManagerAndRound(t *testing.T,
	mgr *mockVTXOManagerBehavior, roundActor *mockRoundActorBehavior,
	vtxoReader VTXOReader) *Ark {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		// Use Background context because t.Context() is already
		// cancelled by the time cleanup runs.
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	})

	// Register the mock VTXO manager.
	mgrKey := actormsg.VTXOManagerServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.VTXOManagerServiceKeyName, mgrKey, mgr,
	)

	// Register the mock round actor.
	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName, roundKey, roundActor,
	)

	return NewArk(
		nil, nil, vtxoReader, nil, system, fn.None[ledger.Sink](),
		btclog.Disabled,
	)
}

// testVTXOReader returns a VTXOReader backed by a static map.
func testVTXOReader(descs map[wire.OutPoint]*VTXODescriptor) VTXOReader {
	return VTXOReaderFunc(func(_ context.Context, outpoint wire.OutPoint) (
		*VTXODescriptor, error) {

		desc, ok := descs[outpoint]
		if !ok {
			return nil, fmt.Errorf("vtxo not found: %s", outpoint)
		}

		return desc, nil
	})
}

// TestRefreshReservesBeforeRoundRegistration verifies that the wallet
// reserves forfeit inputs through the VTXO manager before sending
// RegisterIntentMsg to the round actor.
func TestRefreshReservesBeforeRoundRegistration(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			PolicyTemplate: []byte{
				0xde,
				0xad,
				0xbe,
				0xef,
			},
			Expiry: 100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &RefreshVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			op,
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	// Round should have received the intent.
	require.Equal(t, 1, roundActor.registerCalls)
}

// TestRefreshReleasesOnRoundRejection verifies that the wallet releases
// forfeit reservations when the round actor rejects the intent.
func TestRefreshReleasesOnRoundRejection(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			PolicyTemplate: []byte{
				0xde,
				0xad,
				0xbe,
				0xef,
			},
			Expiry: 100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{
		registerErr: fmt.Errorf("round full"),
	}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &RefreshVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			op,
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "round full")

	// Manager should have received the release call.
	require.Equal(t, 1, mgr.forfeitReleaseCalls)
}

// TestRefreshDeductsPerInputOperatorFee verifies that when the daemon
// pre-quotes a non-zero per-VTXO operator fee on RefreshVTXOsRequest,
// the wallet builds the new VTXO's amount as (forfeit_amount -
// operator_fee). The round's implicit operator fee (sum of forfeit
// inputs minus sum of new VTXO outputs) must equal the sum of the
// per-input fees so the server's validateOperatorFee (#269)
// ComputeForfeitFee check accepts the submission.
func TestRefreshFailsOnManagerRejection(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			PolicyTemplate: []byte{
				0xde,
				0xad,
				0xbe,
				0xef,
			},
			Expiry: 100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveErr: fmt.Errorf(
			"VTXO already spending",
		),
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &RefreshVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			op,
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "VTXO already spending")

	// Round should NOT have been called.
	require.Equal(t, 0, roundActor.registerCalls)
}

// TestLeaveReservesBeforeRoundRegistration verifies that the wallet
// reserves forfeit inputs before sending a leave intent to the round.
func TestLeaveReservesBeforeRoundRegistration(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			Expiry: 100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &LeaveVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			op,
		},
		DestOutput: &wire.TxOut{
			Value: 49000,
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	require.Equal(t, 1, roundActor.registerCalls)
}

// TestLeaveReleasesOnRoundRejection verifies that the wallet releases
// forfeit reservations when the round actor rejects the leave intent.
func TestLeaveReleasesOnRoundRejection(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			Expiry: 100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{
		registerErr: fmt.Errorf("round expired"),
	}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &LeaveVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			op,
		},
		DestOutput: &wire.TxOut{
			Value: 49000,
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "round expired")

	// Manager should have received the release call.
	require.Equal(t, 1, mgr.forfeitReleaseCalls)
}

// TestLeavePerOutpointDestinations verifies that when the caller
// populates DestOutputs with per-outpoint overrides, each leave output
// uses its own destination pkScript. Under the #270 seal-time fee
// handshake the leave output value is the forfeited VTXO's full
// amount; the server stamps the residual on whichever output the FSM
// designates as IsChange=true. The wallet handler intentionally does
// NOT set IsChange — that's the FSM's job during intent assembly so a
// single marker survives across multiple LeaveVTXOs RPCs in the same
// pending-round window.
func TestLeavePerOutpointDestinations(t *testing.T) {
	t.Parallel()

	op1 := testOutpoint(0)
	op2 := testOutpoint(1)

	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op1: {
			Outpoint: op1,
			Amount:   50_000,
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			Expiry: 100,
		},
		op2: {
			Outpoint: op2,
			Amount:   80_000,
			PkScript: []byte{
				0x51,
				0x20,
				0x02,
			},
			Expiry: 200,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	// Distinct per-outpoint destinations. DestOutput is left nil on
	// purpose so a regression that fell through to the default would
	// be caught by the nil-destination error path.
	script1 := []byte{0x51, 0x20, 0xaa}
	script2 := []byte{0x51, 0x20, 0xbb}
	req := &LeaveVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			op1,
			op2,
		},
		DestOutputs: map[wire.OutPoint]*wire.TxOut{
			op1: {
				PkScript: script1,
			},
			op2: {
				PkScript: script2,
			},
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	require.Equal(t, 1, roundActor.registerCalls)
	require.NotNil(t, roundActor.capturedIntent)

	forfeits := roundActor.capturedIntent.Forfeits
	leaves := roundActor.capturedIntent.Leaves
	require.Len(t, forfeits, 2)
	require.Len(t, leaves, 2)

	want := map[wire.OutPoint]struct {
		script []byte
		value  int64
	}{
		op1: {
			script: script1,
			value:  int64(vtxoDescs[op1].Amount),
		},
		op2: {
			script: script2,
			value:  int64(vtxoDescs[op2].Amount),
		},
	}
	for i := range forfeits {
		require.NotNil(t, forfeits[i].VTXOOutpoint)
		op := *forfeits[i].VTXOOutpoint
		expect, ok := want[op]
		require.True(t, ok, "unexpected outpoint %s", op)

		require.NotNil(t, leaves[i].Output)
		require.Equal(
			t, expect.script, leaves[i].Output.PkScript,
			"per-outpoint pkScript is preserved",
		)
		require.Equal(
			t, expect.value, leaves[i].Output.Value,
			"leaf value carries the forfeited VTXO amount",
		)

		require.False(
			t, leaves[i].IsChange, "wallet handler must not "+
				"stamp IsChange — the FSM designates the "+
				"change marker centrally during intent "+
				"assembly",
		)
	}
}

// TestLeaveFallsBackToDefaultDestWhenMapMisses verifies the hybrid
// case where DestOutputs overrides one outpoint and the other falls
// through to DestOutput. Both leave outputs must carry the correct
// pkScript for their source.
func TestLeaveFallsBackToDefaultDestWhenMapMisses(t *testing.T) {
	t.Parallel()

	op1 := testOutpoint(0)
	op2 := testOutpoint(1)

	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op1: {
			Outpoint: op1,
			Amount:   30_000,
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			Expiry: 100,
		},
		op2: {
			Outpoint: op2,
			Amount:   40_000,
			PkScript: []byte{
				0x51,
				0x20,
				0x02,
			},
			Expiry: 200,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	overrideScript := []byte{0x51, 0x20, 0xab}
	defaultScript := []byte{0x51, 0x20, 0xcd}
	req := &LeaveVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			op1,
			op2,
		},
		DestOutput: &wire.TxOut{
			PkScript: defaultScript,
		},
		DestOutputs: map[wire.OutPoint]*wire.TxOut{
			op1: {
				PkScript: overrideScript,
			},
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	leaves := roundActor.capturedIntent.Leaves
	require.Len(t, leaves, 2)

	want := map[wire.OutPoint][]byte{
		op1: overrideScript,
		op2: defaultScript,
	}
	for i, forfeit := range roundActor.capturedIntent.Forfeits {
		op := *forfeit.VTXOOutpoint
		require.Equal(
			t, want[op], leaves[i].Output.PkScript, "outpoint "+
				"%s used wrong script source", op,
		)
	}
}

// TestLeaveRejectsMissingDestination verifies the handler surfaces a
// per-outpoint error when neither DestOutputs[op] nor DestOutput is
// set for a target. The RPC layer is supposed to guarantee a
// destination before dispatch; if it ever slips this guarantee we
// want a named error, not a nil-pointer panic, and no round
// registration on the all-rejected case.
func TestLeaveRejectsMissingDestination(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50_000,
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			Expiry: 100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &LeaveVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			op,
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(
		t, result.IsOk(),
		"handler returns ok with per-outpoint error; got: %v",
		result.Err(),
	)

	respVal, _ := result.Unpack()
	resp, ok := respVal.(*LeaveVTXOsResponse)
	require.True(t, ok)
	require.Zero(t, resp.LeavingCount)
	require.Contains(
		t, resp.Errors[op].Error(),
		"no destination for outpoint",
	)

	require.Equal(t, 0, roundActor.registerCalls)
}

// =============================================================================
// Directed send tests
// =============================================================================

// stubBackend is a minimal BoardingBackend for send tests. It returns
// deterministic keys from DeriveNextKey and stubs all other methods.
type stubBackend struct {
	keyCounter uint32
}

// DeriveNextKey returns a deterministic key descriptor.
func (s *stubBackend) DeriveNextKey(_ context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	s.keyCounter++
	priv, _ := btcec.NewPrivateKey()

	return &keychain.KeyDescriptor{
		PubKey: priv.PubKey(),
		KeyLocator: keychain.KeyLocator{
			Family: family,
			Index:  s.keyCounter,
		},
	}, nil
}

// ImportTaprootScript is a no-op stub.
func (s *stubBackend) ImportTaprootScript(_ context.Context,
	_ *waddrmgr.Tapscript) (btcutil.Address, error) {

	return nil, fmt.Errorf("not implemented")
}

// ListUnspent is a no-op stub.
func (s *stubBackend) ListUnspent(_ context.Context, _ int32, _ int32) ([]*Utxo,
	error) {

	return nil, nil
}

// GetTransaction is a no-op stub.
func (s *stubBackend) GetTransaction(_ context.Context, _ chainhash.Hash) (
	*TxInfo, error) {

	return nil, fmt.Errorf("not implemented")
}

// GetBlock is a no-op stub.
func (s *stubBackend) GetBlock(_ context.Context, _ chainhash.Hash) (
	*wire.MsgBlock, error) {

	return nil, fmt.Errorf("not implemented")
}

// newTestWalletForSend creates a wallet with mock VTXO manager, mock
// round actor, and a stubBackend for key derivation. This is the
// setup needed for directed send tests.
func newTestWalletForSend(t *testing.T, mgr *mockVTXOManagerBehavior,
	roundActor *mockRoundActorBehavior) *Ark {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		// t.Context() is cancelled before cleanup runs, so
		// we need a fresh context for graceful shutdown.
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

	backend := &stubBackend{}

	return NewArk(
		backend, nil, nil, nil, system, fn.None[ledger.Sink](),
		btclog.Disabled,
	)
}

// testSendRecipient returns a SendRecipient for testing.
func testSendRecipient(amount btcutil.Amount) SendRecipient {
	priv, _ := btcec.NewPrivateKey()

	return SendRecipient{
		PkScript: []byte{
			0x51,
			0x20,
			0x01,
		},
		Amount:    amount,
		ClientKey: priv.PubKey(),
	}
}

// testOperatorKey returns a deterministic operator key for testing.
func testOperatorKey() *btcec.PublicKey {
	priv, _ := btcec.NewPrivateKey()

	return priv.PubKey()
}

// selectForfeitResp builds a SelectAndReserveForfeitResponse holding
// one reserved VTXO per supplied amount, with deterministic outpoints.
func selectForfeitResp(
	amounts ...btcutil.Amount) *actormsg.SelectAndReserveForfeitResponse {

	resp := &actormsg.SelectAndReserveForfeitResponse{}
	for i, amt := range amounts {
		resp.SelectedVTXOs = append(
			resp.SelectedVTXOs, actormsg.SelectedVTXO{
				Outpoint: testOutpoint(uint32(i)),
				Amount:   amt,
				PkScript: []byte{0x51, 0x20, 0x01},
			},
		)
		resp.TotalSelected += amt
	}

	return resp
}

// TestSendVTXOs exercises the directed-send admission flow across its
// data-only outcomes: happy path, exact-change, the #270 multi-recipient
// zero-change guard, dust-change rejection, dry-run preview (with and
// without a failing best-effort release), round rejection releasing the
// reservation, and selection failure. All rows share one fixture and
// runner; only the inputs and expected scalars differ.
func TestSendVTXOs(t *testing.T) {
	t.Parallel()

	type want struct {
		errSubstr     string
		status        string
		change        btcutil.Amount
		selectedCount int
		totalSelected btcutil.Amount
		registerCalls int
		releaseCalls  int
	}
	tests := []struct {
		name string

		// selected are the reserved VTXO amounts the manager returns;
		// nil leaves selectForfeitResp unset (selection fails).
		selected []btcutil.Amount

		// recipients are the per-recipient send amounts.
		recipients []btcutil.Amount

		dryRun      bool
		selectErr   string
		releaseErr  string
		registerErr string
		want        want
	}{
		{
			name: "success",
			selected: []btcutil.Amount{
				50000,
			},
			recipients: []btcutil.Amount{
				40000,
			},
			want: want{
				status:        "submitted",
				change:        9000,
				selectedCount: 1,
				totalSelected: 50000,
				registerCalls: 1,
			},
		},
		{
			name: "exact change produces no change output",
			selected: []btcutil.Amount{
				41000,
			},
			recipients: []btcutil.Amount{
				40000,
			},
			want: want{
				status:        "submitted",
				change:        0,
				registerCalls: 1,
			},
		},
		{
			// #270 guard: a multi-recipient send whose selection
			// covers the target exactly (change == 0) must be
			// rejected locally before a round slot is consumed.
			name: "multi-recipient zero change rejects",
			selected: []btcutil.Amount{
				41000,
			},
			recipients: []btcutil.Amount{
				25000,
				15000,
			},
			want: want{
				errSubstr:     "multi-recipient",
				registerCalls: 0,
				releaseCalls:  1,
			},
		},
		{
			name: "dust change rejects",
			selected: []btcutil.Amount{
				41500,
			},
			recipients: []btcutil.Amount{
				40000,
			},
			want: want{
				errSubstr:     "below dust limit",
				registerCalls: 0,
				releaseCalls:  1,
			},
		},
		{
			name: "dry run previews and releases",
			selected: []btcutil.Amount{
				50000,
			},
			recipients: []btcutil.Amount{
				40000,
			},
			dryRun: true,
			want: want{
				status:        "preview",
				change:        9000,
				registerCalls: 0,
				releaseCalls:  1,
			},
		},
		{
			// Dry-run succeeds even when the deferred best-effort
			// release fails; the error is logged, not propagated.
			name: "dry run survives release failure",
			selected: []btcutil.Amount{
				50000,
			},
			recipients: []btcutil.Amount{
				40000,
			},
			dryRun:     true,
			releaseErr: "release timeout",
			want: want{
				status:       "preview",
				change:       9000,
				releaseCalls: 1,
			},
		},
		{
			// Round rejection releases the forfeit reservation.
			name: "round rejects and releases",
			selected: []btcutil.Amount{
				50000,
			},
			recipients: []btcutil.Amount{
				40000,
			},
			registerErr: "round full",
			want: want{
				errSubstr:     "round full",
				registerCalls: 1,
				releaseCalls:  1,
			},
		},
		{
			name: "selection fails",
			recipients: []btcutil.Amount{
				100000,
			},
			selectErr: "insufficient funds",
			want: want{
				errSubstr:     "insufficient funds",
				registerCalls: 0,
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mgr := &mockVTXOManagerBehavior{}
			if tc.selected != nil {
				mgr.selectForfeitResp = selectForfeitResp(
					tc.selected...,
				)
				mgr.forfeitReleaseResp =
					&actormsg.ReleaseForfeitResponse{
						ReleasedCount: 1,
					}
			}
			if tc.selectErr != "" {
				mgr.selectForfeitErr = errors.New(tc.selectErr)
			}
			if tc.releaseErr != "" {
				mgr.forfeitReleaseResp = nil
				mgr.forfeitReleaseErr = errors.New(tc.releaseErr) //nolint:ll
			}

			roundActor := &mockRoundActorBehavior{}
			if tc.registerErr != "" {
				roundActor.registerErr = errors.New(
					tc.registerErr)
			}

			w := newTestWalletForSend(t, mgr, roundActor)

			recipients := make(
				[]SendRecipient, len(tc.recipients),
			)
			for i, amt := range tc.recipients {
				recipients[i] = testSendRecipient(amt)
			}

			result := w.Receive(t.Context(), &SendVTXOsRequest{
				Recipients:    recipients,
				OperatorFee:   1000,
				DustLimit:     546,
				OperatorKey:   testOperatorKey(),
				VTXOExitDelay: 144,
				DryRun:        tc.dryRun,
			})
			resp, err := result.Unpack()

			if tc.want.errSubstr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.want.errSubstr,
				)
				require.Equal(
					t, tc.want.registerCalls,
					roundActor.registerCalls,
				)
				require.Equal(
					t, tc.want.releaseCalls,
					mgr.forfeitReleaseCalls,
				)

				return
			}

			require.NoError(t, err)

			sendResp, ok := resp.(*SendVTXOsResponse)
			require.True(t, ok, "expected *SendVTXOsResponse")
			require.Equal(t, tc.want.status, sendResp.Status)
			require.Equal(t, tc.want.change, sendResp.ChangeAmount)
			if tc.want.selectedCount != 0 {
				require.Equal(
					t, tc.want.selectedCount,
					sendResp.SelectedCount,
				)
			}
			if tc.want.totalSelected != 0 {
				require.Equal(
					t, tc.want.totalSelected,
					sendResp.TotalSelected,
				)
			}
			require.Equal(
				t, tc.want.registerCalls,
				roundActor.registerCalls,
			)
			require.Equal(
				t, tc.want.releaseCalls,
				mgr.forfeitReleaseCalls,
			)
		})
	}
}

// TestSendVTXOsIntentPackageContents verifies the full intent package
// that the wallet registers with the round actor. This is the
// higher-fidelity test that proves the end-to-end flow: wallet selects
// coins via the manager, builds forfeits + recipient VTXOs + change
// VTXO, and registers the correct intent with the round.
func TestSendVTXOsIntentPackageContents(t *testing.T) {
	t.Parallel()

	// Two VTXOs selected as inputs.
	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: testOutpoint(0),
					Amount:   30000,
					PkScript: []byte{
						0x51,
						0x20,
						0x01,
					},
				},
				{
					Outpoint: testOutpoint(1),
					Amount:   25000,
					PkScript: []byte{
						0x51,
						0x20,
						0x02,
					},
				},
			},
			TotalSelected: 55000,
		},
	}
	roundActor := &mockRoundActorBehavior{}

	// Two recipients with known keys.
	recipientKeyA, _ := btcec.NewPrivateKey()
	recipientKeyB, _ := btcec.NewPrivateKey()
	operatorKey := testOperatorKey()

	recipients := []SendRecipient{
		{
			PkScript: []byte{
				0x51,
				0x20,
				0xAA,
			},
			Amount:    20000,
			ClientKey: recipientKeyA.PubKey(),
		},
		{
			PkScript: []byte{
				0x51,
				0x20,
				0xBB,
			},
			Amount:    15000,
			ClientKey: recipientKeyB.PubKey(),
		},
	}

	w := newTestWalletForSend(t, mgr, roundActor)

	operatorFee := btcutil.Amount(1000)
	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients:    recipients,
		OperatorFee:   operatorFee,
		DustLimit:     546,
		OperatorKey:   operatorKey,
		VTXOExitDelay: 144,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	sendResp, ok := resp.(*SendVTXOsResponse)
	require.True(t, ok, "expected *SendVTXOsResponse")
	require.Equal(t, "submitted", sendResp.Status)

	// Expected change: 55000 - (20000+15000) - 1000 = 19000.
	expectedChange := btcutil.Amount(19000)
	require.Equal(t, expectedChange, sendResp.ChangeAmount)
	require.Equal(t, 2, sendResp.SelectedCount)

	// Verify the captured intent package.
	require.NotNil(t, roundActor.capturedIntent)
	intent := roundActor.capturedIntent

	// --- Forfeits: one per selected VTXO, correct outpoints. ---
	require.Len(t, intent.Forfeits, 2)
	require.Equal(t,
		testOutpoint(0), *intent.Forfeits[0].VTXOOutpoint,
	)
	require.Equal(t,
		btcutil.Amount(30000), intent.Forfeits[0].Amount,
	)
	require.Equal(t,
		testOutpoint(1), *intent.Forfeits[1].VTXOOutpoint,
	)
	require.Equal(t,
		btcutil.Amount(25000), intent.Forfeits[1].Amount,
	)

	// --- VTXOs: 2 recipients + 1 change = 3 total. ---
	require.Len(t, intent.VTXOs, 3)

	// Recipient A: ClientKey is the recipient's key.
	vtxoA := intent.VTXOs[0]
	require.Equal(t, btcutil.Amount(20000), vtxoA.Amount)
	// PkScript is derived from the VTXO descriptor, not the
	// RPC-provided value. Verify it's a valid P2TR (34 bytes).
	require.Len(t, vtxoA.PkScript, 34)
	require.Equal(t, byte(0x51), vtxoA.PkScript[0])
	require.True(t, vtxoA.ClientKey.IsEqual(
		recipientKeyA.PubKey(),
	))
	require.Nil(t, vtxoA.OwnerKey.PubKey)
	require.True(t, vtxoA.OperatorKey.IsEqual(operatorKey))
	require.Equal(t, uint32(144), vtxoA.Expiry)

	// Recipient B: ClientKey is the recipient's key.
	vtxoB := intent.VTXOs[1]
	require.Equal(t, btcutil.Amount(15000), vtxoB.Amount)
	require.Len(t, vtxoB.PkScript, 34)
	require.Equal(t, byte(0x51), vtxoB.PkScript[0])
	require.True(t, vtxoB.ClientKey.IsEqual(
		recipientKeyB.PubKey(),
	))
	require.Nil(t, vtxoB.OwnerKey.PubKey)

	// Change VTXO: amount matches, ClientKey is sender-derived
	// (NOT a recipient key).
	vtxoChange := intent.VTXOs[2]
	require.Equal(t, expectedChange, vtxoChange.Amount)
	require.False(
		t,
		vtxoChange.ClientKey.IsEqual(
			recipientKeyA.PubKey(),
		),
	)
	require.False(
		t,
		vtxoChange.ClientKey.IsEqual(
			recipientKeyB.PubKey(),
		),
	)
	require.NotNil(t, vtxoChange.OwnerKey.PubKey)
	require.True(
		t, vtxoChange.OwnerKey.PubKey.IsEqual(
			vtxoChange.ClientKey,
		),
	)
	require.Equal(t, types.VTXOOwnerKeyFamily, vtxoChange.OwnerKey.Family)
	require.True(t, vtxoChange.OperatorKey.IsEqual(operatorKey))

	// Signing keys are NOT derived in the wallet — the round FSM
	// derives them during RegistrationSent per #210. Verify they
	// are empty.
	for i, vtxo := range intent.VTXOs {
		require.Nil(
			t, vtxo.SigningKey.PubKey, "vtxo %d: SigningKey "+
				"should be nil (FSM derives it)", i,
		)
	}

	// --- IsChange contract (#270) ---
	//
	// Under seal-time the server rejects any intent whose
	// VTXORequests + LeaveRequests list doesn't carry exactly one
	// IsChange=true marker (with a single-output exception). The
	// directed-send output shape is "N recipients + 1 self-change",
	// so exactly one request — the change — must set the bit.
	// Recipients MUST NOT set it, otherwise the server would
	// deduct fee from the recipient amount.
	require.False(
		t, vtxoA.IsChange, "recipient A must not carry IsChange "+
			"(would deduct fee from send amount)",
	)
	require.False(
		t, vtxoB.IsChange, "recipient B must not carry IsChange "+
			"(would deduct fee from send amount)",
	)
	require.True(
		t, vtxoChange.IsChange, "self-change must carry "+
			"IsChange=true so the server stamps the residual "+
			"onto this output",
	)

	// Count the markers to catch future regressions that split
	// the bit across multiple outputs.
	var markers int
	for _, vtxo := range intent.VTXOs {
		if vtxo.IsChange {
			markers++
		}
	}
	require.Equal(
		t, 1, markers, "intent must carry exactly one IsChange "+
			"marker across VTXORequests (server-side "+
			"validateChangeDesignation rejects intents with 0 "+
			"or 2+ markers)",
	)
}
