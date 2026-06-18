//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/stretchr/testify/require"
)

// boardingFundAmount is the on-chain value funded into the boarding address
// under test. It is comfortably above dust so the deposit is a viable round
// input.
const boardingFundAmount = btcutil.Amount(200_000)

// TestBoardingRoundFailureSurfacedNotStuck exercises the round-id failure
// routing added in darepo-client#761 for a boarding deposit. A confirmed
// boarding deposit eagerly joins a round; the fake operator admits the client
// (re-keying the round to the server id) and then fails it. Before #761 the
// failure carried no round id, so it was dropped for the re-keyed round and the
// boarding sat projecting a synthetic VTXO_STATUS_PENDING_ROUND forever. With
// #761 the failure reaches the re-keyed round, which surfaces as
// ROUND_STATE_FAILED, and the phantom pending VTXO clears.
//
// The registration timeout is disabled so a FAILED round can only come from the
// server-pushed failure, not the admission timer.
func TestBoardingRoundFailureSurfacedNotStuck(t *testing.T) {
	ParallelN(t)

	fixture := newDirectedSendFixture(t, func(c *darepod.Config) {
		c.EagerRoundJoin = true
		c.RegistrationTimeout = -1
	})

	// Arm the operator to admit-then-fail the boarding round.
	fixture.mailboxServer.failRoundsAfterAdmission(
		"simulated post-admission boarding round failure",
	)

	// Create a boarding address and fund it on-chain. A single confirmation
	// satisfies the operator's MinConfirmations, after which the eager-join
	// wallet drives the round.
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	addrResp, err := fixture.client.NewAddress(
		ctx, &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err, "NewAddress RPC failed")
	require.NotEmpty(t, addrResp.Address)

	fixture.harness.Harness.Faucet(addrResp.Address, boardingFundAmount)
	fixture.harness.Harness.Generate(1)
	fixture.harness.WaitForLNDSync()

	// The boarding deposit confirms, the daemon eagerly joins a round, the
	// operator admits then fails it, and the failure reaches the re-keyed
	// round (#761) so it surfaces as FAILED rather than hanging. On a
	// daemon without #761 the failure is dropped and no round ever reaches
	// FAILED.
	require.Eventuallyf(
		t,
		func() bool {
			for _, r := range listRounds(t, fixture.client) {
				if r.State == daemonrpc.RoundState_ROUND_STATE_FAILED { //nolint:ll
					return true
				}
			}

			return false
		},
		60*time.Second, 250*time.Millisecond,
		"boarding round never surfaced as ROUND_STATE_FAILED",
	)

	// Once the round has failed, the boarding deposit must not keep
	// projecting a synthetic PENDING_ROUND VTXO (the "pending forever"
	// symptom #761 fixes). No further blocks are mined, so the failed round
	// is not re-joined while we observe.
	require.Eventuallyf(
		t,
		func() bool {
			for _, v := range listAllVTXOs(t, fixture.client) {
				pending := daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND //nolint:ll
				if v.Status == pending {
					return false
				}
			}

			return true
		},
		30*time.Second, 250*time.Millisecond,
		"boarding deposit still projects a PENDING_ROUND VTXO after "+
			"the round failed",
	)
}
