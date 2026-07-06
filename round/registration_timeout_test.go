package round

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/stretchr/testify/require"
)

// regTimeoutOutpoint builds a deterministic VTXO outpoint for the
// registration-timeout tests.
func regTimeoutOutpoint(seed byte, index uint32) wire.OutPoint {
	return wire.OutPoint{
		Hash: chainhash.Hash{
			seed,
		},
		Index: index,
	}
}

// mkForfeit builds a forfeit request for the given outpoint so the
// IntentSentState carries something to release on timeout.
func mkForfeit(op wire.OutPoint, amount btcutil.Amount) types.ForfeitRequest {
	opCopy := op

	return types.ForfeitRequest{
		VTXOOutpoint: &opCopy,
		Amount:       amount,
	}
}

// findOutbox returns the first outbox message of type T, or false.
func findOutbox[T ClientOutMsg](outbox []ClientOutMsg) (T, bool) {
	for _, m := range outbox {
		if typed, ok := m.(T); ok {
			return typed, true
		}
	}

	var zero T

	return zero, false
}

// TestRegistrationTimeoutFailsAndReleasesForfeits is the core fix proof: when a
// RegistrationTimedOut event reaches IntentSentState (the server never returned
// a RoundJoined admission watermark), the FSM must fail the round as
// recoverable AND emit a ReleaseForfeitReservation for every forfeit-reserved
// input so they return to LiveState instead of being stranded in
// pending-forfeit (darepo-client#653).
func TestRegistrationTimeoutFailsAndReleasesForfeits(t *testing.T) {
	t.Parallel()

	op1 := regTimeoutOutpoint(0x01, 0)
	op2 := regTimeoutOutpoint(0x02, 1)

	s := &IntentSentState{
		Intents: Intents{
			Forfeits: []types.ForfeitRequest{
				mkForfeit(op1, 10_000),
				mkForfeit(op2, 20_000),
			},
		},
	}
	env := &ClientEnvironment{Log: btclog.Disabled}

	tr, err := s.ProcessEvent(
		context.Background(), &RegistrationTimedOut{}, env,
	)
	require.NoError(t, err)
	require.NotNil(t, tr.NextState)

	// The round must fail, and the failure must be recoverable so the
	// wallet/UI can offer a retry rather than a dead-end.
	failed, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)
	require.True(t, failed.Recoverable)

	require.True(t, tr.NewEvents.IsSome())
	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox

	// A failure notification is emitted for observability.
	_, ok = findOutbox[*RoundFailedNotification](outbox)
	require.True(t, ok, "expected RoundFailedNotification in outbox")

	// The forfeit reservation is released for exactly the reserved inputs.
	release, ok := findOutbox[*ReleaseForfeitReservation](outbox)
	require.True(
		t, ok, "expected ReleaseForfeitReservation in outbox",
	)
	require.ElementsMatch(
		t, []wire.OutPoint{op1, op2}, release.Outpoints,
	)
}

// TestRegistrationTimeoutIgnoredAfterAdmission verifies that a stale
// RegistrationTimedOut arriving after the round has already been admitted
// (AdmittedRoundID populated by RoundJoined) is ignored: the FSM stays in
// IntentSentState and does NOT fail the round or release its forfeits. This
// guards against a late timeout racing past the cancel and aborting a healthy,
// admitted round.
func TestRegistrationTimeoutIgnoredAfterAdmission(t *testing.T) {
	t.Parallel()

	op := regTimeoutOutpoint(0x03, 0)
	s := &IntentSentState{
		Intents: Intents{
			Forfeits: []types.ForfeitRequest{
				mkForfeit(op, 10_000),
			},
		},
		AdmittedRoundID: testRoundIDTr("already-admitted"),
	}
	env := &ClientEnvironment{Log: btclog.Disabled}

	tr, err := s.ProcessEvent(
		context.Background(), &RegistrationTimedOut{}, env,
	)
	require.NoError(t, err)

	// The round must remain parked in IntentSentState (a self-loop), not
	// transition to a failed state.
	_, ok := tr.NextState.(*IntentSentState)
	require.True(t, ok, "expected IntentSentState, got %T", tr.NextState)

	// No release and no failure notification: the admitted round is left
	// untouched.
	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, ok = findOutbox[*ReleaseForfeitReservation](outbox)
	require.False(t, ok, "admitted round must not release forfeits")
	_, ok = findOutbox[*RoundFailedNotification](outbox)
	require.False(t, ok, "admitted round must not be failed")
}

// TestRegistrationTimeoutNoForfeitsStillFails verifies a boarding-only round
// (no forfeit reservation) still fails recoverably on admission timeout, but
// does not emit a forfeit release (there is nothing to release).
func TestRegistrationTimeoutNoForfeitsStillFails(t *testing.T) {
	t.Parallel()

	s := &IntentSentState{Intents: Intents{}}
	env := &ClientEnvironment{Log: btclog.Disabled}

	tr, err := s.ProcessEvent(
		context.Background(), &RegistrationTimedOut{}, env,
	)
	require.NoError(t, err)

	failed, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)
	require.True(t, failed.Recoverable)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, ok = findOutbox[*RoundFailedNotification](outbox)
	require.True(t, ok, "expected RoundFailedNotification in outbox")

	_, ok = findOutbox[*ReleaseForfeitReservation](outbox)
	require.False(
		t, ok, "no forfeit reservation should be released for a "+
			"boarding-only round",
	)
}

// TestBoardingFailedRollsBackForfeitReservations verifies that a server-side
// admission failure releases both normal wallet reservations and custom refresh
// signer actors before any forfeit signatures are produced.
func TestBoardingFailedRollsBackForfeitReservations(t *testing.T) {
	t.Parallel()

	standardOp := regTimeoutOutpoint(0x07, 0)
	customOp := regTimeoutOutpoint(0x08, 1)
	s := &IntentSentState{
		Intents: Intents{
			Forfeits: []types.ForfeitRequest{
				mkForfeit(standardOp, 10_000),
				{
					VTXOOutpoint: &customOp,
					Amount:       20_000,
					AuthSpend: &arkscript.SpendPath{
						RequiredSequence: 1,
					},
					ForfeitSpend: &arkscript.SpendPath{
						RequiredSequence: 2,
					},
				},
			},
		},
	}
	env := &ClientEnvironment{Log: btclog.Disabled}

	tr, err := s.ProcessEvent(
		context.Background(), &BoardingFailed{
			Reason:      "join request invalid",
			Recoverable: true,
		}, env,
	)
	require.NoError(t, err)

	failed, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)
	require.True(t, failed.Recoverable)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, ok = findOutbox[*RoundFailedNotification](outbox)
	require.True(t, ok, "expected RoundFailedNotification in outbox")

	release, ok := findOutbox[*ReleaseForfeitReservation](outbox)
	require.True(t, ok, "expected ReleaseForfeitReservation in outbox")
	require.Equal(t, []wire.OutPoint{standardOp}, release.Outpoints)

	drop, ok := findOutbox[*DropCustomForfeitReservation](outbox)
	require.True(t, ok, "expected DropCustomForfeitReservation in outbox")
	require.Equal(t, []wire.OutPoint{customOp}, drop.Outpoints)
}

// TestQuoteRejectedRollsBackForfeitReservations verifies that quote rejection
// happens before forfeit signing and therefore rolls back both standard wallet
// reservations and custom refresh signer actors.
func TestQuoteRejectedRollsBackForfeitReservations(t *testing.T) {
	t.Parallel()

	standardOp := regTimeoutOutpoint(0x05, 0)
	customOp := regTimeoutOutpoint(0x06, 1)
	s := &QuoteReceivedState{
		Intents: Intents{
			Forfeits: []types.ForfeitRequest{
				mkForfeit(standardOp, 10_000),
				{
					VTXOOutpoint: &customOp,
					Amount:       20_000,
					AuthSpend: &arkscript.SpendPath{
						RequiredSequence: 1,
					},
					ForfeitSpend: &arkscript.SpendPath{
						RequiredSequence: 2,
					},
				},
			},
		},
	}
	env := &ClientEnvironment{Log: btclog.Disabled}

	tr, err := s.ProcessEvent(
		context.Background(), &QuoteRejected{
			RoundID: testRoundIDTr("quote-rejected"),
			Reason:  "fixed output cannot pay fees",
		}, env,
	)
	require.NoError(t, err)

	_, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, ok = findOutbox[*JoinRoundRejectOutbox](outbox)
	require.True(t, ok, "expected JoinRoundRejectOutbox in outbox")

	release, ok := findOutbox[*ReleaseForfeitReservation](outbox)
	require.True(t, ok, "expected ReleaseForfeitReservation in outbox")
	require.Equal(t, []wire.OutPoint{standardOp}, release.Outpoints)

	drop, ok := findOutbox[*DropCustomForfeitReservation](outbox)
	require.True(t, ok, "expected DropCustomForfeitReservation in outbox")
	require.Equal(t, []wire.OutPoint{customOp}, drop.Outpoints)
}

// TestCommitmentValidationFailureRollsBackCustomForfeits verifies that
// post-quote failures before connector-bound forfeit signing still release
// standard reservations and drop custom refresh signer actors.
func TestCommitmentValidationFailureRollsBackCustomForfeits(t *testing.T) {
	t.Parallel()

	standardOp := regTimeoutOutpoint(0x09, 0)
	customOp := regTimeoutOutpoint(0x0a, 1)
	_, operatorKey := generateTestKeyPair(t)
	s := &CommitmentTxReceivedState{
		RoundID: testRoundIDTr("commitment-validation-failed"),
		CommitmentTx: &psbt.Packet{
			UnsignedTx: &wire.MsgTx{},
		},
		VTXOTreePaths: map[int]*tree.Tree{
			0: nil,
		},
		SweepDelay: testExitDelay + 1,
		Intents: Intents{
			Forfeits: []types.ForfeitRequest{
				mkForfeit(standardOp, 10_000),
				{
					VTXOOutpoint: &customOp,
					Amount:       20_000,
					AuthSpend: &arkscript.SpendPath{
						RequiredSequence: 1,
					},
					ForfeitSpend: &arkscript.SpendPath{
						RequiredSequence: 2,
					},
				},
			},
		},
	}
	env := &ClientEnvironment{
		Log: btclog.Disabled,
		OperatorTerms: &types.OperatorTerms{
			PubKey:        operatorKey,
			VTXOExitDelay: testExitDelay,
		},
	}

	tr, err := s.ProcessEvent(
		context.Background(), &CommitmentTxBuilt{}, env,
	)
	require.NoError(t, err)

	_, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	release, ok := findOutbox[*ReleaseForfeitReservation](outbox)
	require.True(t, ok, "expected ReleaseForfeitReservation in outbox")
	require.Equal(t, []wire.OutPoint{standardOp}, release.Outpoints)

	drop, ok := findOutbox[*DropCustomForfeitReservation](outbox)
	require.True(t, ok, "expected DropCustomForfeitReservation in outbox")
	require.Equal(t, []wire.OutPoint{customOp}, drop.Outpoints)
}

// TestRoundJoinedCancelsRegistrationTimeout verifies that once the server's
// RoundJoined admission watermark arrives, the FSM cancels the registration
// timeout: the post-admission wait for the seal-time quote is governed by the
// quote's own expiry, not the admission timer, so the timer must not fire and
// tear down a healthy, admitted round.
func TestRoundJoinedCancelsRegistrationTimeout(t *testing.T) {
	t.Parallel()

	const roundKey = RoundKeyStr("temp:round-joined-cancels")
	s := &IntentSentState{Intents: Intents{}}
	env := &ClientEnvironment{Log: btclog.Disabled, RoundKey: roundKey}

	tr, err := s.ProcessEvent(context.Background(), &RoundJoined{
		RoundID: testRoundIDTr("admitted-round"),
	}, env)
	require.NoError(t, err)

	// The FSM stays parked in IntentSentState awaiting the quote.
	_, ok := tr.NextState.(*IntentSentState)
	require.True(t, ok, "expected IntentSentState, got %T", tr.NextState)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	cancel, ok := findOutbox[*CancelTimeoutReq](outbox)
	require.True(t, ok, "expected CancelTimeoutReq in outbox")
	require.Equal(t, TimeoutPhaseRegistration, cancel.Phase)
	require.Equal(t, roundKey, cancel.RoundKey)
}
