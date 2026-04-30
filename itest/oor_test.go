//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestOORIntegrationAliceToBob verifies a real daemon-to-daemon OOR transfer
// across the public RPC surface after both clients have live VTXOs.
func TestOORIntegrationAliceToBob(t *testing.T) {
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
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	_, aliceLiveVTXO, aliceBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	_, _, bobBalance := boardClientAndConfirmRound(
		t, h, bob.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)

	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	recvResp, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-alice-to-bob",
		},
	)
	require.NoError(t, err, "NewReceiveScript RPC failed")
	require.NotEmpty(t, recvResp.PkScriptHex)

	recipientPkScript, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err, "pk_script_hex must be valid hex")

	sendAmount := aliceLiveVTXO.AmountSat
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPkScript,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err, "SendOOR RPC failed")
	require.Equal(t, "submitted", sendResp.Status)
	require.NotEmpty(t, sendResp.SessionId)
	t.Logf("submitted OOR transfer session_id=%s amount=%d",
		sendResp.SessionId, sendAmount)

	aliceAfterSend := waitForVTXOBalanceBelow(
		t, alice.RPCClient, aliceBalance.VtxoBalanceSat,
	)
	t.Logf("alice balance decreased after OOR send: before=%d after=%d",
		aliceBalance.VtxoBalanceSat, aliceAfterSend.VtxoBalanceSat)

	expectedBobBalance := bobBalance.VtxoBalanceSat + sendAmount
	bobFinalBalance := waitForExactVTXOBalance(
		t, bob.RPCClient, expectedBobBalance,
	)
	require.Equal(t, expectedBobBalance, bobFinalBalance.VtxoBalanceSat)

	receivedVTXO := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)

	require.NotNil(t, receivedVTXO)
	t.Logf("bob received OOR VTXO outpoint=%s amount=%d",
		receivedVTXO.Outpoint, receivedVTXO.AmountSat)
}

// TestOORIntegrationPartialSendCreatesChange verifies an OOR send whose
// selected input exceeds the recipient amount finalizes as a two-output Ark
// package: the external recipient output plus a sender-owned change output.
func TestOORIntegrationPartialSendCreatesChange(t *testing.T) {
	t.Parallel()

	alice, bob, aliceLiveVTXO, aliceStartBalance, bobStartBalance,
		bobPubkey := setupFundedOORValidationHarness(
		t, "itest-oor-partial-send-bob",
	)

	aliceLiveBefore := outpointSet(listLiveVTXOs(t, alice.RPCClient))
	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	sendAmount := int64(30_000)
	require.Less(t, sendAmount, aliceLiveVTXO.AmountSat)
	expectedChange := aliceLiveVTXO.AmountSat - sendAmount
	require.Positive(t, expectedChange)

	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: bobPubkey,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err, "SendOOR RPC failed")
	require.Equal(t, "submitted", sendResp.Status)
	require.NotEmpty(t, sendResp.SessionId)

	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceLiveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)

	bobExpectedBalance := bobStartBalance.VtxoBalanceSat + sendAmount
	bobFinalBalance := waitForExactVTXOBalance(
		t, bob.RPCClient, bobExpectedBalance,
	)
	require.Equal(t, bobExpectedBalance, bobFinalBalance.VtxoBalanceSat)

	bobReceived := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, bobReceived)

	aliceExpectedBalance := aliceStartBalance.VtxoBalanceSat - sendAmount
	aliceFinalBalance := waitForExactVTXOBalance(
		t, alice.RPCClient, aliceExpectedBalance,
	)
	require.Equal(
		t, aliceExpectedBalance, aliceFinalBalance.VtxoBalanceSat,
	)

	aliceChange := waitForNewLiveVTXOWithAmount(
		t, alice.RPCClient, aliceLiveBefore, expectedChange,
	)
	require.NotNil(t, aliceChange)
	require.NotEqual(t, aliceLiveVTXO.Outpoint, aliceChange.Outpoint)

	t.Logf("partial OOR transfer completed: session_id=%s "+
		"send_amount=%d change_outpoint=%s change_amount=%d",
		sendResp.SessionId, sendAmount, aliceChange.Outpoint,
		aliceChange.AmountSat)
}

// TestOORIntegrationDryRunPreview verifies SendOOR dry-run mode validates
// output construction without mutating sender or recipient state.
func TestOORIntegrationDryRunPreview(t *testing.T) {
	t.Parallel()

	alice, bob, aliceLiveVTXO, aliceStartBalance, bobStartBalance,
		recipientPkScript := setupFundedOORValidationHarness(
		t, "itest-oor-dry-run-preview",
	)

	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPkScript,
				},
				AmountSat: aliceLiveVTXO.AmountSat,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err, "SendOOR dry-run RPC failed")
	require.Equal(t, "preview", sendResp.Status)
	require.Empty(t, sendResp.SessionId)

	aliceAfterDryRun := waitForExactVTXOBalance(
		t, alice.RPCClient, aliceStartBalance.VtxoBalanceSat,
	)
	bobAfterDryRun := waitForExactVTXOBalance(
		t, bob.RPCClient, bobStartBalance.VtxoBalanceSat,
	)
	require.Equal(t, aliceStartBalance.VtxoBalanceSat,
		aliceAfterDryRun.VtxoBalanceSat)
	require.Equal(t, bobStartBalance.VtxoBalanceSat,
		bobAfterDryRun.VtxoBalanceSat)

	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceLiveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	)
}

// TestOORIntegrationInsufficientFunds verifies SendOOR rejects requests that
// exceed available spendable VTXO balance without mutating wallet state.
func TestOORIntegrationInsufficientFunds(t *testing.T) {
	t.Parallel()

	alice, bob, aliceLiveVTXO, aliceStartBalance, bobStartBalance,
		recipientPkScript := setupFundedOORValidationHarness(
		t, "itest-oor-insufficient-funds",
	)

	_, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPkScript,
				},
				AmountSat: aliceStartBalance.VtxoBalanceSat + 1,
			},
		},
	)
	require.Error(t, err, "SendOOR should fail when funds are insufficient")
	require.ErrorContains(t, err, "insufficient funds")

	waitForExactVTXOBalance(
		t, alice.RPCClient, aliceStartBalance.VtxoBalanceSat,
	)
	waitForExactVTXOBalance(
		t, bob.RPCClient, bobStartBalance.VtxoBalanceSat,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceLiveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	)
}

// TestOORIntegrationRejectsZeroAmount verifies SendOOR enforces positive
// recipient amounts at the RPC boundary.
func TestOORIntegrationRejectsZeroAmount(t *testing.T) {
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
	bob := h.StartClientDaemon("bob")

	waitForRegisteredClients(t, h, 2)

	aliceStartBalance := waitForExactVTXOBalance(t, alice.RPCClient, 0)
	bobStartBalance := waitForExactVTXOBalance(t, bob.RPCClient, 0)

	recvResp, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-oor-zero-amount",
		},
	)
	require.NoError(t, err, "NewReceiveScript RPC failed")

	recipientPkScript, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err, "pk_script_hex must be valid hex")

	_, err = alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPkScript,
				},
				AmountSat: 0,
			},
		},
	)
	require.Error(t, err, "SendOOR should reject zero amount")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "amount must be positive")

	waitForExactVTXOBalance(
		t, alice.RPCClient, aliceStartBalance.VtxoBalanceSat,
	)
	waitForExactVTXOBalance(
		t, bob.RPCClient, bobStartBalance.VtxoBalanceSat,
	)
}

func setupFundedOORValidationHarness(
	t *testing.T, label string,
) (
	*harness.ClientDaemonHarness, *harness.ClientDaemonHarness,
	*daemonrpc.VTXO,
	*daemonrpc.GetBalanceResponse, *daemonrpc.GetBalanceResponse, []byte,
) {

	t.Helper()

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
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	_, aliceLiveVTXO, aliceStartBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	bobStartBalance := waitForExactVTXOBalance(t, bob.RPCClient, 0)

	recvResp, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: label,
		},
	)
	require.NoError(t, err, "NewReceiveScript RPC failed")

	recipientPkScript, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err, "pk_script_hex must be valid hex")

	return alice, bob, aliceLiveVTXO, aliceStartBalance,
		bobStartBalance, recipientPkScript
}

// TestOORIntegrationBidirectionalTransfer verifies both clients can perform
// OOR sends in opposite directions using real daemon RPCs and persisted state.
func TestOORIntegrationBidirectionalTransfer(t *testing.T) {
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
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	_, aliceLiveVTXO, aliceStartBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	_, _, bobStartBalance := boardClientAndConfirmRound(
		t, h, bob.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)

	// First OOR leg: alice -> bob.
	bobLiveBeforeSend1 := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	bobRecv1, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-bidirectional-bob-recv",
		},
	)
	require.NoError(t, err)

	bobPkScript1, err := hex.DecodeString(bobRecv1.PubkeyXonlyHex)
	require.NoError(t, err)

	sendAmount1 := aliceLiveVTXO.AmountSat
	send1Resp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: bobPkScript1,
				},
				AmountSat: sendAmount1,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", send1Resp.Status)
	require.NotEmpty(t, send1Resp.SessionId)

	aliceAfterSend1 := waitForVTXOBalanceBelow(
		t, alice.RPCClient, aliceStartBalance.VtxoBalanceSat,
	)

	bobAfterSend1 := waitForExactVTXOBalance(
		t, bob.RPCClient, bobStartBalance.VtxoBalanceSat+sendAmount1,
	)
	bobReceived1 := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBeforeSend1, sendAmount1,
	)
	require.NotNil(t, bobReceived1)

	// Second OOR leg: bob -> alice.
	aliceLiveBeforeSend2 := outpointSet(listLiveVTXOs(t, alice.RPCClient))
	aliceRecv2, err := alice.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-bidirectional-alice-recv",
		},
	)
	require.NoError(t, err)

	alicePkScript2, err := hex.DecodeString(aliceRecv2.PubkeyXonlyHex)
	require.NoError(t, err)

	sendAmount2 := bobReceived1.AmountSat
	send2Resp, err := bob.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: alicePkScript2,
				},
				AmountSat: sendAmount2,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", send2Resp.Status)
	require.NotEmpty(t, send2Resp.SessionId)

	bobAfterSend2 := waitForVTXOBalanceBelow(
		t, bob.RPCClient, bobAfterSend1.VtxoBalanceSat,
	)
	waitForNewLiveVTXOWithAmount(
		t, alice.RPCClient, aliceLiveBeforeSend2, sendAmount2,
	)
	waitForExactVTXOBalance(
		t, alice.RPCClient, aliceAfterSend1.VtxoBalanceSat+sendAmount2,
	)

	require.Less(t, bobAfterSend2.VtxoBalanceSat,
		bobAfterSend1.VtxoBalanceSat)
}

// TestOORIntegrationMultiInputTransfer verifies a single OOR send can consume
// multiple live input VTXOs from the sender and converge on terminal spent
// state for each consumed input.
func TestOORIntegrationMultiInputTransfer(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	// Use a short VTXOExitDelay so the end-of-test unroll completes
	// in a feasible number of mined blocks. The OperatorConfigMutator
	// hook tweaks the operator config before the in-process server
	// starts; tests outside the unroll suite use this same pattern.
	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		OperatorConfigMutator: func(cfg *darepo.Config) {
			cfg.Rounds.VTXOExitDelay = testVTXOExitDelay
		},
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 2)

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	_, aliceVTXO1, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	_, aliceVTXO2, aliceBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	// Post-#263 the harness default runs fees-on; each VTXO is
	// boardingAmount - boarding_fee. See the convention in
	// helpers_test.go.
	expectedNet := expectedNetAfterBoarding(
		t, int64(100_000), defaultItestBatchSize,
	)
	require.Equal(t, expectedNet, aliceVTXO1.AmountSat)
	require.Equal(t, expectedNet, aliceVTXO2.AmountSat)
	require.Equal(t, aliceVTXO1.AmountSat+aliceVTXO2.AmountSat,
		aliceBalance.VtxoBalanceSat)

	aliceLiveBefore := outpointSet(listLiveVTXOs(t, alice.RPCClient))
	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	recvResp, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-multi-input-oor",
		},
	)
	require.NoError(t, err, "NewReceiveScript RPC failed")

	recipientPkScript, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err, "pk_script_hex must be valid hex")

	sendAmount := int64(120_000)
	totalInputAmount := aliceVTXO1.AmountSat + aliceVTXO2.AmountSat
	expectedChange := totalInputAmount - sendAmount
	require.Positive(t, expectedChange)

	// Fund Bob's wallet for CPFP fees during unroll. The unroll for a
	// multi-tree VTXO broadcasts one anchor parent per ancestry path
	// (here: two distinct commitment txids → two parents) and each
	// needs its own CPFP fee input. btcwallet/lwwallet treat a wallet
	// UTXO as spent the moment a CPFP child enters the mempool, so a
	// single funded UTXO would starve the second path's CPFP until the
	// first child confirmed. Fund two independent UTXOs up front so
	// both paths can broadcast in parallel without waiting for an
	// in-mempool change to mature.
	h.FundClientWalletN(bob, btcutil.SatoshiPerBitcoin/2, 2)

	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPkScript,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err, "SendOOR RPC failed")
	require.Equal(t, "submitted", sendResp.Status)
	require.NotEmpty(t, sendResp.SessionId)

	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO1.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO2.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)

	waitForExactVTXOBalance(t, bob.RPCClient, sendAmount)
	waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)

	waitForExactVTXOBalance(
		t, alice.RPCClient, aliceBalance.VtxoBalanceSat-sendAmount,
	)
	aliceChange := waitForNewLiveVTXOWithAmount(
		t, alice.RPCClient, aliceLiveBefore, expectedChange,
	)
	require.NotEqual(t, aliceVTXO1.Outpoint, aliceChange.Outpoint)
	require.NotEqual(t, aliceVTXO2.Outpoint, aliceChange.Outpoint)

	// Cross-round multi-input assertion: the two source VTXOs were
	// boarded in distinct rounds, so they carry distinct
	// commitment_txids. Bob's received VTXO must surface every
	// contributing commitment as its own ancestry path so unilateral
	// exit can broadcast both required trees on-chain.
	require.NotEqual(
		t, aliceVTXO1.CommitmentTxid, aliceVTXO2.CommitmentTxid,
		"alice's two boarded VTXOs must come from different "+
			"commitment txids so this exercises the cross-round "+
			"multi-input path",
	)

	bobReceived := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, bobReceived)

	// The daemon RPC surface does not expose per-fragment ancestry
	// paths (those live on the indexer wire); the cross-commitment
	// invariant above plus the successful end-to-end unroll below
	// are the headline assertions that the multi-tree resolver
	// produced and persisted every required parent tree.
	t.Logf("multi-input OOR transfer completed: session_id=%s amount=%d "+
		"inputs=[%s,%s] change_outpoint=%s change_amount=%d "+
		"bob_received_outpoint=%s",
		sendResp.SessionId, sendAmount, aliceVTXO1.Outpoint,
		aliceVTXO2.Outpoint, aliceChange.Outpoint, expectedChange,
		bobReceived.Outpoint)

	// End-to-end unroll: Bob triggers unilateral exit on the
	// cross-round VTXO. The unroll registry materializes every tree
	// node from every ancestry path, waits for CSV maturity, and
	// finally sweeps to Bob's wallet. This is the headline assertion
	// for the multi-tree resolver: unilateral exit must succeed
	// even when the source ancestry spans multiple commitment trees.
	initialWalletUTXOs := confirmedWalletUTXOValues(t, bob)

	unrollResp, err := bob.RPCClient.Unroll(
		t.Context(), &daemonrpc.UnrollRequest{
			Outpoint: bobReceived.Outpoint,
		},
	)
	require.NoError(t, err, "Unroll RPC must succeed for "+
		"multi-tree VTXO")
	require.True(t, unrollResp.Created)

	t.Logf("Unroll job created for cross-round VTXO: actor_id=%s",
		unrollResp.ActorId)

	waitForVTXOStatusByOutpoint(
		t, bob.RPCClient, bobReceived.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
	)

	sweptOutpoint := waitForUnrollSweepToWallet(
		t, h, bob, bob.RPCClient, bobReceived.Outpoint,
		bobReceived.AmountSat, initialWalletUTXOs,
	)

	statusResp, err := bob.RPCClient.GetUnrollStatus(
		t.Context(), &daemonrpc.GetUnrollStatusRequest{
			Outpoint: bobReceived.Outpoint,
		},
	)
	require.NoError(t, err)
	require.True(t, statusResp.Found)
	require.Equal(
		t,
		daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED,
		statusResp.Status,
	)
	require.NotEmpty(t, statusResp.SweepTxid)

	t.Logf("Cross-round multi-input OOR unroll completed: "+
		"VTXO %s swept to wallet UTXO %s, sweep_txid=%s",
		bobReceived.Outpoint, sweptOutpoint, statusResp.SweepTxid)
}

// TestOORIntegrationMultiInputChainedTransfer is the canonical worst-case
// regression bar for the multi-input cross-round OOR work plus chain-depth
// unroll: Alice's first hop is itself a multi-input cross-round transfer
// (two distinct commitments), and Carol's final VTXO inherits that
// multi-tree ancestry while sitting at ChainDepth=2 behind the round-birth
// trees. Carol's end-of-test unilateral exit must walk Bob's
// non-wallet-owned alice->bob package, her own bob->carol package, AND
// every contributing commitment tree to broadcast the full lineage
// on-chain.
//
// Single-input chain-depth coverage is a strict subset of this scenario;
// if the multi-input chain-depth path resolves end-to-end, single-input
// chain-depth is degenerate and does not need its own test.
func TestOORIntegrationMultiInputChainedTransfer(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	// Use the short VTXOExitDelay harness so the end-of-test
	// Carol-side unroll completes in a feasible number of mined
	// blocks. ChainDepth=2 unilateral exit must walk both OOR ark
	// txes plus every round-birth tree to broadcast the full
	// lineage on-chain.
	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		OperatorConfigMutator: func(cfg *darepo.Config) {
			cfg.Rounds.VTXOExitDelay = testVTXOExitDelay
		},
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 3)

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	carol := h.StartClientDaemon("carol")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 3)

	// Fund Carol's wallet for CPFP fees during the trailing unroll.
	// Carol's chain-depth-2 VTXO walks two ancestry paths and each
	// path's tree of ancestor txns needs its own CPFP fee input —
	// btcwallet/lwwallet count a UTXO as spent as soon as a CPFP child
	// hits the mempool, so several independent wallet UTXOs are needed
	// for the parents to broadcast in parallel. Four UTXOs of 0.25 BTC
	// each comfortably cover the worst-case planner shape (two paths,
	// two intermediate ark txs).
	h.FundClientWalletN(carol, btcutil.SatoshiPerBitcoin/4, 4)

	// Alice boards two VTXOs in two distinct rounds so the first OOR
	// hop traverses two separate commitments. Each VTXO ends up with
	// its own commitment_txid; the SendOOR below consumes both, so
	// Bob's resulting VTXO carries multi-tree ancestry (one path per
	// contributing commitment).
	_, aliceVTXO1, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	_, aliceVTXO2, aliceBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)

	require.NotEqual(
		t, aliceVTXO1.CommitmentTxid, aliceVTXO2.CommitmentTxid,
		"alice's two boarded VTXOs must come from different "+
			"commitment txids so this exercises the cross-round "+
			"multi-input path",
	)

	// First leg: alice multi-input cross-round -> bob. Bob receives a
	// single VTXO that combines both alice inputs. Send the full
	// boarded balance so bob can forward the same amount to carol on
	// the second leg without intermediate change accounting.
	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))
	bobRecv, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-multi-input-chained-oor-bob",
		},
	)
	require.NoError(t, err)

	bobPkScript, err := hex.DecodeString(bobRecv.PubkeyXonlyHex)
	require.NoError(t, err)

	send1Amount := aliceBalance.VtxoBalanceSat
	send1Resp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: bobPkScript,
				},
				AmountSat: send1Amount,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", send1Resp.Status)

	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO1.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO2.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
	waitForExactVTXOBalance(t, bob.RPCClient, send1Amount)
	bobReceived := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, send1Amount,
	)
	require.NotNil(t, bobReceived)

	// Second leg: bob -> carol using the multi-input-derived output.
	carolLiveBefore := outpointSet(listLiveVTXOs(t, carol.RPCClient))
	carolRecv, err := carol.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-multi-input-chained-oor-carol",
		},
	)
	require.NoError(t, err)

	carolPkScript, err := hex.DecodeString(carolRecv.PubkeyXonlyHex)
	require.NoError(t, err)

	send2Amount := bobReceived.AmountSat
	send2Resp, err := bob.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: carolPkScript,
				},
				AmountSat: send2Amount,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", send2Resp.Status)

	waitForVTXOStatusByOutpoint(
		t, bob.RPCClient, bobReceived.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
	waitForVTXOBalanceBelow(t, bob.RPCClient, send1Amount)
	waitForExactVTXOBalance(t, carol.RPCClient, send2Amount)
	carolReceived := waitForNewLiveVTXOWithAmount(
		t, carol.RPCClient, carolLiveBefore, send2Amount,
	)
	require.NotNil(t, carolReceived)
	require.Equal(t, uint32(2), carolReceived.ChainDepth,
		"carol's twice-hopped VTXO must have ChainDepth=2 "+
			"(two OOR ark txes between the round-birth trees "+
			"and the final output)")

	// End-to-end ChainDepth=2 unroll on a multi-input ancestor.
	// Carol triggers unilateral exit on the twice-hopped VTXO; the
	// unroller must walk every OOR ark + checkpoint tx in the chain,
	// follow checkpoint inputs through Bob's non-wallet-owned
	// alice->bob package, AND rebuild every contributing
	// round-birth tree (one per commitment from alice's first hop)
	// before broadcasting the full lineage in dependency order.
	initialWalletUTXOs := confirmedWalletUTXOValues(t, carol)

	unrollResp, err := carol.RPCClient.Unroll(
		t.Context(), &daemonrpc.UnrollRequest{
			Outpoint: carolReceived.Outpoint,
		},
	)
	require.NoError(t, err, "Unroll RPC must succeed for "+
		"chained-OOR VTXO")
	require.True(t, unrollResp.Created)

	waitForVTXOStatusByOutpoint(
		t, carol.RPCClient, carolReceived.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
	)

	sweptOutpoint := waitForUnrollSweepToWallet(
		t, h, carol, carol.RPCClient, carolReceived.Outpoint,
		carolReceived.AmountSat, initialWalletUTXOs,
	)

	statusResp, err := carol.RPCClient.GetUnrollStatus(
		t.Context(), &daemonrpc.GetUnrollStatusRequest{
			Outpoint: carolReceived.Outpoint,
		},
	)
	require.NoError(t, err)
	require.True(t, statusResp.Found)
	require.Equal(
		t,
		daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED,
		statusResp.Status,
	)
	require.NotEmpty(t, statusResp.SweepTxid)

	t.Logf("multi-input chained OOR transfer + unroll completed: "+
		"leg1_session=%s leg2_session=%s amount=%d "+
		"alice_inputs=[%s,%s] bob_received_outpoint=%s "+
		"carol_swept=%s sweep_txid=%s",
		send1Resp.SessionId, send2Resp.SessionId, send2Amount,
		aliceVTXO1.Outpoint, aliceVTXO2.Outpoint,
		bobReceived.Outpoint, sweptOutpoint, statusResp.SweepTxid)
}

// TestOORIntegrationResumeAcrossClientRestart verifies an OOR transfer
// submitted before a sender daemon restart still converges after restart
// through persisted daemon state and mailbox replay.
func TestOORIntegrationResumeAcrossClientRestart(t *testing.T) {
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
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)
	aliceMailbox := h.ClientMailbox("alice")
	aliceMailbox.PauseType("FinalizePackageRequest")
	t.Cleanup(aliceMailbox.ClearPausedTypes)

	waitForRegisteredClients(t, h, 2)

	_, aliceLiveVTXO, aliceStartBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)

	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))
	bobRecv, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-oor-restart-bob",
		},
	)
	require.NoError(t, err)

	bobPkScript, err := hex.DecodeString(bobRecv.PubkeyXonlyHex)
	require.NoError(t, err)

	sendAmount := aliceLiveVTXO.AmountSat
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: bobPkScript,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)

	waitCtx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	// The controlled mailbox holds FinalizePackageRequest at the daemon
	// edge so the restart happens after the client has durably produced
	// the finalize step, but before the operator can observe it.
	require.NoError(t, aliceMailbox.WaitForPendingType(
		waitCtx, "FinalizePackageRequest",
	))
	require.Equal(t, 1,
		aliceMailbox.PendingTypeCount("FinalizePackageRequest"))

	// Drop the pre-restart finalize request so the restarted daemon must
	// reproduce it from durable state before the test can proceed.
	aliceMailbox.DropAllPending()

	oldRPCAddr := alice.RPCAddr
	alice = h.RestartClientDaemon("alice")
	t.Logf("restarted sender daemon after OOR submit: old_rpc=%s "+
		"new_rpc=%s session_id=%s", oldRPCAddr, alice.RPCAddr,
		sendResp.SessionId)

	waitCtx, cancel = context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	require.NoError(t, aliceMailbox.WaitForPendingType(
		waitCtx, "FinalizePackageRequest",
	))
	aliceMailbox.ResumeType("FinalizePackageRequest")
	require.Eventually(t, func() bool {
		return aliceMailbox.FlushAll() == nil
	}, defaultTimeout, pollInterval,
		"finalize request never flushed after client restart")

	waitForExactVTXOBalance(t, bob.RPCClient, sendAmount)
	waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceLiveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
	waitForVTXOBalanceBelow(
		t, alice.RPCClient, aliceStartBalance.VtxoBalanceSat,
	)
}

// TestOORIntegrationOfflineRecipientEventVisibility verifies authoritative
// indexer recipient-event queries expose an incoming OOR transfer while the
// recipient daemon is offline. Once the daemon-side reconciliation path is
// restart-safe, this should grow into a full post-restart convergence test.
func TestOORIntegrationOfflineRecipientEventVisibility(t *testing.T) {
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
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	_, aliceLiveVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	recvResp, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-indexer-reconcile-bob",
		},
	)
	require.NoError(t, err)

	recipientPkScript, err := hex.DecodeString(recvResp.PkScriptHex)
	require.NoError(t, err)
	recipientPubkey, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err)

	indexerClient := h.StartIndexerTestClient(
		"bob", recvResp.KeyFamily, recvResp.KeyIndex,
	)

	// The integration harness derives the external signer from
	// Bob's daemon-backed wallet backend. Build the proof-gated
	// query while Bob is still online, then reuse that signed
	// request after shutdown so this test stays focused on
	// authoritative offline event visibility.
	prebuiltQueryReq, err := indexerClient.Indexer.
		BuildListOORRecipientEventsByScriptTaprootRequest(
			t.Context(), recipientPkScript, 0, 20,
		)
	require.NoError(t, err)

	// Prime the external mailbox client so the operator
	// auto-registers it before Bob goes offline. Use a
	// principal-scoped query here rather than the script
	// query under test so later recipient-event polling
	// observes fresh state instead of depending on any
	// request dedupe behavior.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		_, queryErr := indexerClient.Indexer.ListMyReceiveScripts(ctx)

		return queryErr == nil
	}, defaultTimeout, pollInterval,
		"indexer test client never became ready")

	bob.Stop()
	t.Log("stopped bob daemon before OOR send to force offline receive")

	sendAmount := aliceLiveVTXO.AmountSat
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPubkey,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)

	var recipientEvents *arkrpc.ListOORRecipientEventsByScriptResponse
	var lastQueryErr error
	var lastEventCount int
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, queryErr := indexerClient.
			ListOORRecipientEventsByRequest(
				ctx, prebuiltQueryReq,
			)
		if queryErr != nil {
			lastQueryErr = queryErr

			return false
		}

		lastQueryErr = nil
		lastEventCount = len(resp.Events)

		for _, ev := range resp.Events {
			if int64(ev.Value) == sendAmount {
				recipientEvents = resp

				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval,
		"indexer did not expose OOR recipient event while "+
			"bob was offline "+
			"(last_query_err=%v, last_event_count=%d)",
		lastQueryErr, lastEventCount,
	)

	require.NotNil(t, recipientEvents)

	oldRPCAddr := bob.RPCAddr
	bob = h.RestartClientDaemon("bob")
	require.NotNil(t, bob)
	require.NotEmpty(t, bob.RPCAddr)
	t.Logf("restarted recipient daemon after offline OOR receive: "+
		"old_rpc=%s new_rpc=%s session_id=%s", oldRPCAddr,
		bob.RPCAddr, sendResp.SessionId)

	waitForDaemonInfoReachable(t, bob.RPCClient)

	waitForRegisteredClients(t, h, 2)
	waitForExactVTXOBalance(t, bob.RPCClient, sendAmount)
	waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
}

// TestOORIntegrationLineageCapRejection verifies the typed cap-rejection
// path end-to-end. The operator is configured with a tight 1 vB
// MaxOORLineageVBytes so any well-formed OOR submit exceeds the cap.
// The test asserts the cap-check-before-lock invariant: the rejection
// runs in AwaitingSubmitValidationState BEFORE LockInputsReq, so
// Alice's source VTXO must remain LIVE (no phantom forfeit transition)
// and Bob never receives a VTXO.
func TestOORIntegrationLineageCapRejection(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	// Tight cap: 1 vB rejects any well-formed OOR submit because even
	// the smallest synthesized Ark + checkpoint pair contributes far
	// more vbytes than that. The submit reaches the server, fails the
	// cap check, and the FSM transitions to Failed without acquiring
	// VTXO locks.
	const tightCap uint32 = 1

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		OperatorConfigMutator: func(cfg *darepo.Config) {
			cfg.MaxOORLineageVBytes = tightCap
		},
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 2)

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	_, aliceLiveVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient,
		operatorInfo.MinConfirmations, 100_000,
	)
	require.NotNil(t, aliceLiveVTXO)

	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	recvResp, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-oor-cap-reject",
		},
	)
	require.NoError(t, err)
	recipientPkScript, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err)

	// SendOOR: depending on whether the client pre-flights the cap
	// against OperatorTerms.MaxOORLineageVBytes, this may return a
	// synchronous typed error OR succeed with "submitted" and the
	// rejection arrives asynchronously through the actor pipeline.
	// Both paths are valid; the test asserts the key invariant
	// (Alice's VTXO stays LIVE) regardless.
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPkScript,
				},
				AmountSat: aliceLiveVTXO.AmountSat,
			},
		},
	)
	if err != nil {
		// Synchronous-rejection path: surface the error for log
		// visibility but do not require a specific text — the
		// invariant is the lock state, not the error string.
		t.Logf("SendOOR returned synchronous error (expected "+
			"under tight cap): %v", err)
	} else {
		t.Logf("SendOOR submitted asynchronously: session=%s "+
			"(server-side cap-reject expected)",
			sendResp.SessionId)
	}

	// The cap check runs BEFORE LockInputsReq in
	// handleValidateSubmit (per oor/CLAUDE.md "submit validation
	// precedes VTXO locking" invariant). Alice's source VTXO must
	// remain LIVE indefinitely under the tight cap — no phantom
	// forfeit transition can happen.
	//
	// Use require.Never to assert the negative property: over the
	// full window the VTXO never reaches SPENT or FORFEITED.
	require.Never(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		listResp, err := alice.RPCClient.ListVTXOs(
			ctx, &daemonrpc.ListVTXOsRequest{},
		)
		if err != nil {
			return false
		}
		for _, v := range listResp.Vtxos {
			if v.Outpoint != aliceLiveVTXO.Outpoint {
				continue
			}
			switch v.Status {
			case daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
				daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
				return true
			}
		}

		return false
	}, defaultTimeout, 500*time.Millisecond,
		"alice's VTXO must remain LIVE under cap rejection — "+
			"the rejection path runs before LockInputsReq so "+
			"no forfeit transition can fire")

	// Bob must also have received nothing: the rejection short-
	// circuits before any recipient notification.
	bobLiveAfter := outpointSet(listLiveVTXOs(t, bob.RPCClient))
	require.Equal(t, len(bobLiveBefore), len(bobLiveAfter),
		"bob must not receive a VTXO when the OOR submit was "+
			"rejected on the server side")
}
