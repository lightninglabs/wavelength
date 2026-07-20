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
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestBatchCanonicalityGateBlocksReorgedParent proves the F7 acceptance
// scenario: the coin-selection gate governs a VTXO's FULL multi-parent lineage,
// not just its direct commitment. A multi-input (OOR-born) VTXO descends from
// more than one batch, and the gate combines availability across ALL of them
// (worst-of-N). Reorging ONE parent out therefore excludes the VTXO even while
// every other parent stays confirmed; reconfirming that parent re-admits it.
//
// This is the lineage-BREADTH dimension F2/F3 do not exercise: those use a
// single-commitment VTXO, so they only ever exercise the direct commitment.
// Here the VTXO carries its direct commitment PLUS a distinct cross-commitment
// ancestor batch. The gate (gateUnavailableLineage -> lineageCommitmentTxids)
// reloads the full descriptor, collects both commitment txids, and takes the
// worst state across them (CombineAvailability). A reorged-out ancestor must
// block the VTXO even though the direct commitment is still confirmed; if the
// gate stopped at the direct commitment, the reorged-out ancestor would slip
// through and the VTXO would be wrongly spendable against a lineage no longer
// fully on the canonical chain.
//
// The two parents are isolated into distinct blocks (direct commitment first,
// ancestor second) so reorging ONLY the tip block cleanly targets the ancestor
// and leaves the direct commitment untouched -- making the contrast
// unambiguous: the sole batch that changes state is the ancestor.
func TestBatchCanonicalityGateBlocksReorgedParent(t *testing.T) {
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
	const vtxoMgrName = "systest-vtxo-manager-f7-multiparent"
	vtxoKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		vtxoMgrName,
	)
	vtxoRef := actor.RegisterWithSystem(
		h.ActorSystem(), vtxoMgrName, vtxoKey, vtxoMgr,
	)
	require.NoError(t, vtxoMgr.Start(ctx, vtxoRef))

	// Confirm the DIRECT commitment first, in its own earlier block, so
	// reorging the ancestor's later block leaves it untouched. Each batch
	// is a real authenticated tx: build+broadcast -> register -> mine, one
	// block apart, so they land in distinct blocks.
	directOp, directValueBTC, directPkScript :=
		h.Harness.FirstSpendableOutpoint()
	directTx, _ := h.Harness.BuildSignedSpend(directOp, directValueBTC)
	directTxid := directTx.TxHash()
	registerRealBatch(
		ctx, t, bcRef, directTx, directOp, directPkScript,
		int64(
			btcutil.Amount(
				directValueBTC*btcutil.SatoshiPerBitcoin,
			),
		),
	)
	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, directTxid)

	// Now the ANCESTOR, in the next block.
	ancestorOp, ancestorValueBTC, ancestorPkScript :=
		h.Harness.FirstSpendableOutpoint()
	ancestorTx, _ := h.Harness.BuildSignedSpend(
		ancestorOp, ancestorValueBTC,
	)
	ancestorTxid := ancestorTx.TxHash()
	registerRealBatch(
		ctx, t, bcRef, ancestorTx, ancestorOp, ancestorPkScript,
		int64(
			btcutil.Amount(
				ancestorValueBTC*btcutil.SatoshiPerBitcoin,
			),
		),
	)
	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, ancestorTxid)

	// Seed a live VTXO whose direct commitment is directTxid and whose
	// ancestry carries ancestorTxid as a distinct cross-commitment parent.
	outpoint := seedLiveVTXOWithAncestor(
		t, vtxoStore, t.Name(), directTxid, ancestorTxid, f2VTXOAmount,
	)

	// Sanity: with both parents confirmed the VTXO is admitted.
	assertVTXOSelected(ctx, t, vtxoRef, outpoint)
	releaseVTXOToLive(ctx, t, vtxoRef, vtxoStore, outpoint)
	t.Logf("both parents Provisional: coin selection admitted %s", outpoint)

	// ----------------------------------------------------------------
	// Beat 1: reorg ONLY the ancestor off-chain (empty replacement so it is
	// not re-mined) -> the VTXO must be EXCLUDED even though its direct
	// commitment stays confirmed (worst-parent).
	// ----------------------------------------------------------------
	reorg := h.Harness.ReorgExcludingMempool(1, 2)
	require.Len(t, reorg.Connected, 2)

	awaitBatchState(ctx, t, bcRef, ancestorTxid, batchcanon.StateReorgedOut)

	// The direct commitment must remain Provisional throughout, so the
	// reorged block can only be attributed to the ancestor.
	require.Equal(
		t, batchcanon.StateProvisional,
		batchState(ctx, t, bcRef, directTxid),
		"direct commitment must stay confirmed while the ancestor "+
			"is reorged out",
	)
	assertSpendSelectionFails(
		ctx, t, vtxoRef, "coin selection must fail while the "+
			"VTXO's ANCESTOR parent is reorged out, even "+
			"though its direct commitment is still confirmed "+
			"(worst-parent)",
	)
	t.Logf("ancestor ReorgedOut: coin selection excluded %s", outpoint)

	// ----------------------------------------------------------------
	// Beat 2: reconfirm the ancestor -> the whole lineage is canonical
	// again, so the VTXO must be ADMITTED.
	// ----------------------------------------------------------------
	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, ancestorTxid)
	assertVTXOSelected(ctx, t, vtxoRef, outpoint)
	t.Logf(
		"ancestor reconfirmed Provisional: coin selection "+
			"re-admitted %s", outpoint,
	)
}

// batchState reads a batch's current canonicality state via the manager.
func batchState(ctx context.Context, t *testing.T,
	bcRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid chainhash.Hash) batchcanon.State {

	t.Helper()

	resp, err := bcRef.Ask(
		ctx, &batchcanon.GetBatchStateRequest{BatchTxID: txid},
	).Await(ctx).Unpack()
	require.NoError(t, err)
	got, ok := resp.(*batchcanon.GetBatchStateResponse)
	require.True(t, ok, "unexpected get-state response type %T", resp)
	require.True(t, got.Found, "batch %s not found", txid)

	return got.Record.State
}

// seedLiveVTXOWithAncestor persists a single live VTXO whose direct commitment
// is directTxid and whose ancestry carries ancestorTxid as a distinct
// cross-commitment parent, returning its outpoint. It is the multi-parent
// analogue of seedLiveVTXOForBatch: the owner/operator keys and tapscript are
// real so the descriptor is well-formed, while the ancestry tree fragment is a
// minimal placeholder (the canonicality gate reads only the commitment txids).
func seedLiveVTXOWithAncestor(t *testing.T, vtxoStore *db.VTXOPersistenceStore,
	name string, directTxid, ancestorTxid chainhash.Hash,
	amount btcutil.Amount) wire.OutPoint {

	t.Helper()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err, "client key")

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err, "operator key")
	operatorKey := operatorPriv.PubKey()

	roundID, err := round.NewRoundID()
	require.NoError(t, err, "round id")

	descriptor, err := tree.NewVTXODescriptor(
		amount, clientPriv.PubKey(), operatorKey, f2VTXOCSVDelay,
	)
	require.NoError(t, err, "vtxo descriptor")

	tapScript, err := arkscript.VTXOTapScript(
		clientPriv.PubKey(), operatorKey, f2VTXOCSVDelay,
	)
	require.NoError(t, err, "vtxo tapscript")

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte(name + "-seeded-vtxo")),
		Index: 0,
	}

	// Minimal ancestry tree fragment; only the CommitmentTxID is consulted
	// by the canonicality gate.
	ancestorTree := &tree.Tree{
		BatchOutpoint: outpoint,
		Root: &tree.Node{
			Input:     outpoint,
			Outputs:   []*wire.TxOut{},
			CoSigners: []*btcec.PublicKey{},
			Children:  make(map[uint32]*tree.Node),
		},
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
		Ancestry: []types.Ancestry{{
			TreePath:       ancestorTree,
			CommitmentTxID: ancestorTxid,
			TreeDepth:      0,
		}},
		RoundID:        roundID.String(),
		CommitmentTxID: directTxid,
		BatchExpiry:    500000,
		RelativeExpiry: f2VTXOCSVDelay,
		CreatedHeight:  1,
		Status:         vtxo.VTXOStatusLive,
	})
	require.NoError(t, err, "save live vtxo with ancestor")

	return outpoint
}
