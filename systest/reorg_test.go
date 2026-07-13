//go:build systest

package systest

import (
	"crypto/sha256"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// reorgSystestEventTimeout is the per-step deadline used by the
// chainsource reorg systests. The chain-notification pipeline runs
// over gRPC to a real lnd instance, so the timeout is generous enough
// to absorb container startup variance and notifier wake-up latency.
const reorgSystestEventTimeout = 30 * time.Second

// TestChainSourceConfReorgRoundTrip drives a real bitcoind reorg
// through the full chainsource pipeline:
//
//	lnd chainntnfs (in-process)
//	  -> lndclient gRPC (WithReOrgChan)
//	    -> chainbackends.LndClientChainNotifier (bridge)
//	      -> chainbackends.LNDBackend (multi-shot forwarder)
//	        -> chainsource.ConfActor (reorg-aware mode)
//	          -> test actor refs
//
// The flow is:
//
//  1. Register a reorg-aware confirmation watch on a synthetic P2WPKH
//     pkScript whose txid we know once we faucet to it.
//  2. Faucet + mine one block. Assert ConfirmationEvent arrives with
//     the expected (txid, blockHeight, blockHash).
//  3. Drive a 1-block reorg via the harness helper, which invalidates
//     the confirmation block and mines a strictly longer (2-block)
//     replacement branch. Bitcoind preserves the original tx in its
//     mempool, so the transaction re-confirms in the new chain at a
//     different block hash (and potentially the same height).
//  4. Assert ConfReorgedEvent arrives.
//  5. Assert a fresh ConfirmationEvent arrives with the new chain's
//     block hash (NOT the original one), demonstrating that lnd's
//     chainntnfs dispatched a re-confirmation after the reorg and
//     that every layer above it propagated the multi-shot signal
//     correctly.
//
// This is the systest-level oracle for "the reorg-aware pipeline
// actually works over the real gRPC transport". The unit tests in
// chainsource/reorg_test.go and chainbackends/lnd_reorg_test.go prove
// the same lifecycle against mocks, but they cannot prove that
// lndclient.WithReOrgChan fires when wired to real lnd. Only this
// test does.
func TestChainSourceConfReorgRoundTrip(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t)
	ctx := h.Context()

	// Spawn a real chainsource actor over the harness's LND.
	chainSource := h.NewChainSourceActor()

	// Build a synthetic P2WPKH address from a deterministic per-test
	// pubkey hash. The address does not need a controllable key (we
	// never spend it in this test); we just need a known pkScript so
	// we can register a confirmation watch.
	pubKeyHash := sha256.Sum256([]byte(t.Name()))
	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		pubKeyHash[:20], &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err, "build synthetic P2WPKH address")
	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err, "derive pkScript for synthetic address")

	// Wire test refs for each event variant. The Reorged ref also
	// has to be set for NotifyDone-or-NotifyReorged admission to flip
	// the sub-actor into multi-shot mode; we leave Done unwired
	// because the lndclient transport does not synthesize a Done
	// signal so it would never fire over a real lnd run anyway.
	confRef := actor.NewChannelTellOnlyRef[chainsource.ConfirmationEvent](
		"systest-conf", 8,
	)
	reorgRef := actor.NewChannelTellOnlyRef[chainsource.ConfReorgedEvent](
		"systest-conf-reorged", 8,
	)

	// Register the watch BEFORE the tx hits the mempool so we
	// exercise the live-detection path rather than the
	// historical-backfill path.
	heightHint := h.Harness.BlockCount()
	amount := btcutil.Amount(btcutil.SatoshiPerBitcoin / 100)

	// We need the txid up front, which means faucet first, then
	// register, then mine. The watch is on the txid + pkScript pair
	// so the ordering between mempool entry and registration is OK;
	// what must NOT happen is that we mine the block before
	// registering, because lnd's notifier would then dispatch
	// historical confirmation state and our test would be racing two
	// delivery paths.
	txidStr := h.Harness.Faucet(addr.String(), amount)
	txid, err := chainhash.NewHashFromStr(txidStr)
	require.NoError(t, err, "parse faucet txid")

	confNotify := actor.TellOnlyRef[chainsource.ConfirmationEvent](
		confRef,
	)
	reorgNotify := actor.TellOnlyRef[chainsource.ConfReorgedEvent](
		reorgRef,
	)

	regResp := chainSource.Ask(ctx, &chainsource.RegisterConfRequest{
		CallerID:      "test-reorg-conf-" + txidStr,
		Txid:          txid,
		PkScript:      pkScript,
		TargetConfs:   1,
		HeightHint:    heightHint,
		NotifyActor:   fn.Some(confNotify),
		NotifyReorged: fn.Some(reorgNotify),
	}).Await(ctx)
	require.True(t, regResp.IsOk(), "register reorg-aware conf watch")
	resp, err := regResp.Unpack()
	require.NoError(t, err)
	_, ok := resp.(*chainsource.RegisterConfResponse)
	require.True(t, ok, "unexpected register response type")

	// 1. Mine the block that confirms the faucet tx.
	originalBlocks := h.Harness.Generate(1)
	require.Len(t, originalBlocks, 1)
	originalBlock := originalBlocks[0]

	originalHash, err := chainhash.NewHashFromStr(originalBlock.Hash)
	require.NoError(t, err, "parse original block hash")

	// 2. Assert the first ConfirmationEvent.
	firstConf := awaitConfEvent(t, confRef)
	require.Equal(t, *txid, firstConf.Txid, "first conf txid mismatch")
	require.Equal(
		t, int32(originalBlock.Height), firstConf.BlockHeight,
		"first conf block height should match the mined block",
	)
	require.Equal(
		t, *originalHash, firstConf.BlockHash,
		"first conf block hash should match the mined block",
	)
	t.Logf(
		"first ConfirmationEvent: txid=%s height=%d hash=%s",
		firstConf.Txid, firstConf.BlockHeight, firstConf.BlockHash,
	)

	// 3. Drive a reorg: invalidate the conf block, mine a strictly
	// longer replacement branch. The harness Reorg helper waits for
	// lnd's chain sync to catch up before returning.
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

	// 4. Assert the ConfReorgedEvent. Lnd's chainntnfs notifier
	// dispatches NegativeConf for the disconnected confirmation
	// asynchronously after processing the disconnect; the gRPC
	// transport adds a further hop, so this can take longer than
	// the initial confirmation event.
	reorgEvt := awaitReorgEvent(t, reorgRef)
	require.Equal(t, *txid, reorgEvt.Txid, "reorg event txid mismatch")
	t.Logf("ConfReorgedEvent: txid=%s", reorgEvt.Txid)

	// 5. Assert a fresh ConfirmationEvent for the same tx, now in
	// the replacement chain. Bitcoind keeps the tx in mempool across
	// the invalidate, so generatetoaddress picks it back up on the
	// first new block. The new block hash MUST differ from the
	// original; the height may or may not match depending on where
	// the tx landed in the replacement branch.
	secondConf := awaitConfEvent(t, confRef)
	require.Equal(t, *txid, secondConf.Txid, "re-conf txid mismatch")
	require.NotEqual(
		t, firstConf.BlockHash, secondConf.BlockHash,
		"re-confirmation must arrive in a new block",
	)
	t.Logf(
		"second ConfirmationEvent: txid=%s height=%d hash=%s",
		secondConf.Txid, secondConf.BlockHeight, secondConf.BlockHash,
	)

	// Sanity: the new block hash must be one of the harness-reported
	// connected blocks. chainhash.Hash.String() renders the
	// canonical big-endian hex form that bitcoind's RPCs emit, so
	// the strings can be compared directly.
	connectedHashes := make(map[string]struct{}, len(reorg.Connected))
	for _, blk := range reorg.Connected {
		connectedHashes[blk.Hash] = struct{}{}
	}
	require.Contains(
		t, connectedHashes, secondConf.BlockHash.String(),
		"re-confirmation block must belong to the replacement branch",
	)
}

// awaitConfEvent reads a single ConfirmationEvent from the test ref
// with a generous deadline, failing the test on timeout.
func awaitConfEvent(t *testing.T,
	ref *actor.ChannelTellOnlyRef[chainsource.ConfirmationEvent],
) chainsource.ConfirmationEvent {

	t.Helper()

	evt, ok := ref.AwaitMessage(reorgSystestEventTimeout)
	require.True(
		t, ok, "timeout waiting for ConfirmationEvent (%s)",
		reorgSystestEventTimeout,
	)

	return evt
}

// awaitReorgEvent reads a single ConfReorgedEvent from the test ref
// with a generous deadline, failing the test on timeout.
func awaitReorgEvent(t *testing.T,
	ref *actor.ChannelTellOnlyRef[chainsource.ConfReorgedEvent],
) chainsource.ConfReorgedEvent {

	t.Helper()

	evt, ok := ref.AwaitMessage(reorgSystestEventTimeout)
	require.True(
		t, ok, "timeout waiting for ConfReorgedEvent (%s)",
		reorgSystestEventTimeout,
	)

	return evt
}
