//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo/adminrpc"
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

// TestFraudResponseForfeitedVTXO broadcasts the stored forfeit transaction
// when a cooperatively forfeited VTXO is later revealed on-chain.
func TestFraudResponseForfeitedVTXO(t *testing.T) {
	runFraudResponseForfeitedVTXO(t, forfeitResponseOptions{
		forfeitCount: 1,
	})
}

// TestFraudRestartForfeitSweep proves startup replay can recover a confirmed
// forfeit response whose penalty output has not yet been swept.
func TestFraudRestartForfeitSweep(t *testing.T) {
	runFraudResponseForfeitedVTXO(t, forfeitResponseOptions{
		forfeitCount:                    1,
		restartAfterForfeitMinedOffline: true,
	})
}

// TestFraudRestartForfeitResponse proves startup replay can ratchet through a
// forfeited VTXO reveal that confirmed while arkd was offline and then submit
// the stored forfeit response.
func TestFraudRestartForfeitResponse(t *testing.T) {
	runFraudResponseForfeitedVTXO(t, forfeitResponseOptions{
		forfeitCount:                    1,
		restartAfterVTXORevealedOffline: true,
	})
}

// TestFraudResponseTwoForfeitedLeavesSharingConnectorTree verifies that
// when two VTXOs forfeited in the same round are unilaterally exited
// concurrently, the operator broadcasts BOTH connector tree branches
// needed to materialize the two distinct connector leaves and both
// forfeit responses confirm.
//
// Setup is a balanced binary connector tree (radix=2, max=4 leaves,
// depth=2):
//
//	     root
//	    /    \
//	 inner0  inner1
//	 /  \    /  \
//	l0  l1  l2  l3
//
// Forfeit responses for VTXO[0] (leaf l0, under inner0) and VTXO[3]
// (leaf l3, under inner1) share root but diverge at the inner level, so
// the operator must broadcast the connector chain along BOTH inner
// branches. This exercises the "Connector Spend Race Handling" case
// from issue #247 end-to-end: the actor's pending-tx map must fan a
// single root-confirmation out to both jobs so each can advance to its
// own inner branch and finally its own forfeit tx.
func TestFraudResponseTwoForfeitedLeavesSharingConnectorTree(t *testing.T) {
	t.Parallel()

	h := newUnrollHarnessWithMutator(t, func(cfg *darepo.Config) {
		cfg.Rounds.TreeRadix = 2
		cfg.Rounds.MaxConnectorsPerTree = 4
	})

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 1)

	// Board four VTXOs so the refresh round materializes a depth-2
	// connector tree. Two of them at the extreme leaf indices give us
	// the highest chance of landing under different inner branches.
	const forfeitCount = 4
	forfeitedVTXOs := make([]*daemonrpc.VTXO, 0, forfeitCount)
	for i := 0; i < forfeitCount; i++ {
		_, vtxo, _ := boardClientAndConfirmRound(
			t, h, alice.RPCClient, operatorInfo.MinConfirmations,
			100_000,
		)
		forfeitedVTXOs = append(forfeitedVTXOs, vtxo)
	}

	targetA := forfeitedVTXOs[0]
	targetB := forfeitedVTXOs[forfeitCount-1]

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	refreshOutpoints := make([]string, 0, len(forfeitedVTXOs))
	for _, vtxo := range forfeitedVTXOs {
		refreshOutpoints = append(refreshOutpoints, vtxo.Outpoint)
	}

	existingRoundIDs := snapshotClientRoundIDs(t, alice.RPCClient)
	refreshResp, err := alice.RPCClient.RefreshVTXOs(
		t.Context(), &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: refreshOutpoints,
				},
			},
		},
	)
	require.NoError(t, err, "RefreshVTXOs RPC failed")
	require.Equal(t, "queued", refreshResp.Status)
	for _, outpoint := range refreshOutpoints {
		require.Contains(t, refreshResp.QueuedOutpoints, outpoint)
	}

	for _, outpoint := range refreshOutpoints {
		waitForServerVTXOStatus(
			t, h, mustParseOutpoint(t, outpoint),
			"live",
		)
	}

	alice.TriggerRoundRegistration()

	refreshRound := waitForNewClientRoundState(
		t, alice.RPCClient, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, refreshRound.RoundId)
	waitForNamedClientRoundState(
		t, alice.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForPersistedClientRoundState(
		t, alice.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, refreshRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, refreshRound.RoundId, broadcastRound.TxId,
	)
	for _, outpoint := range refreshOutpoints {
		waitForVTXOStatusByOutpoint(
			t, alice.RPCClient, outpoint,
			daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
		)
	}

	// Snapshot the recovery lineage for BOTH targets AFTER the refresh
	// round marks them forfeited. The harness-only
	// GetVTXOLineageTx path routes through
	// LocalProofAssembler.EnsureProofForHarness, which is explicitly
	// terminal-tolerant: Spent / Forfeited / Failed VTXOs still have a
	// walkable historical lineage. That is the entire reason the
	// harness accessor exists — fraud-response itests need to drive a
	// previous owner unilaterally broadcasting a VTXO they no longer
	// own.
	lineageA, lineageOnChainA := collectLineageEntries(
		t, ctx, alice, targetA.Outpoint,
	)
	lineageB, lineageOnChainB := collectLineageEntries(
		t, ctx, alice, targetB.Outpoint,
	)

	// Force-broadcast BOTH targets' lineages back-to-back. This puts
	// both forfeited VTXO leaves on chain in close succession, so the
	// fraud responder receives two VTXOOnChainNotifications and must
	// plan two forfeit responses whose connector ancestor chains
	// share the root but diverge at the inner level.
	forceBroadcastCollectedLineageToOutpoint(
		t, h, lineageA, lineageOnChainA, targetA.Outpoint,
	)
	forceBroadcastCollectedLineageToOutpoint(
		t, h, lineageB, lineageOnChainB, targetB.Outpoint,
	)

	forfeitOutpointA := mustParseOutpoint(t, targetA.Outpoint)
	forfeitOutpointB := mustParseOutpoint(t, targetB.Outpoint)

	// Each forfeit response must reach the mempool or chain. Without
	// the multi-job pending fan-out, only one of the two would
	// actually broadcast — the second would be stranded at the
	// shared root ancestor.
	forfeitTxidA, forfeitTxA := waitForSpendOnChainOrMempool(
		t, h, forfeitOutpointA, nil,
	)
	require.Len(t, forfeitTxA.TxIn, 2)
	require.Equal(t, forfeitOutpointA,
		forfeitTxA.TxIn[0].PreviousOutPoint)

	forfeitTxidB, forfeitTxB := waitForSpendOnChainOrMempool(
		t, h, forfeitOutpointB, nil,
	)
	require.Len(t, forfeitTxB.TxIn, 2)
	require.Equal(t, forfeitOutpointB,
		forfeitTxB.TxIn[0].PreviousOutPoint)

	require.NotEqual(
		t, forfeitTxidA, forfeitTxidB,
		"each forfeited VTXO must have its own forfeit tx",
	)

	// Mine until both forfeits confirm. Each ancestor in the connector
	// chain has confirmed independently before its child could be
	// submitted, so the chain is already on-chain when we mine here.
	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, forfeitTxidA, 30*time.Second,
		),
	)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, forfeitTxidB, 30*time.Second,
		),
	)
	requireOnlyConfirmedSpender(t, h, forfeitOutpointA, forfeitTxidA)
	requireOnlyConfirmedSpender(t, h, forfeitOutpointB, forfeitTxidB)

	// Each forfeit penalty output must be swept by its own sweep tx.
	penaltyOutpointA := wire.OutPoint{Hash: forfeitTxidA, Index: 0}
	penaltyOutpointB := wire.OutPoint{Hash: forfeitTxidB, Index: 0}

	sweepTxidA, sweepTxA := waitForSpendOnChainOrMempool(
		t, h, penaltyOutpointA, nil,
	)
	require.Len(t, sweepTxA.TxIn, 1)
	require.Equal(t, penaltyOutpointA,
		sweepTxA.TxIn[0].PreviousOutPoint)

	sweepTxidB, sweepTxB := waitForSpendOnChainOrMempool(
		t, h, penaltyOutpointB, nil,
	)
	require.Len(t, sweepTxB.TxIn, 1)
	require.Equal(t, penaltyOutpointB,
		sweepTxB.TxIn[0].PreviousOutPoint)

	require.NotEqual(
		t, sweepTxidA, sweepTxidB,
		"each forfeit penalty must have its own sweep tx",
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, sweepTxidA, 30*time.Second,
		),
	)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, sweepTxidB, 30*time.Second,
		),
	)
	requireOnlyConfirmedSpender(t, h, penaltyOutpointA, sweepTxidA)
	requireOnlyConfirmedSpender(t, h, penaltyOutpointB, sweepTxidB)
}

// TestFraudResponseForfeitedVTXODeepConnectorTree verifies forfeit response
// planning traverses a multi-level connector tree before broadcasting the
// final stored forfeit transaction.
func TestFraudResponseForfeitedVTXODeepConnectorTree(t *testing.T) {
	runFraudResponseForfeitedVTXO(t, forfeitResponseOptions{
		forfeitCount: 3,
		mutator: func(cfg *darepo.Config) {
			cfg.Rounds.TreeRadix = 2
			cfg.Rounds.MaxConnectorsPerTree = 4
		},
		assertDeepConnector: true,
	})
}

type forfeitResponseOptions struct {
	forfeitCount                    int
	restartAfterVTXORevealedOffline bool
	restartAfterForfeitMinedOffline bool
	assertDeepConnector             bool
	mutator                         func(*darepo.Config)
}

func runFraudResponseForfeitedVTXO(t *testing.T, opts forfeitResponseOptions) {
	t.Helper()

	if opts.forfeitCount == 0 {
		opts.forfeitCount = 1
	}

	h := newUnrollHarnessWithMutator(t, opts.mutator)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 1)

	forfeitedVTXOs := make([]*daemonrpc.VTXO, 0, opts.forfeitCount)
	for i := 0; i < opts.forfeitCount; i++ {
		_, vtxo, _ := boardClientAndConfirmRound(
			t, h, alice.RPCClient, operatorInfo.MinConfirmations,
			100_000,
		)
		forfeitedVTXOs = append(forfeitedVTXOs, vtxo)
	}

	targetVTXO := forfeitedVTXOs[0]

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// Snapshot the recovery lineage. The harness-only
	// GetVTXOLineageTx path routes through
	// LocalProofAssembler.EnsureProofForHarness, which is explicitly
	// terminal-tolerant: Spent / Forfeited / Failed VTXOs still have a
	// walkable historical lineage. That is the entire reason the
	// harness accessor exists — fraud-response itests need to drive a
	// previous owner unilaterally broadcasting a VTXO they no longer
	// own — so the call here works equally well before or after the
	// refresh round marks the VTXO forfeited.
	lineageEntries, lineageOnChain := collectLineageEntries(
		t, ctx, alice, targetVTXO.Outpoint,
	)

	refreshOutpoints := make([]string, 0, len(forfeitedVTXOs))
	for _, vtxo := range forfeitedVTXOs {
		refreshOutpoints = append(refreshOutpoints, vtxo.Outpoint)
	}

	existingRoundIDs := snapshotClientRoundIDs(t, alice.RPCClient)
	refreshResp, err := alice.RPCClient.RefreshVTXOs(
		t.Context(), &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: refreshOutpoints,
				},
			},
		},
	)
	require.NoError(t, err, "RefreshVTXOs RPC failed")
	require.Equal(t, "queued", refreshResp.Status)
	for _, outpoint := range refreshOutpoints {
		require.Contains(t, refreshResp.QueuedOutpoints, outpoint)
	}

	for _, outpoint := range refreshOutpoints {
		waitForServerVTXOStatus(
			t, h, mustParseOutpoint(t, outpoint),
			"live",
		)
	}

	alice.TriggerRoundRegistration()

	refreshRound := waitForNewClientRoundState(
		t, alice.RPCClient, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, refreshRound.RoundId)
	waitForNamedClientRoundState(
		t, alice.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForPersistedClientRoundState(
		t, alice.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, refreshRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, refreshRound.RoundId, broadcastRound.TxId,
	)
	for _, outpoint := range refreshOutpoints {
		waitForVTXOStatusByOutpoint(
			t, alice.RPCClient, outpoint,
			daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
		)
	}

	if opts.restartAfterVTXORevealedOffline {
		h.RestartArkdDuring(func() {
			forceBroadcastCollectedLineageToOutpoint(
				t, h, lineageEntries, lineageOnChain,
				targetVTXO.Outpoint,
			)
		})
		waitForDaemonInfoReachable(t, alice.RPCClient)
	} else {
		forceBroadcastCollectedLineageToOutpoint(
			t, h, lineageEntries, lineageOnChain,
			targetVTXO.Outpoint,
		)
	}

	forfeitedOutpoint := mustParseOutpoint(t, targetVTXO.Outpoint)
	forfeitTxid, forfeitTx := waitForSpendOnChainOrMempool(
		t, h, forfeitedOutpoint, nil,
	)
	require.Len(t, forfeitTx.TxIn, 2)
	require.Equal(t, forfeitedOutpoint,
		forfeitTx.TxIn[0].PreviousOutPoint)

	if opts.assertDeepConnector {
		require.Len(t, forfeitTx.TxIn, 2)
		requireDeepConnectorAncestor(
			t, h, broadcastRound.TxId,
			forfeitTx.TxIn[1].PreviousOutPoint,
		)
	}

	if opts.restartAfterForfeitMinedOffline {
		h.RestartArkdDuring(func() {
			h.Generate(1)
			require.NoError(
				t, h.WaitForTxConfirmed(
					ctx, forfeitTxid, 30*time.Second,
				),
			)
		})
		waitForDaemonInfoReachable(t, alice.RPCClient)
	} else {
		h.Generate(1)
		require.NoError(
			t, h.WaitForTxConfirmed(
				ctx, forfeitTxid, 30*time.Second,
			),
		)
	}
	requireOnlyConfirmedSpender(t, h, forfeitedOutpoint, forfeitTxid)

	forfeitPenaltyOutpoint := wire.OutPoint{
		Hash:  forfeitTxid,
		Index: 0,
	}
	sweepTxid, sweepTx := waitForSpendOnChainOrMempool(
		t, h, forfeitPenaltyOutpoint, nil,
	)
	require.Len(t, sweepTx.TxIn, 1)
	require.Equal(
		t, forfeitPenaltyOutpoint, sweepTx.TxIn[0].PreviousOutPoint,
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, sweepTxid, 30*time.Second,
		),
	)
	requireOnlyConfirmedSpender(
		t, h, forfeitPenaltyOutpoint, sweepTxid,
	)

	walletUTXO := waitForOperatorWalletUTXO(
		t, h, sweepTxid.String(), sweepTx.TxOut[0].PkScript,
	)
	require.Equal(
		t, btcutil.Amount(forfeitTx.TxOut[0].Value), walletUTXO.Value,
	)
}

func requireDeepConnectorAncestor(t *testing.T, h *harness.ArkHarness,
	commitmentTxID string, connectorLeaf wire.OutPoint) {

	t.Helper()

	commitmentHash, err := chainhash.NewHashFromStr(commitmentTxID)
	require.NoError(t, err)

	depth := 0
	nextTxid := connectorLeaf.Hash
	for {
		tx := findConfirmedTxInRecentBlocks(t, h, nextTxid, 80)
		require.NotNil(
			t, tx, "connector ancestor %s not confirmed", nextTxid,
		)
		require.NotEmpty(t, tx.TxIn)

		depth++
		parentTxid := tx.TxIn[0].PreviousOutPoint.Hash
		if parentTxid == *commitmentHash {
			require.GreaterOrEqual(
				t, depth, 2,
				"connector response used a shallow tree",
			)

			return
		}

		require.LessOrEqual(
			t, depth, 20,
			"connector ancestor chain did not reach commitment tx",
		)
		nextTxid = parentTxid
	}
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
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
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
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointTxid, 30*time.Second,
		),
	)

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
	require.GreaterOrEqual(
		t, sweepTx.TxIn[0].Sequence, uint32(testVTXOExitDelay),
		"sweep sequence must satisfy CSV maturity",
	)
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
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, sweepTxid, 30*time.Second,
		),
	)
	waitForOperatorWalletUTXO(
		t, h, sweepTxid.String(), sweepTx.TxOut[0].PkScript,
	)
}

// TestFraudResponseMultihopOORRatchet covers the multihop OOR fraud response
// path. Setup is two chained OOR transfers: Alice -> Bob -> Carol. After
// Alice unilaterally exits her source VTXO, the operator must ratchet
// through every persisted checkpoint by reacting to each ark tx as it
// confirms on chain.
//
// Story:
//
//  1. Alice boards a round VTXO and OORs it to Bob (vtxo_b live on Bob).
//  2. Alice's source VTXO is forced on chain via Bob's recovery lineage
//     (vtxo_b is still live at this point, so Bob's proof assembler
//     accepts the request).
//  3. Server detects the spend and broadcasts checkpoint_ab. Confirm it.
//  4. Bob OORs vtxo_b to Carol (vtxo_c live on Carol). Server now holds
//     persisted checkpoint_bc + arktx_bc. Bob's vtxo_b transitions to
//     "spent" in the server DB.
//  5. The harness manually broadcasts arktx_ab using a server-side
//     accessor (Bob's local proof assembler can no longer build proofs
//     for vtxo_b once it is spent). This simulates the not-yet-
//     implemented client-side checkpoint-confirmed broadcaster.
//  6. Once arktx_ab confirms, the operator detects that the spender of
//     checkpoint_ab:0 has an output that is a known recipient VTXO
//     (vtxo_b) whose status is now "spent" because of step 4, classifies
//     that output as SpentLeaf, and broadcasts checkpoint_bc.
//  7. Confirm checkpoint_bc, broadcast arktx_bc the same way, confirm.
//  8. The operator should ratchet again, find that arktx_bc's output is
//     vtxo_c whose status is "live", and call MarkVTXOUnrolledByClient.
//  9. The test asserts vtxo_c has server-side status "unrolled_by_client"
//     and that the only confirmed spenders of either checkpoint:0 were
//     the legitimate ark txs (no operator timeout sweep).
//
// Hops are sequenced (broadcast-first, then second OOR) to keep Bob's
// vtxo_b "live" throughout the lineage walk in step 2. The server only
// observes the chain via its own batchwatcher, so the order in which the
// test broadcasts the source VTXO vs. the second OOR does not change the
// server-side multihop scenario being exercised in steps 6-8.
func TestFraudResponseMultihopOORRatchet(t *testing.T) {
	h := newUnrollHarness(t)

	// Operator wallet UTXOs are needed to fund the CPFP child packages
	// for every checkpoint broadcast (parent has zero fee).
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	carol := h.StartClientDaemon("carol")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 3)

	// Alice boards a 100k-sat round VTXO; this is the eventual on-chain
	// trigger.
	_, aliceLiveVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)

	// Hop 1: Alice OORs vtxo_a to Bob.
	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	bobReceiveResp, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-multihop-oor-A-to-B",
		},
	)
	require.NoError(t, err)
	bobPubkey, err := hex.DecodeString(bobReceiveResp.PubkeyXonlyHex)
	require.NoError(t, err)

	sendAmount := aliceLiveVTXO.AmountSat
	sendABResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: bobPubkey,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendABResp.Status)

	bobReceivedVTXO := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, bobReceivedVTXO)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	// Step 2: force Alice's source VTXO on chain via Bob's recovery
	// lineage. Bob's recovery DAG for vtxo_b reaches back through
	// arktx_ab and checkpoint_ab to alice's source VTXO and the round
	// tree above it. We do this BEFORE Bob's onward OOR so vtxo_b is
	// still "live" in Bob's local store and the proof assembler accepts
	// the request. The walker stops broadcasting once alice's vtxo_a
	// appears on chain, so checkpoint_ab and arktx_ab are not broadcast
	// by this helper.
	forceBroadcastLineageToOutpoint(
		t, h, bob, bobReceivedVTXO.Outpoint, aliceLiveVTXO.Outpoint,
	)

	// Step 3: server reacts to Alice's source VTXO on chain by
	// broadcasting checkpoint_ab. Confirm it.
	sourceOutpoint := mustParseOutpoint(t, aliceLiveVTXO.Outpoint)
	checkpointABTxid, checkpointABTx := waitForMempoolSpend(
		t, h, nil, sourceOutpoint,
	)
	require.Equal(
		t, sourceOutpoint, checkpointABTx.TxIn[0].PreviousOutPoint,
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointABTxid, 30*time.Second,
		),
	)
	checkpointABOutpoint := wire.OutPoint{
		Hash:  checkpointABTxid,
		Index: 0,
	}

	// Step 4: now that alice's source is on chain (and the operator
	// has already broadcast checkpoint_ab), do the second OOR hop:
	// Bob -> Carol. The OOR ceremony succeeds because vtxo_b is still
	// virtual (arktx_ab is not yet on chain) and the server tracks
	// it as live in its DB. After this, server-side vtxo_b transitions
	// to "spent" because Bob has now OORed it onward.
	carolLiveBefore := outpointSet(listLiveVTXOs(t, carol.RPCClient))

	carolReceiveResp, err := carol.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-multihop-oor-B-to-C",
		},
	)
	require.NoError(t, err)
	carolPubkey, err := hex.DecodeString(carolReceiveResp.PubkeyXonlyHex)
	require.NoError(t, err)

	sendBCResp, err := bob.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: carolPubkey,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendBCResp.Status)

	carolReceivedVTXO := waitForNewLiveVTXOWithAmount(
		t, carol.RPCClient, carolLiveBefore, sendAmount,
	)
	require.NotNil(t, carolReceivedVTXO)

	// Step 5: harness broadcasts arktx_ab on Bob's behalf. Read the
	// recovery-lineage entry for Bob's received outpoint via the
	// terminal-tolerant harness path: vtxo_b is now "spent" because Bob
	// has OORed it onward, but the harness assembler still walks the
	// lineage so the test can grab arktx_ab without going through the
	// production EnsureProof guard.
	arktxABEntry, err := bob.GetVTXOLineageTx(
		ctx, bobReceivedVTXO.Outpoint, bobReceivedVTXO.Outpoint,
	)
	require.NoError(t, err)
	require.NotNil(t, arktxABEntry.Tx)
	arktxABTxid := arktxABEntry.Tx.TxHash()

	_, err = h.ForceBroadcastLineageTx(
		ctx, arktxABEntry, btcutil.Amount(1000),
	)
	require.NoError(t, err)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arktxABTxid, 30*time.Second,
		),
	)

	// Step 6: server ratchets through arktx_ab's outputs, recognises
	// vtxo_b as a known recipient VTXO whose status is now "spent"
	// (because Bob has OORed it to Carol), and broadcasts checkpoint_bc.
	// handleCheckpointOutputSpend's classifyAndNotify dispatch + the
	// trackRecipientLeaf enrolment make this happen so the next-hop
	// follow-up checkpoint can be observed.
	bobOutpoint := mustParseOutpoint(t, bobReceivedVTXO.Outpoint)
	checkpointBCTxid, checkpointBCTx := waitForMempoolSpend(
		t, h, map[string]struct{}{
			checkpointABTxid.String(): {},
			arktxABTxid.String():      {},
		}, bobOutpoint,
	)
	require.Equal(t, bobOutpoint, checkpointBCTx.TxIn[0].PreviousOutPoint)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointBCTxid, 30*time.Second,
		),
	)
	checkpointBCOutpoint := wire.OutPoint{
		Hash:  checkpointBCTxid,
		Index: 0,
	}

	// Step 7: harness broadcasts arktx_bc on Carol's behalf via the
	// terminal-tolerant harness lineage walk. Carol's view stops at her
	// own checkpoint (Bob's session is private), but querying the
	// recipient outpoint itself returns arktx_bc, which is all the
	// broadcaster needs.
	arktxBCEntry, err := carol.GetVTXOLineageTx(
		ctx, carolReceivedVTXO.Outpoint, carolReceivedVTXO.Outpoint,
	)
	require.NoError(t, err)
	require.NotNil(t, arktxBCEntry.Tx)
	arktxBCTxid := arktxBCEntry.Tx.TxHash()

	_, err = h.ForceBroadcastLineageTx(
		ctx, arktxBCEntry, btcutil.Amount(1000),
	)
	require.NoError(t, err)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arktxBCTxid, 30*time.Second,
		),
	)

	// Step 8: server ratchets again, finds vtxo_c is "live" in its store,
	// and calls MarkVTXOUnrolledByClient. Poll until the transition
	// lands.
	carolOutpoint := mustParseOutpoint(t, carolReceivedVTXO.Outpoint)
	waitForServerVTXOStatus(t, h, carolOutpoint, "unrolled_by_client")

	// Final invariants: the legitimate ark txs are the only spenders of
	// either checkpoint:0; no operator timeout sweep ever confirmed.
	requireOnlyConfirmedSpender(
		t, h, checkpointABOutpoint, arktxABTxid,
	)
	requireOnlyConfirmedSpender(
		t, h, checkpointBCOutpoint, arktxBCTxid,
	)
}

// waitForServerVTXOStatus polls the operator's VTXO store until the row at
// outpoint reports the requested status. Used to assert server-side
// transitions like unrolled_by_client that are not exposed through client
// RPCs or the indexer.
func waitForServerVTXOStatus(t *testing.T, h *harness.ArkHarness,
	outpoint wire.OutPoint, want string) {

	t.Helper()

	var lastSeen string
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), 5*time.Second,
		)
		defer cancel()

		got, err := h.GetServerVTXOStatus(ctx, outpoint)
		if err != nil {
			return false
		}
		lastSeen = got

		return got == want
	}, 90*time.Second, 500*time.Millisecond,
		"vtxo %s never reached status %q (last seen %q)",
		outpoint, want, lastSeen)
}

// requireOnlyConfirmedSpender asserts that the only confirmed transaction
// spending outpoint within the last 50 blocks is wantTxid. Use this to
// prove that a fraud-response timeout sweep was never broadcast against a
// checkpoint output that the legitimate ark tx already consumed.
func requireOnlyConfirmedSpender(t *testing.T, h *harness.ArkHarness,
	outpoint wire.OutPoint, wantTxid chainhash.Hash) {

	t.Helper()

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)

	bestHeight := int64(h.Harness.BlockCount())
	startHeight := bestHeight - 50
	if startHeight < 0 {
		startHeight = 0
	}

	for height := startHeight; height <= bestHeight; height++ {
		hash, err := rpcClient.GetBlockHash(height)
		require.NoError(t, err)

		block, err := rpcClient.GetBlock(hash)
		require.NoError(t, err)

		for _, tx := range block.Transactions {
			for _, txIn := range tx.TxIn {
				if txIn.PreviousOutPoint != outpoint {
					continue
				}

				require.Equal(
					t, wantTxid, tx.TxHash(),
					"unexpected confirmed spender of %s",
					outpoint,
				)
			}
		}
	}
}

// waitForSpendBaselineSlack bounds how far below the helper's starting
// height the on-chain scan reaches. A fixed-window lookback (e.g. "last 6
// blocks") races the test's own block-mining cadence: when the spender
// confirms many blocks before the wait is invoked AND the test then mines
// more blocks before polling (e.g. between two sibling-checkpoint waits,
// while the recipient's CPFP retries push the chain forward), the spend
// ends up buried outside the window. Anchoring the lower bound to the
// helper's starting height minus this slack makes the helper invariant to
// whether the responder broadcast its checkpoints in the same block or
// staggered them across blocks, and to how aggressively the surrounding
// test loop mined while reaching the wait. Sized to comfortably cover the
// observed worst-case gap between a deferred-checkpoint confirmation and
// the subsequent sibling-checkpoint wait on lwwallet (~130 blocks), with
// margin for slower CI runners.
const waitForSpendBaselineSlack = 256

// waitForSpendTimeout bounds how long the helper polls for the spender.
// Sized above the worst-case observed local wall-clock between a responder
// broadcast and its CPFP-mined confirmation while leaving slack for slow CI
// runners that block on per-block GenerateAndWait time-cushion sleeps.
const waitForSpendTimeout = 5 * time.Minute

// waitForSpendOnChainOrMempool polls for a transaction that spends outpoint,
// looking in both the mempool and every block at-or-above a baseline height
// captured at invocation, mining one block per iteration to keep the chain
// moving until it is found. Mining inside the loop covers the case where the
// spender races ahead of the previous mined block: the responder sees a new
// block epoch, broadcasts, and the next `Generate(1)` confirms the spender
// before the test can sample the mempool. Anchoring the scan to a baseline
// (rather than a fixed N-block window relative to chain tip) keeps the
// helper robust against the spender confirming several blocks before the
// caller could invoke the wait — including the case where two sibling
// checkpoints share a deferred-broadcast deadline and confirm in the same
// block.
//
// If an optional expectedTxid is supplied, only a spender whose hash equals
// it satisfies the wait; racing noise spenders (e.g. an operator-side
// premature checkpoint timeout sweep that broadcasts before the recipient's
// ark tx) are ignored and the helper keeps polling until the intended
// spender materializes. Callers that cannot predict the txid up front (e.g.
// waiting for a checkpoint built fresh by the responder) pass no
// expectedTxid and accept any spender.
func waitForSpendOnChainOrMempool(t *testing.T, h *harness.ArkHarness,
	outpoint wire.OutPoint, knownTxIDs map[string]struct{},
	expectedTxid ...chainhash.Hash) (chainhash.Hash, *wire.MsgTx) {

	t.Helper()

	baselineHeight := int64(h.Harness.BlockCount()) -
		waitForSpendBaselineSlack
	if baselineHeight < 0 {
		baselineHeight = 0
	}

	matches := func(txid chainhash.Hash) bool {
		if len(expectedTxid) == 0 {
			return true
		}

		return txid == expectedTxid[0]
	}

	var (
		foundTxid chainhash.Hash
		foundTx   *wire.MsgTx
	)
	require.Eventually(t, func() bool {
		// Check the mempool first.
		txid, tx := findMempoolSpend(t, h, knownTxIDs, outpoint)
		if tx != nil && matches(txid) {
			foundTxid = txid
			foundTx = tx

			return true
		}

		// Mempool was empty (or only had a non-matching spender).
		// Check whether the spender already confirmed in any block at
		// or above the baseline.
		txid, tx = findSpendSinceHeight(
			t, h, outpoint, knownTxIDs, baselineHeight,
		)
		if tx != nil && matches(txid) {
			foundTxid = txid
			foundTx = tx

			return true
		}

		// No match on chain or in mempool; mine another block to give
		// batchwatcher a fresh BlockEpoch and try again.
		h.Generate(1)

		return false
	}, waitForSpendTimeout, 1*time.Second,
		"never observed tx spending %s (expected: %v)", outpoint,
		expectedTxid)

	return foundTxid, foundTx
}

// findSpendSinceHeight scans every block from fromHeight through the current
// best height for a transaction that spends outpoint. Returns the matched
// txid and parsed tx, or zero values if no spender is found.
func findSpendSinceHeight(t *testing.T, h *harness.ArkHarness,
	outpoint wire.OutPoint, knownTxIDs map[string]struct{},
	fromHeight int64) (chainhash.Hash, *wire.MsgTx) {

	t.Helper()

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)

	bestHeight := int64(h.Harness.BlockCount())

	if fromHeight < 0 {
		fromHeight = 0
	}

	for height := bestHeight; height >= fromHeight; height-- {
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

// findConfirmedTxInRecentBlocks scans recent blocks for txid.
func findConfirmedTxInRecentBlocks(t *testing.T, h *harness.ArkHarness,
	txid chainhash.Hash, lookback int) *wire.MsgTx {

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
		require.NoError(t, err)

		block, err := rpcClient.GetBlock(hash)
		require.NoError(t, err)

		for _, tx := range block.Transactions {
			if tx.TxHash() == txid {
				return tx
			}
		}
	}

	return nil
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

	forceBroadcastCollectedLineageToOutpoint(
		t, h, entries, onChain, targetOutpoint,
	)
}

// forceBroadcastCollectedLineageToOutpoint confirms cached lineage
// transactions until targetOutpoint is materialized on chain.
//
// Only the dependency closure of targetOutpoint is broadcast; sibling
// branches in the cached map (e.g. the second source of a multi-input target
// when only the first source has been requested) are intentionally left
// untouched so the test harness does not materialize more than the caller
// asked for. Iteration order within the closure is deterministic so test
// runs reproduce regardless of Go map iteration order.
func forceBroadcastCollectedLineageToOutpoint(t *testing.T,
	h *harness.ArkHarness,
	entries map[wire.OutPoint]*darepod.VTXOLineageEntry,
	onChain map[wire.OutPoint]bool, targetOutpoint string) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	target := mustParseOutpoint(t, targetOutpoint)

	required := requiredLineageOutpoints(entries, target)
	ops := sortedLineageOutpoints(required)

	for {
		if onChain[target] {
			return
		}

		progress := false
		for _, op := range ops {
			if onChain[op] {
				continue
			}
			entry := entries[op]
			if entry == nil {
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
			require.NoError(
				t, h.WaitForTxConfirmed(
					ctx, txid, 30*time.Second,
				),
			)

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

// requiredLineageOutpoints returns the dependency closure of target — every
// entry whose transaction must broadcast to make target land on chain.
// Entries reachable only via sibling branches are excluded.
func requiredLineageOutpoints(
	entries map[wire.OutPoint]*darepod.VTXOLineageEntry,
	target wire.OutPoint) map[wire.OutPoint]struct{} {

	required := make(map[wire.OutPoint]struct{})

	// Seed: every cached entry whose tx creates target. Typically one,
	// but the map may carry multiple outputs of the same tx; visiting
	// any of them schedules the underlying tx for broadcast.
	queue := make([]wire.OutPoint, 0, 1)
	for op, entry := range entries {
		if entry == nil || entry.Tx == nil {
			continue
		}
		if entry.Tx.TxHash() == target.Hash {
			queue = append(queue, op)
		}
	}

	// BFS back through ParentOutpoints. Off-graph parents (external
	// wallet UTXOs already on chain) drop out because they are not
	// keys in `entries`.
	for len(queue) > 0 {
		op := queue[0]
		queue = queue[1:]
		if _, ok := required[op]; ok {
			continue
		}
		required[op] = struct{}{}

		entry, ok := entries[op]
		if !ok {
			continue
		}
		for _, parent := range entry.ParentOutpoints {
			if _, ok := entries[parent]; !ok {
				continue
			}
			queue = append(queue, parent)
		}
	}

	return required
}

// sortedLineageOutpoints returns the keys of required in a deterministic
// order (txid hex, then output index) so a force-broadcast walk reproduces
// across runs regardless of Go map iteration order.
func sortedLineageOutpoints(
	required map[wire.OutPoint]struct{}) []wire.OutPoint {

	ops := make([]wire.OutPoint, 0, len(required))
	for op := range required {
		ops = append(ops, op)
	}

	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Hash != ops[j].Hash {
			return ops[i].Hash.String() < ops[j].Hash.String()
		}

		return ops[i].Index < ops[j].Index
	})

	return ops
}

// collectLineageEntries walks a VTXO lineage and returns every broadcastable
// entry plus the outpoints that are already on chain.
func collectLineageEntries(t *testing.T, ctx context.Context,
	client *harness.ClientDaemonHarness,
	targetOutpoint string) (map[wire.OutPoint]*darepod.VTXOLineageEntry,
	map[wire.OutPoint]bool) {

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
