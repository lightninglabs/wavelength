package round

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// insufficientFundsWire aliases the verbose wire enum value so the assertions
// below stay within the line-length limit.
const insufficientFundsWire = roundpb.
	RoundFailureCode_ROUND_FAILURE_INSUFFICIENT_OPERATOR_FUNDS

// TestRoundFailureCodeIsTerminalForJob verifies only the operator-funds code
// terminally fails the originating job; unknown stays recoverable.
func TestRoundFailureCodeIsTerminalForJob(t *testing.T) {
	t.Parallel()

	require.False(t, RoundFailureUnknown.IsTerminalForJob())
	require.True(
		t, RoundFailureInsufficientOperatorFunds.IsTerminalForJob(),
	)
}

// TestFailureCodeFromProto verifies the wire-to-native mapping, including the
// degrade of an unrecognized (newer-server) wire code to unknown.
func TestFailureCodeFromProto(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, RoundFailureUnknown, failureCodeFromProto(
			roundpb.RoundFailureCode_ROUND_FAILURE_UNKNOWN,
		),
	)
	require.Equal(
		t, RoundFailureInsufficientOperatorFunds,
		failureCodeFromProto(insufficientFundsWire),
	)
	require.Equal(
		t, RoundFailureUnknown,
		failureCodeFromProto(
			roundpb.RoundFailureCode(99),
		),
	)
}

// TestBoardingFailedFromProtoCarriesCode verifies BoardingFailed decodes the
// typed failure code from the ClientRoundFailedResp at the RPC boundary.
func TestBoardingFailedFromProtoCarriesCode(t *testing.T) {
	t.Parallel()

	e := &BoardingFailed{}
	err := e.FromProto(&roundpb.ClientRoundFailedResp{
		Reason:      "fund psbt: insufficient funds",
		FailureCode: insufficientFundsWire,
	})
	require.NoError(t, err)
	require.Equal(t, RoundFailureInsufficientOperatorFunds, e.FailureCode)
	require.True(t, e.Recoverable)
}

// terminalJobMsg returns the sole TerminalJobFailedNotification in the
// transition's outbox, or nil when none is present.
func terminalJobMsg(t *ClientStateTransition) *TerminalJobFailedNotification {
	var found *TerminalJobFailedNotification
	t.NewEvents.WhenSome(func(e ClientEmittedEvent) {
		for _, msg := range e.Outbox {
			if n, ok := msg.(*TerminalJobFailedNotification); ok {
				found = n
			}
		}
	})

	return found
}

// TestReleaseForfeitsOnFailureTerminalEmitsDrop verifies that a
// terminal-for-job failure into ClientFailedState, with forfeits reserved,
// emits a TerminalJobFailedNotification carrying the forfeited outpoints so the
// actor can fail the originating job.
func TestReleaseForfeitsOnFailureTerminalEmitsDrop(t *testing.T) {
	t.Parallel()

	roundID := fn.Some(RoundID{0xaa})
	outpoint := op(0x10, 1)
	forfeits := []types.ForfeitRequest{{VTXOOutpoint: opPtr(outpoint)}}

	transition := &ClientStateTransition{
		NextState: &ClientFailedState{
			Reason:      "operator broke",
			Recoverable: true,
			FailureCode: RoundFailureInsufficientOperatorFunds,
		},
	}

	out, err := releaseForfeitsOnFailure(
		transition, nil, roundID, forfeits,
	)
	require.NoError(t, err)

	msg := terminalJobMsg(out)
	require.NotNil(t, msg, "terminal-for-job failure must emit a drop")
	require.Equal(t, RoundFailureInsufficientOperatorFunds, msg.FailureCode)
	require.Equal(t, "operator broke", msg.Reason)
	require.Equal(t, []wire.OutPoint{outpoint}, msg.ForfeitOutpoints)
}

// TestReleaseForfeitsOnFailureUnknownNoDrop verifies a generic (recoverable)
// failure still releases the forfeit but does NOT terminally fail the job, so
// its intent stays eligible for replay.
func TestReleaseForfeitsOnFailureUnknownNoDrop(t *testing.T) {
	t.Parallel()

	roundID := fn.Some(RoundID{0xbb})
	forfeits := []types.ForfeitRequest{{VTXOOutpoint: opPtr(op(0x11, 0))}}

	transition := &ClientStateTransition{
		NextState: &ClientFailedState{
			Reason:      "transient hiccup",
			Recoverable: true,
			FailureCode: RoundFailureUnknown,
		},
	}

	out, err := releaseForfeitsOnFailure(
		transition, nil, roundID, forfeits,
	)
	require.NoError(t, err)
	require.Nil(
		t, terminalJobMsg(out),
		"unknown failure must not terminally fail the job",
	)

	// The forfeit release must still fire so the VTXO returns to LiveState.
	var sawRelease bool
	out.NewEvents.WhenSome(func(e ClientEmittedEvent) {
		for _, msg := range e.Outbox {
			if _, ok := msg.(*ReleaseForfeitReservation); ok {
				sawRelease = true
			}
		}
	})
	require.True(t, sawRelease, "forfeit release must still fire")
}
