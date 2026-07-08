//go:build systest

package systest

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// batchCanonPollInterval is how often the systest re-reads the manager's
// persisted batch record while waiting for an expected canonicality state.
// It is shorter than reorgSystestEventTimeout so several reads land inside a
// single event-propagation window; 250ms keeps the Ask traffic on the
// manager mailbox light without lengthening the test materially.
const batchCanonPollInterval = 250 * time.Millisecond

// TestBatchCanonicalityReorgRoundTrip drives a real bitcoind reorg through the
// full batch-canonicality pipeline and proves the manager re-anchors a batch's
// interpreted canonicality to the replacement chain:
//
//	bitcoind invalidate/mine
//	  -> lnd chainntnfs (in-process)
//	    -> lndclient gRPC (WithReOrgChan)
//	      -> chainbackends.LNDBackend (multi-shot forwarder)
//	        -> chainsource.ConfActor (reorg-aware mode)
//	          -> batchcanon.Manager (conf/reorg interpretation)
//	            -> db.BatchCanonicalityPersistenceStore (durable record)
//
// The batchcanon unit tests (batchcanon/manager_test.go) already prove the
// StateProvisional -> StateReorgedOut -> StateProvisional transition against a
// mock conf actor, including the transient reorged-out beat. They cannot prove
// that lndclient.WithReOrgChan actually fires the reorg signal over the real
// gRPC transport, nor that the durable store survives the round-trip. This
// test is the systest-level oracle for that.
//
// The oracle is re-anchoring: a batch confirmed at block X must, after the
// reorg, end up Provisional again but confirmed at a DIFFERENT block Y that
// belongs to the replacement branch. The only way the manager's persisted
// confirmation block can move from X to Y is the full reorg -> re-confirmation
// round-trip (a non-reorg-aware watch would stay pinned to X forever). The
// intermediate reorged-out state is transient — bitcoind preserves the tx in
// its mempool across the invalidate so it re-confirms on the first new block —
// so it is not asserted here (the unit tests own that beat); the block-hash
// move is the end-to-end proof.
//
// The flow is:
//
//  1. Faucet to a synthetic P2WPKH pkScript (no spendable key needed) so we
//     know the batch txid + confirmation pkScript up front.
//  2. Register the batch with the manager BEFORE mining, exercising the
//     live-detection conf watch rather than the historical-backfill path.
//  3. Mine one block. Assert StateProvisional anchored to the mined block
//     (one conf is < DefaultFinalityDepth, so not yet Finalized).
//  4. Drive a 1-block reorg replaced by a longer 2-block branch. Assert the
//     batch becomes Provisional again, re-anchored to a NEW block on the
//     replacement branch.
func TestBatchCanonicalityReorgRoundTrip(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t)
	ctx := h.Context()

	// Spawn a real chainsource actor over the harness's LND, then build
	// the batch-canonicality manager on top of a real durable store.
	chainSource := h.NewChainSourceActor()

	sqlDB := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	canonStore := dbStore.NewBatchCanonicalityStore(
		clock.NewDefaultClock(),
	)

	mgr := batchcanon.NewManager(batchcanon.ManagerConfig{
		Store:       canonStore,
		ChainSource: chainSource,
		Log:         fn.Some(h.SubLogger("BCAN")),
	})
	mgrRef := actor.RegisterWithSystem(
		h.ActorSystem(),
		"batch-canonicality", batchcanon.ManagerServiceKey, mgr,
	)
	mgr.SetSelfRef(mgrRef)

	// Build a synthetic P2WPKH address from a deterministic per-test
	// pubkey hash. We never spend it; we only need a known pkScript to
	// faucet to and register a confirmation watch on.
	pubKeyHash := sha256.Sum256([]byte(t.Name()))
	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		pubKeyHash[:20], &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err, "build synthetic P2WPKH address")
	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err, "derive pkScript for synthetic address")

	// Faucet first so we have the txid before registering the watch. The
	// watch keys on (txid, pkScript), so the ordering between mempool
	// entry and registration is fine; what must NOT happen is mining the
	// block before registering, which would dispatch historical conf
	// state and race two delivery paths.
	amount := btcutil.Amount(btcutil.SatoshiPerBitcoin / 100)
	txidStr := h.Harness.Faucet(addr.String(), amount)
	txid, err := chainhash.NewHashFromStr(txidStr)
	require.NoError(t, err, "parse faucet txid")

	// Register the batch. CSVExpiryDelta is a representative non-zero
	// value; this test does not exercise expiry. No consumed inputs or
	// dependent VTXOs are needed to observe the conf/reorg lifecycle.
	const csvExpiryDelta = 144
	regResp := mgrRef.Ask(ctx, &batchcanon.RegisterBatchRequest{
		BatchTxID:            *txid,
		ConfirmationPkScript: pkScript,
		CSVExpiryDelta:       csvExpiryDelta,
	}).Await(ctx)
	require.True(t, regResp.IsOk(), "register batch with manager")

	// 1. Mine the block that confirms the faucet tx and assert the batch
	// becomes Provisional, anchored to the mined block.
	originalBlocks := h.Harness.Generate(1)
	require.Len(t, originalBlocks, 1)
	originalHash, err := chainhash.NewHashFromStr(originalBlocks[0].Hash)
	require.NoError(t, err, "parse original block hash")

	rec := awaitBatchProvisionalAt(ctx, t, mgrRef, *txid, *originalHash)
	t.Logf(
		"batch %s Provisional at height %d block %s", txid,
		rec.ConfirmationHeight.UnwrapOr(0), originalHash,
	)

	// 2. Drive a reorg: invalidate the confirmation block and mine a
	// strictly longer (2-block) replacement branch. bitcoind preserves
	// the tx in its mempool across the invalidate, so it re-confirms in
	// the new chain. The manager's conf watch fires its reorg and then a
	// fresh confirmation, re-anchoring the record to the new branch.
	reorg := h.Harness.Reorg(1, 2)
	require.Equal(
		t, originalBlocks[0].Hash, reorg.Disconnected[0].Hash,
		"the reorg should have disconnected the confirmation block",
	)
	require.Len(t, reorg.Connected, 2)
	t.Logf(
		"reorg: disconnected=%d connected=%d fork_height=%d",
		len(reorg.Disconnected), len(reorg.Connected),
		reorg.ForkPoint.Height,
	)

	// 3. Assert the manager re-anchored the batch to a NEW block on the
	// replacement branch, proving the reorg round-trip propagated all the
	// way to the durable canonicality record.
	reanchored := awaitBatchReanchored(
		ctx, t, mgrRef, *txid, *originalHash,
	)
	newHash := reanchored.ConfirmationBlock.UnwrapOr(chainhash.Hash{})

	replacementHashes := make(map[string]struct{}, len(reorg.Connected))
	for _, blk := range reorg.Connected {
		replacementHashes[blk.Hash] = struct{}{}
	}
	require.Contains(
		t, replacementHashes, newHash.String(),
		"re-confirmation block must belong to the replacement branch",
	)
	t.Logf(
		"batch %s re-anchored Provisional at height %d block %s", txid,
		reanchored.ConfirmationHeight.UnwrapOr(0), newHash,
	)
}

// awaitBatchProvisionalAt polls the manager's persisted record until the batch
// is Provisional and anchored to the wanted confirmation block, failing on
// timeout. The returned record is the one observed in that state.
func awaitBatchProvisionalAt(ctx context.Context, t *testing.T,
	mgrRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid, wantBlock chainhash.Hash) *batchcanon.Record {

	t.Helper()

	return awaitBatchRecord(
		ctx, t, mgrRef, txid,
		func(rec *batchcanon.Record) bool {
			return rec.State == batchcanon.StateProvisional &&
				rec.ConfirmationBlock.UnwrapOr(
					chainhash.Hash{},
				) == wantBlock
		},
		"Provisional anchored at %s", wantBlock,
	)
}

// awaitBatchReanchored polls the manager's persisted record until the batch is
// Provisional and anchored to a confirmation block DIFFERENT from oldBlock,
// failing on timeout. This is the re-anchoring oracle: it can only succeed if
// the reorg disconnected the original confirmation and a fresh confirmation
// re-anchored the record to the replacement chain.
func awaitBatchReanchored(ctx context.Context, t *testing.T,
	mgrRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid, oldBlock chainhash.Hash) *batchcanon.Record {

	t.Helper()

	return awaitBatchRecord(
		ctx, t, mgrRef, txid,
		func(rec *batchcanon.Record) bool {
			if rec.State != batchcanon.StateProvisional ||
				rec.ConfirmationBlock.IsNone() {
				return false
			}
			block := rec.ConfirmationBlock.UnwrapOr(
				chainhash.Hash{},
			)

			return block != oldBlock
		},
		"Provisional re-anchored off %s", oldBlock,
	)
}

// awaitBatchRecord polls the manager's persisted record for a batch until pred
// holds, failing the test on timeout. Because the conf/reorg events propagate
// asynchronously over the real gRPC transport, we retry rather than read once.
// The returned record is the one that satisfied pred.
func awaitBatchRecord(ctx context.Context, t *testing.T,
	mgrRef actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp],
	txid chainhash.Hash, pred func(*batchcanon.Record) bool,
	wantDesc string, wantArgs ...any) *batchcanon.Record {

	t.Helper()

	var matched *batchcanon.Record
	require.Eventuallyf(
		t, func() bool {
			resp, err := mgrRef.Ask(
				ctx, &batchcanon.GetBatchStateRequest{
					BatchTxID: txid,
				},
			).Await(ctx).Unpack()
			if err != nil {
				return false
			}
			got, ok := resp.(*batchcanon.GetBatchStateResponse)
			if !ok || !got.Found {
				return false
			}
			if !pred(got.Record) {
				return false
			}
			matched = got.Record

			return true
		}, reorgSystestEventTimeout, batchCanonPollInterval,
		"batch %s never reached state: "+wantDesc,
		append([]any{txid}, wantArgs...)...,
	)

	return matched
}
