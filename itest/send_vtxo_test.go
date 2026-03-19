//go:build itest

package itest

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestSendVTXOIntegrationDryRunPreview verifies SendVTXO dry-run mode
// validates recipients and reports transfer totals without mutating state.
func TestSendVTXOIntegrationDryRunPreview(t *testing.T) {
	alice, bob, aliceStartBalance, bobStartBalance,
		recipientPkScript := setupSendVTXOValidationHarness(
		t, "itest-sendvtxo-dry-run-preview",
	)

	const sendAmount = int64(50_000)
	sendResp, err := alice.RPCClient.SendVTXO(
		t.Context(), &daemonrpc.SendVTXORequest{
			Recipients: []*daemonrpc.Output{
				{
					Destination: &daemonrpc.Output_PkScript{
						PkScript: recipientPkScript,
					},
					AmountSat: sendAmount,
				},
			},
			DryRun: true,
		},
	)
	require.NoError(t, err, "SendVTXO dry-run RPC failed")
	require.Equal(t, "preview", sendResp.Status)
	require.Empty(t, sendResp.RoundId)
	require.Equal(t, sendAmount, sendResp.TotalAmountSat)

	waitForExactVTXOBalance(
		t, alice.RPCClient, aliceStartBalance.VtxoBalanceSat,
	)
	waitForExactVTXOBalance(
		t, bob.RPCClient, bobStartBalance.VtxoBalanceSat,
	)
}

func setupSendVTXOValidationHarness(t *testing.T, label string) (
	*harness.ClientDaemonHarness, *harness.ClientDaemonHarness,
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

	_, _, aliceStartBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)
	bobStartBalance := waitForExactVTXOBalance(t, bob.RPCClient, 0)

	recvResp, err := bob.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{Label: label},
	)
	require.NoError(t, err, "NewOORReceiveScript RPC failed")

	recipientPkScript, err := hex.DecodeString(recvResp.PkScriptHex)
	require.NoError(t, err, "pk_script_hex must be valid hex")

	return alice, bob, aliceStartBalance, bobStartBalance, recipientPkScript
}

// TestSendVTXOIntegrationUnimplemented verifies non-dry-run SendVTXO requests
// currently return unimplemented and do not mutate wallet balances.
func TestSendVTXOIntegrationUnimplemented(t *testing.T) {
	alice, bob, aliceStartBalance, bobStartBalance,
		recipientPkScript := setupSendVTXOValidationHarness(
		t, "itest-sendvtxo-unimplemented",
	)

	const sendAmount = int64(50_000)
	_, err = alice.RPCClient.SendVTXO(
		t.Context(), &daemonrpc.SendVTXORequest{
			Recipients: []*daemonrpc.Output{
				{
					Destination: &daemonrpc.Output_PkScript{
						PkScript: recipientPkScript,
					},
					AmountSat: sendAmount,
				},
			},
		},
	)
	require.Error(t, err, "SendVTXO should be unimplemented")
	require.Equal(t, codes.Unimplemented, status.Code(err))
	require.ErrorContains(t, err, "not yet implemented")

	waitForExactVTXOBalance(
		t, alice.RPCClient, aliceStartBalance.VtxoBalanceSat,
	)
	waitForExactVTXOBalance(
		t, bob.RPCClient, bobStartBalance.VtxoBalanceSat,
	)
}
