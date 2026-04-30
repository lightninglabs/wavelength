//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestFraudResponseSpentVTXOCheckpointTimeoutSweep is the Step 1 PR 1
// integration test. It walks the full no-client-response path:
//
//  1. Alice receives a round-issued VTXO and sends it to Bob via OOR.
//     The OOR session finalizes server-side, persisting a sender-signed
//     checkpoint PSBT keyed on Alice's input.
//  2. Alice's client force-broadcasts every recovery-DAG transaction
//     above her source VTXO until the source VTXO leaf appears on
//     chain. This is the on-chain trigger the operator's batchwatcher
//     watches for.
//  3. Once batchwatcher emits VTXOOnChainNotification for the spent
//     source VTXO, the server fraud responder loads the persisted OOR
//     checkpoint, validates it byte-for-byte, and broadcasts it via
//     txconfirm (TRUC v3 parent + CPFP child paid from the operator's
//     wallet through the ephemeral anchor).
//  4. When the checkpoint confirms, batchwatcher observes that it spent
//     Alice's source VTXO and moves its watched frontier to checkpoint
//     output 0. The client never broadcasts the matching Ark tx, so that
//     output remains unspent until CSV maturity.
//  5. At maturity, batchwatcher asks the fraud responder to build the
//     operator-timeout sweep. Fraud signs the CSV-gated leaf and submits
//     the sweep through txconfirm. The sweep must confirm and pay a
//     server-controlled output.
func TestFraudResponseSpentVTXOCheckpointTimeoutSweep(t *testing.T) {
	runFraudResponseSpentVTXOCheckpointTimeoutSweep(
		t, fraudRestartNever,
	)
}

// TestFraudRestartCheckpointMaturity proves the checkpoint frontier is rebuilt
// from durable round/OOR state on server restart. The checkpoint is already
// confirmed before arkd stops, so startup must reload the active confirmed
// batch, replay the spent source VTXO, watch checkpoint output 0, and request
// the CSV sweep once it matures.
func TestFraudRestartCheckpointMaturity(t *testing.T) {
	runFraudResponseSpentVTXOCheckpointTimeoutSweep(
		t, fraudRestartAfterCheckpointConfirmed,
	)
}

// fraudRestartPoint selects where the shared fraud-response scenario restarts
// arkd.
type fraudRestartPoint uint8

const (
	// fraudRestartNever runs the scenario without restarting arkd.
	fraudRestartNever fraudRestartPoint = iota

	// fraudRestartAfterCheckpointConfirmed restarts arkd after checkpoint
	// confirmation but before the checkpoint output reaches CSV maturity.
	fraudRestartAfterCheckpointConfirmed
)

// runFraudResponseSpentVTXOCheckpointTimeoutSweep runs the shared
// spent-VTXO checkpoint timeout-sweep scenario, optionally restarting arkd at
// a specific point to exercise startup replay.
func runFraudResponseSpentVTXOCheckpointTimeoutSweep(t *testing.T,
	restartPoint fraudRestartPoint) {

	h := newUnrollHarness(t)

	// Fund the operator's LND wallet with confirmed taproot UTXOs.
	// txconfirm needs wallet UTXOs to fund the CPFP child that pays the
	// package fee for OOR checkpoint broadcast; the OOR checkpoint
	// parent itself has zero fee and only relays as part of a v3
	// package. NewSweepPkScript on the operator hands out P2TR change,
	// so we use taproot fee inputs as well to keep the package wallet
	// signing path on a single script class — mixing P2WPKH inputs with
	// a P2TR change output trips an LND FinalizePsbt edge case in the
	// itest LND build that produces a witness rejected by the script
	// engine.
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	// Alice boards a 100k-sat round-issued VTXO; this is the eventual
	// "spent" VTXO that, once revealed on chain, triggers the fraud
	// response path under test.
	_, aliceLiveVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations,
		100_000,
	)

	// Snapshot Bob's pre-OOR live VTXO set so the test can detect Bob's
	// new VTXO once the OOR transfer materializes on his side.
	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	// Bob registers a fresh receive script. Alice will use it as the
	// destination for an OOR transfer; this is a stable, non-keyed
	// receive (no need to share Bob's identity key with Alice).
	recvResp, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-fraud-response-oor",
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, recvResp.PubkeyXonlyHex)

	recipientPubkey, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err)

	// Alice fires off a full-amount OOR send to Bob. Once finalized the
	// server has persisted a sender-signed checkpoint PSBT that spends
	// Alice's source VTXO and pays into a checkpoint output owned by
	// Alice (collab leaf) with an operator timeout fallback (CSV leaf).
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

	// Wait for Bob's wallet to materialize the new live VTXO; this is
	// proof that the OOR finalize completed on the server side.
	receivedVTXO := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, receivedVTXO)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// Walk Bob's incoming VTXO lineage backwards and force-broadcast
	// every recovery-DAG tx until Alice's source VTXO leaf appears on
	// chain. This bypasses the legitimate cooperative path entirely:
	// the lineage txs are pre-signed unilateral-exit transactions, and
	// each one is CPFP-funded by the harness so it can confirm without
	// a fee-paying child. Once the leaf tx commits, batchwatcher will
	// emit VTXOOnChainNotification for Alice's source outpoint.
	forceBroadcastLineageToOutpoint(
		t, h, bob, receivedVTXO.Outpoint, aliceLiveVTXO.Outpoint,
	)

	// The fraud responder reacts to VTXOOnChainNotification by
	// broadcasting the persisted OOR checkpoint. Wait for that broadcast
	// to land in the mempool: the operator's checkpoint tx is a TRUC v3
	// parent whose only input is Alice's now-on-chain source VTXO.
	sourceOutpoint := mustParseOutpoint(t, aliceLiveVTXO.Outpoint)
	checkpointTxid, checkpointTx := waitForMempoolSpend(
		t, h, nil, sourceOutpoint,
	)
	require.Equal(t, sourceOutpoint,
		checkpointTx.TxIn[0].PreviousOutPoint)

	// Mine the operator's checkpoint into a block. Batchwatcher replays
	// the source-VTXO spend, recognizes that the spender is the persisted
	// checkpoint tx, and starts watching checkpoint output 0 instead of
	// asking fraud to broadcast the same checkpoint again.
	h.Generate(1)
	require.NoError(t, h.WaitForTxConfirmed(
		ctx, checkpointTxid, 30*time.Second,
	))

	checkpointOutpoint := wire.OutPoint{
		Hash:  checkpointTxid,
		Index: 0,
	}

	if restartPoint == fraudRestartAfterCheckpointConfirmed {
		// Restart with the checkpoint already confirmed and output 0
		// still unspent. On startup, rounds reloads active confirmed
		// batches into batchwatcher; the chain source then replays the
		// source spend so batchwatcher can ratchet to the checkpoint
		// output without any fraud-owned checkpoint scan.
		h.RestartArkd()
	}

	// Drive the chain past CSV maturity so the operator's timeout leaf
	// becomes spendable. Batchwatcher owns this timer because it owns the
	// active frontier; once the checkpoint output is mature and still
	// unspent it sends a CheckpointSweepNotification to fraud.
	h.Generate(int(testVTXOExitDelay))

	// Find the operator timeout sweep wherever it is — mempool or a
	// recently-mined block. The watcher reacts to BlockEpoch
	// asynchronously, so a `Generate(1)` issued by this test can race
	// the sweep request and confirm the sweep in the same block it was
	// broadcast in. The helper also keeps mining if neither place has
	// the spender yet, so the test never wedges waiting for a sweep
	// that already confirmed before the previous poll.
	sweepTxid, sweepTx := waitForSpendOnChainOrMempool(
		t, h, checkpointOutpoint, map[string]struct{}{
			checkpointTxid.String(): {},
		},
	)

	// Sweep invariants:
	//   - input 0 spends checkpoint output 0 (the only output the
	//     operator controls via the timeout leaf);
	//   - sequence satisfies the checkpoint CSV (>= CSVDelay);
	//   - exactly two outputs: server-controlled value + ephemeral
	//     anchor for any future CPFP fee-bump.
	require.Equal(t, checkpointOutpoint,
		sweepTx.TxIn[0].PreviousOutPoint)
	require.GreaterOrEqual(t, sweepTx.TxIn[0].Sequence,
		uint32(testVTXOExitDelay),
		"sweep sequence must satisfy CSV maturity")
	require.Len(t, sweepTx.TxOut, 2)

	// The spender helper can return a mempool transaction. Mine once more
	// before waiting so the happy path does not depend on a later unrelated
	// block. If the helper already found a confirmed transaction, this only
	// adds depth.
	h.Generate(1)

	// Wait for the sweep to confirm and surface in the operator's LND
	// wallet under the destination pkScript built by NewSweepPkScript.
	// This proves the funds are recoverable end-to-end: the operator
	// can spend them again from its wallet.
	require.NoError(t, h.WaitForTxConfirmed(
		ctx, sweepTxid, 30*time.Second,
	))
	waitForOperatorWalletUTXO(t, h, sweepTxid.String(),
		sweepTx.TxOut[0].PkScript)
}
// waitForSpendOnChainOrMempool polls for a transaction that spends outpoint,
// looking in both the mempool and the most-recent block, mining one block per
// iteration to keep the chain moving until it is found. Mining inside the
// loop covers the case where the spender races ahead of the previous mined
// block: the responder sees a new block epoch, broadcasts, and the next
// `Generate(1)` confirms the spender before the test can sample the mempool.
func waitForSpendOnChainOrMempool(t *testing.T, h *harness.ArkHarness,
	outpoint wire.OutPoint, knownTxIDs map[string]struct{}) (
	chainhash.Hash, *wire.MsgTx) {

	t.Helper()

	var (
		foundTxid chainhash.Hash
		foundTx   *wire.MsgTx
	)
	require.Eventually(t, func() bool {
		// Check the mempool first.
		txid, tx := findMempoolSpend(t, h, knownTxIDs, outpoint)
		if tx != nil {
			foundTxid = txid
			foundTx = tx
			return true
		}

		// Mempool was empty. Check whether the spender already
		// confirmed in the most recently mined block.
		txid, tx = findSpendInRecentBlocks(
			t, h, outpoint, knownTxIDs, 6,
		)
		if tx != nil {
			foundTxid = txid
			foundTx = tx
			return true
		}

		// No match on chain or in mempool; mine another block to give
		// batchwatcher a fresh BlockEpoch and try again.
		h.Generate(1)

		return false
	}, 2*time.Minute, 1*time.Second,
		"never observed tx spending %s", outpoint)

	return foundTxid, foundTx
}

// findSpendInRecentBlocks scans the last `lookback` blocks for a transaction
// that spends outpoint. Returns the matched txid and parsed tx, or zero
// values if no spender is found.
func findSpendInRecentBlocks(t *testing.T, h *harness.ArkHarness,
	outpoint wire.OutPoint, knownTxIDs map[string]struct{},
	lookback int) (chainhash.Hash, *wire.MsgTx) {

	t.Helper()

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)

	bestHeight := int64(h.Harness.BlockCount())

	startHeight := bestHeight - int64(lookback)
	if startHeight < 0 {
		startHeight = 0
	}

	for height := bestHeight; height >= startHeight; height-- {
		hash, err := rpcClient.GetBlockHash(height)
		if err != nil {
			continue
		}

		block, err := rpcClient.GetBlock(hash)
		if err != nil {
			continue
		}

		for _, tx := range block.Transactions {
			txid := tx.TxHash()
			if _, known := knownTxIDs[txid.String()]; known {
				continue
			}

			for _, txIn := range tx.TxIn {
				if txIn.PreviousOutPoint != outpoint {
					continue
				}

				return txid, tx
			}
		}
	}

	return chainhash.Hash{}, nil
}

// forceBroadcastLineageToOutpoint confirms every lineage transaction needed
// to materialize targetOutpoint on chain.
func forceBroadcastLineageToOutpoint(t *testing.T, h *harness.ArkHarness,
	client *harness.ClientDaemonHarness, rootOutpoint,
	targetOutpoint string) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	entries, onChain := collectLineageEntries(
		t, ctx, client, rootOutpoint,
	)
	target := mustParseOutpoint(t, targetOutpoint)

	for {
		if onChain[target] {
			return
		}

		progress := false
		for op, entry := range entries {
			if onChain[op] {
				continue
			}
			if !lineageParentsOnChain(entry, onChain) {
				continue
			}

			txid := entry.Tx.TxHash()
			_, err := h.ForceBroadcastLineageTx(
				ctx, entry, btcutil.Amount(1000),
			)
			require.NoError(t, err)

			h.Generate(1)
			require.NoError(t, h.WaitForTxConfirmed(
				ctx, txid.String(), 30*time.Second,
			))

			markLineageTxOutputsOnChain(onChain, entry.Tx)
			if onChain[target] {
				return
			}

			progress = true
		}

		if !progress {
			t.Fatalf("could not materialize lineage target %s",
				targetOutpoint)
		}
	}
}

// collectLineageEntries walks a VTXO lineage and returns every broadcastable
// entry plus the outpoints that are already on chain.
func collectLineageEntries(t *testing.T, ctx context.Context,
	client *harness.ClientDaemonHarness, targetOutpoint string) (
	map[wire.OutPoint]*darepod.VTXOLineageEntry, map[wire.OutPoint]bool) {

	t.Helper()

	entries := make(map[wire.OutPoint]*darepod.VTXOLineageEntry)
	onChain := make(map[wire.OutPoint]bool)
	visited := map[string]bool{}
	queue := []string{targetOutpoint}

	for len(queue) > 0 {
		op := queue[0]
		queue = queue[1:]

		if visited[op] {
			continue
		}
		visited[op] = true

		entry, err := client.GetVTXOLineageTx(
			ctx, targetOutpoint, op,
		)
		require.NoError(t, err)

		if entry.OnChainRoot {
			onChain[entry.Outpoint] = true
			continue
		}
		require.NotNil(t, entry.Tx)

		entries[entry.Outpoint] = entry
		for _, parent := range entry.ParentOutpoints {
			queue = append(queue, parent.String())
		}
	}

	return entries, onChain
}

// lineageParentsOnChain returns true when every parent input is materialized.
func lineageParentsOnChain(entry *darepod.VTXOLineageEntry,
	onChain map[wire.OutPoint]bool) bool {

	if len(entry.ParentOutpoints) == 0 {
		return false
	}

	for _, parent := range entry.ParentOutpoints {
		if !onChain[parent] {
			return false
		}
	}

	return true
}

// markLineageTxOutputsOnChain records every output created by tx.
func markLineageTxOutputsOnChain(onChain map[wire.OutPoint]bool,
	tx *wire.MsgTx) {

	txid := tx.TxHash()
	for i := range tx.TxOut {
		onChain[wire.OutPoint{
			Hash:  txid,
			Index: uint32(i),
		}] = true
	}
}

// waitForMempoolSpend waits for a transaction in the mempool that spends
// outpoint and returns it.
func waitForMempoolSpend(t *testing.T, h *harness.ArkHarness,
	knownTxIDs map[string]struct{}, outpoint wire.OutPoint) (
	chainhash.Hash, *wire.MsgTx) {

	t.Helper()

	var (
		txid chainhash.Hash
		tx   *wire.MsgTx
	)
	require.Eventually(t, func() bool {
		txid, tx = findMempoolSpend(t, h, knownTxIDs, outpoint)
		return tx != nil
	}, defaultTimeout, pollInterval,
		"never observed mempool tx spending %s", outpoint)

	return txid, tx
}

// findMempoolSpend returns the first mempool transaction spending outpoint.
func findMempoolSpend(t *testing.T, h *harness.ArkHarness,
	knownTxIDs map[string]struct{},
	outpoint wire.OutPoint) (chainhash.Hash, *wire.MsgTx) {

	t.Helper()

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)

	for _, txID := range h.MempoolTxIDs() {
		if _, known := knownTxIDs[txID]; known {
			continue
		}

		hash, err := chainhash.NewHashFromStr(txID)
		if err != nil {
			continue
		}

		rawTx, err := rpcClient.GetRawTransaction(hash)
		if err != nil {
			continue
		}

		tx := rawTx.MsgTx()
		for _, txIn := range tx.TxIn {
			if txIn.PreviousOutPoint == outpoint {
				return *hash, tx
			}
		}
	}

	return chainhash.Hash{}, nil
}

// mustParseOutpoint parses the canonical txid:vout form used by the daemon.
func mustParseOutpoint(t *testing.T, outpoint string) wire.OutPoint {
	t.Helper()

	txid, indexStr, ok := strings.Cut(outpoint, ":")
	require.True(t, ok)
	require.Len(t, txid, 64)

	hash, err := chainhash.NewHashFromStr(txid)
	require.NoError(t, err)

	index, err := strconv.ParseUint(indexStr, 10, 32)
	require.NoError(t, err)

	return wire.OutPoint{
		Hash:  *hash,
		Index: uint32(index),
	}
}
