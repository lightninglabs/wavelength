//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/stretchr/testify/require"
)

// insufficientOperatorFundsCode aliases the verbose wire enum value so the
// arming below stays within the line-length limit.
const insufficientOperatorFundsCode = roundpb.
	RoundFailureCode_ROUND_FAILURE_INSUFFICIENT_OPERATOR_FUNDS

// TestSendOnChainInsufficientOperatorFundsRetiresJob is the end-to-end
// reproduction of issue #889 on the cooperative send path. A send --onchain
// --sweep-all reserves the client's VTXO and registers a round, but the
// operator admits the registration and then fails the round because it cannot
// fund the commitment transaction. The client must classify that failure,
// return the VTXO to LIVE, and terminally retire the send intent so it never
// replays into the broke operator on restart.
//
// This exercises the exact path the Fable review's Finding 1 broke: the client
// sits in IntentSentState when the terminal ClientRoundFailedResp arrives, and
// that handler builds its own failure outbox, so the release-idempotency guard
// in releaseForfeitsOnFailure used to early-return before the terminal-drop
// block. The restart-and-assert-no-replay check is what distinguishes a durably
// retired job from one that merely released its VTXO but stayed pending: a
// swallowed terminal drop would replay the send on restart, sending a second
// JoinRound.
func TestSendOnChainInsufficientOperatorFundsRetiresJob(t *testing.T) {
	ParallelN(t)

	// EagerRoundJoin drives the send straight into IntentSentState,
	// matching the wallet/SDK hosts where #889 was observed.
	fixture := newDirectedSendFixture(t, func(c *darepod.Config) {
		c.EagerRoundJoin = true
	})

	// Arm the operator to admit the registration and then immediately fail
	// the round with the typed insufficient-operator-funds code.
	fixture.mailboxServer.setFailRoundOnJoin(&roundpb.ClientRoundFailedResp{
		Reason:      "fund psbt: insufficient funds",
		FailureCode: insufficientOperatorFundsCode,
	})

	// The seeded VTXO starts LIVE and is the whole spendable balance.
	startVTXOs := listAllVTXOs(t, fixture.client)
	require.Len(t, startVTXOs, 1)
	require.Equal(
		t, daemonrpc.VTXOStatus_VTXO_STATUS_LIVE, startVTXOs[0].Status,
	)

	// A standard P2TR destination script; only its script class matters to
	// the RPC's leave-script guard.
	destPkScript := append([]byte{0x51, 0x20}, make([]byte, 32)...)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Issue the cooperative sweep-all send. It persists the send intent,
	// reserves the VTXO, and registers the round.
	sendResp, err := fixture.client.SendOnChain(
		ctx, &daemonrpc.SendOnChainRequest{
			Destination: &daemonrpc.LeaveDestination{
				Target: &daemonrpc.LeaveDestination_PkScript{
					PkScript: destPkScript,
				},
			},
			Amount: &daemonrpc.SendOnChainRequest_SweepAll{
				SweepAll: true,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)

	// The operator failed the round, so the client must return the VTXO to
	// LIVE rather than strand it in pending-forfeit.
	requireVTXOStatusEventually(
		t, fixture.client, fixture.seededOutpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_LIVE, 30*time.Second,
	)
	require.Equal(
		t, testSeededAmountSat, vtxoBalanceSat(t, fixture.client),
		"spendable balance must be restored after the failed send",
	)

	// Exactly one JoinRound was sent for the now-dead send.
	require.Eventually(t, func() bool {
		return len(fixture.mailboxServer.joinRoundRequests()) == 1
	}, 5*time.Second, 100*time.Millisecond,
		"the send should have registered exactly one round")

	// The heart of the fix: the send intent is terminally retired, not left
	// pending. Restart the daemon, which replays pending intents on
	// startup. A durably failed intent is skipped, so no second JoinRound
	// is sent, whereas a swallowed terminal drop would replay it here.
	fixture.restart()
	waitForDaemonReady(t, fixture.client)

	// Disarm the failure so that a wrongly-replayed send would proceed
	// (and register a second JoinRound) rather than re-fail, making any
	// replay unambiguous.
	fixture.mailboxServer.setFailRoundOnJoin(nil)

	require.Never(t, func() bool {
		return len(fixture.mailboxServer.joinRoundRequests()) > 1
	}, 5*time.Second, 200*time.Millisecond,
		"a terminally failed send must not replay on restart")

	// The balance is still the full seeded VTXO after the restart, which
	// also implies the VTXO stayed LIVE (a stranded forfeit would zero it).
	require.Equal(
		t, testSeededAmountSat, vtxoBalanceSat(t, fixture.client),
	)
}
