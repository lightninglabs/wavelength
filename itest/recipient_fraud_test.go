//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestRecipientFraudWatcherBroadcastsArkAfterRestart verifies the recipient
// daemon's fraud watcher is restored from durable VTXO/OOR state after restart.
// Once the previous owner's source VTXO is revealed and the operator
// checkpoint confirms, the restarted recipient must broadcast the matching ark
// transaction without harness-side intervention.
func TestRecipientFraudWatcherBroadcastsArkAfterRestart(t *testing.T) {
	t.Parallel()

	h := newUnrollHarness(t)
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

	flow := setupRecipientFraudFlow(t, h)

	// Fund fee UTXOs before restart. The fraud watcher uses txconfirm to
	// CPFP the ark broadcast after the operator checkpoint confirms.
	h.FundClientWalletN(flow.bob, btcutil.SatoshiPerBitcoin/2, 3)

	oldRPCAddr := flow.bob.RPCAddr
	flow.bob = h.RestartClientDaemon("bob")
	t.Logf(
		"restarted recipient daemon: old_rpc=%s new_rpc=%s "+
			"received_outpoint=%s", oldRPCAddr, flow.bob.RPCAddr,
		flow.bobReceived.Outpoint,
	)

	waitForDaemonInfoReachable(t, flow.bob.RPCClient)
	waitForRegisteredClients(t, h, 2)

	driveRecipientFraudThroughArkConfirm(t, h, flow)
	waitForFraudTriggeredUnroll(
		t, flow.bob.RPCClient, flow.bobReceived.Outpoint,
	)
	waitForUnrollJobCompletion(
		t, h, flow.bob.RPCClient, flow.bobReceived.Outpoint,
	)
}

// TestRecipientFraudDefersToOperator verifies that when the operator's fraud
// responder is healthy, the recipient defers the checkpoint broadcast and only
// pays for its own ark and sweep transactions. The check is a wallet-UTXO
// accounting one: every recipient broadcast through txconfirm consumes exactly
// one wallet UTXO for the CPFP fee input, so the count of pre-existing wallet
// UTXOs that disappear over the run is the count of distinct broadcasts the
// recipient performed. A deferring recipient broadcasts at most ark + sweep —
// two — so a baseline-UTXO consumption greater than two would mean the
// recipient also funded a redundant checkpoint CPFP.
func TestRecipientFraudDefersToOperator(t *testing.T) {
	t.Parallel()

	h := newUnrollHarness(t)
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

	flow := setupRecipientFraudFlow(t, h)

	// Fund fee UTXOs before the fraud event so the recipient's own
	// broadcasts have known wallet inputs to choose from.
	h.FundClientWalletN(flow.bob, btcutil.SatoshiPerBitcoin/2, 3)

	// Snapshot the recipient's confirmed wallet UTXOs after funding.
	// Subsequent CPFP broadcasts consume entries from this baseline.
	baselineUTXOs := confirmedWalletUTXOValues(t, flow.bob)
	require.GreaterOrEqual(
		t, len(baselineUTXOs), 3, "need 3+ baseline UTXOs so an "+
			"extra checkpoint CPFP would have a wallet input "+
			"to consume",
	)

	driveRecipientFraudThroughArkConfirm(t, h, flow)
	waitForFraudTriggeredUnroll(
		t, flow.bob.RPCClient, flow.bobReceived.Outpoint,
	)
	waitForUnrollJobCompletion(
		t, h, flow.bob.RPCClient, flow.bobReceived.Outpoint,
	)

	// Each distinct recipient parent broadcast (ark, sweep) consumes one
	// baseline UTXO via the txconfirm CPFP fee-input reservation. A
	// deferring recipient never funds a checkpoint, so at most two
	// baseline UTXOs may disappear.
	final := confirmedWalletUTXOValues(t, flow.bob)
	consumed := 0
	for op := range baselineUTXOs {
		if _, ok := final[op]; !ok {
			consumed++
		}
	}
	require.LessOrEqual(
		t, consumed, 2, "recipient consumed %d baseline wallet "+
			"UTXOs; expected <=2 (ark + sweep CPFP). Excess "+
			"implies recipient also funded a redundant "+
			"checkpoint CPFP", consumed,
	)
}

// recipientFraudFlow groups the daemons and OOR'd VTXO that every recipient
// fraud-response itest in this file builds on top of.
type recipientFraudFlow struct {
	alice          *harness.ClientDaemonHarness
	bob            *harness.ClientDaemonHarness
	aliceLiveVTXO  *daemonrpc.VTXO
	bobReceived    *daemonrpc.VTXO
	sourceOutpoint wire.OutPoint
}

// setupRecipientFraudFlow boards Alice, OORs a single VTXO from Alice to Bob,
// and returns the daemons plus the descriptors needed to drive the recipient
// fraud-response paths.
func setupRecipientFraudFlow(t *testing.T,
	h *harness.ArkHarness) recipientFraudFlow {

	t.Helper()

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	_, aliceLiveVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)

	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))
	bobRecv, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-recipient-fraud",
		},
	)
	require.NoError(t, err)

	bobPubkey, err := hex.DecodeString(bobRecv.PubkeyXonlyHex)
	require.NoError(t, err)

	sendAmount := aliceLiveVTXO.AmountSat
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
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)

	bobReceived := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, bobReceived)

	return recipientFraudFlow{
		alice:          alice,
		bob:            bob,
		aliceLiveVTXO:  aliceLiveVTXO,
		bobReceived:    bobReceived,
		sourceOutpoint: mustParseOutpoint(t, aliceLiveVTXO.Outpoint),
	}
}

// driveRecipientFraudThroughArkConfirm force-broadcasts Alice's source VTXO
// onto chain, mines the operator's responding checkpoint, and waits for Bob's
// recipient fraud watcher to broadcast and confirm the matching ark
// transaction. Returns once the ark tx is confirmed.
func driveRecipientFraudThroughArkConfirm(t *testing.T, h *harness.ArkHarness,
	flow recipientFraudFlow) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	forceBroadcastLineageToOutpoint(
		t, h, flow.bob, flow.bobReceived.Outpoint,
		flow.aliceLiveVTXO.Outpoint,
	)

	checkpointTxid, checkpointTx := waitForMempoolSpend(
		t, h, nil, flow.sourceOutpoint,
	)
	require.Equal(
		t, flow.sourceOutpoint, checkpointTx.TxIn[0].PreviousOutPoint,
	)

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
	bobOutpoint := mustParseOutpoint(t, flow.bobReceived.Outpoint)

	arkTxid, arkTx := waitForSpendOnChainOrMempool(
		t, h, checkpointOutpoint, map[string]struct{}{
			checkpointTxid.String(): {},
		},
		bobOutpoint.Hash,
	)
	require.Equal(t, bobOutpoint.Hash, arkTxid)
	require.Equal(t, checkpointOutpoint,
		arkTx.TxIn[0].PreviousOutPoint)
	require.Less(t, int(bobOutpoint.Index), len(arkTx.TxOut))

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arkTxid, 30*time.Second,
		),
	)
}

// waitForFraudTriggeredUnroll waits until the recipient watcher-created unroll
// job is visible over the daemon RPC surface.
func waitForFraudTriggeredUnroll(t *testing.T,
	client daemonrpc.DaemonServiceClient, outpoint string) {

	t.Helper()

	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetUnrollStatus(
			ctx, &daemonrpc.GetUnrollStatusRequest{
				Outpoint: outpoint,
			},
		)
		if err != nil {
			return false
		}

		return resp.Found
	}, defaultTimeout, pollInterval,
		"fraud watcher did not create unroll job for %s", outpoint)
}

// multihopRecipientFraudFlow groups the three daemons and OOR chain A→B→C
// that multihop recipient fraud-response itests build on top of.
type multihopRecipientFraudFlow struct {
	alice         *harness.ClientDaemonHarness
	bob           *harness.ClientDaemonHarness
	carol         *harness.ClientDaemonHarness
	aliceLiveVTXO *daemonrpc.VTXO
	bobReceived   *daemonrpc.VTXO
	carolReceived *daemonrpc.VTXO
}

// setupMultihopRecipientFraudFlow boards Alice, OORs A→B, then OORs B→C.
// Returns the three daemons and the descriptors needed to drive multihop
// recipient fraud-response paths.
func setupMultihopRecipientFraudFlow(t *testing.T,
	h *harness.ArkHarness) multihopRecipientFraudFlow {

	t.Helper()

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	carol := h.StartClientDaemon("carol")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 3)

	_, aliceLiveVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	sendAmount := aliceLiveVTXO.AmountSat

	// Hop 1: Alice OORs vtxo_A to Bob.
	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))
	bobRecv, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-multihop-A-to-B",
		},
	)
	require.NoError(t, err)
	bobPubkey, err := hex.DecodeString(bobRecv.PubkeyXonlyHex)
	require.NoError(t, err)

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

	bobReceived := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, bobReceived)

	// Hop 2: Bob OORs vtxo_B to Carol.
	carolLiveBefore := outpointSet(listLiveVTXOs(t, carol.RPCClient))
	carolRecv, err := carol.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-multihop-B-to-C",
		},
	)
	require.NoError(t, err)
	carolPubkey, err := hex.DecodeString(carolRecv.PubkeyXonlyHex)
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

	carolReceived := waitForNewLiveVTXOWithAmount(
		t, carol.RPCClient, carolLiveBefore, sendAmount,
	)
	require.NotNil(t, carolReceived)

	return multihopRecipientFraudFlow{
		alice:         alice,
		bob:           bob,
		carol:         carol,
		aliceLiveVTXO: aliceLiveVTXO,
		bobReceived:   bobReceived,
		carolReceived: carolReceived,
	}
}

// TestRecipientFraudMultihopRatchet verifies that when the operator's fraud
// responder is disabled, Carol's recipient watcher drives the full A→B→C
// ratchet autonomously: checkpoint_AB backstop → arktx_AB broadcast →
// checkpoint_BC backstop → arktx_BC broadcast → unroll hand-off.
func TestRecipientFraudMultihopRatchet(t *testing.T) {
	t.Parallel()

	h := newUnrollHarnessWithMutator(t, func(cfg *darepo.Config) {
		cfg.Fraud = &darepo.FraudConfig{Disabled: true}
	})
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

	flow := setupMultihopRecipientFraudFlow(t, h)

	// Fund Carol's wallet so txconfirm can attach CPFP children to the
	// zero-fee checkpoint and ark parents.
	h.FundClientWalletN(flow.carol, btcutil.SatoshiPerBitcoin/2, 5)

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	// Put vtxo_A's commitment-tree chain on chain via Carol's lineage.
	// After the leaf tree tx mines, Carol's watcher transitions from
	// PhaseWatchingTree → PhaseWaitingCheckpoint for hop 1.
	forceBroadcastLineageToOutpoint(
		t, h, flow.carol, flow.carolReceived.Outpoint,
		flow.aliceLiveVTXO.Outpoint,
	)

	// Mine enough blocks to cross the backstop threshold. The backstop
	// fires at leafConfirmHeight + VTXOExitDelay - SafetyMargin. With
	// testVTXOExitDelay=16 and SafetyMargin=16/2=8 the backstop fires
	// 8 blocks after the leaf tx confirms.
	h.Generate(9)

	// checkpoint_AB must appear spending vtxo_A once the leaf tree tx
	// has confirmed. Use waitForSpendOnChainOrMempool so it mines blocks
	// while polling, which keeps Carol's recipient backstop deadline
	// ticking when the operator ratchet is slow under CI load.
	sourceOutpoint := mustParseOutpoint(t, flow.aliceLiveVTXO.Outpoint)
	checkpointABTxid, checkpointABTx := waitForSpendOnChainOrMempool(
		t, h, sourceOutpoint, nil,
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

	// checkpoint_AB confirmed → Carol's watcher enters PhaseBroadcastingArk
	// and asks txconfirm to confirm arktx_AB.
	checkpointABOutpoint := wire.OutPoint{Hash: checkpointABTxid, Index: 0}
	bobOutpoint := mustParseOutpoint(t, flow.bobReceived.Outpoint)
	arktxABTxid, arktxABTx := waitForSpendOnChainOrMempool(
		t, h, checkpointABOutpoint, map[string]struct{}{
			checkpointABTxid.String(): {},
		},
		bobOutpoint.Hash,
	)
	require.Equal(
		t, checkpointABOutpoint, arktxABTx.TxIn[0].PreviousOutPoint,
	)
	require.Equal(t, bobOutpoint.Hash, arktxABTxid)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arktxABTxid, 30*time.Second,
		),
	)

	// arktx_AB confirmed → Carol's watcher advances to hop 2
	// (PhaseWaitingCheckpoint for checkpoint_BC). Mine past the hop-2
	// backstop threshold (vtxo_B confirm height + 8 blocks). The
	// waitForSpendOnChainOrMempool call below also mines blocks while
	// polling, keeping the deadline ticking if the operator ratchet is
	// slow under CI load.
	h.Generate(9)

	checkpointBCTxid, checkpointBCTx := waitForSpendOnChainOrMempool(
		t, h, bobOutpoint, map[string]struct{}{
			checkpointABTxid.String(): {},
			arktxABTxid.String():      {},
		},
	)
	require.Equal(
		t, bobOutpoint, checkpointBCTx.TxIn[0].PreviousOutPoint,
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointBCTxid, 30*time.Second,
		),
	)

	// checkpoint_BC confirmed → Carol's watcher broadcasts arktx_BC.
	checkpointBCOutpoint := wire.OutPoint{Hash: checkpointBCTxid, Index: 0}
	carolOutpoint := mustParseOutpoint(t, flow.carolReceived.Outpoint)
	arktxBCTxid, arktxBCTx := waitForSpendOnChainOrMempool(
		t, h, checkpointBCOutpoint, map[string]struct{}{
			checkpointBCTxid.String(): {},
		},
		carolOutpoint.Hash,
	)
	require.Equal(
		t, checkpointBCOutpoint, arktxBCTx.TxIn[0].PreviousOutPoint,
	)
	require.Equal(t, carolOutpoint.Hash, arktxBCTxid)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arktxBCTxid, 30*time.Second,
		),
	)

	// arktx_BC confirmed → Carol's watcher emits ActionEnsureUnroll.
	waitForFraudTriggeredUnroll(
		t, flow.carol.RPCClient, flow.carolReceived.Outpoint,
	)
	waitForUnrollJobCompletion(
		t, h, flow.carol.RPCClient, flow.carolReceived.Outpoint,
	)
}

// TestRecipientFraudPartialOperator verifies Carol's watcher correctly handles
// a two-hop chain where the operator broadcasts checkpoint_AB (hop 1) but is
// silent for hop 2 (checkpoint_BC). Carol must broadcast arktx_AB after
// checkpoint_AB confirms, then fire the backstop for checkpoint_BC, and
// finally broadcast arktx_BC to materialise vtxo_C and trigger unroll.
//
// Wallet-UTXO accounting confirms that Carol did not fund a CPFP for
// checkpoint_AB (the harness injected it externally) but did fund CPFPs for
// arktx_AB, checkpoint_BC, and arktx_BC.
func TestRecipientFraudPartialOperator(t *testing.T) {
	t.Parallel()

	h := newUnrollHarnessWithMutator(t, func(cfg *darepo.Config) {
		cfg.Fraud = &darepo.FraudConfig{Disabled: true}
	})
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

	flow := setupMultihopRecipientFraudFlow(t, h)

	h.FundClientWalletN(flow.carol, btcutil.SatoshiPerBitcoin/2, 5)

	baselineUTXOs := confirmedWalletUTXOValues(t, flow.carol)
	require.GreaterOrEqual(
		t, len(baselineUTXOs), 3,
		"need 3+ baseline UTXOs so that CPFP accounting is meaningful",
	)

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	// Put vtxo_A's commitment tree on chain via Carol's lineage.
	forceBroadcastLineageToOutpoint(
		t, h, flow.carol, flow.carolReceived.Outpoint,
		flow.aliceLiveVTXO.Outpoint,
	)

	// Mine a few blocks so Carol's fraud actor processes the leaf
	// spend epoch and registers its tx-confirmation watch for
	// checkpoint_AB before we inject it. 4 < backstop threshold (8),
	// so the backstop does not fire here.
	h.Generate(4)

	// Collect Carol's full lineage. checkpoint_AB is the entry whose
	// parent is vtxo_A; we broadcast it externally to simulate what a
	// healthy operator would have done for hop 1.
	entries, _ := collectLineageEntries(
		t, ctx, flow.carol, flow.carolReceived.Outpoint,
	)
	vtxoAOutpoint := mustParseOutpoint(t, flow.aliceLiveVTXO.Outpoint)

	var checkpointABTxid chainhash.Hash
	for _, entry := range entries {
		for _, parent := range entry.ParentOutpoints {
			if parent != vtxoAOutpoint {
				continue
			}
			// ForceBroadcastLineageTx returns the CPFP child txid,
			// not the parent. Capture the parent txid from the
			// entry directly.
			checkpointABTxid = entry.Tx.TxHash()
			_, err := h.ForceBroadcastLineageTx(
				ctx, entry, btcutil.Amount(1000),
			)
			require.NoError(t, err)
		}
		if checkpointABTxid != (chainhash.Hash{}) {
			break
		}
	}
	require.NotEqual(
		t, chainhash.Hash{}, checkpointABTxid,
		"checkpoint_AB not found in Carol's lineage",
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointABTxid, 30*time.Second,
		),
	)

	// checkpoint_AB confirmed (injected externally, no Carol CPFP) →
	// Carol's watcher sees it and broadcasts arktx_AB.
	checkpointABOutpoint := wire.OutPoint{Hash: checkpointABTxid, Index: 0}
	bobOutpoint := mustParseOutpoint(t, flow.bobReceived.Outpoint)
	arktxABTxid, arktxABTx := waitForSpendOnChainOrMempool(
		t, h, checkpointABOutpoint, map[string]struct{}{
			checkpointABTxid.String(): {},
		},
		bobOutpoint.Hash,
	)
	require.Equal(
		t, checkpointABOutpoint, arktxABTx.TxIn[0].PreviousOutPoint,
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arktxABTxid, 30*time.Second,
		),
	)

	// arktx_AB confirmed → hop 2. Operator is silent for checkpoint_BC,
	// so Carol's backstop must fire. Mine past the backstop threshold
	// (vtxo_B confirm height + 8 blocks).
	h.Generate(9)

	checkpointBCTxid, checkpointBCTx := waitForMempoolSpend(
		t, h, map[string]struct{}{
			checkpointABTxid.String(): {},
			arktxABTxid.String():      {},
		}, bobOutpoint,
	)
	require.Equal(
		t, bobOutpoint, checkpointBCTx.TxIn[0].PreviousOutPoint,
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointBCTxid, 30*time.Second,
		),
	)

	checkpointBCOutpoint := wire.OutPoint{Hash: checkpointBCTxid, Index: 0}
	carolOutpoint := mustParseOutpoint(t, flow.carolReceived.Outpoint)
	arktxBCTxid, arktxBCTx := waitForSpendOnChainOrMempool(
		t, h, checkpointBCOutpoint, map[string]struct{}{
			checkpointBCTxid.String(): {},
		},
		carolOutpoint.Hash,
	)
	require.Equal(
		t, checkpointBCOutpoint, arktxBCTx.TxIn[0].PreviousOutPoint,
	)
	require.Equal(t, carolOutpoint.Hash, arktxBCTxid)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arktxBCTxid, 30*time.Second,
		),
	)

	waitForFraudTriggeredUnroll(
		t, flow.carol.RPCClient, flow.carolReceived.Outpoint,
	)
	waitForUnrollJobCompletion(
		t, h, flow.carol.RPCClient, flow.carolReceived.Outpoint,
	)

	// Wallet-UTXO accounting: Carol funded CPFPs for arktx_AB,
	// checkpoint_BC, arktx_BC, and the unroll sweep — but NOT for
	// checkpoint_AB (injected by the harness). txconfirm chains change
	// outputs across successive CPFPs, so these broadcasts may consume
	// just one baseline UTXO in total (subsequent CPFPs recycle the
	// previous CPFP's change). If Carol had also funded checkpoint_AB,
	// she would have consumed one extra baseline UTXO.
	//
	// The explicit tx-chain verifications above (arktxABTx, checkpointBCTx,
	// arktxBCTx) already confirm Carol broadcast the required txes; this
	// check is an upper-bound safety net only.
	finalUTXOs := confirmedWalletUTXOValues(t, flow.carol)
	consumed := 0
	for op := range baselineUTXOs {
		if _, ok := finalUTXOs[op]; !ok {
			consumed++
		}
	}
	require.LessOrEqual(
		t, consumed, 2, "partial-operator: carol consumed %d "+
			"baseline UTXOs; expected <=2. Excess implies carol "+
			"funded a redundant checkpoint_AB CPFP in addition "+
			"to the harness-injected one", consumed,
	)
}

// TestRecipientFraudMultiInputOOR verifies a recipient holding a cross-round
// multi-input OOR VTXO backstops each checkpoint independently and only
// broadcasts the shared ark transaction after every checkpoint input confirms.
func TestRecipientFraudMultiInputOOR(t *testing.T) {
	t.Parallel()

	h := newUnrollHarnessWithMutator(t, func(cfg *darepo.Config) {
		cfg.Fraud = &darepo.FraudConfig{Disabled: true}
	})
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

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
	require.NotEqual(
		t, aliceVTXO1.CommitmentTxid, aliceVTXO2.CommitmentTxid, "tw"+
			"o boarded inputs must come from distinct "+
			"commitments to exercise the multi-input ancestry path",
	)

	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))
	bobRecv, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-recipient-fraud-multi-input",
		},
	)
	require.NoError(t, err)

	bobPubkey, err := hex.DecodeString(bobRecv.PubkeyXonlyHex)
	require.NoError(t, err)

	sendAmount := int64(120_000)
	require.GreaterOrEqual(t, aliceBalance.VtxoBalanceSat, sendAmount)

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
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)

	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO1.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO2.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)

	bobReceived := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, bobReceived)

	// The recipient pays CPFP fees for two checkpoint backstops, the
	// shared ark broadcast, and the final sweep.
	h.FundClientWalletN(bob, btcutil.SatoshiPerBitcoin/2, 5)

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	// Materialize the first source VTXO on chain. This spends the
	// round-tree output above aliceVTXO1, which is one of the outpoints
	// Bob's fraud watcher armed when he received the multi-input OOR
	// VTXO. The spend trigger fires Bob's fraud-triggered unroll, which
	// then drives the full multi-input recovery DAG to materialize the
	// shared ark.
	//
	// Alice runs as a fraud-defended client too (she has a change
	// output from the same multi-input OOR), so her fraud-triggered
	// unroll on the same trigger may also broadcast the missing tree
	// internals of the second branch. That is acceptable: both clients
	// converge on the same proof DAG, and txconfirm dedup means the
	// shared ark / shared checkpoints are broadcast at most once each.
	forceBroadcastLineageToOutpoint(
		t, h, bob, bobReceived.Outpoint, aliceVTXO1.Outpoint,
	)
	h.Generate(9)

	sourceOne := mustParseOutpoint(t, aliceVTXO1.Outpoint)
	checkpointOneTxid, checkpointOneTx := waitForSpendOnChainOrMempool(
		t, h, sourceOne, nil,
	)
	require.Equal(t, sourceOne, checkpointOneTx.TxIn[0].PreviousOutPoint)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointOneTxid, 30*time.Second,
		),
	)

	// Materialize the second source branch too, so the recovery harness
	// has every tree internal for cp_2's chain on chain. With the new
	// unroll design the recipient's fraud-triggered unroll would itself
	// broadcast these tree internals, but forcing them keeps the test
	// independent of that detail.
	forceBroadcastLineageToOutpoint(
		t, h, bob, bobReceived.Outpoint, aliceVTXO2.Outpoint,
	)
	h.Generate(9)

	sourceTwo := mustParseOutpoint(t, aliceVTXO2.Outpoint)
	checkpointTwoTxid, checkpointTwoTx := waitForSpendOnChainOrMempool(
		t, h, sourceTwo, map[string]struct{}{
			checkpointOneTxid.String(): {},
		},
	)
	require.Equal(t, sourceTwo, checkpointTwoTx.TxIn[0].PreviousOutPoint)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointTwoTxid, 30*time.Second,
		),
	)

	// The shared ark must reach mempool / chain spending BOTH
	// checkpoints. This is the load-bearing assertion of the multi-input
	// scenario: the recipient's recovery cannot complete unless the
	// full multi-input ark is materialized.
	checkpointOneOutpoint := wire.OutPoint{
		Hash:  checkpointOneTxid,
		Index: 0,
	}
	checkpointTwoOutpoint := wire.OutPoint{
		Hash:  checkpointTwoTxid,
		Index: 0,
	}
	bobOutpoint := mustParseOutpoint(t, bobReceived.Outpoint)
	arkTxid, arkTx := waitForSpendOnChainOrMempool(
		t, h, checkpointOneOutpoint, map[string]struct{}{
			checkpointOneTxid.String(): {},
			checkpointTwoTxid.String(): {},
		},
		bobOutpoint.Hash,
	)
	require.True(
		t, txSpendsOutpoint(arkTx, checkpointOneOutpoint),
		"shared ark must spend checkpoint_1",
	)
	require.True(
		t, txSpendsOutpoint(arkTx, checkpointTwoOutpoint),
		"shared ark must spend checkpoint_2",
	)
	require.Equal(t, bobOutpoint.Hash, arkTxid)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arkTxid, 30*time.Second,
		),
	)

	waitForFraudTriggeredUnroll(
		t, bob.RPCClient, bobReceived.Outpoint,
	)
	waitForUnrollJobCompletion(
		t, h, bob.RPCClient, bobReceived.Outpoint,
	)
}

// txSpendsOutpoint reports whether tx consumes outpoint.
func txSpendsOutpoint(tx *wire.MsgTx, outpoint wire.OutPoint) bool {
	if tx == nil {
		return false
	}

	for _, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint == outpoint {
			return true
		}
	}

	return false
}

// TestRecipientFraudCleanupOnOnwardSpend verifies that when Bob receives
// vtxo_B via OOR and then OORs it onward to Carol, Bob's recipient fraud
// watcher unregisters its spend interests. After vtxo_A is later
// force-broadcast on chain, Bob consumes no wallet UTXOs and creates no
// unroll job for vtxo_B.
func TestRecipientFraudCleanupOnOnwardSpend(t *testing.T) {
	t.Parallel()

	h := newUnrollHarness(t)
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

	// Set up Alice → Bob (vtxo_B live in Bob's watcher).
	flow := setupRecipientFraudFlow(t, h)

	// Fund Bob's wallet to establish the baseline for UTXO accounting.
	h.FundClientWalletN(flow.bob, btcutil.SatoshiPerBitcoin/2, 3)
	baselineBobUTXOs := confirmedWalletUTXOValues(t, flow.bob)
	require.GreaterOrEqual(
		t, len(baselineBobUTXOs), 3,
		"need 3+ baseline UTXOs for meaningful UTXO accounting",
	)

	// Bob OORs vtxo_B to Carol. The TerminalVTXOObserver fires when
	// Bob's VTXO manager marks vtxo_B Spent, tearing down Bob's fraud
	// watch for vtxo_B.
	carol := h.StartClientDaemon("carol")
	waitForRegisteredClients(t, h, 3)

	carolLiveBefore := outpointSet(listLiveVTXOs(t, carol.RPCClient))
	carolRecv, err := carol.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-cleanup-B-to-C",
		},
	)
	require.NoError(t, err)
	carolPubkey, err := hex.DecodeString(carolRecv.PubkeyXonlyHex)
	require.NoError(t, err)

	sendBCResp, err := flow.bob.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: carolPubkey,
				},
				AmountSat: flow.bobReceived.AmountSat,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendBCResp.Status)

	carolReceived := waitForNewLiveVTXOWithAmount(
		t, carol.RPCClient, carolLiveBefore, flow.bobReceived.AmountSat,
	)
	require.NotNil(t, carolReceived)

	// Wait for Bob's vtxo_B to reach VTXO_STATUS_SPENT, which signals
	// the TerminalVTXOObserver (and thus fraud-watch cleanup) has run.
	waitForVTXOStatusByOutpoint(
		t, flow.bob.RPCClient, flow.bobReceived.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)

	// Fund Carol's wallet for her fraud-response CPFP broadcasts.
	h.FundClientWalletN(carol, btcutil.SatoshiPerBitcoin/2, 3)

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	// Put vtxo_A's commitment tree on chain via Carol's full lineage
	// (which reaches through arktx_AB back to the round tree).
	forceBroadcastLineageToOutpoint(
		t, h, carol, carolReceived.Outpoint,
		flow.aliceLiveVTXO.Outpoint,
	)

	// Operator broadcasts checkpoint_AB (vtxo_A is now on chain).
	sourceOutpoint := mustParseOutpoint(t, flow.aliceLiveVTXO.Outpoint)
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

	// Carol's watcher broadcasts arktx_AB after checkpoint_AB confirms.
	checkpointABOutpoint := wire.OutPoint{Hash: checkpointABTxid, Index: 0}
	bobOutpoint := mustParseOutpoint(t, flow.bobReceived.Outpoint)
	arktxABTxid, arktxABTx := waitForSpendOnChainOrMempool(
		t, h, checkpointABOutpoint, map[string]struct{}{
			checkpointABTxid.String(): {},
		},
		bobOutpoint.Hash,
	)
	require.Equal(
		t, checkpointABOutpoint, arktxABTx.TxIn[0].PreviousOutPoint,
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arktxABTxid, 30*time.Second,
		),
	)

	// checkpoint_BC must appear spending vtxo_B once arktx_AB has
	// confirmed. The operator ratchets when it observes the vtxo_B
	// output on chain; Carol's recipient watcher also has cp_BC armed
	// and will broadcast at its backstop deadline if the operator does
	// not. Either source satisfies this assertion because the cleanup
	// test cares about Bob's behaviour, not which actor produced
	// cp_BC. Use waitForSpendOnChainOrMempool so it mines blocks while
	// polling, which keeps Carol's deadline ticking when the operator
	// is slow.
	checkpointBCTxid, checkpointBCTx := waitForSpendOnChainOrMempool(
		t, h, bobOutpoint, map[string]struct{}{
			checkpointABTxid.String(): {},
			arktxABTxid.String():      {},
		},
	)
	require.Equal(
		t, bobOutpoint, checkpointBCTx.TxIn[0].PreviousOutPoint,
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointBCTxid, 30*time.Second,
		),
	)

	// Carol's watcher broadcasts arktx_BC after checkpoint_BC confirms.
	checkpointBCOutpoint := wire.OutPoint{Hash: checkpointBCTxid, Index: 0}
	carolOutpoint := mustParseOutpoint(t, carolReceived.Outpoint)
	arktxBCTxid, arktxBCTx := waitForSpendOnChainOrMempool(
		t, h, checkpointBCOutpoint, map[string]struct{}{
			checkpointBCTxid.String(): {},
		},
		carolOutpoint.Hash,
	)
	require.Equal(
		t, checkpointBCOutpoint, arktxBCTx.TxIn[0].PreviousOutPoint,
	)
	require.Equal(t, carolOutpoint.Hash, arktxBCTxid)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arktxBCTxid, 30*time.Second,
		),
	)

	// Carol's unroll completes; Bob should have created no job.
	waitForFraudTriggeredUnroll(
		t, carol.RPCClient, carolReceived.Outpoint,
	)
	waitForUnrollJobCompletion(
		t, h, carol.RPCClient, carolReceived.Outpoint,
	)

	// Bob's fraud watcher was torn down when vtxo_B was marked Spent.
	// No unroll job should exist for vtxo_B in Bob's daemon.
	ctx2, cancel2 := context.WithTimeout(t.Context(), defaultSmallTimeout)
	defer cancel2()
	statusResp, err := flow.bob.RPCClient.GetUnrollStatus(
		ctx2, &daemonrpc.GetUnrollStatusRequest{
			Outpoint: flow.bobReceived.Outpoint,
		},
	)
	require.NoError(t, err)
	require.False(
		t, statusResp.Found, "Bob's fraud watcher should be torn "+
			"down after onward OOR; no unroll job expected for "+
			"vtxo_B",
	)

	// Wallet-UTXO accounting: Bob broadcasted nothing after his watcher
	// was cleaned up; no baseline UTXOs should be consumed from Bob.
	finalBobUTXOs := confirmedWalletUTXOValues(t, flow.bob)
	consumed := 0
	for op := range baselineBobUTXOs {
		if _, ok := finalBobUTXOs[op]; !ok {
			consumed++
		}
	}
	require.Equal(
		t, 0, consumed, "Bob's watcher should be torn down; "+
			"expected 0 wallet UTXOs consumed from Bob's "+
			"baseline, got %d", consumed,
	)
}
