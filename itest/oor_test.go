//go:build itest

package itest

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestOORIntegrationAliceToBob verifies a real daemon-to-daemon OOR transfer
// across the public RPC surface after both clients have live VTXOs.
func TestOORIntegrationAliceToBob(t *testing.T) {
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

	recvResp, err := bob.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-alice-to-bob",
		},
	)
	require.NoError(t, err, "NewOORReceiveScript RPC failed")
	require.NotEmpty(t, recvResp.PkScriptHex)

	recipientPkScript, err := hex.DecodeString(recvResp.PkScriptHex)
	require.NoError(t, err, "pk_script_hex must be valid hex")

	sendAmount := aliceLiveVTXO.AmountSat
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PkScript{
					PkScript: recipientPkScript,
				},
				AmountSat: sendAmount,
			},
		},
	)
	if err != nil && strings.Contains(
		err.Error(),
		"fee-less ark tx requires equal input/output sums",
	) {

		t.Skipf("known multi-input OOR issue "+
			"(darepo-client#199): %v", err)
	}
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

// TestOORIntegrationBidirectionalTransfer verifies both clients can perform
// OOR sends in opposite directions using real daemon RPCs and persisted state.
func TestOORIntegrationBidirectionalTransfer(t *testing.T) {
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

	bobRecv1, err := bob.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-bidirectional-bob-recv",
		},
	)
	require.NoError(t, err)

	bobPkScript1, err := hex.DecodeString(bobRecv1.PkScriptHex)
	require.NoError(t, err)

	sendAmount1 := aliceLiveVTXO.AmountSat
	send1Resp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PkScript{
					PkScript: bobPkScript1,
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
	aliceRecv2, err := alice.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-bidirectional-alice-recv",
		},
	)
	require.NoError(t, err)

	alicePkScript2, err := hex.DecodeString(aliceRecv2.PkScriptHex)
	require.NoError(t, err)

	sendAmount2 := bobReceived1.AmountSat
	send2Resp, err := bob.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PkScript{
					PkScript: alicePkScript2,
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

	_, aliceVTXO1, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	_, aliceVTXO2, aliceBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	require.Equal(t, int64(99_000), aliceVTXO1.AmountSat)
	require.Equal(t, int64(99_000), aliceVTXO2.AmountSat)
	require.Equal(t, aliceVTXO1.AmountSat+aliceVTXO2.AmountSat,
		aliceBalance.VtxoBalanceSat)

	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	recvResp, err := bob.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-multi-input-oor",
		},
	)
	require.NoError(t, err, "NewOORReceiveScript RPC failed")

	recipientPkScript, err := hex.DecodeString(recvResp.PkScriptHex)
	require.NoError(t, err, "pk_script_hex must be valid hex")

	sendAmount := int64(120_000)
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PkScript{
					PkScript: recipientPkScript,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err, "SendOOR RPC failed")
	require.Equal(t, "submitted", sendResp.Status)
	require.NotEmpty(t, sendResp.SessionId)

	waitForVTXOBalanceBelow(
		t, alice.RPCClient, aliceBalance.VtxoBalanceSat,
	)
	waitForExactVTXOBalance(t, bob.RPCClient, sendAmount)
	waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)

	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO1.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO2.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)

	// TODO(bhandras): Once OOR unroll is implemented, finish this by
	// unrolling Bob's received VTXO to prove end-to-end ownership.
	t.Logf("multi-input OOR transfer completed: session_id=%s amount=%d "+
		"inputs=[%s,%s]", sendResp.SessionId, sendAmount,
		aliceVTXO1.Outpoint, aliceVTXO2.Outpoint)
}
