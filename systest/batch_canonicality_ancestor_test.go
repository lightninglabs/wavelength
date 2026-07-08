//go:build systest

package systest

import (
	"context"
	"crypto/sha256"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestBatchCanonicalityGateBlocksReorgedAncestor proves the LIVE coin-selection
// gate governs the FULL multi-parent lineage, not just a VTXO's direct
// commitment: a VTXO whose ANCESTOR batch (a cross-commitment parent, distinct
// from its direct commitment) is reorged off the canonical chain is excluded
// from coin selection, then admitted again once the ancestor reconfirms. This
// is the F4 acceptance scenario (OOR ancestor reorged then reconfirmed) at the
// vtxo.Manager seam.
//
// It proves the lineage-depth dimension that F2/F3 do not: those use a
// single-commitment VTXO, so they exercise only the direct commitment. Here the
// VTXO carries a separate ancestor in its Ancestry. The gate
// (gateUnavailableLineage) reloads the full descriptor via GetVTXO -- which
// hydrates the ancestry side table -- so lineageCommitmentTxids yields BOTH the
// direct commitment and the ancestor, and CombineAvailability takes the worst
// state across them. A reorged-out ancestor must therefore block the VTXO even
// while its direct commitment stays confirmed.
//
// The ancestor is isolated from the direct commitment by confirming them in
// different blocks (direct first, ancestor second) and reorging ONLY the
// ancestor's (later) block with an empty replacement branch. The direct
// commitment's earlier block is untouched, so the contrast is unambiguous: the
// only batch that changes state is the ancestor.
func TestBatchCanonicalityGateBlocksReorgedAncestor(t *testing.T) {
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
	// mirroring waved.
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
	const vtxoMgrName = "systest-vtxo-manager-f4-ancestor"
	vtxoKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		vtxoMgrName,
	)
	vtxoRef := actor.RegisterWithSystem(
		h.ActorSystem(), vtxoMgrName, vtxoKey, vtxoMgr,
	)
	require.NoError(t, vtxoMgr.Start(ctx, vtxoRef))

	// Confirm the DIRECT commitment first, in its own earlier block, so
	// reorging the ancestor's later block leaves it untouched. Each batch
	// is faucet -> register (live watch) -> mine, one block apart, so they
	// land in distinct blocks (fauceting both up front would confirm them
	// in the same block and defeat the isolation).
	directTxid, directScript := faucetSyntheticBatch(t, h, "direct")
	registerBatch(ctx, t, bcRef, *directTxid, directScript)
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *directTxid)

	// Now the ANCESTOR, in the next block.
	ancestorTxid, ancestorScript := faucetSyntheticBatch(t, h, "ancestor")
	registerBatch(ctx, t, bcRef, *ancestorTxid, ancestorScript)
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *ancestorTxid)

	// Seed a live VTXO whose direct commitment is directTxid and whose
	// ancestry carries ancestorTxid as a cross-commitment parent.
	const vtxoAmount = btcutil.Amount(50_000)
	outpoint := seedLiveVTXOWithAncestor(
		t, vtxoStore, t.Name(), *directTxid, *ancestorTxid, vtxoAmount,
	)

	// ----------------------------------------------------------------
	// Beat 1: reorg ONLY the ancestor off-chain -> the VTXO must be
	// EXCLUDED even though its direct commitment stays confirmed.
	// ----------------------------------------------------------------
	reorg := h.Harness.ReorgExcludingMempool(1, 2)
	require.Len(t, reorg.Connected, 2)

	awaitBatchState(
		ctx, t, bcRef, *ancestorTxid, batchcanon.StateReorgedOut,
	)
	// The direct commitment must remain Provisional throughout, so the
	// block can only be attributed to the ancestor.
	require.Equal(
		t, batchcanon.StateProvisional,
		batchState(ctx, t, bcRef, *directTxid),
		"direct commitment must stay confirmed while the ancestor "+
			"is reorged out",
	)

	blockedResp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: vtxoAmount,
	}).Await(ctx)
	require.False(
		t, blockedResp.IsOk(),
		"coin selection must fail while the VTXO's ANCESTOR batch "+
			"is reorged out, even though its direct commitment "+
			"is still confirmed",
	)
	t.Logf("ancestor ReorgedOut: coin selection excluded %s", outpoint)

	// ----------------------------------------------------------------
	// Beat 2: reconfirm the ancestor -> the VTXO must be ADMITTED again.
	// ----------------------------------------------------------------
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *ancestorTxid)

	admittedResp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: vtxoAmount,
	}).Await(ctx)
	require.True(
		t, admittedResp.IsOk(),
		"coin selection must succeed once the ancestor reconfirms",
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
		"the VTXO must be selected once its ancestor reconfirms",
	)
	t.Logf(
		"ancestor reconfirmed Provisional: coin selection admitted %s",
		outpoint,
	)
}

// faucetSyntheticBatch faucets a real tx to a deterministic synthetic P2WPKH
// script derived from the test name + a label, returning the tx's txid and
// pkScript. The tx stands in for a batch (commitment) tx the canonicality
// manager can track and reorg.
func faucetSyntheticBatch(t *testing.T, h *SysTestHarness,
	label string) (*chainhash.Hash, []byte) {

	t.Helper()

	pubKeyHash := sha256.Sum256([]byte(t.Name() + "-" + label))
	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		pubKeyHash[:20], &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err, "build synthetic P2WPKH address (%s)", label)
	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err, "derive pkScript (%s)", label)

	amount := btcutil.Amount(btcutil.SatoshiPerBitcoin / 100)
	txidStr := h.Harness.Faucet(addr.String(), amount)
	txid, err := chainhash.NewHashFromStr(txidStr)
	require.NoError(t, err, "parse faucet txid (%s)", label)

	return txid, pkScript
}

// registerBatch registers a batch (no consumed inputs) with the manager and
// waits for the synchronous response.
func registerBatch(ctx context.Context, t *testing.T,
	bcRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid chainhash.Hash, pkScript []byte) {

	t.Helper()

	resp := bcRef.Ask(ctx, &batchcanon.RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: pkScript,
		CSVExpiryDelta:       f2VTXOCSVDelay,
	}).Await(ctx)
	require.True(t, resp.IsOk(), "register batch %s", txid)
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
// cross-commitment parent, returning its outpoint. The owner/operator keys and
// tapscript are real so the descriptor is well-formed; the ancestry tree
// fragment is a minimal placeholder (the gate reads only the commitment txids).
func seedLiveVTXOWithAncestor(t *testing.T, vtxoStore *db.VTXOPersistenceStore,
	name string, directTxid, ancestorTxid chainhash.Hash,
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
		RoundID: chainhash.
			HashH([]byte(name + "-round")).
			String(),
		CommitmentTxID: directTxid,
		BatchExpiry:    500000,
		RelativeExpiry: f2VTXOCSVDelay,
		CreatedHeight:  1,
		Status:         vtxo.VTXOStatusLive,
	})
	require.NoError(t, err, "save live vtxo with ancestor")

	return outpoint
}
