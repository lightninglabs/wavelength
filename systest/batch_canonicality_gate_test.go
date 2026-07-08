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
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// f2VTXOCSVDelay is the relative-expiry CSV delay stamped on the synthetic
// test VTXO. The value is arbitrary for this test (expiry is never exercised);
// it just has to be a valid non-zero delay for descriptor/tapscript
// construction.
const f2VTXOCSVDelay = 144

// TestBatchCanonicalityGateBlocksReorgedVTXO proves the LIVE coin-selection
// reorg-safety gate does its job end to end: a VTXO whose batch (commitment
// tx) is reorged off the canonical chain is excluded from coin selection,
// then admitted again once the batch reconfirms. This is the F2 acceptance
// scenario (batch reorged out then reconfirmed) at the vtxo.Manager seam,
// driven by a real bitcoind reorg.
//
// The wiring mirrors production: a real chainsource actor over the harness
// LND, a real batchcanon.Manager arming reorg-aware watches, and a real
// vtxo.Manager whose ManagerConfig.BatchCanonicality points at the SAME
// durable store the manager writes -- exactly how waved.initBatchCanonicality
// connects them. Only the VTXO is seeded directly (as seedLiveVTXO does for the
// directed-send systest) rather than produced by a live round; the round
// production path is covered by TestSendVTXOEndToEnd.
//
// The batch (commitment) tx is a real faucet transaction so its txid is a live
// on-chain tx the canonicality conf-watch can track and reorg. The seeded
// VTXO's CommitmentTxID is set to that txid, so the gate (which reloads the
// full descriptor via GetVTXO and reads its direct commitment txid) governs
// the VTXO by that batch.
//
// To make the "batch off-chain" window deterministic, the reorg mines its
// replacement branch with EMPTY blocks (ReorgExcludingMempool), so the batch
// tx is NOT auto-re-confirmed and the canonicality record holds ReorgedOut
// stably. A plain Reorg would re-mine the tx from the mempool on the first
// replacement block, collapsing the window before coin selection could observe
// it. A subsequent normal block re-confirms the stranded tx.
//
// The proof is a contrast on a single VTXO with a single selection target,
// where the ONLY thing that changes between the two beats is the batch's chain
// canonicality:
//
//  1. Batch ReorgedOut  -> SelectAndReserveSpendRequest fails (gated out).
//  2. Batch reconfirmed -> SelectAndReserveSpendRequest succeeds (admitted).
func TestBatchCanonicalityGateBlocksReorgedVTXO(t *testing.T) {
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

	// Faucet a real tx whose txid is the batch (commitment) tx. We faucet
	// to a synthetic P2WPKH script we never spend; we only need a known
	// txid + pkScript to anchor the VTXO lineage and arm the conf watch.
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

	// Seed a live VTXO anchored on the batch tx, BEFORE the manager starts
	// so it is recovered into a resident actor. Its outpoint is synthetic
	// (a VTXO leaf is not the batch tx itself); only its CommitmentTxID
	// matters to the gate.
	const vtxoAmount = btcutil.Amount(50_000)
	outpoint := seedLiveVTXOForBatch(
		t, vtxoStore, t.Name(), *batchTxid, vtxoAmount,
	)

	// Real batchcanon.Manager over the durable store, wired to the real
	// chainsource so it arms reorg-aware watches.
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

	// Real vtxo.Manager with the coin-selection gate pointed at the SAME
	// canonicality store, mirroring waved's wiring.
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
	const vtxoMgrName = "systest-vtxo-manager-f2-gate"
	vtxoKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		vtxoMgrName,
	)
	vtxoRef := actor.RegisterWithSystem(
		h.ActorSystem(), vtxoMgrName, vtxoKey, vtxoMgr,
	)
	require.NoError(t, vtxoMgr.Start(ctx, vtxoRef))

	// Register the batch so the manager arms a reorg-aware conf watch on
	// the faucet tx.
	regResp := bcRef.Ask(ctx, &batchcanon.RegisterBatchRequest{
		BatchTxID:            *batchTxid,
		ConfirmationPkScript: batchPkScript,
		CSVExpiryDelta:       f2VTXOCSVDelay,
	}).Await(ctx)
	require.True(t, regResp.IsOk(), "register batch with manager")

	// Confirm the batch tx -> Provisional. (The gate is not asserted here;
	// the reconfirm beat below proves Provisional is selectable.)
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *batchTxid)

	// ----------------------------------------------------------------
	// Beat 1: reorg the batch off-chain (stable ReorgedOut) -> the VTXO
	// must be EXCLUDED from coin selection. The replacement branch is
	// mined empty so the batch tx is not auto-re-confirmed.
	// ----------------------------------------------------------------
	reorg := h.Harness.ReorgExcludingMempool(1, 2)
	require.Len(t, reorg.Connected, 2)
	t.Logf(
		"reorg (empty replacement): disconnected=%d connected=%d "+
			"fork_height=%d", len(reorg.Disconnected),
		len(reorg.Connected), reorg.ForkPoint.Height,
	)

	awaitBatchState(ctx, t, bcRef, *batchTxid, batchcanon.StateReorgedOut)

	blockedResp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: vtxoAmount,
	}).Await(ctx)
	require.False(
		t, blockedResp.IsOk(),
		"coin selection must fail while the VTXO's batch is "+
			"reorged out: the only candidate is gated out, "+
			"leaving no liquidity",
	)
	t.Logf(
		"batch ReorgedOut: coin selection correctly excluded %s",
		outpoint,
	)

	// ----------------------------------------------------------------
	// Beat 2: reconfirm the batch -> Provisional -> the VTXO must be
	// ADMITTED again. Mine a normal block so the stranded mempool tx is
	// re-included.
	// ----------------------------------------------------------------
	h.Harness.Generate(1)
	awaitBatchProvisionalAtNoBlock(ctx, t, bcRef, *batchTxid)

	admittedResp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: vtxoAmount,
	}).Await(ctx)
	require.True(
		t, admittedResp.IsOk(),
		"coin selection must succeed once the VTXO's batch reconfirms",
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
		"the reconfirmed VTXO must be selected",
	)
	t.Logf(
		"batch reconfirmed Provisional: coin selection admitted %s",
		outpoint,
	)
}

// awaitBatchProvisionalAtNoBlock polls the manager until the batch is
// Provisional, without asserting a specific confirmation block (the block hash
// changes across the reorg). It reuses the shared awaitBatchRecord poller.
func awaitBatchProvisionalAtNoBlock(ctx context.Context, t *testing.T,
	mgrRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid chainhash.Hash) *batchcanon.Record {

	t.Helper()

	return awaitBatchRecord(
		ctx, t, mgrRef, txid,
		func(rec *batchcanon.Record) bool {
			return rec.State == batchcanon.StateProvisional
		},
		"Provisional",
	)
}

// awaitBatchState polls the manager until the batch reaches the wanted state.
func awaitBatchState(ctx context.Context, t *testing.T,
	mgrRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid chainhash.Hash, want batchcanon.State) *batchcanon.Record {

	t.Helper()

	return awaitBatchRecord(
		ctx, t, mgrRef, txid,
		func(rec *batchcanon.Record) bool {
			return rec.State == want
		},
		"state %v", want,
	)
}

// seedLiveVTXOForBatch persists a single live VTXO whose lineage is anchored on
// batchTxid, returning its outpoint. It is a focused analogue of the directed-
// send systest's seedLiveVTXO: it writes straight to the provided VTXO store
// (SaveVTXO auto-inserts the backing round row) instead of a daemon DB dir, and
// stamps CommitmentTxID = batchTxid so the canonicality gate governs it by that
// batch. The owner/operator keys and tapscript are real so the descriptor is
// well-formed, but they are never used to sign in this test.
func seedLiveVTXOForBatch(t *testing.T, vtxoStore *db.VTXOPersistenceStore,
	name string, batchTxid chainhash.Hash,
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
		OperatorKey:    operatorKey,
		TapScript:      tapScript,
		RoundID:        roundID.String(),
		CommitmentTxID: batchTxid,
		BatchExpiry:    500000,
		RelativeExpiry: f2VTXOCSVDelay,
		CreatedHeight:  1,
		Status:         vtxo.VTXOStatusLive,
	})
	require.NoError(t, err, "save live vtxo")

	return outpoint
}
