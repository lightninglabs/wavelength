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
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/batchcanon"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestBatchCanonicalityGateBlocksConflictedVTXO proves the LIVE coin-selection
// reorg-safety gate handles the INPUT-CONFLICT path: a VTXO whose batch has one
// of its consumed inputs double-spent by a competing transaction is excluded
// from coin selection, then admitted again once the conflicting spend is
// reorged away. This is the F3 acceptance scenario (batch input conflicted) at
// the vtxo.Manager seam, driven by a real bitcoind double-spend + reorg.
//
// It exercises a DIFFERENT code path from the F2 reorg test
// (TestBatchCanonicalityGateBlocksReorgedVTXO): F2 trips the conf-watch
// (LimboReorg) by reorging the batch tx itself off-chain, whereas this test
// trips the per-input SPEND watch (LimboConflict). The batchcanon.Manager flags
// a batch ConflictProvisional the moment one of its registered consumed inputs
// is spent by any tx other than the batch tx (handleInputSpent:
// spendingTxid != w.txid), and clears the conflict when that spend is reorged
// out (handleInputSpendReorged). Both layers are governed by the same gate
// (batchcanon.LineageBlocked treats LimboConflict as blocked), so coin
// selection must refuse the VTXO while the conflict stands and resume once it
// clears.
//
// The wiring mirrors production exactly as the F2 test does: real chainsource,
// real batchcanon.Manager arming reorg-aware conf + spend watches, real
// vtxo.Manager whose gate reads the same durable store.
//
// The flow:
//
//  1. Faucet the batch (commitment) tx and pick an independent spendable
//     outpoint O. Register the batch with O as its consumed input and seed a
//     live VTXO anchored on the batch. Confirm -> Provisional.
//  2. Double-spend O with a competing tx and confirm it. The spend watch fires
//     with spendingTxid != batchTxid -> ConflictProvisional. Coin selection
//     must FAIL (the VTXO is gated out).
//  3. Reorg the conflicting spend off-chain (empty replacement branch, so it is
//     not re-mined). The spend watch reports the spend reorged out -> the
//     conflict clears -> Provisional. Coin selection must SUCCEED again.
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

	// Faucet the batch (commitment) tx to a synthetic P2WPKH script.
	pubKeyHash := sha256.Sum256([]byte(t.Name()))
	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		pubKeyHash[:20], &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err, "build synthetic P2WPKH address")
	batchPkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err, "derive pkScript for synthetic address")

	batchAmount := btcutil.Amount(btcutil.SatoshiPerBitcoin / 100)
	batchTxidStr := h.Harness.Faucet(addr.String(), batchAmount)
	batchTxid, err := chainhash.NewHashFromStr(batchTxidStr)
	require.NoError(t, err, "parse faucet txid")

	// Pick an independent confirmed wallet outpoint to register as the
	// batch's consumed input and later double-spend. listunspent excludes
	// the faucet tx's own (mempool-spent) input and its unconfirmed
	// outputs, so this outpoint is unrelated to the batch tx -- which is
	// fine: the manager flags a conflict on whatever consumed inputs it is
	// told to watch, by comparing the spending txid to the batch txid.
	conflictInput, conflictValueBTC, conflictScript :=
		h.Harness.FirstSpendableOutpoint()

	// Seed a live VTXO anchored on the batch tx, before the manager starts.
	const vtxoAmount = btcutil.Amount(50_000)
	outpoint := seedLiveVTXOForBatch(
		t, vtxoStore, t.Name(), *batchTxid, vtxoAmount,
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

	// Register the batch with the conflict input as its consumed input, so
	// the manager arms a reorg-aware spend watch on it.
	regResp := bcRef.Ask(ctx, &batchcanon.RegisterBatchRequest{
		BatchTxID:            *batchTxid,
		ConfirmationPkScript: batchPkScript,
		CSVExpiryDelta:       f2VTXOCSVDelay,
		ConsumedInputs: []batchcanon.ConsumedInput{{
			Outpoint: conflictInput,
			PkScript: conflictScript,
		}},
	}).Await(ctx)
	require.True(t, regResp.IsOk(), "register batch with manager")

	// Confirm the batch tx -> Provisional.
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *batchTxid)

	// ----------------------------------------------------------------
	// Beat 1: double-spend the consumed input -> ConflictProvisional ->
	// the VTXO must be EXCLUDED from coin selection.
	// ----------------------------------------------------------------
	conflictTxid := h.Harness.SpendOutpoint(conflictInput, conflictValueBTC)
	require.NotEqual(
		t, batchTxidStr, conflictTxid,
		"the conflicting spend must be a different tx from the batch",
	)
	h.Harness.Generate(1)

	awaitBatchState(
		ctx, t, bcRef, *batchTxid, batchcanon.StateConflictProvisional,
	)

	blockedResp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: vtxoAmount,
	}).Await(ctx)
	require.False(
		t, blockedResp.IsOk(),
		"coin selection must fail while the VTXO's batch input is "+
			"conflicted: the only candidate is gated out",
	)
	t.Logf(
		"batch ConflictProvisional: coin selection excluded %s",
		outpoint,
	)

	// ----------------------------------------------------------------
	// Beat 2: reorg the conflicting spend off-chain (empty replacement
	// branch, so it is not re-mined) -> the spend watch reports it reorged
	// out -> the conflict clears -> Provisional -> the VTXO is ADMITTED.
	// ----------------------------------------------------------------
	reorg := h.Harness.ReorgExcludingMempool(1, 2)
	require.Len(t, reorg.Connected, 2)
	t.Logf(
		"reorg (empty replacement): disconnected=%d connected=%d "+
			"fork_height=%d", len(reorg.Disconnected),
		len(reorg.Connected), reorg.ForkPoint.Height,
	)

	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *batchTxid)

	admittedResp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: vtxoAmount,
	}).Await(ctx)
	require.True(
		t, admittedResp.IsOk(),
		"coin selection must succeed once the conflicting spend is "+
			"reorged away",
	)
	resp, err := admittedResp.Unpack()
	require.NoError(t, err)
	selected, ok := resp.(*vtxo.SelectAndReserveSpendResponse)
	require.True(t, ok, "unexpected select response type %T", resp)

	selectedOutpoints := make(
		[]wire.OutPoint, 0, len(selected.SelectedVTXOs),
	)
	for _, s := range selected.SelectedVTXOs {
		selectedOutpoints = append(selectedOutpoints, s.Outpoint)
	}
	require.Contains(
		t, selectedOutpoints, outpoint,
		"the de-conflicted VTXO must be selected",
	)
	t.Logf(
		"conflict cleared, batch Provisional: coin selection "+
			"admitted %s", outpoint,
	)
}
