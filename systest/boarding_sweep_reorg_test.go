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
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/stretchr/testify/require"
)

// recordingBoardingSweepRef collects the BoardingSweepTxNotification
// values that txconfirm's lifecycle, fanned through the boarding
// sweep MapNotification, delivers to the actor's mailbox. The test
// uses this to assert that each chainsource event maps to the right
// BoardingSweepTxStatus rather than collapsing reorgs into a failure.
type recordingBoardingSweepRef struct {
	id   string
	msgs chan wallet.BoardingSweepTxNotification
}

func newRecordingBoardingSweepRef(id string) *recordingBoardingSweepRef {
	return &recordingBoardingSweepRef{
		id:   id,
		msgs: make(chan wallet.BoardingSweepTxNotification, 16),
	}
}

// ID returns the subscriber identifier.
func (r *recordingBoardingSweepRef) ID() string {
	return r.id
}

// Tell records the inbound boarding-sweep notification on the channel
// for the test to consume.
func (r *recordingBoardingSweepRef) Tell(_ context.Context,
	msg wallet.WalletMsg) error {

	notif, ok := msg.(wallet.BoardingSweepTxNotification)
	if !ok {

		// Not a notification we care about (and there should be
		// no others on this subscriber path).
		return nil
	}

	r.msgs <- notif

	return nil
}

// await pulls the next boarding-sweep notification or fails the test
// on timeout.
func (r *recordingBoardingSweepRef) await(
	t *testing.T) wallet.BoardingSweepTxNotification {

	t.Helper()

	select {
	case msg := <-r.msgs:
		return msg

	case <-time.After(txConfirmSystestEventTimeout):
		t.Fatalf("timeout waiting for boarding-sweep notification (%s)",
			txConfirmSystestEventTimeout)

		return wallet.BoardingSweepTxNotification{}
	}
}

// TestBoardingSweepReorgRoundTrip exercises the boarding sweep
// MapNotification under a real bitcoind reorg, end-to-end through the
// full txconfirm pipeline. It is the boarding-sweep complement of
// TestTxConfirmReorgRoundTrip: the same chain-event substrate, but
// asserting on the wallet-level lifecycle statuses that drive
// handleSweepTxNotification rather than the raw txconfirm
// notifications.
//
// Lifecycle asserted:
//
//	BoardingSweepTxStatusConfirmed  (first conf landed)
//	BoardingSweepTxStatusReorged    (conf block disconnected)
//	BoardingSweepTxStatusConfirmed  (re-confirmation on canonical chain)
//
// TxFinalized is exercised by TestTxConfirmReorgRoundTrip — that test
// proves the chainsource finality-depth synthesizer fires after the
// reorg-safety horizon, which is the same wire that would deliver
// BoardingSweepTxStatusFinalized to the wallet handler. Including a
// dedicated Finalized assertion here would require mining
// DefaultFinalityDepth more blocks (~6 additional rounds of harness
// generation) without exercising any boarding-sweep-specific code
// path that the unit tests in
// wallet/boarding_sweep_actor_test.go::TestSweepTxNotificationFinalizedIsBenign
// do not already cover.
//
// What this test specifically pins:
//
//   - The boarding-sweep MapNotification correctly classifies a real
//     chainsource Confirmed event as
//     BoardingSweepTxStatusConfirmed (not Failed).
//   - The boarding-sweep MapNotification correctly classifies a real
//     chainsource Reorged event as
//     BoardingSweepTxStatusReorged (not Failed). This is the load-
//     bearing regression the wallet refactor was designed to prevent.
//   - The handler is multi-shot: txconfirm keeps the watch armed
//     past the first TxConfirmed, so a re-confirmation on the
//     canonical chain re-fires
//     BoardingSweepTxStatusConfirmed.
//   - BlockHeight is preserved across the wire on each Confirmed
//     event.
func TestBoardingSweepReorgRoundTrip(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t)
	ctx := h.Context()

	chainSource := h.NewChainSourceActor()

	// Spawn a real txconfirm actor over the real chainsource. Wallet
	// is nil because the systest tx is non-anchor; CPFP fee-input
	// selection is never triggered.
	txconfBehavior := txconfirm.NewTxBroadcasterActor(txconfirm.Config{
		ChainSource: chainSource,
	})
	txconfInstance := actor.NewActor(actor.ActorConfig[
		txconfirm.Msg, txconfirm.Resp,
	]{
		ID:          "txconfirm-boarding-sweep-reorg",
		Behavior:    txconfBehavior,
		MailboxSize: 64,
	})
	txconfBehavior.SetSelfRef(txconfInstance.TellRef())
	txconfInstance.Start()
	t.Cleanup(txconfInstance.Stop)

	// Synthetic watched pkScript — same trick as
	// TestTxConfirmReorgRoundTrip.
	pubKeyHash := sha256.Sum256([]byte(t.Name()))
	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		pubKeyHash[:20], &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err, "build synthetic P2WPKH address")
	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err, "derive pkScript for synthetic address")

	heightHint := h.Harness.BlockCount()

	signedTx := h.Harness.SignedV3Tx(
		pkScript, btcutil.Amount(btcutil.SatoshiPerBitcoin/100),
	)
	txidVal := signedTx.TxHash()
	txid := &txidVal
	t.Logf("constructed v3 tx: txid=%s", txid)

	// Build the subscriber chain that the production code uses:
	//
	//	walletNotif (TellOnlyRef[BoardingSweepTxNotification], wrap
	//	  via MapMessage so it can sit on a WalletMsg pipe)
	//	  -> wallet.MapBoardingSweepNotification (the production
	//	     classifier from boarding_sweep_actor.go) hosted on a
	//	     txconfirm subscriber.
	//
	// The recording ref stands in for the wallet actor's mailbox; the
	// MapNotification function is the same one the production
	// submitSweepConfirmer wires up.
	walletRecorder := newRecordingBoardingSweepRef(
		"boarding-sweep-systest-sub",
	)
	subscriber := wallet.NewBoardingSweepTxconfirmSubscriber(
		walletRecorder,
	)

	// Register the EnsureConfirmedReq BEFORE mining so we exercise
	// the live-detection path.
	_, err = txconfInstance.Ref().Ask(
		ctx, &txconfirm.EnsureConfirmedReq{
			Tx:                   signedTx,
			ConfirmationPkScript: pkScript,
			Label:                "systest-boarding-sweep-reorg",
			HeightHint:           heightHint,
			TargetConfs:          1,
			Subscriber:           subscriber,
		},
	).Await(ctx).Unpack()
	require.NoError(t, err, "EnsureConfirmedReq failed")

	// 1. Mine the block that confirms the systest tx.
	originalBlocks := h.Harness.Generate(1)
	require.Len(t, originalBlocks, 1)
	originalBlock := originalBlocks[0]

	// 2. Expect Status=Confirmed at the original block height with
	// at least one confirmation. NumConfs being non-zero pins the
	// classifier's assignment of ev.NumConfs through the
	// txconfirm.TxConfirmed -> BoardingSweepTxNotification mapping;
	// a regression that zeroed it (e.g. by accidentally pulling
	// from the wrong field on the event) would otherwise be silently
	// invisible to consumers that read NumConfs to gate on
	// confirmation depth.
	first := walletRecorder.await(t)
	require.Equal(
		t, wallet.BoardingSweepTxStatusConfirmed, first.Status, "fir"+
			"st notification must be Confirmed, got status=%d",
		first.Status,
	)
	require.Equal(t, *txid, first.Txid)
	require.Equal(
		t, int32(originalBlock.Height), first.BlockHeight,
		"first Confirmed BlockHeight should match the mined block",
	)
	require.GreaterOrEqual(
		t, first.NumConfs, uint32(1),
		"first Confirmed NumConfs must be at least the target (1)",
	)
	t.Logf(
		"first Confirmed: txid=%s height=%d num_confs=%d", first.Txid,
		first.BlockHeight, first.NumConfs,
	)

	// 3. Reorg the conf block out, mine a strictly longer replacement
	// branch, wait for lnd to chain-sync.
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

	// 4. Expect Status=Reorged. The load-bearing assertion: a real
	// chainsource Reorged event must NOT classify as Failed (which
	// would trigger MarkBoardingSweepFailed in the production
	// handler).
	second := walletRecorder.await(t)
	require.Equal(
		t, wallet.BoardingSweepTxStatusReorged, second.Status, "seco"+
			"nd notification must be Reorged (NOT Failed); got "+
			"status=%d. A Reorged-as-Failed regression is "+
			"exactly what this systest exists to catch",
		second.Status,
	)
	require.Equal(t, *txid, second.Txid)
	require.Empty(
		t, second.Reason, "Reorged must not carry a failure reason",
	)
	t.Logf("Reorged: txid=%s", second.Txid)

	// 5. Expect a fresh Status=Confirmed on the canonical chain. The
	// re-confirmation must land in one of the new connected blocks
	// (NumConfs is also asserted to pin the field-through invariant
	// under the re-confirmation path — the classifier's TxConfirmed
	// arm is exercised twice in this lifecycle). Note: the
	// re-confirmation can legitimately land at the SAME block height
	// as the disconnected block — Reorg(1, 2) replaces 1 block with
	// 2, so the first new block sits at the same height as the old
	// one, just with a different hash. The notification doesn't
	// carry BlockHash so we can't directly assert hash inequality;
	// the canonical-height membership check below is the strongest
	// available signal that this is a fresh confirmation on the new
	// tip rather than a recycled stale event.
	third := walletRecorder.await(t)
	require.Equal(
		t, wallet.BoardingSweepTxStatusConfirmed, third.Status, "thi"+
			"rd notification must be Confirmed "+
			"(re-confirmation), got status=%d", third.Status,
	)
	require.Equal(t, *txid, third.Txid)
	canonicalHeights := make(map[int32]struct{}, len(reorg.Connected))
	for _, blk := range reorg.Connected {
		canonicalHeights[int32(blk.Height)] = struct{}{}
	}
	_, onCanonical := canonicalHeights[third.BlockHeight]
	require.True(
		t, onCanonical, "re-confirmation BlockHeight=%d must be one "+
			"of the new connected block heights %v",
		third.BlockHeight, reorg.Connected,
	)
	require.GreaterOrEqual(
		t, third.NumConfs, uint32(1),
		"re-confirmation NumConfs must be at least the target (1)",
	)
	t.Logf(
		"re-Confirmed: txid=%s height=%d num_confs=%d", third.Txid,
		third.BlockHeight, third.NumConfs,
	)
}
