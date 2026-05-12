//go:build itest

package itest

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// placeholderP2TRPkScript returns a well-formed (but
// intentionally unspendable) P2TR scriptPubKey: OP_1
// OP_PUSHBYTES_32 followed by a 32-byte x-only key whose last
// byte is `tag`. Used by tests that need a leave destination the
// daemon will accept its standardness check on (post-#296), but
// where the actual on-chain spendability is irrelevant because
// the test never broadcasts.
func placeholderP2TRPkScript(tag byte) []byte {
	s := make([]byte, 34)
	s[0] = 0x51
	s[1] = 0x20
	s[33] = tag

	return s
}

// TestLeaveIntegrationSingleVTXOLifecycle verifies the daemon-level
// cooperative leave (offboard) path for one live VTXO: issue
// LeaveVTXOs against an on-chain destination, drive the round to
// confirmation, and assert that (a) the VTXO transitions to
// Forfeited, (b) the destination address receives exactly
// (vtxo.Amount - operator_fee) on-chain, and (c) the seal-time
// fee handshake (#270) lands the right residual against the
// non-zero itest fee schedule.
//
// The test relies on the harness's default itest fee schedule (a
// non-zero, lower-magnitude variant of production) so the seal-time
// builder actually deducts a fee. Under a zero fee schedule the
// leave output would carry the full VTXO amount and the assertion
// would be a no-op.
func TestLeaveIntegrationSingleVTXOLifecycle(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 2)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForClientRegistration(t, h)

	_, liveVTXO, startBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)
	require.Equal(t, liveVTXO.AmountSat, startBalance.VtxoBalanceSat)

	// Resolve the expected on-chain amount BEFORE triggering the
	// leave round. Under the #270 seal-time fee handshake the server
	// stamps the residual at seal time using the round's chain tip;
	// the subsequent mineUntilOperatorRoundConfirmed call advances
	// the tip. If we resolved the expected amount after the round
	// confirmed we'd read a smaller remaining-blocks value and
	// under-state the liquidity leg. expectedNetAfterRefresh is
	// reused here because leave and refresh are priced identically
	// (both paths pass isBoarding=false through the same EstimateFee
	// RPC the seal-time fee builder ultimately consults).
	expectedOnChainSat := expectedNetAfterRefresh(t, h, liveVTXO)
	require.Less(
		t, expectedOnChainSat, liveVTXO.AmountSat, "itest fee "+
			"schedule must be non-zero so the leave actually "+
			"exercises the #269 fee gate",
	)

	// Use bitcoind as the on-chain destination. bitcoind is always
	// in the harness regardless of which client wallet backend is
	// selected, and its wallet automatically tracks the returned
	// address so we can assert the payout via GetReceivedByAddress
	// without importing the address into alice's LND wallet.
	btcRPC, err := h.BitcoinRPCClient()
	require.NoError(t, err, "bitcoind RPC client")
	t.Cleanup(btcRPC.Shutdown)

	destAddr, err := btcRPC.GetNewAddress("itest-leave-dest")
	require.NoError(t, err, "bitcoind getnewaddress")

	existingRoundIDs := snapshotClientRoundIDs(t, alice.RPCClient)
	leaveResp, err := alice.RPCClient.LeaveVTXOs(
		t.Context(), &daemonrpc.LeaveVTXOsRequest{
			Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{liveVTXO.Outpoint},
				},
			},
			DefaultDestination: &daemonrpc.LeaveDestination{
				Target: &daemonrpc.LeaveDestination_Address{
					Address: destAddr.String(),
				},
			},
		},
	)
	require.NoError(t, err, "LeaveVTXOs RPC failed")
	require.Equal(t, "queued", leaveResp.Status)
	require.Contains(t, leaveResp.QueuedOutpoints, liveVTXO.Outpoint)

	// Leave intents follow the same admission path as refresh: the
	// wallet reserves the forfeit inputs synchronously but the
	// round actor only emits RegistrationRequested after an
	// explicit trigger (the itest harness stops the FSM mid-flight
	// to keep tests deterministic).
	alice.TriggerRoundRegistration()

	leaveRound := waitForNewClientRoundState(
		t, alice.RPCClient, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, leaveRound.RoundId)
	require.False(t, leaveRound.IsTemp)

	waitForNamedClientRoundState(
		t, alice.RPCClient, leaveRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForPersistedClientRoundState(
		t, alice.RPCClient, leaveRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, leaveRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf(
		"Leave round transaction broadcast: round_id=%q txid=%s",
		leaveRound.RoundId, broadcastRound.TxId,
	)

	mineUntilOperatorRoundConfirmed(
		t, h, leaveRound.RoundId, broadcastRound.TxId,
	)

	// Source of truth on the VTXO side: the outpoint must be
	// Forfeited (not just Spent — Spent would indicate an OOR
	// transition). This distinguishes cooperative leave from
	// in-round sends.
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, liveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	)

	// And the client's off-chain balance should be zero (the
	// single VTXO is gone and no refresh VTXO was produced).
	finalBalance := waitForExactVTXOBalance(t, alice.RPCClient, 0)
	require.Zero(
		t, finalBalance.VtxoBalanceSat,
		"cooperative leave must drain the VTXO balance",
	)

	// Source of truth on the on-chain side: bitcoind saw the
	// destination address receive exactly the fee-adjusted amount
	// in a confirmed transaction. Poll because mining is already
	// done but bitcoind's wallet may need a tick to index the
	// new UTXO into its address store.
	require.Eventually(t, func() bool {
		const minConf = 1
		recv, err := btcRPC.GetReceivedByAddressMinConf(
			destAddr, minConf,
		)
		if err != nil {
			t.Logf("getreceivedbyaddress: %v", err)

			return false
		}

		return int64(recv.ToUnit(btcutil.AmountSatoshi)) ==
			expectedOnChainSat
	}, defaultTimeout, pollInterval,
		"destination should receive vtxo.Amount - operator_fee")
}

// TestLeaveIntegrationDryRunPreview verifies LeaveVTXOs dry-run
// mode validates target selection without queuing a real leave
// intent: the VTXO stays Live, no new round is created, and the
// client's balance is untouched. Mirror of the equivalent refresh
// dry-run coverage.
func TestLeaveIntegrationDryRunPreview(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 2)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForClientRegistration(t, h)

	_, liveVTXO, startBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)
	existingRoundIDs := snapshotClientRoundIDs(t, alice.RPCClient)

	// Dry-run can use any destination (the handler returns before
	// resolving it). Use a pk_script literal so the test does not
	// depend on bitcoind's wallet.
	leaveResp, err := alice.RPCClient.LeaveVTXOs(
		t.Context(), &daemonrpc.LeaveVTXOsRequest{
			Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{liveVTXO.Outpoint},
				},
			},
			DefaultDestination: &daemonrpc.LeaveDestination{
				Target: &daemonrpc.LeaveDestination_PkScript{
					PkScript: placeholderP2TRPkScript(0x01),
				},
			},
			DryRun: true,
		},
	)
	require.NoError(t, err, "LeaveVTXOs dry-run RPC failed")
	require.Equal(t, "preview", leaveResp.Status)
	require.Contains(t, leaveResp.QueuedOutpoints, liveVTXO.Outpoint)

	// VTXO and balance should be untouched after a dry-run.
	waitForExactVTXOBalance(
		t, alice.RPCClient, startBalance.VtxoBalanceSat,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, liveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	)

	// No new round should appear. Poll for a short grace window;
	// the refresh dry-run coverage uses the same shape.
	require.Never(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := alice.RPCClient.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{},
		)
		if err != nil {
			t.Logf(
				"ListRounds failed during dry-run check: %v",
				err,
			)

			return true
		}

		for _, round := range resp.Rounds {
			if _, known := existingRoundIDs[round.RoundId]; !known {
				return true
			}
		}

		return false
	}, 3*pollInterval, pollInterval,
		"dry-run must not create a new round")
}

// TestLeaveIntegrationRejectsAllWithOverrides guards the primary
// "selection=all + destinations map" safety invariant end-to-end:
// the daemon must reject this combination with codes.InvalidArgument
// because it cannot resolve per-outpoint overrides without knowing
// the target set up front.
func TestLeaveIntegrationRejectsAllWithOverrides(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 2)

	alice := h.StartClientDaemon("alice")

	waitForClientRegistration(t, h)

	ctx, cancel := context.WithTimeout(
		t.Context(), defaultSmallTimeout,
	)
	defer cancel()

	override := &daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_PkScript{
			PkScript: placeholderP2TRPkScript(0x02),
		},
	}
	_, err := alice.RPCClient.LeaveVTXOs(
		ctx, &daemonrpc.LeaveVTXOsRequest{
			Selection: &daemonrpc.LeaveVTXOsRequest_All{
				All: true,
			},
			DefaultDestination: &daemonrpc.LeaveDestination{
				Target: &daemonrpc.LeaveDestination_PkScript{
					PkScript: placeholderP2TRPkScript(0x01),
				},
			},
			Destinations: map[string]*daemonrpc.LeaveDestination{
				"bogusoutpointkey:0": override,
			},
		},
	)
	require.Error(t, err, "all + destinations must be rejected")
	require.Contains(
		t, err.Error(),
		"selection=all",
		"error should cite the combination that was rejected",
	)
}
