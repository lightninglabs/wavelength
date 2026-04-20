//go:build itest

package itest

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightninglabs/darepo/harness"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestOORIntegrationVHTLCClaimSweep verifies that real client daemons can use
// a vHTLC policy end to end over the public RPC surface:
//
//  1. Alice boards a standard Ark VTXO.
//  2. Alice sends that value out-of-round to a vHTLC policy template.
//  3. Bob discovers the indexed vHTLC output via daemon RPC.
//  4. Bob claims the vHTLC through SendOOR custom_inputs and sweeps the value
//     into a fresh standard Ark VTXO for himself.
func TestOORIntegrationVHTLCClaimSweep(t *testing.T) {
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

	aliceInfo := waitForDaemonInfoReachable(t, alice.RPCClient)
	bobInfo := waitForDaemonInfoReachable(t, bob.RPCClient)

	aliceKeyBytes, err := hex.DecodeString(aliceInfo.IdentityPubkey)
	require.NoError(t, err, "alice identity_pubkey must be valid hex")

	aliceKey, err := btcec.ParsePubKey(aliceKeyBytes)
	require.NoError(t, err, "alice identity_pubkey must be a pubkey")

	bobKeyBytes, err := hex.DecodeString(bobInfo.IdentityPubkey)
	require.NoError(t, err, "bob identity_pubkey must be valid hex")

	bobKey, err := btcec.ParsePubKey(bobKeyBytes)
	require.NoError(t, err, "bob identity_pubkey must be a pubkey")

	operatorKey, err := btcec.ParsePubKey(operatorInfo.Pubkey)
	require.NoError(t, err, "operator pubkey must be a pubkey")

	_, aliceLiveVTXO, aliceBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)

	var preimage lntypes.Preimage
	copy(preimage[:], []byte("itest-vhtlc-claim-sweep-preimage"))

	vhtlcPolicy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               aliceKey,
		Receiver:                             bobKey,
		Server:                               operatorKey,
		PreimageHash:                         preimage.Hash(),
		RefundLocktime:                       500_000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	})
	require.NoError(t, err, "construct vHTLC policy")

	vhtlcPolicyTemplate, err := vhtlcPolicy.Template.Encode()
	require.NoError(t, err, "encode vHTLC policy template")

	vhtlcPkScript, err := vhtlcPolicy.PkScript()
	require.NoError(t, err, "derive vHTLC pkScript")

	aliceInputPkScript, err := hex.DecodeString(aliceLiveVTXO.PkScript)
	require.NoError(t, err, "alice input pk_script must be valid hex")

	sendAmount := aliceLiveVTXO.AmountSat
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PolicyTemplate{
					PolicyTemplate: vhtlcPolicyTemplate,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err, "alice SendOOR to vHTLC failed")
	require.Equal(t, "submitted", sendResp.Status)
	require.NotEmpty(t, sendResp.SessionId)

	waitForVTXOBalanceBelow(
		t, alice.RPCClient, aliceBalance.VtxoBalanceSat,
	)

	vhtlcIndexed := waitForIndexedVTXOByPkScript(
		t, bob.RPCClient, vhtlcPkScript,
		daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	)
	require.Equal(t, sendAmount, vhtlcIndexed.AmountSat)

	aliceSessionHash, err := chainhash.NewHashFromStr(sendResp.SessionId)
	require.NoError(t, err, "session_id must be a valid txid")

	sessionResp, err := alice.RPCClient.GetIndexedOORSessionByTxid(
		t.Context(), &daemonrpc.GetIndexedOORSessionByTxidRequest{
			PkScript:    aliceInputPkScript,
			SessionTxid: aliceSessionHash.CloneBytes(),
		},
	)
	require.NoError(t, err, "indexed OOR session lookup failed")
	require.NotEmpty(
		t, sessionResp.ArkPsbt, "indexed session must return ark psbt",
	)

	arkPSBT, err := psbtutil.Parse(sessionResp.ArkPsbt)
	require.NoError(t, err, "indexed ark psbt must parse")
	require.Equal(t, *aliceSessionHash, arkPSBT.UnsignedTx.TxHash())

	var expectedVHTLCOutpoint string
	for i, txOut := range arkPSBT.UnsignedTx.TxOut {
		if !bytes.Equal(txOut.PkScript, vhtlcPkScript) {
			continue
		}

		require.Equal(t, sendAmount, txOut.Value)
		expectedVHTLCOutpoint = fmt.Sprintf(
			"%s:%d", aliceSessionHash.String(), i,
		)

		break
	}

	require.NotEmpty(t, expectedVHTLCOutpoint,
		"indexed ark psbt must contain the vHTLC output")
	require.Equal(
		t, expectedVHTLCOutpoint, vhtlcIndexed.Outpoint,
		"bob should discover the exact vHTLC output "+
			"created by alice's OOR session",
	)

	claimPath, err := vhtlcPolicy.ClaimPath(preimage)
	require.NoError(t, err, "derive claim spend path")

	claimPathBytes, err := claimPath.Encode()
	require.NoError(t, err, "encode claim spend path")

	recvResp, err := bob.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-vhtlc-claim-sweep",
		},
	)
	require.NoError(t, err, "bob NewOORReceiveScript RPC failed")

	recipientPubkey, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err, "pubkey_xonly_hex must be valid hex")

	recipientPkScript, err := hex.DecodeString(recvResp.PkScriptHex)
	require.NoError(t, err, "pk_script_hex must be valid hex")

	bobSweepResp, err := bob.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPubkey,
				},
				AmountSat: sendAmount,
			},
			CustomInputs: []*daemonrpc.CustomOORInput{{
				Outpoint:           vhtlcIndexed.Outpoint,
				VtxoPolicyTemplate: vhtlcPolicyTemplate,
				SpendPath:          claimPathBytes,
				AmountSat:          vhtlcIndexed.AmountSat,
				PkScript:           vhtlcPkScript,
			}},
		},
	)
	require.NoError(t, err, "bob SendOOR vHTLC claim sweep failed")
	require.Equal(t, "submitted", bobSweepResp.Status)
	require.NotEmpty(t, bobSweepResp.SessionId)

	waitForIndexedVTXOByPkScript(
		t, bob.RPCClient, vhtlcPkScript,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)

	receivedVTXO := waitForIndexedVTXOByPkScript(
		t, bob.RPCClient, recipientPkScript,
		daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	)
	// Existing OOR itests sweep the requested amount 1:1.
	// No output-side fee deduction should apply here either.
	require.Equal(t, sendAmount, receivedVTXO.AmountSat)
	t.Logf("bob claimed vHTLC outpoint=%s into live vtxo=%s amount=%d",
		vhtlcIndexed.Outpoint, receivedVTXO.Outpoint,
		receivedVTXO.AmountSat)
}
