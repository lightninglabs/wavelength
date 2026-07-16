//go:build systest

package systest

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/stretchr/testify/require"
)

// txConfirmSystestEventTimeout is the per-step deadline used by the
// txconfirm reorg systest. Generous because the chain-notification
// pipeline runs over gRPC to a real lnd instance plus this test waits
// for txconfirm's tracked-tx FSM to forward each event.
const txConfirmSystestEventTimeout = 30 * time.Second

// recordingNotificationRef captures every Notification delivered to a
// txconfirm subscriber so the test can assert on the order and shape
// of the lifecycle without racing concurrent delivery.
type recordingNotificationRef struct {
	id   string
	msgs chan txconfirm.Notification
}

func newRecordingNotificationRef(id string) *recordingNotificationRef {
	return &recordingNotificationRef{
		id:   id,
		msgs: make(chan txconfirm.Notification, 16),
	}
}

// ID returns the subscriber identifier.
func (r *recordingNotificationRef) ID() string {
	return r.id
}

// Tell records the inbound notification on the channel for the test to
// consume.
func (r *recordingNotificationRef) Tell(_ context.Context,
	msg txconfirm.Notification) error {

	r.msgs <- msg

	return nil
}

// await pulls the next notification or fails the test on timeout.
func (r *recordingNotificationRef) await(t *testing.T) txconfirm.Notification {
	t.Helper()

	select {
	case msg := <-r.msgs:
		return msg

	case <-time.After(txConfirmSystestEventTimeout):
		t.Fatalf("timeout waiting for txconfirm notification (%s)",
			txConfirmSystestEventTimeout)

		return nil
	}
}

// TestTxConfirmReorgRoundTrip drives a real bitcoind reorg through
// the full txconfirm pipeline:
//
//	lnd chainntnfs (in-process)
//	  -> lndclient gRPC (WithReOrgChan)
//	    -> chainbackends.LndClientChainNotifier (bridge)
//	      -> chainbackends.LNDBackend (multi-shot forwarder)
//	        -> chainsource.ConfActor (reorg-aware mode + finality synth)
//	          -> txconfirm.TxBroadcasterActor (tracked-tx FSM)
//	            -> recording subscriber
//
// This is the systest-level oracle for the layer that unroll consumes.
// The chainsource-level systest (TestChainSourceConfReorgRoundTrip)
// proves the chain-event plumbing; this one proves the tracked-tx FSM
// transitions Confirmed -> AwaitingConfirmation -> Confirmed correctly
// on real reorgs and that subscribers see TxConfirmed -> TxReorged ->
// TxConfirmed in order, with TxFinalized arriving once the
// height-based safety depth is reached.
//
// Full daemon-level end-to-end coverage (the VTXOUnrollActor walking
// a real proof through a real wallet under a real reorg) belongs in
// itest; the unroll FSM's reducer behavior on these events is already
// covered by unit tests in unroll/reorg_safety_test.go.
func TestTxConfirmReorgRoundTrip(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t)
	ctx := h.Context()

	chainSource := h.NewChainSourceActor()

	// Spawn a txconfirm actor over the real chainsource. Wallet is
	// nil because the faucet tx is non-anchor and we never trigger
	// CPFP fee-input selection in this test.
	txconfBehavior := txconfirm.NewTxBroadcasterActor(txconfirm.Config{
		ChainSource: chainSource,
	})
	txconfInstance := actor.NewActor(actor.ActorConfig[
		txconfirm.Msg, txconfirm.Resp,
	]{
		ID:          "txconfirm-systest",
		Behavior:    txconfBehavior,
		MailboxSize: 64,
	})
	txconfBehavior.SetSelfRef(txconfInstance.TellRef())
	txconfInstance.Start()
	t.Cleanup(txconfInstance.Stop)

	// Synthetic watched address: deterministic P2WPKH derived from
	// the test name. We faucet to it so we have a known txid and
	// pkScript for txconfirm to track. The address is never spent.
	pubKeyHash := sha256.Sum256([]byte(t.Name()))
	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		pubKeyHash[:20], &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err, "build synthetic P2WPKH address")
	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err, "derive pkScript for synthetic address")

	heightHint := h.Harness.BlockCount()

	// txconfirm's CPFP broadcaster enforces v3/TRUC at the version
	// gate, and the standard bitcoind faucet returns v2 txs. Build a
	// v3 tx that pays our synthetic address from bitcoind's wallet,
	// sign it, but do NOT broadcast — txconfirm will handle the
	// first broadcast on EnsureConfirmedReq.
	signedTx := h.Harness.SignedV3Tx(
		pkScript, btcutil.Amount(btcutil.SatoshiPerBitcoin/100),
	)
	txidVal := signedTx.TxHash()
	txid := &txidVal
	t.Logf("constructed v3 tx: txid=%s", txid)

	subscriber := newRecordingNotificationRef("txconfirm-sub")
	var subRef actor.TellOnlyRef[txconfirm.Notification] = subscriber

	// Register the EnsureConfirmedReq BEFORE mining so we exercise
	// the live-detection path and the tracked-tx FSM walks the full
	// Broadcasting -> AwaitingConfirmation -> Confirmed transitions.
	ensureResp, err := txconfInstance.Ref().Ask(
		ctx, &txconfirm.EnsureConfirmedReq{
			Tx:                   signedTx,
			ConfirmationPkScript: pkScript,
			Label:                "systest-reorg",
			HeightHint:           heightHint,
			TargetConfs:          1,
			Subscriber:           subRef,
		},
	).Await(ctx).Unpack()
	require.NoError(t, err, "EnsureConfirmedReq failed")
	require.IsType(t, &txconfirm.EnsureConfirmedResp{}, ensureResp)

	// 1. Mine the block that confirms the faucet tx.
	originalBlocks := h.Harness.Generate(1)
	require.Len(t, originalBlocks, 1)
	originalBlock := originalBlocks[0]

	// 2. Expect TxConfirmed.
	first := subscriber.await(t)
	firstConfirmed, ok := first.(*txconfirm.TxConfirmed)
	require.True(
		t, ok, "first notification must be TxConfirmed, got %T", first,
	)
	require.Equal(t, *txid, firstConfirmed.Txid)
	require.Equal(
		t, int32(originalBlock.Height), firstConfirmed.BlockHeight,
		"first conf block height should match the mined block",
	)
	t.Logf(
		"first TxConfirmed: txid=%s height=%d", firstConfirmed.Txid,
		firstConfirmed.BlockHeight,
	)

	// 3. Reorg the conf block out, mine a strictly longer
	// replacement branch, wait for lnd to chain-sync.
	reorg := h.Harness.Reorg(1, 2)
	require.Equal(
		t, originalBlock.Hash, reorg.Disconnected[0].Hash,
		"the reorg should have disconnected the conf block",
	)
	require.Len(t, reorg.Connected, 2)
	t.Logf(
		"reorg: disconnected=%d connected=%d fork=%d",
		len(reorg.Disconnected), len(reorg.Connected),
		reorg.ForkPoint.Height,
	)

	// 4. Expect TxReorged.
	second := subscriber.await(t)
	reorgedMsg, ok := second.(*txconfirm.TxReorged)
	require.True(
		t, ok, "second notification must be TxReorged, got %T", second,
	)
	require.Equal(t, *txid, reorgedMsg.Txid)
	t.Logf("TxReorged: txid=%s", reorgedMsg.Txid)

	// 5. Expect a fresh TxConfirmed on the replacement chain. The
	// faucet tx stays in mempool across the invalidate so it
	// re-confirms in the first new block.
	third := subscriber.await(t)
	secondConfirmed, ok := third.(*txconfirm.TxConfirmed)
	require.True(
		t, ok, "third notification must be TxConfirmed, got %T", third,
	)
	require.Equal(t, *txid, secondConfirmed.Txid)
	t.Logf(
		"second TxConfirmed: txid=%s height=%d", secondConfirmed.Txid,
		secondConfirmed.BlockHeight,
	)
}
