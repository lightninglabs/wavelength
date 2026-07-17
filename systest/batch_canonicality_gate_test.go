//go:build systest

package systest

import (
	"bytes"
	"context"
	"testing"
	"time"

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

const (
	// f2VTXOCSVDelay is the relative-expiry CSV delay stamped on the
	// synthetic test VTXO and registered as the batch's CSV expiry delta.
	// The value is arbitrary for this test (expiry is never exercised); it
	// just has to be a valid non-zero delay for descriptor/tapscript
	// construction.
	f2VTXOCSVDelay = 144

	// f2VTXOAmount is the value of the seeded live VTXO. It is the sole
	// coin selection candidate, so its exact value only needs to be a
	// spendable amount comfortably above dust.
	f2VTXOAmount = btcutil.Amount(50_000)

	// f2ChainPollTimeout bounds how long the test waits for a chain
	// observation (confirmation, reorg, reconfirmation) to propagate
	// through LND -> chainsource -> the canonicality manager and land in
	// the durable record. It is generous because a reorg forces a full LND
	// chain resync.
	f2ChainPollTimeout = 90 * time.Second

	// f2ChainPollInterval is how often the chain-observation pollers
	// re-read the durable record.
	f2ChainPollInterval = 500 * time.Millisecond
)

// TestBatchCanonicalityGateBlocksReorgedVTXO proves the LIVE coin-selection
// reorg-safety gate does its job end to end: a VTXO usable at ONE confirmation
// becomes UNAVAILABLE when its batch (commitment tx) is reorged off the
// canonical chain, then USABLE again once the batch reconfirms. This is the F2
// acceptance scenario at the vtxo.Manager seam, driven by a real bitcoind
// reorg.
//
// The wiring mirrors production: a real chainsource actor over the harness
// LND, a real batchcanon.Manager arming reorg-aware watches, and a real
// vtxo.Manager whose ManagerConfig.BatchCanonicality points at the SAME durable
// store the manager writes -- exactly how waved threads s.batchCanonStore into
// the VTXO config when the gate is activated. Only the VTXO is seeded directly
// (as seedLiveVTXO does for the directed-send systest) rather than produced by
// a live round; the round production path is covered by TestSendVTXOEndToEnd.
//
// The batch (commitment) tx is a REAL wire.MsgTx built by the harness: it
// spends one confirmed wallet outpoint to a fresh output. Registration is
// authenticated on the corrected API -- the manager cross-checks the serialized
// tx (hash == BatchTxID, output pkScript, and every TxIn registered) -- so the
// batch must be registered with its serialized bytes and the exact consumed
// input. The seeded VTXO's CommitmentTxID is set to the batch txid so the gate
// (which reloads the full descriptor via GetVTXO and reads its direct
// commitment txid) governs the VTXO by that batch.
//
// To make the "batch off-chain" window deterministic, the reorg mines its
// replacement branch with EMPTY blocks (ReorgExcludingMempool), so the batch tx
// is NOT auto-re-confirmed and the canonicality record holds ReorgedOut stably.
// A plain Reorg would re-mine the tx from the mempool on the first replacement
// block, collapsing the window before coin selection could observe it. A
// subsequent normal block re-confirms the stranded tx.
//
// The proof is a contrast on a single VTXO with a single selection target,
// where the ONLY thing that changes between beats is the batch's chain
// canonicality:
//
//  1. Batch Provisional (1 conf) -> SelectAndReserveSpendRequest succeeds.
//  2. Batch ReorgedOut           -> SelectAndReserveSpendRequest fails.
//  3. Batch reconfirmed          -> SelectAndReserveSpendRequest succeeds.
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

	// Build a real batch (commitment) tx that spends a confirmed wallet
	// outpoint. The corrected registration API authenticates the serialized
	// tx and its exact input set, so we cannot use an opaque sendtoaddress
	// faucet tx (whose inputs we do not control): we must build the tx
	// ourselves so ConsumedInputs matches every TxIn.
	consumedOp, valueBTC, inputPkScript :=
		h.Harness.FirstSpendableOutpoint()
	batchTx, batchTxidStr := h.Harness.BuildSignedSpend(
		consumedOp, valueBTC,
	)
	batchTxid := batchTx.TxHash()
	require.Equal(
		t, batchTxidStr, batchTxid.String(),
		"broadcast txid must match the serialized tx hash",
	)

	var batchBuf bytes.Buffer
	require.NoError(t, batchTx.Serialize(&batchBuf), "serialize batch tx")

	inputValueSat := int64(
		btcutil.Amount(
			valueBTC * btcutil.SatoshiPerBitcoin,
		),
	)
	confirmationPkScript := batchTx.TxOut[0].PkScript

	// Seed a live VTXO anchored on the batch tx BEFORE the manager starts
	// so it is recovered into a resident actor. Its outpoint is synthetic
	// (a VTXO leaf is not the batch tx itself); only its CommitmentTxID
	// matters to the gate.
	outpoint := seedLiveVTXOForBatch(
		t, vtxoStore, t.Name(), batchTxid, f2VTXOAmount,
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

	// Register the batch with authenticated evidence so the manager arms a
	// reorg-aware conf watch on the batch tx plus a spend watch on the
	// consumed input.
	regResp := bcRef.Ask(ctx, &batchcanon.RegisterBatchRequest{
		BatchTxID:            batchTxid,
		BatchTx:              batchBuf.Bytes(),
		BatchOutputIndex:     0,
		ConfirmationPkScript: confirmationPkScript,
		CSVExpiryDelta:       f2VTXOCSVDelay,
		ConsumedInputs: []batchcanon.ConsumedInput{{
			Outpoint: consumedOp,
			Value:    inputValueSat,
			PkScript: inputPkScript,
		}},
	}).Await(ctx)
	require.True(t, regResp.IsOk(), "register batch with manager")

	// ----------------------------------------------------------------
	// Beat 1: confirm the batch at ONE confirmation -> Provisional +
	// Ready -> the VTXO must be ADMITTED into coin selection.
	// ----------------------------------------------------------------
	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, batchTxid)

	assertVTXOSelected(ctx, t, vtxoRef, outpoint)
	t.Logf(
		"batch Provisional (1 conf): coin selection admitted %s",
		outpoint,
	)

	// Release the reservation so the SAME VTXO is a Live candidate again
	// for the reorg beat, and confirm it settled back to Live before
	// reorging so the next exclusion is unambiguously the gate's doing.
	releaseVTXOToLive(ctx, t, vtxoRef, vtxoStore, outpoint)

	// ----------------------------------------------------------------
	// Beat 2: reorg the batch off-chain (stable ReorgedOut) -> the VTXO
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

	awaitBatchState(ctx, t, bcRef, batchTxid, batchcanon.StateReorgedOut)

	blockedResp := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: f2VTXOAmount,
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
	// Beat 3: reconfirm the batch -> Provisional -> the VTXO must be
	// ADMITTED again. Mine a normal block so the stranded mempool tx is
	// re-included.
	// ----------------------------------------------------------------
	h.Harness.Generate(1)
	awaitBatchUsable(ctx, t, bcRef, batchTxid)

	assertVTXOSelected(ctx, t, vtxoRef, outpoint)
	t.Logf(
		"batch reconfirmed Provisional: coin selection admitted %s",
		outpoint,
	)
}

// assertVTXOSelected asserts that a single SelectAndReserveSpendRequest for the
// VTXO amount succeeds and that the given outpoint is among the reserved VTXOs.
func assertVTXOSelected(ctx context.Context, t *testing.T,
	vtxoRef actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp],
	want wire.OutPoint) {

	t.Helper()

	admitted := vtxoRef.Ask(ctx, &vtxo.SelectAndReserveSpendRequest{
		TargetAmount: f2VTXOAmount,
	}).Await(ctx)
	require.True(
		t, admitted.IsOk(),
		"coin selection must succeed while the batch is a ready, "+
			"confirmed member of the canonical chain",
	)

	resp, err := admitted.Unpack()
	require.NoError(t, err)
	selected, ok := resp.(*vtxo.SelectAndReserveSpendResponse)
	require.True(t, ok, "unexpected select response type %T", resp)

	outpoints := make([]wire.OutPoint, 0, len(selected.SelectedVTXOs))
	for _, s := range selected.SelectedVTXOs {
		outpoints = append(outpoints, s.Outpoint)
	}
	require.Contains(t, outpoints, want, "the usable VTXO must be selected")
}

// releaseVTXOToLive releases a previously reserved VTXO and waits until it has
// durably settled back to LiveState, so the same VTXO can be re-tested by a
// later coin-selection beat without a stale reservation or Spending status
// masking the gate's decision.
//
// The spend reservation is detached (the manager marks the outpoint reserved
// in-memory and hands the FSM event to the child without awaiting its write),
// so the release Ask is retried until the child has settled into SpendingState
// and can accept the release.
func releaseVTXOToLive(ctx context.Context, t *testing.T,
	vtxoRef actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp],
	vtxoStore *db.VTXOPersistenceStore, op wire.OutPoint) {

	t.Helper()

	require.Eventually(t, func() bool {
		resp := vtxoRef.Ask(ctx, &vtxo.ReleaseSpendRequest{
			Outpoints: []wire.OutPoint{op},
		}).Await(ctx)

		return resp.IsOk()
	}, f2ChainPollTimeout, f2ChainPollInterval,
		"spend reservation never released back to live")

	require.Eventually(t, func() bool {
		desc, err := vtxoStore.GetVTXO(ctx, op)
		require.NoError(t, err, "load released vtxo status")

		return desc.Status == vtxo.VTXOStatusLive
	}, f2ChainPollTimeout, f2ChainPollInterval,
		"released VTXO never settled back to Live in the store")
}

// awaitBatchUsable polls the manager until the batch record is Ready and its
// state confers a usable (provisional or final) lineage availability -- exactly
// the condition the fail-closed coin-selection gate requires to admit a VTXO.
func awaitBatchUsable(ctx context.Context, t *testing.T,
	mgrRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid chainhash.Hash) *batchcanon.Record {

	t.Helper()

	return awaitBatchRecord(ctx, t, mgrRef, txid,
		func(rec *batchcanon.Record) bool {
			return rec.Ready() &&
				batchcanon.AvailabilityForState(
					rec.State,
				).Usable()
		}, "ready + usable")
}

// awaitBatchState polls the manager until the batch reaches the wanted state.
func awaitBatchState(ctx context.Context, t *testing.T,
	mgrRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid chainhash.Hash, want batchcanon.State) *batchcanon.Record {

	t.Helper()

	return awaitBatchRecord(ctx, t, mgrRef, txid,
		func(rec *batchcanon.Record) bool {
			return rec.State == want
		}, "state %v", want)
}

// awaitBatchRecord polls the batch-canonicality manager's GetBatchStateRequest
// until the returned record satisfies pred, then returns it. It fails the test
// if pred is not satisfied within f2ChainPollTimeout.
func awaitBatchRecord(ctx context.Context, t *testing.T,
	mgrRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid chainhash.Hash, pred func(*batchcanon.Record) bool,
	descFormat string, descArgs ...any) *batchcanon.Record {

	t.Helper()

	var last *batchcanon.Record
	require.Eventuallyf(t, func() bool {
		resp, err := mgrRef.Ask(ctx, &batchcanon.GetBatchStateRequest{
			BatchTxID: txid,
		}).Await(ctx).Unpack()
		if err != nil {
			return false
		}

		state, ok := resp.(*batchcanon.GetBatchStateResponse)
		if !ok || !state.Found || state.Record == nil {
			return false
		}
		last = state.Record

		return pred(state.Record)
	}, f2ChainPollTimeout, f2ChainPollInterval,
		"batch %s never reached "+descFormat,
		append([]any{txid}, descArgs...)...)

	return last
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
