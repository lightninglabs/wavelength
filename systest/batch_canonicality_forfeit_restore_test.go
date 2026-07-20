//go:build systest

package systest

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestBatchCanonicalityRestoresForfeitedVTXO is the F6 acceptance scenario --
// the strongest proof that business state tracks chain canonicality rather than
// the reverse. A VTXO forfeited into a consumer batch is restored to a
// spendable state when that batch is invalidated (its forfeit reversed by a
// finalized conflict), driven end to end through real bitcoind + LND. The test
// also proves the NO-false-restore guard: a consumer batch that merely reorgs
// out and reconfirms (never reaching finality) must leave the forfeit standing.
//
// It exercises the reverse-dependency restore wired across two managers on the
// corrected, authenticated API:
//
// batchcanon.Manager (records the ConsumerEdge{ConsumedVTXO, ExpectedRevision,
// CreatorLineage}, detects the finalized conflict invalidating the consumer) ->
// Store.ResolveConsumerEdge: the exact ForfeitedBy compare-and-swap (business
// revision + forfeit-consumer marker + creator lineage usable + no competing
// edge/reservation) atomically restores the VTXO to Live ->
// ActivateRestoredVTXO callback (materialize the Live actor)
//
// The conditional-restore CAS is the safety core: it fires ONLY when the
// consumed VTXO still carries the exact business revision installed by that
// forfeiture, is forfeited by that exact consumer batch, its own creator
// lineage is ready and usable, and no competing consumer or reservation is
// outstanding.
//
// The consumer batch must be authenticated (serialized tx + every real TxIn),
// so invalidating it means double-spending one of its REAL inputs with a
// competing tx that reaches finality. Because an input a confirmed batch spends
// can only become spent by a different tx via a reorg, the conflict is created
// by reorging the batch out and confirming the competing spend in its place
// (reorgToForkHeightWithTx), then maturing that spend past the reorg-safety
// depth.
func TestBatchCanonicalityRestoresForfeitedVTXO(t *testing.T) {
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

	// Real vtxo.Manager first so coin selection is available and the
	// store's CAS-restored rows can be re-materialized by selection
	// self-heal.
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
	const vtxoMgrName = "systest-vtxo-manager-f6-restore"
	vtxoKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		vtxoMgrName,
	)
	vtxoRef := actor.RegisterWithSystem(
		h.ActorSystem(), vtxoMgrName, vtxoKey, vtxoMgr,
	)
	require.NoError(t, vtxoMgr.Start(ctx, vtxoRef))

	// Real batchcanon.Manager with the restore-activation callback. The
	// store CAS is what atomically restores the VTXO row to Live; the
	// callback records the activation so the test can assert the restore
	// fired.
	rec := &forfeitRestoreRecorder{}
	bcMgr := batchcanon.NewManager(batchcanon.ManagerConfig{
		Store:                canonStore,
		ChainSource:          chainSource,
		ActivateRestoredVTXO: rec.record,
		Log:                  fn.Some(h.SubLogger("BCAN")),
	})
	bcRef := actor.RegisterWithSystem(
		h.ActorSystem(),
		"batch-canonicality", batchcanon.ManagerServiceKey, bcMgr,
	)
	bcMgr.SetSelfRef(bcRef)

	// ================================================================
	// Phase 1 -- NO false restore: a consumer batch that only reorgs out
	// and reconfirms (never final) must leave its forfeit standing.
	// ================================================================
	assertNoFalseRestore(t, h, bcRef, vtxoStore)

	// ================================================================
	// Phase 2 -- real restore: a consumer batch invalidated by a finalized
	// conflict must restore its forfeited VTXO to Live.
	// ================================================================

	// A_c: the CREATOR batch that makes the forfeited VTXO exist. Its
	// lineage must be ready and usable for the restore CAS to fire, so it
	// is a real, registered, confirmed batch.
	creatorOp, creatorValueBTC, creatorPkScript :=
		h.Harness.FirstSpendableOutpoint()
	creatorTx, _ := h.Harness.BuildSignedSpend(creatorOp, creatorValueBTC)
	creatorTxid := creatorTx.TxHash()
	registerRealBatch(
		ctx, t, bcRef, creatorTx, creatorOp, creatorPkScript,
		int64(
			btcutil.Amount(
				creatorValueBTC*btcutil.SatoshiPerBitcoin,
			),
		),
	)
	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, creatorTxid)

	// B: the CONSUMER batch that forfeits the VTXO. Build both B and a
	// conflicting double-spend of its input up front (the conflict is
	// signed while the input is still unspent).
	consumedOp, consumedValueBTC, consumedPkScript :=
		h.Harness.FirstSpendableOutpoint()
	conflictTx, _ := h.Harness.BuildSignedSpendNoBroadcast(
		consumedOp, consumedValueBTC,
	)
	consumerTx, _ := h.Harness.BuildSignedSpend(
		consumedOp, consumedValueBTC,
	)
	consumerTxid := consumerTx.TxHash()

	// Seed the VTXO Live anchored on its creator batch, then forfeit it
	// into the consumer batch B: MarkForfeited installs status=Forfeited,
	// the forfeit-consumer marker (B), and a fresh business revision -- the
	// exact (revision, consumer) pair the restore CAS keys on.
	forfeitedOutpoint := seedLiveVTXOForBatch(
		t, vtxoStore, t.Name()+"-restore", creatorTxid, f2VTXOAmount,
	)
	forfeitTxid := chainhash.HashH([]byte(t.Name() + "-forfeit-tx"))
	require.NoError(
		t, vtxoStore.MarkForfeited(
			ctx, forfeitedOutpoint, forfeitTxid, consumerTxid,
		),
		"forfeit the seeded VTXO into the consumer batch",
	)

	forfeited, err := vtxoStore.GetVTXO(ctx, forfeitedOutpoint)
	require.NoError(t, err, "load forfeited vtxo")
	require.Equal(
		t, vtxo.VTXOStatusForfeited, forfeited.Status,
		"precondition: the VTXO must be forfeited",
	)
	expectedRevision := forfeited.BusinessRevision

	// Register the consumer batch with the authenticated ConsumerEdge
	// binding the exact business revision and complete creator lineage.
	registerRealConsumerBatch(
		ctx, t, bcRef, consumerTx, consumedOp, consumedPkScript,
		int64(
			btcutil.Amount(
				consumedValueBTC*btcutil.SatoshiPerBitcoin,
			),
		),
		[]batchcanon.ConsumerEdge{{
			ConsumedVTXO:     forfeitedOutpoint,
			ConsumerBatch:    consumerTxid,
			ExpectedRevision: expectedRevision,
			CreatorLineage:   []chainhash.Hash{creatorTxid},
		}},
	)

	// Anchor the fork below the consumer batch's block, confirm the
	// consumer batch, and double-spend its input to a finalized conflict.
	forkHeight := h.Harness.BestBlockHeader().Height
	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, consumerTxid)
	require.Equal(
		t, vtxo.VTXOStatusForfeited,
		vtxoStatus(ctx, t, vtxoStore, forfeitedOutpoint),
		"the VTXO must stay forfeited while the consumer batch is "+
			"provisionally canonical",
	)

	reorgToForkHeightWithTx(t, h, forkHeight, conflictTx)
	awaitBatchState(
		ctx, t, bcRef, consumerTxid,
		batchcanon.StateConflictProvisional,
	)
	require.Equal(
		t, vtxo.VTXOStatusForfeited,
		vtxoStatus(ctx, t, vtxoStore, forfeitedOutpoint),
		"a provisional (non-final) conflict must not restore the "+
			"forfeited VTXO",
	)

	// Mature the conflicting spend past the reorg-safety depth: the
	// consumer batch is now permanently invalidated, so its forfeit is
	// reversed.
	h.Harness.Generate(int(chainsource.DefaultFinalityDepth) + 2)
	awaitBatchState(
		ctx, t, bcRef, consumerTxid, batchcanon.StateConflictFinalized,
	)

	// The forfeit is reversed: the VTXO is restored to Live, its activation
	// callback fired, and it is selectable for spending again.
	awaitVTXOStatus(
		ctx, t, vtxoStore, forfeitedOutpoint, vtxo.VTXOStatusLive,
	)
	require.Eventually(t, func() bool {
		return rec.contains(forfeitedOutpoint)
	}, f2ChainPollTimeout, f2ChainPollInterval,
		"the restored VTXO must be activated via ActivateRestoredVTXO")
	assertVTXOSelected(ctx, t, vtxoRef, forfeitedOutpoint)
	t.Logf(
		"consumer ConflictFinalized: forfeited VTXO %s restored to "+
			"Live and re-admitted", forfeitedOutpoint,
	)
}

// assertNoFalseRestore proves the negative half of the restore lifecycle: a
// consumer batch that reorgs out and then reconfirms (same txid) never reaches
// policy finality, so its forfeit must remain standing throughout. It seeds an
// independent forfeited VTXO, registers + confirms its consumer batch, reorgs
// that batch out (empty replacement, so it is not auto-re-mined) and then
// reconfirms it, asserting the VTXO stays forfeited at every step.
func assertNoFalseRestore(t *testing.T, h *SysTestHarness,
	bcRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	vtxoStore *db.VTXOPersistenceStore) {

	t.Helper()

	ctx := h.Context()

	consumedOp, valueBTC, pkScript := h.Harness.FirstSpendableOutpoint()
	consumerTx, _ := h.Harness.BuildSignedSpend(consumedOp, valueBTC)
	consumerTxid := consumerTx.TxHash()

	// A synthetic (unregistered) creator lineage is sufficient here: no
	// restore is ever attempted, so the lineage is never consulted. It only
	// has to satisfy the non-zero registration validation.
	syntheticCreator := chainhash.HashH(
		[]byte(t.Name() + "-nofalse-creator"),
	)
	forfeitedOutpoint := seedLiveVTXOForBatch(
		t, vtxoStore, t.Name()+"-nofalse", syntheticCreator,
		f2VTXOAmount,
	)
	forfeitTxid := chainhash.HashH([]byte(t.Name() + "-nofalse-forfeit"))
	require.NoError(
		t, vtxoStore.MarkForfeited(
			ctx, forfeitedOutpoint, forfeitTxid, consumerTxid,
		),
		"forfeit the seeded VTXO into the consumer batch",
	)
	forfeited, err := vtxoStore.GetVTXO(ctx, forfeitedOutpoint)
	require.NoError(t, err)

	registerRealConsumerBatch(
		ctx, t, bcRef, consumerTx, consumedOp, pkScript,
		int64(btcutil.Amount(valueBTC*btcutil.SatoshiPerBitcoin)),
		[]batchcanon.ConsumerEdge{{
			ConsumedVTXO:     forfeitedOutpoint,
			ConsumerBatch:    consumerTxid,
			ExpectedRevision: forfeited.BusinessRevision,
			CreatorLineage:   []chainhash.Hash{syntheticCreator},
		}},
	)

	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, consumerTxid)

	// Reorg the consumer batch off-chain (empty replacement so it is not
	// re-mined) -> ReorgedOut, then reconfirm it (same txid) ->
	// Provisional. Neither transition is terminal, so the forfeit must
	// never be reversed.
	reorg := h.Harness.ReorgExcludingMempool(1, 2)
	require.Len(t, reorg.Connected, 2)
	awaitBatchState(
		ctx, t, bcRef, consumerTxid, batchcanon.StateReorgedOut,
	)
	require.Equal(
		t, vtxo.VTXOStatusForfeited,
		vtxoStatus(ctx, t, vtxoStore, forfeitedOutpoint),
		"a reorged-out (non-final) consumer batch must not restore "+
			"the forfeited VTXO",
	)

	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, consumerTxid)
	require.Equal(
		t, vtxo.VTXOStatusForfeited,
		vtxoStatus(ctx, t, vtxoStore, forfeitedOutpoint),
		"a reconfirmed (still non-final) consumer batch must not "+
			"restore the forfeited VTXO",
	)
	t.Logf(
		"consumer reorged out + reconfirmed: forfeited VTXO %s "+
			"stayed forfeited (no false restore)",
		forfeitedOutpoint,
	)
}

// registerRealBatch registers an authenticated batch with a single real
// consumed input and no consumer edges, and asserts the registration succeeded.
func registerRealBatch(ctx context.Context, t *testing.T,
	bcRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	tx *wire.MsgTx, input wire.OutPoint, inputPkScript []byte,
	inputValueSat int64) {

	t.Helper()

	registerRealConsumerBatch(
		ctx, t, bcRef, tx, input, inputPkScript, inputValueSat, nil,
	)
}

// registerRealConsumerBatch registers an authenticated batch (serialized tx,
// its confirmation output, and its single real consumed input) together with
// the given consumer edges, and asserts the registration succeeded.
func registerRealConsumerBatch(ctx context.Context, t *testing.T,
	bcRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	tx *wire.MsgTx, input wire.OutPoint, inputPkScript []byte,
	inputValueSat int64, edges []batchcanon.ConsumerEdge) {

	t.Helper()

	txid := tx.TxHash()
	var buf bytes.Buffer
	require.NoError(t, tx.Serialize(&buf), "serialize batch tx")

	resp := bcRef.Ask(ctx, &batchcanon.RegisterBatchRequest{
		BatchTxID:            txid,
		BatchTx:              buf.Bytes(),
		BatchOutputIndex:     0,
		ConfirmationPkScript: tx.TxOut[0].PkScript,
		CSVExpiryDelta:       f2VTXOCSVDelay,
		ConsumedInputs: []batchcanon.ConsumedInput{{
			Outpoint: input,
			Value:    inputValueSat,
			PkScript: inputPkScript,
		}},
		ConsumedVTXOs: edges,
	}).Await(ctx)
	require.True(t, resp.IsOk(), "register batch %s", txid)
}

// vtxoStatus reads a VTXO's persisted status.
func vtxoStatus(ctx context.Context, t *testing.T,
	store *db.VTXOPersistenceStore, op wire.OutPoint) vtxo.VTXOStatus {

	t.Helper()

	desc, err := store.GetVTXO(ctx, op)
	require.NoError(t, err)
	require.NotNil(t, desc)

	return desc.Status
}

// awaitVTXOStatus polls until a VTXO reaches the wanted persisted status. The
// restore is asynchronous (the canonicality manager resolves the edge and
// invokes the activation callback after the store CAS), so a retry is required.
func awaitVTXOStatus(ctx context.Context, t *testing.T,
	store *db.VTXOPersistenceStore, op wire.OutPoint,
	want vtxo.VTXOStatus) {

	t.Helper()

	require.Eventuallyf(t, func() bool {
		desc, err := store.GetVTXO(ctx, op)
		if err != nil || desc == nil {
			return false
		}

		return desc.Status == want
	}, f2ChainPollTimeout, f2ChainPollInterval,
		"vtxo %s never reached status %v", op, want)
}

// forfeitRestoreRecorder captures the VTXO outpoints activated after an atomic
// store restore, so a test can assert the restore-activation callback fired.
type forfeitRestoreRecorder struct {
	mu       sync.Mutex
	restored []wire.OutPoint
}

// record appends an activated outpoint. It satisfies the
// ManagerConfig.ActivateRestoredVTXO callback signature.
func (r *forfeitRestoreRecorder) record(_ context.Context,
	op wire.OutPoint) error {

	r.mu.Lock()
	defer r.mu.Unlock()
	r.restored = append(r.restored, op)

	return nil
}

// contains reports whether the given outpoint was activated.
func (r *forfeitRestoreRecorder) contains(op wire.OutPoint) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, restored := range r.restored {
		if restored == op {
			return true
		}
	}

	return false
}
