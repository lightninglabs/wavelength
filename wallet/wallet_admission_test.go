package wallet

import (
	"context"
	"fmt"
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

	switch msg.(type) {
	case *actormsg.SelectAndReserveSpendRequest:
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
func newTestWalletWithManager(t *testing.T,
	mgr *mockVTXOManagerBehavior) *Ark {

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
		nil, nil, nil, nil, system,
		fn.None[ledger.Sink](), btclog.Disabled,
	)
}

// testOutpoint returns a deterministic outpoint for testing.
func testOutpoint(idx uint32) wire.OutPoint {
	return wire.OutPoint{
		Hash:  chainhash.Hash{byte(idx), 0xaa},
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
					PkScript: []byte{0x51, 0x20, 0x01},
				},
				{
					Outpoint: testOutpoint(1),
					Amount:   30000,
					PkScript: []byte{0x51, 0x20, 0x02},
				},
			},
			TotalSelected: 80000,
		},
	}
	w := newTestWalletWithManager(t, mgr)

	req := &SelectAndLockVTXOsRequest{
		TargetAmount: 70000,
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
		Outpoints: []wire.OutPoint{testOutpoint(99)},
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
		Outpoints: []wire.OutPoint{testOutpoint(7)},
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
		nil, nil, nil, nil, nil,
		fn.None[ledger.Sink](), btclog.Disabled,
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
type mockRoundActorBehavior struct {
	// registerErr when set causes RegisterIntentMsg to fail.
	registerErr error

	// registerCalls tracks how many times RegisterIntentMsg was received.
	registerCalls int

	// capturedIntent holds the last RegisterIntentMsg received, so
	// tests can inspect the intent package contents.
	capturedIntent *actormsg.RegisterIntentMsg
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
	mgr *mockVTXOManagerBehavior,
	roundActor *mockRoundActorBehavior,
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
		system, actormsg.VTXOManagerServiceKeyName,
		mgrKey, mgr,
	)

	// Register the mock round actor.
	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName,
		roundKey, roundActor,
	)

	return NewArk(
		nil, nil, vtxoReader, nil, system,
		fn.None[ledger.Sink](), btclog.Disabled,
	)
}

// testVTXOReader returns a VTXOReader backed by a static map.
func testVTXOReader(
	descs map[wire.OutPoint]*VTXODescriptor) VTXOReader {

	return VTXOReaderFunc(func(_ context.Context,
		outpoint wire.OutPoint) (*VTXODescriptor, error) {

		desc, ok := descs[outpoint]
		if !ok {
			return nil, fmt.Errorf(
				"vtxo not found: %s", outpoint,
			)
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
			Outpoint:       op,
			Amount:         50000,
			PkScript:       []byte{0x51, 0x20, 0x01},
			PolicyTemplate: []byte{0xde, 0xad, 0xbe, 0xef},
			Expiry:         100,
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
		TargetOutpoints: []wire.OutPoint{op},
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
			Outpoint:       op,
			Amount:         50000,
			PkScript:       []byte{0x51, 0x20, 0x01},
			PolicyTemplate: []byte{0xde, 0xad, 0xbe, 0xef},
			Expiry:         100,
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
		TargetOutpoints: []wire.OutPoint{op},
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
func TestRefreshDeductsPerInputOperatorFee(t *testing.T) {
	t.Parallel()

	op1 := testOutpoint(0)
	op2 := testOutpoint(1)

	// Policy template need only be non-empty so
	// EffectivePolicyTemplate returns ok — the round actor mock
	// does not parse it.
	policy := []byte{0xde, 0xad, 0xbe, 0xef}

	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op1: {
			Outpoint:       op1,
			Amount:         50_000,
			PkScript:       []byte{0x51, 0x20, 0x01},
			PolicyTemplate: policy,
			Expiry:         100,
		},
		op2: {
			Outpoint:       op2,
			Amount:         80_000,
			PkScript:       []byte{0x51, 0x20, 0x02},
			PolicyTemplate: policy,
			Expiry:         200,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	const (
		fee1 = btcutil.Amount(123)
		fee2 = btcutil.Amount(456)
	)
	req := &RefreshVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{op1, op2},
		OperatorFees: map[wire.OutPoint]btcutil.Amount{
			op1: fee1,
			op2: fee2,
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	require.Equal(t, 1, roundActor.registerCalls)
	require.NotNil(t, roundActor.capturedIntent)

	// The intent should carry one forfeit at full amount and one
	// new VTXO at (amount - fee) per input.
	forfeits := roundActor.capturedIntent.Forfeits
	vtxos := roundActor.capturedIntent.VTXOs
	require.Len(t, forfeits, 2)
	require.Len(t, vtxos, 2)

	forfeitByOp := make(map[wire.OutPoint]btcutil.Amount, 2)
	for _, f := range forfeits {
		require.NotNil(t, f.VTXOOutpoint)
		forfeitByOp[*f.VTXOOutpoint] = f.Amount
	}
	require.Equal(t,
		vtxoDescs[op1].Amount, forfeitByOp[op1],
		"forfeit input carries original amount",
	)
	require.Equal(t,
		vtxoDescs[op2].Amount, forfeitByOp[op2],
		"forfeit input carries original amount",
	)

	// Aggregate implicit fee = Σforfeits − Σnew_vtxos. Computed
	// over the slices independently so the assertion does not rely
	// on forfeits and vtxos being appended in the same order.
	var sumForfeits, sumNewVTXOs btcutil.Amount
	for _, f := range forfeits {
		sumForfeits += f.Amount
	}
	for _, v := range vtxos {
		sumNewVTXOs += v.Amount
	}
	require.Equal(t, fee1+fee2, sumForfeits-sumNewVTXOs,
		"aggregate implicit fee matches the per-input quote sum")

	// Per-input deduction: the multiset of new VTXO amounts must
	// equal {desc.Amount − fee} across both inputs. Multiset-style
	// check so a swap between op1 and op2 would be caught but the
	// test doesn't wire in a forfeit→vtxo index pairing assumption.
	wantNewAmounts := map[btcutil.Amount]int{
		vtxoDescs[op1].Amount - fee1: 1,
		vtxoDescs[op2].Amount - fee2: 1,
	}
	gotNewAmounts := map[btcutil.Amount]int{}
	for _, v := range vtxos {
		gotNewAmounts[v.Amount]++
	}
	require.Equal(t, wantNewAmounts, gotNewAmounts,
		"each new VTXO amount equals its source VTXO's amount "+
			"minus its operator fee")
}

// TestRefreshOrphansNoForfeitOnFeeValidationFailure verifies that if
// fee validation fails for one outpoint in a mixed request, that
// outpoint's forfeit is NOT appended to the intent package — so the
// RegisterIntentMsg submitted to the round actor stays strictly
// paired (len(Forfeits) == len(VTXOs)) and no VTXO is reserved into
// PendingForfeitState without a matching replacement request.
func TestRefreshOrphansNoForfeitOnFeeValidationFailure(t *testing.T) {
	t.Parallel()

	goodOp := testOutpoint(0)
	badOp := testOutpoint(1)

	policy := []byte{0xde, 0xad, 0xbe, 0xef}

	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		goodOp: {
			Outpoint:       goodOp,
			Amount:         50_000,
			PkScript:       []byte{0x51, 0x20, 0x01},
			PolicyTemplate: policy,
			Expiry:         100,
		},
		badOp: {
			Outpoint:       badOp,
			Amount:         1_000,
			PkScript:       []byte{0x51, 0x20, 0x02},
			PolicyTemplate: policy,
			Expiry:         200,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	const goodFee = btcutil.Amount(123)

	// badOp's fee exceeds its VTXO amount so validation rejects it.
	// goodOp carries a valid fee so it should survive and land in
	// the intent alone.
	req := &RefreshVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{goodOp, badOp},
		OperatorFees: map[wire.OutPoint]btcutil.Amount{
			goodOp: goodFee,
			badOp:  vtxoDescs[badOp].Amount + 1,
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	require.Equal(t, 1, roundActor.registerCalls)
	require.NotNil(t, roundActor.capturedIntent)

	forfeits := roundActor.capturedIntent.Forfeits
	vtxos := roundActor.capturedIntent.VTXOs

	// Only goodOp should reach the intent. A forfeit for badOp
	// without a matching VTXO would be a mismatched package the
	// server rejects and a PendingForfeitState reservation with
	// no replacement.
	require.Len(t, forfeits, 1,
		"only the valid outpoint's forfeit is appended")
	require.Len(t, vtxos, 1,
		"forfeit and VTXO slices stay paired")
	require.NotNil(t, forfeits[0].VTXOOutpoint)
	require.Equal(t, goodOp, *forfeits[0].VTXOOutpoint)
	require.Equal(t,
		vtxoDescs[goodOp].Amount-goodFee, vtxos[0].Amount,
		"the surviving new VTXO carries the fee-adjusted amount")

	resp, err := result.Unpack()
	require.NoError(t, err)
	refreshResp, ok := resp.(*RefreshVTXOsResponse)
	require.True(t, ok)
	require.Equal(t, 1, refreshResp.RefreshingCount,
		"response reports the count of successful refreshes")
	require.Contains(t, refreshResp.Errors, badOp,
		"response surfaces the rejected outpoint in Errors")
	require.NotContains(t, refreshResp.Errors, goodOp)
}

// TestRefreshFailsOnManagerRejection verifies that the wallet surfaces
// manager reservation errors without sending to the round actor.
func TestRefreshFailsOnManagerRejection(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint:       op,
			Amount:         50000,
			PkScript:       []byte{0x51, 0x20, 0x01},
			PolicyTemplate: []byte{0xde, 0xad, 0xbe, 0xef},
			Expiry:         100,
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
		TargetOutpoints: []wire.OutPoint{op},
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
			PkScript: []byte{0x51, 0x20, 0x01},
			Expiry:   100,
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
		TargetOutpoints: []wire.OutPoint{op},
		DestOutput:      &wire.TxOut{Value: 49000},
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
			PkScript: []byte{0x51, 0x20, 0x01},
			Expiry:   100,
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
		TargetOutpoints: []wire.OutPoint{op},
		DestOutput:      &wire.TxOut{Value: 49000},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "round expired")

	// Manager should have received the release call.
	require.Equal(t, 1, mgr.forfeitReleaseCalls)
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
func (s *stubBackend) ListUnspent(_ context.Context,
	_ int32, _ int32) ([]*Utxo, error) {

	return nil, nil
}

// GetTransaction is a no-op stub.
func (s *stubBackend) GetTransaction(_ context.Context,
	_ chainhash.Hash) (*TxInfo, error) {

	return nil, fmt.Errorf("not implemented")
}

// GetBlock is a no-op stub.
func (s *stubBackend) GetBlock(_ context.Context,
	_ chainhash.Hash) (*wire.MsgBlock, error) {

	return nil, fmt.Errorf("not implemented")
}

// newTestWalletForSend creates a wallet with mock VTXO manager, mock
// round actor, and a stubBackend for key derivation. This is the
// setup needed for directed send tests.
func newTestWalletForSend(t *testing.T,
	mgr *mockVTXOManagerBehavior,
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
		system, actormsg.VTXOManagerServiceKeyName,
		mgrKey, mgr,
	)

	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName,
		roundKey, roundActor,
	)

	backend := &stubBackend{}

	return NewArk(
		backend, nil, nil, nil, system,
		fn.None[ledger.Sink](), btclog.Disabled,
	)
}

// testSendRecipient returns a SendRecipient for testing.
func testSendRecipient(amount btcutil.Amount) SendRecipient {
	priv, _ := btcec.NewPrivateKey()

	return SendRecipient{
		PkScript:  []byte{0x51, 0x20, 0x01},
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
					PkScript: []byte{0x51, 0x20, 0x01},
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
					PkScript: []byte{0x51, 0x20, 0x01},
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

// TestSendVTXOsDustChange verifies that change below the dust limit
// is rejected.
func TestSendVTXOsDustChange(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectForfeitResp: &actormsg.SelectAndReserveForfeitResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: testOutpoint(0),
					Amount:   41500,
					PkScript: []byte{0x51, 0x20, 0x01},
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
	require.Contains(t, err.Error(), "below dust limit")

	// Round should not have been called.
	require.Equal(t, 0, roundActor.registerCalls)
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
					PkScript: []byte{0x51, 0x20, 0x01},
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
					PkScript: []byte{0x51, 0x20, 0x01},
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
					PkScript: []byte{0x51, 0x20, 0x01},
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
					PkScript: []byte{0x51, 0x20, 0x01},
				},
				{
					Outpoint: testOutpoint(1),
					Amount:   25000,
					PkScript: []byte{0x51, 0x20, 0x02},
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
			PkScript:  []byte{0x51, 0x20, 0xAA},
			Amount:    20000,
			ClientKey: recipientKeyA.PubKey(),
		},
		{
			PkScript:  []byte{0x51, 0x20, 0xBB},
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
	require.False(t, vtxoChange.ClientKey.IsEqual(
		recipientKeyA.PubKey(),
	))
	require.False(t, vtxoChange.ClientKey.IsEqual(
		recipientKeyB.PubKey(),
	))
	require.NotNil(t, vtxoChange.OwnerKey.PubKey)
	require.True(t, vtxoChange.OwnerKey.PubKey.IsEqual(
		vtxoChange.ClientKey,
	))
	require.Equal(t, types.VTXOOwnerKeyFamily, vtxoChange.OwnerKey.Family)
	require.True(t, vtxoChange.OperatorKey.IsEqual(operatorKey))

	// Signing keys are NOT derived in the wallet — the round FSM
	// derives them during RegistrationSent per #210. Verify they
	// are empty.
	for i, vtxo := range intent.VTXOs {
		require.Nil(t, vtxo.SigningKey.PubKey,
			"vtxo %d: SigningKey should be nil "+
				"(FSM derives it)", i,
		)
	}
}
