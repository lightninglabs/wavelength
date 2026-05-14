//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo"
	clientchain "github.com/lightninglabs/darepo-client/chain"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	treepkg "github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

const (
	// nodePackageFeeSat is the fixed CPFP fee used to relay
	// fee-less v3 tree node transactions via package submission.
	nodePackageFeeSat = btcutil.Amount(15_000)

	// minCPFPUTXOValueSat is the smallest confirmed wallet UTXO
	// we will use as the fee-paying child input. Keeps the change
	// output well above dust after paying nodePackageFeeSat.
	minCPFPUTXOValueSat = btcutil.Amount(30_000)
)

// waitForBatchTreeState waits until the operator's BatchWatcher reports a tree
// state matching the given predicate for the specified round output.
func waitForBatchTreeState(t *testing.T,
	h *harness.ArkHarness, roundID string, outputIdx int,
	predicate func(*batchwatcher.BatchTreeState) bool,
) *batchwatcher.BatchTreeState {

	t.Helper()

	var matched *batchwatcher.BatchTreeState
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		state, found, err := h.GetBatchTreeState(
			ctx, roundID, outputIdx,
		)
		if err != nil || !found || state == nil {
			return false
		}

		if !predicate(state) {
			return false
		}

		matched = state

		return true
	}, defaultTimeout, pollInterval,
		"batch watcher state never matched the expected predicate")

	return matched
}

// reserveCPFPUTXO picks a confirmed bitcoind wallet output that can pay for a
// node-package CPFP child and marks it reserved in usedOutpoints.
func reserveCPFPUTXO(t *testing.T, rpc *rpcclient.Client,
	usedOutpoints map[string]struct{}) (wire.OutPoint, *wire.TxOut) {

	t.Helper()

	utxos, err := rpc.ListUnspentMin(1)
	require.NoError(t, err, "ListUnspent should succeed")

	for _, utxo := range utxos {
		if !utxo.Spendable {
			continue
		}

		amount, err := btcutil.NewAmount(utxo.Amount)
		require.NoError(t, err, "wallet UTXO amount should parse")

		if amount < minCPFPUTXOValueSat {
			continue
		}

		outpointKey := fmt.Sprintf("%s:%d", utxo.TxID, utxo.Vout)
		if _, used := usedOutpoints[outpointKey]; used {
			continue
		}

		script, err := hex.DecodeString(utxo.ScriptPubKey)
		require.NoError(t, err, "wallet UTXO script should decode")

		hash, err := chainhash.NewHashFromStr(utxo.TxID)
		require.NoError(t, err, "wallet UTXO txid should parse")

		usedOutpoints[outpointKey] = struct{}{}

		return wire.OutPoint{
				Hash:  *hash,
				Index: utxo.Vout,
			}, &wire.TxOut{
				Value:    int64(amount),
				PkScript: script,
			}
	}

	t.Fatalf("no confirmed bitcoind wallet UTXO was suitable for CPFP")

	return wire.OutPoint{}, nil
}

// submitSignedNodePackage relays a signed v3 tree node by pairing it with a
// fee-paying CPFP child, then atomically submitting both transactions as a
// package through bitcoind.
//
// The tree node itself is intentionally fee-less: it spends one watched tree
// output and preserves all value in the next tree outputs plus the zero-value
// anchor. A plain SendRawTransaction call will therefore fail policy checks.
// The CPFP child spends the parent's anchor together with one confirmed wallet
// UTXO so the package has enough aggregate fee to relay and mine. This mirrors
// the package-submission approach used by the client-side unilateral-exit work.
func submitSignedNodePackage(t *testing.T, rpc *rpcclient.Client,
	bitcoind *clientchain.BitcoindRPCClient, node *treepkg.Node,
	usedOutpoints map[string]struct{}) string {

	t.Helper()

	parentTx, err := node.ToSignedTx()
	require.NoError(t, err, "tree node should be signed")

	parentHash := parentTx.TxHash()
	anchorOutpoint := wire.OutPoint{
		Hash:  parentHash,
		Index: uint32(len(parentTx.TxOut) - 1),
	}

	cpfpOutpoint, cpfpPrevOut := reserveCPFPUTXO(
		t, rpc, usedOutpoints,
	)
	require.Greater(
		t, cpfpPrevOut.Value, int64(nodePackageFeeSat),
		"CPFP UTXO should exceed package fee",
	)

	childTx := wire.NewMsgTx(3)
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: anchorOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: cpfpOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	childTx.AddTxOut(&wire.TxOut{
		Value:    cpfpPrevOut.Value - int64(nodePackageFeeSat),
		PkScript: cpfpPrevOut.PkScript,
	})

	anchorAmountBTC := 0.0
	signedChildTx, complete, err := rpc.SignRawTransactionWithWallet2(
		childTx, []btcjson.RawTxWitnessInput{{
			Txid: anchorOutpoint.Hash.String(),
			Vout: anchorOutpoint.Index,
			ScriptPubKey: hex.EncodeToString(
				arkscript.AnchorPkScript,
			),
			Amount: &anchorAmountBTC,
		}},
	)
	require.NoError(t, err, "SignRawTransactionWithWallet2 should succeed")
	require.True(t, complete, "CPFP child should be fully signed")

	pkgResult, err := bitcoind.SubmitPackage(
		[]*wire.MsgTx{parentTx}, signedChildTx, nil,
	)
	require.NoError(t, err, "SubmitPackage should succeed")
	require.Equal(
		t, "success", pkgResult.PackageMsg,
		"submitpackage should accept the node package",
	)

	for wtxid, txResult := range pkgResult.TxResults {
		if txResult.Error == nil || *txResult.Error == "" {
			continue
		}

		t.Fatalf("submitpackage rejected wtxid=%s txid=%s err=%s",
			wtxid, txResult.TxID.String(), *txResult.Error)
	}

	return parentHash.String()
}

// onlyChildNode returns the single extracted child in a client's TreePath
// root, which is the next branch along that client's unilateral-exit path.
func onlyChildNode(t *testing.T, root *treepkg.Node) *treepkg.Node {
	t.Helper()

	require.NotNil(t, root)
	require.Len(
		t, root.Children, 1,
		"extracted client tree path should retain exactly one child",
	)

	for _, child := range root.Children {
		return child
	}

	t.Fatalf("tree path root unexpectedly had no child")

	return nil
}

// TestPartialUnrollIntegrationRatchetsWatcherForward verifies the server-side
// watcher behavior for a partial tree unroll.
//
// The scenario is:
//  1. Create a shared round large enough to produce a multi-level VTXO tree.
//  2. Read one client's persisted TreePath after the round confirms.
//  3. Broadcast the signed root node from that path.
//  4. Assert the BatchWatcher replaces the single watched batch output with the
//     two branch outputs revealed by the root spend.
//  5. Broadcast the next signed branch node on that same extracted path.
//  6. Assert the BatchWatcher ratchets forward again: one watched branch output
//     is replaced by its descendants, while the sibling branch output remains
//     watched.
//
// The exact state counts matter:
//   - Initially: 1 existing output, 1 watched outpoint.
//   - After root spend: 2 existing outputs, 3 watched outpoints.
//     WatchedOutpoints is additive (consumed outputs stay in the
//     set), so the 3 are: original batch outpoint + 2 new branch
//     outputs.
//   - After branch spend: 3 existing outputs, 2 VTXOs on chain,
//     5 watched outpoints (3 prior + 2 new leaf VTXO outputs).
//     One branch has been partially unrolled down to leaves while
//     the other branch is still being watched.
func TestPartialUnrollIntegrationRatchetsWatcherForward(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		OperatorConfigMutator: func(cfg *darepo.Config) {
			// Four clients join the same shared round
			// serially in this test, and the assertion
			// at the end is "all clients land on the
			// same round_id". The harness default
			// RegistrationTimeout of 500ms is fine for
			// single-client tests but can race the
			// fourth join under CI load: the round
			// seals before everyone arrives and the
			// late client lands in a fresh round. Pin
			// a generous registration window so this
			// test deterministically captures all four
			// clients in the same round.
			cfg.Rounds.RegistrationTimeout = 30 * time.Second
		},
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 2)

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should create bitcoind RPC client")
	t.Cleanup(rpcClient.Shutdown)

	bitcoindClient, err := h.BitcoindClient()
	require.NoError(t, err, "should create bitcoind package client")

	operatorInfo := getOperatorInfo(t, h)

	type testClient struct {
		name   string
		daemon *harness.ClientDaemonHarness
	}

	clients := []testClient{
		{
			name:   "alice",
			daemon: h.StartClientDaemon("alice"),
		},
		{
			name:   "bob",
			daemon: h.StartClientDaemon("bob"),
		},
		{
			name:   "carol",
			daemon: h.StartClientDaemon("carol"),
		},
		{
			name:   "dave",
			daemon: h.StartClientDaemon("dave"),
		},
	}

	const boardingAmount = btcutil.Amount(100_000)
	for _, tc := range clients {
		newAddrResp, err := tc.daemon.RPCClient.NewAddress(
			t.Context(), &daemonrpc.NewAddressRequest{},
		)
		require.NoError(t, err, "%s NewAddress RPC failed", tc.name)
		require.NotEmpty(t, newAddrResp.Address)

		fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
		t.Logf(
			"%s funded boarding address via txid=%s", tc.name,
			fundingTxID,
		)
	}

	h.Generate(int(operatorInfo.MinConfirmations) + 1)

	for _, tc := range clients {
		waitForConfirmedBoardingBalance(
			t, tc.daemon.RPCClient, int64(boardingAmount),
		)
		resp := waitForBoardRegistered(t, tc.daemon.RPCClient)
		require.Equal(t, "registered", resp.Status)
	}

	waitForRegisteredClients(t, h, len(clients))

	sharedRoundID := ""
	for _, tc := range clients {
		joined := waitForClientRoundState(
			t, tc.daemon.RPCClient,
			daemonrpc.RoundState_ROUND_STATE_JOINED,
		)
		require.NotEmpty(t, joined.RoundId)
		require.False(t, joined.IsTemp)

		if sharedRoundID == "" {
			sharedRoundID = joined.RoundId
			continue
		}

		require.Equal(
			t, sharedRoundID, joined.RoundId,
			"all clients should join the same round",
		)
	}

	for _, tc := range clients {
		waitForNamedClientRoundState(
			t, tc.daemon.RPCClient, sharedRoundID,
			daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
		)
		waitForPersistedClientRoundState(
			t, tc.daemon.RPCClient, sharedRoundID,
			daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
		)
	}

	broadcastRound := waitForOperatorRoundStatus(
		t, h, sharedRoundID,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, sharedRoundID, broadcastRound.TxId,
	)
	for _, tc := range clients {
		waitForNamedClientRoundState(
			t, tc.daemon.RPCClient, sharedRoundID,
			daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
		)
	}

	liveVTXO := waitForLiveVTXO(
		t, clients[0].daemon.RPCClient, sharedRoundID,
	)
	storedVTXO, err := clients[0].daemon.GetStoredVTXO(
		t.Context(), liveVTXO.Outpoint,
	)
	require.NoError(t, err, "client should persist live VTXO")
	require.NotEmpty(
		t, storedVTXO.Ancestry,
		"client should persist at least one ancestry fragment",
	)
	primaryTree := storedVTXO.Ancestry[0].TreePath
	require.NotNil(t, primaryTree)
	require.NotNil(t, primaryTree.Root)
	require.Greater(
		t, primaryTree.Root.Depth(), 2,
		"expected a multi-level tree path",
	)

	// Query BatchWatcher state by deterministic batch ID, derived
	// from the commitment output index for this client's tree.
	batchOutputIdx := int(primaryTree.BatchOutpoint.Index)
	usedCPFPUTXOs := make(map[string]struct{})

	// Before any unilateral tree tx is broadcast, the watcher
	// should still watch only the single confirmed batch output.
	initialState := waitForBatchTreeState(
		t, h, sharedRoundID, batchOutputIdx,
		func(state *batchwatcher.BatchTreeState) bool {
			return len(state.ExistingOutputs) == 1
		},
	)
	require.Len(t, initialState.WatchedOutpoints, 1)

	// Spend the batch root with the client's signed root node.
	// This should reveal the next layer and make the watcher
	// fan out to the newly confirmed branch outputs.
	rootTxID := submitSignedNodePackage(
		t, rpcClient, bitcoindClient, primaryTree.Root, usedCPFPUTXOs,
	)
	h.WaitMempoolTx(rootTxID)
	h.GenerateAndWait(1)

	rootState := waitForBatchTreeState(
		t, h, sharedRoundID, batchOutputIdx,
		func(state *batchwatcher.BatchTreeState) bool {
			return len(state.ExistingOutputs) == 2 &&
				len(state.VTXOsOnChain) == 0
		},
	)
	require.Len(t, rootState.WatchedOutpoints, 3)

	// The extracted path keeps exactly one child from the
	// client's perspective. That child is still a branch node,
	// so spending it should trigger one more ratchet-forward
	// step rather than the final leaf-spend handling path.
	branchNode := onlyChildNode(t, primaryTree.Root)
	require.False(
		t, branchNode.IsLeaf(),
		"selected client path should include a branch step",
	)

	// Spend the next branch node in the same way. The watcher
	// should keep the untouched sibling branch live while
	// replacing this spent output with the confirmed children.
	branchTxID := submitSignedNodePackage(
		t, rpcClient, bitcoindClient, branchNode, usedCPFPUTXOs,
	)
	h.WaitMempoolTx(branchTxID)
	h.GenerateAndWait(1)

	branchState := waitForBatchTreeState(
		t, h, sharedRoundID, batchOutputIdx,
		func(state *batchwatcher.BatchTreeState) bool {
			return len(state.ExistingOutputs) == 3 &&
				len(state.VTXOsOnChain) == 2
		},
	)
	// 5 watched = batch root + 2 branch outputs from root + 2 leaf
	// VTXOs from the spent branch. Leaf outputs are now watched for
	// spend detection by the recovery classification path.
	require.Len(t, branchState.WatchedOutpoints, 5)
}
