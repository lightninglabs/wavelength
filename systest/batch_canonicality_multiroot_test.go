//go:build systest

package systest

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/batchcanon"
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

// TestBatchCanonicalityGateBlocksPartialRootReorg proves the F7 acceptance
// scenario: the coin-selection gate combines availability across a VTXO's
// ENTIRE multi-root ancestry, so reorging out any STRICT SUBSET of its roots
// (a "partial-root" reorg) excludes the VTXO even while every other root stays
// confirmed. Once the reorged root reconfirms, the VTXO is admitted again.
//
// This is the N>2 generalization of F4
// (TestBatchCanonicalityGateBlocksReorgedAncestor). F4 has a single ancestor,
// so its gate combines availability over exactly {direct commitment, one
// ancestor} — a 2-element set where "worst-of-N" is indistinguishable from a
// simple pairwise min. A VTXO minted from a merge/OOR that draws inputs from
// several distinct commitment batches carries MULTIPLE cross-commitment roots;
// the gate must reduce over all of them and take the worst. Here the VTXO
// carries two independent ancestors (rootA, rootB) plus its direct commitment,
// and we reorg ONLY rootB. If the gate stopped at the first canonical root, or
// short-circuited on the direct commitment, the reorged-out rootB would slip
// through and the VTXO would be wrongly spendable against a lineage that is no
// longer fully on-chain.
//
// The roots are isolated into distinct blocks (direct, then rootA, then rootB,
// one block apart) so reorging only the tip block cleanly targets rootB and
// leaves the direct commitment and rootA untouched — making the contrast
// unambiguous: the sole batch that changes state is rootB.
func TestBatchCanonicalityGateBlocksPartialRootReorg(t *testing.T) {
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

	// Real batchcanon.Manager + vtxo.Manager (gate on the same store),
	// mirroring darepod.
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
	const vtxoMgrName = "systest-vtxo-manager-f7-multiroot"
	vtxoKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		vtxoMgrName,
	)
	vtxoRef := actor.RegisterWithSystem(
		h.ActorSystem(), vtxoMgrName, vtxoKey, vtxoMgr,
	)
	require.NoError(t, vtxoMgr.Start(ctx, vtxoRef))

	// Confirm the DIRECT commitment first, then each root, one block apart,
	// so the roots land in distinct blocks and reorging only the tip block
	// targets rootB alone (fauceting them together would confirm them in
	// the same block and defeat the isolation).
	directTxid, directScript := faucetSyntheticBatch(t, h, "direct")
	registerBatch(ctx, t, bcRef, *directTxid, directScript)
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *directTxid)

	rootATxid, rootAScript := faucetSyntheticBatch(t, h, "rootA")
	registerBatch(ctx, t, bcRef, *rootATxid, rootAScript)
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *rootATxid)

	rootBTxid, rootBScript := faucetSyntheticBatch(t, h, "rootB")
	registerBatch(ctx, t, bcRef, *rootBTxid, rootBScript)
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *rootBTxid)

	// Seed a live VTXO whose direct commitment is directTxid and whose
	// ancestry carries BOTH rootA and rootB as distinct cross-commitment
	// parents.
	const vtxoAmount = btcutil.Amount(50_000)
	outpoint := seedLiveVTXOWithAncestors(
		t, vtxoStore, t.Name(), *directTxid,
		[]chainhash.Hash{*rootATxid, *rootBTxid}, vtxoAmount,
	)

	// ----------------------------------------------------------------
	// Beat 1: reorg ONLY rootB off-chain (a partial-root reorg) -> the
	// VTXO must be EXCLUDED even though its direct commitment and rootA
	// both stay confirmed.
	// ----------------------------------------------------------------
	reorg := h.Harness.ReorgExcludingMempool(1, 2)
	require.Len(t, reorg.Connected, 2)

	awaitBatchState(ctx, t, bcRef, *rootBTxid, batchcanon.StateReorgedOut)

	// The direct commitment and rootA must remain Provisional throughout,
	// so the reorged block can only be attributed to rootB.
	require.Equal(
		t, batchcanon.StateProvisional,
		batchState(ctx, t, bcRef, *directTxid),
		"direct commitment must stay confirmed while rootB is "+
			"reorged out",
	)
	require.Equal(
		t, batchcanon.StateProvisional,
		batchState(ctx, t, bcRef, *rootATxid),
		"rootA must stay confirmed while rootB is reorged out",
	)

	blockedResp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: vtxoAmount,
	}).Await(ctx)
	require.False(
		t, blockedResp.IsOk(),
		"coin selection must fail while ANY root (rootB) is "+
			"reorged out, even though the direct commitment "+
			"and rootA are still confirmed",
	)
	t.Logf("partial-root ReorgedOut: coin selection excluded %s", outpoint)

	// ----------------------------------------------------------------
	// Beat 2: reconfirm rootB -> the whole lineage is canonical again, so
	// the VTXO must be ADMITTED.
	// ----------------------------------------------------------------
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *rootBTxid)

	admittedResp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: vtxoAmount,
	}).Await(ctx)
	require.True(
		t, admittedResp.IsOk(),
		"coin selection must succeed once every root is canonical "+
			"again",
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
		t, selectedOutpoints, outpoint, "the VTXO must be selected "+
			"once its full multi-root lineage reconfirms",
	)
	t.Logf(
		"all roots reconfirmed Provisional: coin selection admitted %s",
		outpoint,
	)
}

// seedLiveVTXOWithAncestors persists a single live VTXO whose direct commitment
// is directTxid and whose ancestry carries EACH of ancestorTxids as a distinct
// cross-commitment parent, returning its outpoint. It is the multi-root
// generalization of seedLiveVTXOWithAncestor. The owner/operator keys and
// tapscript are real so the descriptor is well-formed; each ancestry tree
// fragment is a minimal placeholder (the gate reads only the commitment txids).
func seedLiveVTXOWithAncestors(t *testing.T, vtxoStore *db.VTXOPersistenceStore,
	name string, directTxid chainhash.Hash, ancestorTxids []chainhash.Hash,
	amount btcutil.Amount) wire.OutPoint {

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

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte(name + "-seeded-vtxo")),
		Index: 0,
	}

	// One minimal ancestry entry per root; only the CommitmentTxID of each
	// is consulted by the canonicality gate.
	ancestry := make([]types.Ancestry, 0, len(ancestorTxids))
	for _, ancestorTxid := range ancestorTxids {
		ancestorTree := &tree.Tree{
			BatchOutpoint: outpoint,
			Root: &tree.Node{
				Input:     outpoint,
				Outputs:   []*wire.TxOut{},
				CoSigners: []*btcec.PublicKey{},
				Children:  make(map[uint32]*tree.Node),
			},
		}
		ancestry = append(ancestry, types.Ancestry{
			TreePath:       ancestorTree,
			CommitmentTxID: ancestorTxid,
			TreeDepth:      0,
		})
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
		Ancestry:    ancestry,
		RoundID: chainhash.
			HashH([]byte(name + "-round")).
			String(),
		CommitmentTxID: directTxid,
		BatchExpiry:    500000,
		RelativeExpiry: f2VTXOCSVDelay,
		CreatedHeight:  1,
		Status:         vtxo.VTXOStatusLive,
	})
	require.NoError(t, err, "save live vtxo with ancestors")

	return outpoint
}
