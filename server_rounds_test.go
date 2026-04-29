package darepo

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestRoundsTermsFromConfigIncludesConnectorDustAmount verifies the
// operator's round terms carry the configured connector dust amount.
func TestRoundsTermsFromConfigIncludesConnectorDustAmount(t *testing.T) {
	t.Parallel()

	cfg := DefaultRoundsConfig()
	cfg.ConnectorDustAmount = 777

	terms := roundsTermsFromConfig(cfg)

	require.Equal(
		t, btcutil.Amount(cfg.ConnectorDustAmount),
		terms.ConnectorDustAmount,
	)
}

// TestRestoreConfirmedBatchWatchesRegistersConfirmedTrees verifies startup
// restoration re-registers persisted confirmed round trees with BatchWatcher.
func TestRestoreConfirmedBatchWatchesRegistersConfirmedTrees(t *testing.T) {
	t.Parallel()

	sqlStore := db.NewTestDB(t)
	store := newTestIndexerStore(sqlStore.BaseDB)
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	round := testConfirmedWatchRound(t)
	const confirmationHeight = int32(321)
	err := roundStore.PersistRound(ctx, round)
	require.NoError(t, err)

	err = roundStore.MarkRoundConfirmed(
		ctx, round.RoundID, confirmationHeight, chainhash.Hash{0x02},
	)
	require.NoError(t, err)

	watcher := &captureBatchWatcherRef{}
	server := &Server{
		batchWatcherRef: watcher,
		log:             btclog.Disabled,
	}

	err = server.restoreConfirmedBatchWatches(ctx, roundStore)
	require.NoError(t, err)

	require.Len(t, watcher.registers, 1)

	req := watcher.registers[0]
	require.Equal(
		t, batchwatcher.BatchIDForRoundOutput(
			uuid.UUID(round.RoundID), 0,
		), req.BatchID,
	)
	require.Equal(t, uint32(confirmationHeight), req.ConfirmationHeight)
	require.Equal(
		t, uint32(confirmationHeight)+round.CSVDelay, req.ExpiryHeight,
	)
	require.NotNil(t, req.Tree)
	require.Equal(
		t, round.VTXOTrees[0].BatchOutpoint, req.Tree.BatchOutpoint,
	)
}

// captureBatchWatcherRef records RegisterBatchRequest messages for tests.
type captureBatchWatcherRef struct {
	registers []*batchwatcher.RegisterBatchRequest
}

// batchWatcherFuture aliases the verbose batch watcher actor future type.
type batchWatcherFuture = actor.Future[batchwatcher.BatchWatcherResp]

// ID returns the stable actor ID for the capture reference.
func (c *captureBatchWatcherRef) ID() string {
	return "capture-batch-watcher"
}

// Tell records register-batch requests and rejects other messages.
func (c *captureBatchWatcherRef) Tell(_ context.Context,
	msg batchwatcher.BatchWatcherMsg) error {

	req, ok := msg.(*batchwatcher.RegisterBatchRequest)
	if !ok {
		return fmt.Errorf("unexpected batch watcher message %T", msg)
	}

	c.registers = append(c.registers, req)

	return nil
}

// Ask returns an unsupported-operation response for the capture reference.
func (c *captureBatchWatcherRef) Ask(_ context.Context,
	msg batchwatcher.BatchWatcherMsg) batchWatcherFuture {

	promise := actor.NewPromise[batchwatcher.BatchWatcherResp]()
	_ = promise.Complete(fn.Err[batchwatcher.BatchWatcherResp](
		fmt.Errorf("unexpected batch watcher ask %T", msg),
	))

	return promise.Future()
}

// testConfirmedWatchRound returns a minimal persisted round with one VTXO tree.
func testConfirmedWatchRound(t *testing.T) *rounds.Round {
	t.Helper()

	roundID := rounds.RoundID(uuid.New())
	finalTx := wire.NewMsgTx(2)
	finalTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{0x01},
		},
	})
	finalTx.AddTxOut(&wire.TxOut{
		Value:    100_000,
		PkScript: []byte{0x51},
	})

	batchOutpoint := wire.OutPoint{
		Hash: finalTx.TxHash(),
	}

	sweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const csvDelay = uint32(144)
	sweepRoot := testSweepTaproot(t, sweepKey.PubKey(), csvDelay)
	vtxoTree := &tree.Tree{
		BatchOutpoint: batchOutpoint,
		BatchOutput:   finalTx.TxOut[0],
		Root: &tree.Node{
			Input: batchOutpoint,
			Outputs: []*wire.TxOut{
				{
					Value:    100_000,
					PkScript: []byte{0x51},
				},
			},
			CoSigners: []*btcec.PublicKey{sweepKey.PubKey()},
			Children:  make(map[uint32]*tree.Node),
			Amount:    btcutil.Amount(100_000),
		},
		SweepTapscriptRoot: sweepRoot,
	}

	return &rounds.Round{
		RoundID:              roundID,
		FinalTx:              finalTx,
		VTXOTrees:            map[int]*tree.Tree{0: vtxoTree},
		ConnectorDescriptors: []*rounds.ConnectorTreeDescriptor{},
		ForfeitInfos: make(
			map[wire.OutPoint]*rounds.ForfeitInfo,
		),
		ClientRegistrations: make(
			map[rounds.ClientID]*rounds.ClientRegistration,
		),
		SweepKey: sweepKey.PubKey(),
		CSVDelay: csvDelay,
	}
}

// testSweepTaproot returns the sweep tapscript root for a test VTXO tree.
func testSweepTaproot(t *testing.T, sweepKey *btcec.PublicKey,
	csvDelay uint32) []byte {

	t.Helper()

	sweepTapLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey, csvDelay,
	)
	require.NoError(t, err)

	sweepTapRoot := sweepTapLeaf.TapHash()

	return sweepTapRoot[:]
}
