package wallet

import (
	"context"
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
	"github.com/lightninglabs/darepo-client/lib/arkscript"
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

	// forfeitReserveCalls tracks how many ReserveForfeitRequest were
	// received.
	forfeitReserveCalls int

	// forfeitReserveErr when set causes ReserveForfeitRequest to fail.
	forfeitReserveErr error

	// forfeitReleaseResp is returned for ReleaseForfeitRequest.
	forfeitReleaseResp *actormsg.ReleaseForfeitResponse

	// forfeitReleaseErr when set causes ReleaseForfeitRequest to fail.
	forfeitReleaseErr error

	// forfeitReleaseCalls tracks how many ReleaseForfeitRequest were
	// received.
	forfeitReleaseCalls int

	// customActivateReq is the last ActivateCustomForfeitInputsRequest
	// received.
	customActivateReq *actormsg.ActivateCustomForfeitInputsRequest

	// customActivateResp is returned for
	// ActivateCustomForfeitInputsRequest.
	customActivateResp *actormsg.ActivateCustomForfeitInputsResponse

	// customActivateErr when set causes
	// ActivateCustomForfeitInputsRequest to fail.
	customActivateErr error

	// customDropReq is the last DropCustomForfeitInputsRequest received.
	customDropReq *actormsg.DropCustomForfeitInputsRequest

	// customDropResp is returned for DropCustomForfeitInputsRequest.
	customDropResp *actormsg.DropCustomForfeitInputsResponse

	// customDropErr when set causes DropCustomForfeitInputsRequest to fail.
	customDropErr error

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
		m.forfeitReserveCalls++

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

	case *actormsg.ActivateCustomForfeitInputsRequest:
		m.customActivateReq = msg

		if m.customActivateErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.customActivateErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](
			m.customActivateResp,
		)

	case *actormsg.DropCustomForfeitInputsRequest:
		m.customDropReq = msg

		if m.customDropErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.customDropErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](
			m.customDropResp,
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

	// registerGate, when set, blocks RegisterIntentMsg handling until
	// the channel is closed. Tests use this to keep the Ask pending
	// while exercising caller-side Await cancellation.
	registerGate <-chan struct{}

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

	// capturedTriggerBoards holds every TriggerBoardMsg received in
	// order, so idempotency tests can inspect the Outpoints each
	// successive trigger sized its amounts over. Read under mu.
	capturedTriggerBoards []*actormsg.TriggerBoardMsg
}

type triggerBoardMsgs = []*actormsg.TriggerBoardMsg

// TriggerBoardCalls returns a snapshot of triggerBoardCalls under the
// mock's mutex so test goroutines can poll the counter without racing
// the actor's Receive loop.
func (m *mockRoundActorBehavior) TriggerBoardCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.triggerBoardCalls
}

// CapturedTriggerBoards returns a snapshot of every TriggerBoardMsg the
// mock received, in order, under the mock's mutex.
func (m *mockRoundActorBehavior) CapturedTriggerBoards() triggerBoardMsgs {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(triggerBoardMsgs, len(m.capturedTriggerBoards))
	copy(out, m.capturedTriggerBoards)

	return out
}

// Receive processes round actor messages from the wallet.
func (m *mockRoundActorBehavior) Receive(_ context.Context,
	msg actormsg.RoundReceivable) fn.Result[actormsg.RoundActorResp] {

	switch typedMsg := msg.(type) {
	case *actormsg.RegisterIntentMsg:
		if m.registerGate != nil {
			<-m.registerGate
		}

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
		m.capturedTriggerBoards = append(
			m.capturedTriggerBoards, typedMsg,
		)
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

// TestCustomRefreshActivatesSignerWithoutLiveReservation verifies that the
// custom refresh path composes a round intent package from caller-provided
// metadata and deliberately bypasses the normal wallet-owned VTXO reservation
// path. Swap vHTLCs are tracked by their swap FSMs, so the wallet activates a
// temporary PendingForfeit signer actor instead of requiring an existing live
// descriptor for the old vHTLC.
func TestCustomRefreshActivatesSignerWithoutLiveReservation(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	authSpend := &arkscript.SpendPath{
		RequiredSequence: 1,
	}
	forfeitSpend := &arkscript.SpendPath{
		RequiredSequence: 2,
	}
	op := testOutpoint(42)
	commitmentTxID := chainhash.HashH([]byte("custom-refresh-commitment"))
	policyTemplate := []byte{0xde, 0xad, 0xbe, 0xef}
	pkScript := []byte{0x51, 0x20, 0x01}
	ancestry := []types.Ancestry{{
		CommitmentTxID: commitmentTxID,
		TreeDepth:      3,
	}}
	activateResp := &actormsg.ActivateCustomForfeitInputsResponse{
		ActivatedCount: 1,
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveErr:  fmt.Errorf("should not reserve"),
		customActivateResp: activateResp,
	}
	roundActor := &mockRoundActorBehavior{}
	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(nil),
	)

	result := w.Receive(t.Context(), &RefreshCustomVTXOsRequest{
		Inputs: []CustomRefreshInput{{
			Outpoint:       op,
			Amount:         42_000,
			PkScript:       pkScript,
			PolicyTemplate: policyTemplate,
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientPriv.PubKey(),
			},
			OperatorKey:    operatorPriv.PubKey(),
			RelativeExpiry: 2,
			RoundID:        "round-custom-refresh",
			CommitmentTxID: commitmentTxID,
			BatchExpiry:    1234,
			ChainDepth:     1,
			CreatedHeight:  99,
			Ancestry:       ancestry,
			AuthSpend:      authSpend,
			ForfeitSpend:   forfeitSpend,
		}},
		Outputs: []CustomRefreshOutput{{
			Amount:         41_000,
			PolicyTemplate: policyTemplate,
			PkScript:       pkScript,
			FixedAmount:    true,
		}},
	})
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	respVal, err := result.Unpack()
	require.NoError(t, err)
	resp, ok := respVal.(*RefreshCustomVTXOsResponse)
	require.True(t, ok, "unexpected response type: %T", respVal)
	require.Equal(t, 1, resp.RefreshingCount)

	require.Zero(t, mgr.forfeitReserveCalls)
	require.NotNil(t, mgr.customActivateReq)
	require.Len(t, mgr.customActivateReq.Inputs, 1)
	require.Equal(t, op, mgr.customActivateReq.Inputs[0].Outpoint)
	require.Equal(
		t, btcutil.Amount(42_000),
		mgr.customActivateReq.Inputs[0].Amount,
	)
	require.Equal(t, pkScript, mgr.customActivateReq.Inputs[0].PkScript)
	require.Equal(
		t, policyTemplate,
		mgr.customActivateReq.Inputs[0].PolicyTemplate,
	)
	require.Equal(
		t, clientPriv.PubKey(),
		mgr.customActivateReq.Inputs[0].ClientKey.PubKey,
	)
	require.True(
		t, operatorPriv.PubKey().IsEqual(
			mgr.customActivateReq.Inputs[0].OperatorKey,
		),
	)
	require.Equal(
		t, uint32(2), mgr.customActivateReq.Inputs[0].RelativeExpiry,
	)
	require.Equal(
		t, "round-custom-refresh",
		mgr.customActivateReq.Inputs[0].RoundID,
	)
	require.Equal(
		t, commitmentTxID,
		mgr.customActivateReq.Inputs[0].CommitmentTxID,
	)
	require.EqualValues(
		t, 1234, mgr.customActivateReq.Inputs[0].BatchExpiry,
	)
	require.Equal(t, 1, mgr.customActivateReq.Inputs[0].ChainDepth)
	require.EqualValues(
		t, 99, mgr.customActivateReq.Inputs[0].CreatedHeight,
	)
	require.Equal(t, ancestry, mgr.customActivateReq.Inputs[0].Ancestry)
	require.Equal(t, 1, roundActor.registerCalls)
	intent := roundActor.capturedIntent
	require.NotNil(t, intent)
	require.Len(t, intent.Forfeits, 1)
	require.Len(t, intent.VTXOs, 1)

	require.Equal(t, op, *intent.Forfeits[0].VTXOOutpoint)
	require.Equal(t, btcutil.Amount(42_000), intent.Forfeits[0].Amount)
	require.Same(t, authSpend, intent.Forfeits[0].AuthSpend)
	require.Same(t, forfeitSpend, intent.Forfeits[0].ForfeitSpend)

	require.Equal(t, btcutil.Amount(41_000), intent.VTXOs[0].Amount)
	require.Equal(t, policyTemplate, intent.VTXOs[0].PolicyTemplate)
	require.Equal(t, pkScript, intent.VTXOs[0].PkScript)
	require.True(t, intent.VTXOs[0].FixedAmount)
	require.Equal(t, types.VTXOOriginRoundRefresh, intent.VTXOs[0].Origin)
	require.Nil(t, intent.VTXOs[0].OwnerKey.PubKey)
}

// TestCustomRefreshDropsSignerOnRoundRejection verifies that custom refresh
// cleanup deletes the temporary signer descriptor instead of releasing it to
// LiveState when the round rejects the intent.
func TestCustomRefreshDropsSignerOnRoundRejection(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	op := testOutpoint(43)
	activateResp := &actormsg.ActivateCustomForfeitInputsResponse{
		ActivatedCount: 1,
	}
	mgr := &mockVTXOManagerBehavior{
		customActivateResp: activateResp,
		customDropResp: &actormsg.DropCustomForfeitInputsResponse{
			DroppedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{
		registerErr: fmt.Errorf("round full"),
	}
	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(nil),
	)

	result := w.Receive(t.Context(), &RefreshCustomVTXOsRequest{
		Inputs: []CustomRefreshInput{{
			Outpoint:       op,
			Amount:         42_000,
			PkScript:       []byte{0x51, 0x20, 0x01},
			PolicyTemplate: []byte{0xde, 0xad, 0xbe, 0xef},
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientPriv.PubKey(),
			},
			OperatorKey: operatorPriv.PubKey(),
			AuthSpend: &arkscript.SpendPath{
				RequiredSequence: 1,
			},
			ForfeitSpend: &arkscript.SpendPath{
				RequiredSequence: 2,
			},
		}},
		Outputs: []CustomRefreshOutput{{
			Amount:         41_000,
			PolicyTemplate: []byte{0xde, 0xad, 0xbe, 0xef},
			PkScript:       []byte{0x51, 0x20, 0x02},
		}},
	})
	require.True(t, result.IsErr())
	require.ErrorContains(
		t, result.Err(),
		"round rejected custom refresh intent",
	)

	require.Zero(t, mgr.forfeitReleaseCalls)
	require.NotNil(t, mgr.customDropReq)
	require.Equal(t, []wire.OutPoint{op}, mgr.customDropReq.Outpoints)
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

// TestSendVTXOsSuccess verifies the happy path: select VTXOs, build
// intent with forfeits + recipient + change, register with round.
func TestSendVTXOsSuccess(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
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
			},
			TotalSelected: 50000,
		},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletForSend(t, mgr, roundActor)

	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients:    []SendRecipient{testSendRecipient(40000)},
		OperatorFee:   1000,
		DustLimit:     546,
		OperatorKey:   testOperatorKey(),
		VTXOExitDelay: 144,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	sendResp, ok := resp.(*SendVTXOsResponse)
	require.True(t, ok)
	require.Equal(t, "submitted", sendResp.Status)
	require.Equal(t, 1, sendResp.SelectedCount)
	require.Equal(t, btcutil.Amount(50000), sendResp.TotalSelected)
	require.Equal(t, btcutil.Amount(9000), sendResp.ChangeAmount)
	require.Equal(t, 1, roundActor.registerCalls)
}

// TestSendVTXOsNoChange verifies that exact-amount sends produce no
// change output.
func TestSendVTXOsNoChange(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: testOutpoint(0),
					Amount:   41000,
					PkScript: []byte{
						0x51,
						0x20,
						0x01,
					},
				},
			},
			TotalSelected: 41000,
		},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletForSend(t, mgr, roundActor)

	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients:    []SendRecipient{testSendRecipient(40000)},
		OperatorFee:   1000,
		DustLimit:     546,
		OperatorKey:   testOperatorKey(),
		VTXOExitDelay: 144,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	sendResp, ok := resp.(*SendVTXOsResponse)
	require.True(t, ok, "expected *SendVTXOsResponse")
	require.Equal(t, btcutil.Amount(0), sendResp.ChangeAmount)
	require.Equal(t, 1, roundActor.registerCalls)
}

// TestSendVTXOsMultiRecipientZeroChangeRejects verifies the #270
// pre-flight guard: a multi-recipient directed send whose coin
// selection covers the target exactly (change == 0) must be
// rejected locally. Without this, the intent ships with zero
// IsChange=true markers across 2+ outputs; the server rejects with
// INVALID_CHANGE_DESIGNATION after a round slot is already
// consumed. Single-recipient exact-match sends are still allowed
// (the proto's single-output change marker exception applies).
func TestSendVTXOsMultiRecipientZeroChangeRejects(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: testOutpoint(0),
					Amount:   41000,
					PkScript: []byte{
						0x51,
						0x20,
						0x01,
					},
				},
			},
			TotalSelected: 41000,
		},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletForSend(t, mgr, roundActor)

	// Two recipients summing to 40000, operator fee 1000 → total
	// 41000 == selected → change 0. Must reject, and the round
	// actor must not see any register call.
	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients: []SendRecipient{
			testSendRecipient(25000),
			testSendRecipient(15000),
		},
		OperatorFee:   1000,
		DustLimit:     546,
		OperatorKey:   testOperatorKey(),
		VTXOExitDelay: 144,
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "multi-recipient")
	require.Zero(t, roundActor.registerCalls)
}

// TestSendVTXOsBelowFloorChange verifies that change below the VTXO floor
// is rejected.
func TestSendVTXOsBelowFloorChange(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: testOutpoint(0),
					Amount:   41500,
					PkScript: []byte{
						0x51,
						0x20,
						0x01,
					},
				},
			},
			TotalSelected: 41500,
		},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletForSend(t, mgr, roundActor)

	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients:    []SendRecipient{testSendRecipient(40000)},
		OperatorFee:   1000,
		DustLimit:     546,
		OperatorKey:   testOperatorKey(),
		VTXOExitDelay: 144,
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "below VTXO minimum")

	// Round should not have been called.
	require.Equal(t, 0, roundActor.registerCalls)
}

// TestSendVTXOsFloorChange verifies that change exactly at the VTXO floor
// is accepted.
func TestSendVTXOsFloorChange(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: testOutpoint(0),
					Amount:   41546,
					PkScript: []byte{
						0x51,
						0x20,
						0x01,
					},
				},
			},
			TotalSelected: 41546,
		},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletForSend(t, mgr, roundActor)

	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients:    []SendRecipient{testSendRecipient(40000)},
		OperatorFee:   1000,
		DustLimit:     546,
		OperatorKey:   testOperatorKey(),
		VTXOExitDelay: 144,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	sendResp, ok := resp.(*SendVTXOsResponse)
	require.True(t, ok)
	require.Equal(t, btcutil.Amount(546), sendResp.ChangeAmount)
	require.Equal(t, 1, roundActor.registerCalls)
}

// TestSendVTXOsDryRun verifies that dry-run validates selection then
// immediately releases the reservation.
func TestSendVTXOsDryRun(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
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
			},
			TotalSelected: 50000,
		},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletForSend(t, mgr, roundActor)

	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients:    []SendRecipient{testSendRecipient(40000)},
		OperatorFee:   1000,
		DustLimit:     546,
		OperatorKey:   testOperatorKey(),
		VTXOExitDelay: 144,
		DryRun:        true,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	sendResp, ok := resp.(*SendVTXOsResponse)
	require.True(t, ok, "expected *SendVTXOsResponse")
	require.Equal(t, "preview", sendResp.Status)
	require.Equal(t, btcutil.Amount(9000), sendResp.ChangeAmount)

	// Round should NOT have been called.
	require.Equal(t, 0, roundActor.registerCalls)

	// Release should have been called.
	require.Equal(t, 1, mgr.forfeitReleaseCalls)
}

// TestSendVTXOsDryRunReleaseFails verifies that a dry-run succeeds
// even when the deferred forfeit release fails. The release is
// best-effort — errors are logged but don't propagate.
func TestSendVTXOsDryRunReleaseFails(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
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
			},
			TotalSelected: 50000,
		},
		forfeitReleaseErr: fmt.Errorf("release timeout"),
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletForSend(t, mgr, roundActor)

	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients:    []SendRecipient{testSendRecipient(40000)},
		OperatorFee:   1000,
		DustLimit:     546,
		OperatorKey:   testOperatorKey(),
		VTXOExitDelay: 144,
		DryRun:        true,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	sendResp, ok := resp.(*SendVTXOsResponse)
	require.True(t, ok)
	require.Equal(t, "preview", sendResp.Status)
}

// TestSendVTXOsRoundRejectsAndReleases verifies that when the round
// rejects the intent, the forfeit reservation is released.
func TestSendVTXOsRoundRejectsAndReleases(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
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
			},
			TotalSelected: 50000,
		},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{
		registerErr: fmt.Errorf("round full"),
	}

	w := newTestWalletForSend(t, mgr, roundActor)

	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients:    []SendRecipient{testSendRecipient(40000)},
		OperatorFee:   1000,
		DustLimit:     546,
		OperatorKey:   testOperatorKey(),
		VTXOExitDelay: 144,
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "round full")
	require.Equal(t, 1, mgr.forfeitReleaseCalls)
}

// TestSendVTXOsSelectionFails verifies that the send fails gracefully
// when the manager reports insufficient funds.
func TestSendVTXOsSelectionFails(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitErr: fmt.Errorf("insufficient funds"),
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletForSend(t, mgr, roundActor)

	result := w.Receive(t.Context(), &SendVTXOsRequest{
		Recipients:    []SendRecipient{testSendRecipient(100000)},
		OperatorFee:   1000,
		DustLimit:     546,
		OperatorKey:   testOperatorKey(),
		VTXOExitDelay: 144,
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "insufficient funds")
	require.Equal(t, 0, roundActor.registerCalls)
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
