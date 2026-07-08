//go:build systest

package systest

import (
	"crypto/sha256"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestRoundCommitmentReorgRoundTrip drives the full reorg-aware
// confirmation lifecycle through real bitcoind for a watch shaped
// like the one round/actor.go registers for a server-built commitment
// transaction. The round actor's Option B finality-gating contract
// depends on three properties of the chainsource pipeline holding
// over the real gRPC transport:
//
//  1. The watch is multi-shot once NotifyReorged or NotifyDone is
//     wired — a single registration must surface
//     ConfirmationEvent -> ConfReorgedEvent -> ConfirmationEvent ->
//     ConfDoneEvent, in that order, without the sub-actor tearing
//     itself down after the first positive event.
//
//  2. The chainsource FinalityDepth synthesizer (default six blocks)
//     actually fires on a real chain. lndclient over gRPC does not
//     write the upstream Done channel, so without the synthesizer
//     no Done event would ever arrive at the round actor and the
//     FSM would stay parked in InputSigSent forever. This is the
//     property TestChainSourceConfReorgRoundTrip deliberately does
//     NOT cover — it leaves Done unwired — so this test is the
//     systest-level oracle for the synthesizer's wire integration
//     under a real reorg cycle.
//
//  3. A reorg between the first confirmation and finality resets the
//     synthesizer's depth counter. Done must only fire once the
//     re-confirmation is past the reorg-safety depth, not the
//     pre-reorg observation.
//
// What this test pins specifically: the chainsource events reaching
// the actor layer match what the round actor's Option B handlers
// (handleConfirmation, handleCommitmentReorged,
// handleCommitmentFinalized) expect. The round actor's behavior
// under that event sequence is unit-tested in
// round/actor_test.go::TestActorRoundCommitmentLifecycleGatedOnFinality;
// this test proves the wire-level events the unit test mocks are
// genuinely delivered by the production pipeline.
func TestRoundCommitmentReorgRoundTrip(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t)
	ctx := h.Context()

	chainSource := h.NewChainSourceActor()

	// Synthetic watched address — deterministic per test name.
	pubKeyHash := sha256.Sum256([]byte(t.Name()))
	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		pubKeyHash[:20], &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err, "build synthetic P2WPKH address")
	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err, "derive pkScript for synthetic address")

	// Three recording refs mirroring the round actor's wiring in
	// registerCommitmentConfirmation: MapConfirmationEvent,
	// MapConfReorgedEvent, MapConfDoneEvent. Using
	// ChannelTellOnlyRef instead of a real actor keeps the test
	// focused on the wire-level event sequence without standing up
	// the round FSM (whose behavior is unit-tested separately).
	confRef := actor.NewChannelTellOnlyRef[chainsource.ConfirmationEvent](
		"round-commit-conf", 8,
	)
	reorgRef := actor.NewChannelTellOnlyRef[chainsource.ConfReorgedEvent](
		"round-commit-reorged", 4,
	)
	doneRef := actor.NewChannelTellOnlyRef[chainsource.ConfDoneEvent](
		"round-commit-done", 4,
	)

	// Faucet first so we have a known txid, then register, then mine.
	// The watch is keyed on (txid, pkScript) so the ordering between
	// mempool entry and registration is fine. Mining BEFORE
	// registration would dispatch historical confirmation state and
	// race against the live path.
	heightHint := h.Harness.BlockCount()
	amount := btcutil.Amount(btcutil.SatoshiPerBitcoin / 100)
	txidStr := h.Harness.Faucet(addr.String(), amount)
	txid, err := chainhash.NewHashFromStr(txidStr)
	require.NoError(t, err, "parse faucet txid")
	t.Logf("faucet tx: txid=%s", txid)

	confNotify := actor.TellOnlyRef[chainsource.ConfirmationEvent](
		confRef,
	)
	reorgNotify := actor.TellOnlyRef[chainsource.ConfReorgedEvent](
		reorgRef,
	)
	doneNotify := actor.TellOnlyRef[chainsource.ConfDoneEvent](
		doneRef,
	)

	_, err = chainSource.Ask(ctx, &chainsource.RegisterConfRequest{
		CallerID:      "round-commit-reorg-systest-" + txidStr,
		Txid:          txid,
		PkScript:      pkScript,
		TargetConfs:   1,
		HeightHint:    heightHint,
		NotifyActor:   fn.Some(confNotify),
		NotifyReorged: fn.Some(reorgNotify),
		NotifyDone:    fn.Some(doneNotify),
	}).Await(ctx).Unpack()
	require.NoError(t, err, "RegisterConfRequest failed")

	// 1. Mine the conf block.
	originalBlocks := h.Harness.Generate(1)
	require.Len(t, originalBlocks, 1)
	originalBlock := originalBlocks[0]
	originalHash, err := chainhash.NewHashFromStr(originalBlock.Hash)
	require.NoError(t, err, "parse original block hash")

	// 2. Expect ConfirmationEvent at the original block height.
	first := awaitConfEvent(t, confRef)
	require.Equal(t, *txid, first.Txid)
	require.Equal(t, int32(originalBlock.Height), first.BlockHeight)
	require.Equal(t, *originalHash, first.BlockHash)
	t.Logf(
		"first Confirmation: txid=%s height=%d hash=%s", first.Txid,
		first.BlockHeight, first.BlockHash,
	)

	// 3. Reorg the conf block out.
	reorg := h.Harness.Reorg(1, 2)
	require.Equal(
		t, originalBlock.Hash, reorg.Disconnected[0].Hash,
		"reorg must disconnect the original conf block",
	)
	require.Len(t, reorg.Connected, 2)
	t.Logf(
		"reorg: disconnected=%d connected=%d fork=%d",
		len(reorg.Disconnected), len(reorg.Connected),
		reorg.ForkPoint.Height,
	)

	// 4. Expect ConfReorgedEvent — the property the round actor's
	// handleCommitmentReorged consumes. The FSM stays in
	// InputSigSent because nothing user-visible was committed.
	reorgEvt := awaitReorgEvent(t, reorgRef)
	require.Equal(t, *txid, reorgEvt.Txid)
	t.Logf("Reorged: txid=%s", reorgEvt.Txid)

	// 5. Re-ConfirmationEvent on the canonical chain. Bitcoind
	// preserves the tx in its mempool across the invalidate, so the
	// faucet tx re-confirms in one of the new connected blocks.
	second := awaitConfEvent(t, confRef)
	require.Equal(t, *txid, second.Txid)
	require.NotEqual(
		t, *originalHash, second.BlockHash, "re-confirmation "+
			"BlockHash must differ from the disconnected "+
			"block; if it matches, the watch is replaying the "+
			"stale pre-reorg event",
	)
	t.Logf(
		"re-Confirmation: txid=%s height=%d hash=%s", second.Txid,
		second.BlockHeight, second.BlockHash,
	)

	// 6. Mine `DefaultFinalityDepth - 1` additional blocks past the
	// re-conf height. The synthesizer fires Done when the block
	// epoch subscription observes a height of
	// `confirmHeight + FinalityDepth - 1`. The re-confirmation block
	// itself counts as depth=1, so we need (FinalityDepth-1) more
	// blocks. With DefaultFinalityDepth=6 that's 5 more blocks.
	additionalBlocks := int(chainsource.DefaultFinalityDepth) - 1
	t.Logf(
		"mining %d additional blocks to trigger finality "+
			"synthesizer (FinalityDepth=%d)", additionalBlocks,
		chainsource.DefaultFinalityDepth,
	)
	h.Harness.Generate(additionalBlocks)

	// 7. Expect ConfDoneEvent — the property the round actor's
	// handleCommitmentFinalized consumes. Without this event, the
	// round FSM under Option B would never reach its terminal state
	// (no user-visible side effects, no onRoundComplete cleanup).
	doneEvt, ok := doneRef.AwaitMessage(reorgSystestEventTimeout)
	require.True(
		t, ok, "timeout waiting for ConfDoneEvent from the "+
			"FinalityDepth synthesizer (%s); without this "+
			"signal the round FSM would never reach ConfirmedState",
		reorgSystestEventTimeout,
	)
	require.Equal(
		t, *txid, doneEvt.Txid,
		"Done event txid must match the watched commitment tx",
	)
	t.Logf(
		"Done: txid=%s (synthesizer fired at depth=%d past "+
			"re-confirmation)", doneEvt.Txid,
		int(chainsource.DefaultFinalityDepth),
	)
}
