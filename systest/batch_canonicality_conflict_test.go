//go:build systest

package systest

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestBatchCanonicalityGateBlocksConflictedVTXO proves the LIVE coin-selection
// reorg-safety gate handles the INPUT-CONFLICT path end to end (the F3
// acceptance scenario), driven by a real bitcoind double-spend + reorg:
//
//  1. A VTXO whose batch confirmed at ONE confirmation is usable
//     (AvailableProvisional) and admitted into coin selection.
//  2. A competing transaction double-spends one of the batch's registered
//     consumed inputs and confirms in its place: the batch becomes
//     ConflictProvisional (limbo_conflict) and the VTXO is EXCLUDED.
//  3. Reorging the conflicting spend away lets the batch reconfirm
//     (Provisional): the VTXO is ADMITTED again -- the conflict was recoverable
//     because it never reached policy finality.
//  4. In a second flow the conflict is re-established and matured PAST the
//     reorg-safety depth: the batch reaches ConflictFinalized (Invalidated) and
//     the VTXO is EXCLUDED terminally -- it never recovers.
//
// It exercises a DIFFERENT code path from the F2 reorg test
// (TestBatchCanonicalityGateBlocksReorgedVTXO). F2 trips the conf watch
// (LimboReorg) by reorging the batch tx itself off-chain; this test trips the
// per-input SPEND watch (LimboConflict / Invalidated). The manager flags a
// batch conflicting the moment one of its registered consumed inputs is spent
// by a transaction other than the batch itself, clears the conflict when that
// spend reorgs out, and finalizes it once the spend matures past the
// reorg-safety depth. All three are governed by the same fail-closed gate, so
// coin selection admits the VTXO only while its lineage is a ready, confirmed,
// non-conflicted member of the canonical chain.
//
// The corrected registration API is authenticated: the manager cross-checks the
// serialized batch tx (hash == BatchTxID, output pkScript, every TxIn
// registered). A conflict must therefore be a double-spend of a REAL input of
// the batch tx, which the harness cannot create against an already-confirmed
// input without a reorg: the batch is reorged out and a conflicting spend of
// the same input confirms in its place (ReorgReplacingTxs). The competing spend
// is built up front (BuildSignedSpendNoBroadcast) while the input is still
// unspent, so signing succeeds, and mined explicitly against the UTXO set
// rather than broadcast into a mempool conflict.
func TestBatchCanonicalityGateBlocksConflictedVTXO(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t)
	ctx := h.Context()

	chainSource := h.NewChainSourceActor()

	sqlDB := db.NewTestDB(t)
	clk := clock.NewDefaultClock()
	dbStore := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	vtxoStore := dbStore.NewVTXOStore(clk)
	canonStore := dbStore.NewBatchCanonicalityStore(clk)

	// Pick a confirmed wallet outpoint and build BOTH the batch tx (which
	// spends it) and a conflicting double-spend of it. The conflicting
	// spend is signed now -- while the outpoint is still unspent -- but not
	// broadcast, so it can be mined later against the UTXO set without a
	// mempool conflict against the batch tx.
	consumedOp, valueBTC, inputPkScript :=
		h.Harness.FirstSpendableOutpoint()
	conflictTx, conflictTxidStr := h.Harness.BuildSignedSpendNoBroadcast(
		consumedOp, valueBTC,
	)
	batchTx, batchTxidStr := h.Harness.BuildSignedSpend(
		consumedOp, valueBTC,
	)
	require.NotEqual(
		t, batchTxidStr, conflictTxidStr,
		"batch tx and its conflicting double-spend must differ",
	)
	batchTxid := batchTx.TxHash()

	var batchBuf bytes.Buffer
	require.NoError(t, batchTx.Serialize(&batchBuf), "serialize batch tx")

	inputValueSat := int64(
		btcutil.Amount(valueBTC * btcutil.SatoshiPerBitcoin),
	)

	// Seed a live VTXO anchored on the batch tx before the manager starts
	// so it is recovered into a resident actor.
	outpoint := seedLiveVTXOForBatch(
		t, vtxoStore, t.Name(), batchTxid, f2VTXOAmount,
	)

	// Real batchcanon.Manager over the durable store + real chainsource.
	bcMgr := batchcanon.NewManager(batchcanon.ManagerConfig{
		Store:       canonStore,
		ChainSource: chainSource,
		Log:         fn.Some(h.SubLogger("BCAN")),
	})
	bcRef := actor.RegisterWithSystem(
		h.ActorSystem(),
		"batch-canonicality", batchcanon.ManagerServiceKey, bcMgr,
	)
	bcMgr.SetSelfRef(bcRef)

	// Real vtxo.Manager with the coin-selection gate on the same store.
	vtxoWallet := lndbackend.NewClientWallet(
		h.Harness.LND.Signer, h.Harness.LND.WalletKit,
	)
	vtxoMgr := vtxo.NewManager(&vtxo.ManagerConfig{
		Store:             vtxoStore,
		Wallet:            vtxoWallet,
		ChainSource:       chainSource,
		ActorSystem:       h.ActorSystem(),
		ChainParams:       h.ChainParams(),
		BatchCanonicality: canonStore,
		Log:               fn.Some(h.SubLogger(vtxo.Subsystem)),
	})
	const vtxoMgrName = "systest-vtxo-manager-f3-conflict"
	vtxoKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		vtxoMgrName,
	)
	vtxoRef := actor.RegisterWithSystem(
		h.ActorSystem(), vtxoMgrName, vtxoKey, vtxoMgr,
	)
	require.NoError(t, vtxoMgr.Start(ctx, vtxoRef))

	// Register the batch with authenticated evidence: the serialized tx,
	// its confirmation output, and its exact consumed input (which arms the
	// reorg-aware spend watch used to detect the conflict).
	regResp := bcRef.Ask(ctx, &batchcanon.RegisterBatchRequest{
		BatchTxID:            batchTxid,
		BatchTx:              batchBuf.Bytes(),
		BatchOutputIndex:     0,
		ConfirmationPkScript: batchTx.TxOut[0].PkScript,
		CSVExpiryDelta:       f2VTXOCSVDelay,
		ConsumedInputs: []batchcanon.ConsumedInput{{
			Outpoint: consumedOp,
			Value:    inputValueSat,
			PkScript: inputPkScript,
		}},
	}).Await(ctx)
	require.True(t, regResp.IsOk(), "register batch with manager")

	// Anchor a fixed fork point below the batch tx's block so every reorg
	// beat can swap the batch tx and its conflicting double-spend across
	// the same fork.
	forkHeight := h.Harness.BestBlockHeader().Height

	// ----------------------------------------------------------------
	// Beat 1: confirm the batch at ONE confirmation -> Provisional -> the
	// VTXO is ADMITTED into coin selection.
	// ----------------------------------------------------------------
	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, batchTxid)
	assertVTXOSelected(ctx, t, vtxoRef, outpoint)
	t.Logf(
		"batch Provisional (1 conf): coin selection admitted %s",
		outpoint,
	)

	releaseVTXOToLive(ctx, t, vtxoRef, vtxoStore, outpoint)

	// ----------------------------------------------------------------
	// Beat 2: double-spend the batch's consumed input -> conflict
	// (limbo_conflict) -> the VTXO must be EXCLUDED from coin selection.
	// ----------------------------------------------------------------
	reorgToForkHeightWithTx(t, h, forkHeight, conflictTx)
	awaitBatchState(
		ctx, t, bcRef, batchTxid, batchcanon.StateConflictProvisional,
	)
	assertSpendSelectionFails(
		ctx, t, vtxoRef, "coin selection must fail while the "+
			"VTXO's batch input is conflicted "+
			"(limbo_conflict): the only candidate is gated out",
	)
	t.Logf(
		"batch ConflictProvisional: coin selection excluded %s",
		outpoint,
	)

	// ----------------------------------------------------------------
	// Beat 3: reorg the conflicting spend away -> the batch reconfirms
	// (Provisional) -> the VTXO is ADMITTED again. The conflict was
	// recoverable because it never reached policy finality.
	// ----------------------------------------------------------------
	reorgToForkHeightWithTx(t, h, forkHeight, batchTx)
	awaitBatchUsable(ctx, t, bcRef, batchTxid)
	assertVTXOSelected(ctx, t, vtxoRef, outpoint)
	t.Logf(
		"conflict reorged away, batch Provisional: coin selection "+
			"re-admitted %s", outpoint,
	)

	releaseVTXOToLive(ctx, t, vtxoRef, vtxoStore, outpoint)

	// ----------------------------------------------------------------
	// Beat 4: re-establish the conflict, then mature it past the
	// reorg-safety depth -> ConflictFinalized (Invalidated) -> the VTXO is
	// EXCLUDED terminally and never recovers.
	// ----------------------------------------------------------------
	reorgToForkHeightWithTx(t, h, forkHeight, conflictTx)
	awaitBatchState(
		ctx, t, bcRef, batchTxid, batchcanon.StateConflictProvisional,
	)

	// Mature the conflicting spend past the finality depth so the spend
	// Done event synthesizes and the batch reaches ConflictFinalized. A
	// margin beyond DefaultFinalityDepth absorbs the height at which the
	// spend was re-mined above the fork point.
	h.Harness.Generate(int(chainsource.DefaultFinalityDepth) + 2)
	awaitBatchState(
		ctx, t, bcRef, batchTxid, batchcanon.StateConflictFinalized,
	)
	assertSpendSelectionFails(
		ctx, t, vtxoRef, "coin selection must fail terminally once "+
			"the batch's input conflict is finalized "+
			"(invalidated): the VTXO never recovers",
	)
	t.Logf(
		"batch ConflictFinalized: coin selection terminally "+
			"excluded %s", outpoint,
	)
}

// reorgToForkHeightWithTx reorgs the chain back to forkHeight and mines a
// strictly-longer replacement branch whose first block confirms tx (bypassing
// the mempool). It is the primitive the conflict systests use to alternately
// confirm the batch tx and a conflicting double-spend of the batch's registered
// input across a fixed fork point: because both spend the same outpoint, only a
// reorg can swap which one is on the canonical chain.
func reorgToForkHeightWithTx(t *testing.T, h *SysTestHarness, forkHeight int64,
	tx *wire.MsgTx) harness.ReorgResult {

	t.Helper()

	tip := h.Harness.BestBlockHeader().Height
	depth := int(tip - forkHeight)
	require.Positive(
		t, depth, "fork height %d is not below tip %d", forkHeight, tip,
	)

	reorg := h.Harness.ReorgReplacingTxs(
		depth, depth+1, []*wire.MsgTx{tx},
	)
	t.Logf(
		"reorg to fork height %d (depth %d): disconnected=%d "+
			"connected=%d, confirmed %s", forkHeight, depth,
		len(reorg.Disconnected), len(reorg.Connected), tx.TxHash(),
	)

	return reorg
}

// assertSpendSelectionFails asserts that a single SelectAndReserveSpendRequest
// for the standard test VTXO amount fails, i.e. the sole gated-out candidate
// leaves no spendable liquidity.
func assertSpendSelectionFails(ctx context.Context, t *testing.T,
	vtxoRef actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp], msg string) {

	t.Helper()

	resp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: f2VTXOAmount,
	}).Await(ctx)
	require.False(t, resp.IsOk(), msg)
}
