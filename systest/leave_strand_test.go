//go:build systest

package systest

import (
	"context"
	"fmt"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/stretchr/testify/require"
)

// leaveAdmissionTimeout is the short registration (admission) timeout used by
// the leave-stranding systest so the recovery path is exercised in seconds
// rather than the production default. It is kept comfortably larger than
// parkedObserveWindow so the transient pending-forfeit / REGISTRATION_SENT
// state is reliably observable before the timeout fires it away, even when the
// suite runs several Dockerized daemons in parallel.
const leaveAdmissionTimeout = 15 * time.Second

// parkedObserveWindow bounds how long we poll for the transient stranded state
// (pending-forfeit + temp REGISTRATION_SENT). It must stay below
// leaveAdmissionTimeout so the observation does not race the recovery.
const parkedObserveWindow = 8 * time.Second

// leaveRecoveryWindow bounds how long we wait for the admission timeout to fire
// and the VTXO to be released back to LIVE. It is generous on top of the
// admission timeout to tolerate actor/Docker scheduling latency under parallel
// systest load (the release path is several async actor hops).
const leaveRecoveryWindow = leaveAdmissionTimeout + 60*time.Second

// TestLeaveStrandedVTXORecoversOnAdmissionTimeout reproduces darepo-client#653
// and proves the fix: a cooperative leave reserves the VTXO into
// pending-forfeit and parks the round in a temp REGISTRATION_SENT state, but
// the fake operator never returns a RoundJoined admission watermark. Without
// the registration-timeout safety net the VTXO would be stranded in
// pending-forfeit forever (balance permanently zero). With it, the admission
// timeout fires, the round fails recoverably, and the VTXO is released back to
// LIVE with the spendable balance restored.
//
// The fake mailbox server gives the client zero cooperation after the
// JoinRoundRequest, so a VTXO that returns to LIVE proves the daemon recovers
// entirely on its own, with no help from the server.
func TestLeaveStrandedVTXORecoversOnAdmissionTimeout(t *testing.T) {
	ParallelN(t)

	// EagerRoundJoin makes the leave drive the round FSM into
	// IntentSentState immediately (matching the wallet/SDK hosts where the
	// issue was observed); the short admission timeout keeps the test fast.
	fixture := newDirectedSendFixture(t, func(c *darepod.Config) {
		c.EagerRoundJoin = true
		c.RegistrationTimeout = leaveAdmissionTimeout
	})

	// The seeded VTXO starts LIVE and is the entire spendable balance.
	startVTXOs := listAllVTXOs(t, fixture.client)
	require.Len(t, startVTXOs, 1)
	require.Equal(
		t, daemonrpc.VTXOStatus_VTXO_STATUS_LIVE, startVTXOs[0].Status,
	)
	require.Equal(
		t, testSeededAmountSat, vtxoBalanceSat(t, fixture.client),
	)

	destAddr := newRegtestTaprootAddr(t)

	// Issue the cooperative leave. The reservation into pending-forfeit is
	// synchronous, so by the time this returns the VTXO is already
	// reserved.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	leaveResp, err := fixture.client.LeaveVTXOs(
		ctx, &daemonrpc.LeaveVTXOsRequest{
			Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{
						outpointString(
							fixture.seededOutpoint,
						),
					},
				},
			},
			DefaultDestination: &daemonrpc.LeaveDestination{
				Target: &daemonrpc.LeaveDestination_Address{
					Address: destAddr,
				},
			},
		},
	)
	require.NoError(t, err, "LeaveVTXOs RPC failed")
	require.Equal(t, "queued", leaveResp.Status)
	require.Contains(
		t, leaveResp.QueuedOutpoints,
		outpointString(fixture.seededOutpoint),
	)

	// The stranded condition from the issue: the VTXO is reserved into
	// pending-forfeit while the round parks in a temp REGISTRATION_SENT
	// state with no round id. Both hold synchronously once LeaveVTXOs
	// returns, so we capture them together well inside the admission
	// window (the combined poll avoids racing the timeout that later
	// clears the REGISTRATION_SENT round).
	require.Eventuallyf(
		t,
		func() bool {
			v := findVTXOByOutpoint(
				listAllVTXOs(t, fixture.client),
				fixture.seededOutpoint,
			)
			pendingForfeit := v != nil && v.Status ==
				daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT

			return pendingForfeit &&
				hasTempRegistrationSentRound(t, fixture.client)
		},
		parkedObserveWindow, 100*time.Millisecond,
		"leave did not park the VTXO in pending-forfeit with a temp "+
			"REGISTRATION_SENT round",
	)

	// The fix: the admission timeout fires, the round fails recoverably,
	// and the VTXO is released back to LIVE rather than stranded. On the
	// unfixed daemon this never happens and the assertion times out.
	requireVTXOStatusEventually(
		t, fixture.client, fixture.seededOutpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_LIVE, leaveRecoveryWindow,
	)

	// The spendable balance is restored to its starting value.
	require.Eventually(
		t,
		func() bool {
			return vtxoBalanceSat(t, fixture.client) ==
				testSeededAmountSat
		},
		20*time.Second, 100*time.Millisecond,
		"vtxo balance not restored after leave recovery",
	)
}

// vtxoBalanceSat returns the daemon's current off-chain (VTXO) balance.
func vtxoBalanceSat(t *testing.T, client daemonrpc.DaemonServiceClient) int64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	resp, err := client.GetBalance(ctx, &daemonrpc.GetBalanceRequest{})
	require.NoError(t, err)

	return resp.GetVtxoBalanceSat()
}

// requireVTXOStatusEventually polls until the VTXO at the given outpoint
// reaches the expected status, failing the test if it does not within the
// timeout.
func requireVTXOStatusEventually(t *testing.T,
	client daemonrpc.DaemonServiceClient, outpoint wire.OutPoint,
	want daemonrpc.VTXOStatus, timeout time.Duration) {

	t.Helper()

	require.Eventuallyf(
		t,
		func() bool {
			v := findVTXOByOutpoint(
				listAllVTXOs(t, client), outpoint,
			)

			return v != nil && v.Status == want
		},
		timeout, 100*time.Millisecond,
		"VTXO %s did not reach status %s within %s",
		outpoint, want, timeout,
	)
}

// hasTempRegistrationSentRound reports whether the daemon currently tracks a
// temp-keyed round in REGISTRATION_SENT — the parked state a leave reaches
// once its JoinRoundRequest is sent but unacknowledged.
func hasTempRegistrationSentRound(t *testing.T,
	client daemonrpc.DaemonServiceClient) bool {

	t.Helper()

	for _, r := range listRounds(t, client) {
		if r.IsTemp && r.State ==
			daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT {
			return true
		}
	}

	return false
}

// outpointString renders an outpoint in the "txid:index" form the daemon RPC
// uses for VTXO selection and reporting.
func outpointString(op wire.OutPoint) string {
	return fmt.Sprintf("%s:%d", op.Hash, op.Index)
}

// newRegtestTaprootAddr derives a fresh regtest P2TR address for use as a leave
// destination. The round never completes in this test, so the address only
// needs to be a valid on-chain destination the client can encode.
func newRegtestTaprootAddr(t *testing.T) string {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	taprootKey := txscript.ComputeTaprootKeyNoScript(priv.PubKey())
	addr, err := btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	return addr.String()
}
