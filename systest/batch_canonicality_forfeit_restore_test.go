//go:build systest

package systest

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/batchcanon"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestBatchCanonicalityRestoresForfeitedVTXO is the F6 acceptance scenario --
// the strongest proof that business state tracks chain canonicality rather than
// the reverse. A VTXO forfeited into a round-2 commitment is restored to a
// spendable state when that commitment is invalidated (its forfeit reversed by
// a finalized conflict), driven end to end through real bitcoind + LND.
//
// It exercises the reverse-dependency restore wired across two managers:
//
//	batchcanon.Manager (records the consumer-batch edge, detects the
//	  finalized conflict that invalidates the consumer batch)
//	  -> RestoreConsumedVTXO callback
//	    -> vtxo.Manager.RestoreForfeitedVTXORequest
//	      -> re-materialize the forfeited VTXO as Live from its descriptor
//
// The round-1 VTXO is seeded directly in the FORFEITED state (as the FSM leaves
// it once a round consuming it confirms; its actor was reaped). The round-2
// commitment is a faucet tx registered with that VTXO as a forfeited VTXO and
// with an independent consumed input we can double-spend. Double-spending the
// input and maturing the conflict past the reorg-safety depth drives the
// consumer batch to ConflictFinalized, which fires the restore. The VTXO must
// return to Live and become selectable again.
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

	// Seed the round-1 VTXO in the FORFEITED state (its actor reaped), as
	// the FSM leaves it once the round-2 commitment that consumes it
	// confirms.
	const vtxoAmount = btcutil.Amount(50_000)
	forfeitedOutpoint := seedForfeitedVTXO(
		t, vtxoStore, t.Name(), vtxoAmount,
	)

	// The round-2 commitment batch + an independent consumed input we can
	// double-spend to invalidate it.
	batchTxid, batchScript := faucetSyntheticBatch(t, h, "round2")
	conflictInput, conflictValueBTC, conflictScript :=
		h.Harness.FirstSpendableOutpoint()

	// Real vtxo.Manager (with the restore handler) first, so the
	// batchcanon manager's restore callback can target it.
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

	// Real batchcanon.Manager with the restore callback wired to the VTXO
	// manager -- mirroring darepod.restoreForfeitedVTXO.
	bcMgr := batchcanon.NewManager(batchcanon.ManagerConfig{
		Store:       canonStore,
		ChainSource: chainSource,
		Log:         fn.Some(h.SubLogger("BCAN")),
		RestoreConsumedVTXO: func(ctx context.Context,
			op wire.OutPoint) error {

			_, err := vtxoRef.Ask(
				ctx, &vtxo.RestoreForfeitedVTXORequest{
					Outpoint: op,
				},
			).Await(ctx).Unpack()

			return err
		},
	})
	bcRef := actor.RegisterWithSystem(
		h.ActorSystem(),
		"batch-canonicality", batchcanon.ManagerServiceKey, bcMgr,
	)
	bcMgr.SetSelfRef(bcRef)

	// Register the round-2 commitment batch: it forfeits the round-1 VTXO
	// and consumes the input we will double-spend.
	regResp := bcRef.Ask(ctx, &batchcanon.RegisterBatchRequest{
		BatchTxID:            *batchTxid,
		ConfirmationPkScript: batchScript,
		CSVExpiryDelta:       f2VTXOCSVDelay,
		ConsumedInputs: []batchcanon.ConsumedInput{{
			Outpoint: conflictInput,
			PkScript: conflictScript,
		}},
		ForfeitedVTXOs: []wire.OutPoint{forfeitedOutpoint},
	}).Await(ctx)
	require.True(t, regResp.IsOk(), "register round-2 batch")

	// Confirm the round-2 commitment so the forfeit looks complete.
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *batchTxid)

	// Precondition: the round-1 VTXO is forfeited and therefore not
	// spendable.
	require.Equal(
		t, vtxo.VTXOStatusForfeited,
		vtxoStatus(ctx, t, vtxoStore, forfeitedOutpoint),
		"precondition: the round-1 VTXO must be forfeited",
	)

	// ----------------------------------------------------------------
	// Invalidate the round-2 commitment: double-spend its consumed input
	// and mature the conflict past the reorg-safety depth, driving the
	// batch to ConflictFinalized.
	// ----------------------------------------------------------------
	conflictTxid := h.Harness.SpendOutpoint(conflictInput, conflictValueBTC)
	require.NotEqual(t, batchTxid.String(), conflictTxid)
	h.Harness.Generate(1)
	awaitBatchState(
		ctx, t, bcRef, *batchTxid, batchcanon.StateConflictProvisional,
	)

	// Mature the conflicting spend past the finality depth so it
	// finalizes. DefaultFinalityDepth + a margin guarantees the spend Done
	// event synthesizes and the batch reaches ConflictFinalized.
	h.Harness.Generate(int(chainsource.DefaultFinalityDepth) + 2)
	awaitBatchState(
		ctx, t, bcRef, *batchTxid, batchcanon.StateConflictFinalized,
	)

	// ----------------------------------------------------------------
	// The forfeit is reversed: the round-1 VTXO must be restored to Live
	// and become selectable again.
	// ----------------------------------------------------------------
	awaitVTXOStatus(
		ctx, t, vtxoStore, forfeitedOutpoint, vtxo.VTXOStatusLive,
	)
	t.Logf(
		"round-2 ConflictFinalized: forfeited VTXO %s restored to live",
		forfeitedOutpoint,
	)

	resp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: vtxoAmount,
	}).Await(ctx)
	require.True(
		t, resp.IsOk(),
		"the restored VTXO must be selectable for spending again",
	)
	unpacked, err := resp.Unpack()
	require.NoError(t, err)
	selected, ok := unpacked.(*vtxo.SelectAndReserveSpendResponse)
	require.True(t, ok, "unexpected select response type %T", unpacked)

	selectedOutpoints := make(
		[]wire.OutPoint, 0, len(selected.SelectedVTXOs),
	)
	for _, s := range selected.SelectedVTXOs {
		selectedOutpoints = append(selectedOutpoints, s.Outpoint)
	}
	require.Contains(
		t, selectedOutpoints, forfeitedOutpoint,
		"the restored VTXO must be among the selected coins",
	)
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

// awaitVTXOStatus polls until a VTXO reaches the wanted persisted status,
// failing on timeout. The restore is asynchronous (the canonicality manager
// fires a callback that asks the VTXO manager), so a retry is required.
func awaitVTXOStatus(ctx context.Context, t *testing.T,
	store *db.VTXOPersistenceStore, op wire.OutPoint,
	want vtxo.VTXOStatus) {

	t.Helper()

	require.Eventuallyf(
		t, func() bool {
			desc, err := store.GetVTXO(ctx, op)
			if err != nil || desc == nil {
				return false
			}

			return desc.Status == want
		}, reorgSystestEventTimeout, batchCanonPollInterval,
		"vtxo %s never reached status %v", op, want,
	)
}

// seedForfeitedVTXO persists a single VTXO in the FORFEITED state, returning
// its outpoint. It mirrors the descriptor a round leaves behind once the
// commitment that forfeits the VTXO confirms (status Forfeited, no live actor).
func seedForfeitedVTXO(t *testing.T, vtxoStore *db.VTXOPersistenceStore,
	name string, amount btcutil.Amount) wire.OutPoint {

	t.Helper()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err, "client key")

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err, "operator key")
	operatorKey := operatorPriv.PubKey()

	descriptor, err := tree.NewVTXODescriptor(
		amount, clientPriv.PubKey(), operatorKey, f2VTXOCSVDelay,
	)
	require.NoError(t, err, "vtxo descriptor")

	tapScript, err := arkscript.VTXOTapScript(
		clientPriv.PubKey(), operatorKey, f2VTXOCSVDelay,
	)
	require.NoError(t, err, "vtxo tapscript")

	// A distinct synthetic commitment txid for the round-1 batch. It is not
	// registered with the canonicality manager, so its lineage is unseen
	// (permissive) and the restored VTXO is selectable.
	commitmentTxid := chainhash.HashH([]byte(name + "-round1-commitment"))
	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte(name + "-forfeited-vtxo")),
		Index: 0,
	}

	err = vtxoStore.SaveVTXO(t.Context(), &vtxo.Descriptor{
		Outpoint:       outpoint,
		Amount:         amount,
		PolicyTemplate: descriptor.PolicyTemplate,
		PkScript:       descriptor.PkScript,
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientPriv.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  7,
			},
		},
		OperatorKey: operatorKey,
		TapScript:   tapScript,
		RoundID: chainhash.
			HashH([]byte(name + "-round1")).
			String(),
		CommitmentTxID: commitmentTxid,
		BatchExpiry:    500000,
		RelativeExpiry: f2VTXOCSVDelay,
		CreatedHeight:  1,
		Status:         vtxo.VTXOStatusForfeited,
	})
	require.NoError(t, err, "save forfeited vtxo")

	// SaveVTXO always persists a freshly-created VTXO as Live, so flip it
	// to Forfeited explicitly. This must happen before the VTXO manager
	// starts so the VTXO is excluded from live recovery (no actor spawned),
	// exactly as a reaped forfeit leaves it.
	require.NoError(
		t,
		vtxoStore.UpdateVTXOStatus(
			t.Context(), outpoint, vtxo.VTXOStatusForfeited,
		),
		"mark seeded vtxo forfeited",
	)

	return outpoint
}
