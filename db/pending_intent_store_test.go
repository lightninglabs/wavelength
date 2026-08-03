package db

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/stretchr/testify/require"
)

// pendingIntentStoreHarness bundles the persistence store with raw query
// access so tests can exercise the CommitState-side anchor clears that the
// wallet-facing interface intentionally does not expose.
type pendingIntentStoreHarness struct {
	store *PendingIntentPersistenceStore
	raw   *TransactionExecutor[PendingIntentStore]
}

// newPendingIntentStoreForTest creates a pending-intent store backed by a
// fresh test database.
func newPendingIntentStoreForTest(t *testing.T) *pendingIntentStoreHarness {
	t.Helper()

	db := NewTestDB(t)

	intentDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) PendingIntentStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return &pendingIntentStoreHarness{
		store: NewPendingIntentPersistenceStore(intentDB),
		raw:   intentDB,
	}
}

// makePendingIntent builds a pending intent from the given concrete payload
// over the given anchors with a deterministic ID.
func makePendingIntent(payload wallet.PendingIntentPayload, requestedAt int64,
	anchors ...wire.OutPoint) wallet.PendingIntent {

	return wallet.PendingIntent{
		ID:          wallet.NewPendingIntentID(payload, anchors),
		Payload:     payload,
		RequestedAt: requestedAt,
		Anchors:     anchors,
	}
}

// boardPayload builds a board payload with the given target VTXO count.
func boardPayload(target uint32) *wallet.BoardIntentPayload {
	return &wallet.BoardIntentPayload{TargetVTXOCount: target}
}

// sendPayload builds a minimal valid bounded send payload with the given
// target amount, used to give distinct payloads (hence distinct IDs).
func sendPayload(amountSat int64) *wallet.SendOnChainIntentPayload {
	return &wallet.SendOnChainIntentPayload{
		DestinationPkScript: append(
			[]byte{0x51, 0x20}, make([]byte, 32)...,
		),
		TargetAmountSat: btcutil.Amount(amountSat),
	}
}

// TestPendingIntentUpsertListRoundtrip verifies that an upserted intent
// lists back with its payload and full anchor set, and that kinds are
// isolated from each other.
func TestPendingIntentUpsertListRoundtrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	h := newPendingIntentStoreForTest(t)

	opA := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 0}
	opB := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 7}
	opC := wire.OutPoint{Hash: chainhash.Hash{0xcc}, Index: 1}

	board := makePendingIntent(boardPayload(1), 100, opA, opB)
	send := makePendingIntent(sendPayload(3), 200, opC)

	require.NoError(t, h.store.UpsertPendingIntent(ctx, board))
	require.NoError(t, h.store.UpsertPendingIntent(ctx, send))

	gotBoard, err := h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindBoard,
	)
	require.NoError(t, err)
	require.Len(t, gotBoard, 1)
	require.Equal(t, board.ID, gotBoard[0].ID)
	require.Equal(t, board.Payload, gotBoard[0].Payload)
	require.Equal(t, board.RequestedAt, gotBoard[0].RequestedAt)
	require.ElementsMatch(t, board.Anchors, gotBoard[0].Anchors)

	gotSend, err := h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindSendOnChain,
	)
	require.NoError(t, err)
	require.Len(t, gotSend, 1)
	require.Equal(t, send.ID, gotSend[0].ID)
	require.ElementsMatch(t, send.Anchors, gotSend[0].Anchors)

	// Re-upserting the same intent with a fresh timestamp updates in
	// place rather than duplicating.
	board.RequestedAt = 150
	require.NoError(t, h.store.UpsertPendingIntent(ctx, board))

	gotBoard, err = h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindBoard,
	)
	require.NoError(t, err)
	require.Len(t, gotBoard, 1)
	require.EqualValues(t, 150, gotBoard[0].RequestedAt)
}

// TestPendingIntentBoardCustomPolicyRoundtrip verifies that a board intent's
// custom VTXO policy template and pinned pk_script persist to their typed
// columns and list back byte-for-byte, so restart replay recreates the same
// custom-owned output. This exercises the full persistence path: the
// 000016 migration columns, the sqlc upsert/list, and the store's nil-vs-empty
// handling.
func TestPendingIntentBoardCustomPolicyRoundtrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	h := newPendingIntentStoreForTest(t)

	opA := wire.OutPoint{Hash: chainhash.Hash{0xd1}, Index: 0}

	payload := &wallet.BoardIntentPayload{
		TargetVTXOCount: 4,
		PolicyTemplate: []byte{
			0x01,
			0xde,
			0xad,
			0xbe,
			0xef,
		},
		PkScript: append(
			[]byte{0x51, 0x20}, make([]byte, 32)...,
		),
	}
	board := makePendingIntent(payload, 100, opA)
	require.NoError(t, h.store.UpsertPendingIntent(ctx, board))

	got, err := h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindBoard,
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, board.Payload, got[0].Payload)

	gotPayload, ok := got[0].Payload.(*wallet.BoardIntentPayload)
	require.True(t, ok)
	require.Equal(t, payload.PolicyTemplate, gotPayload.PolicyTemplate)
	require.Equal(t, payload.PkScript, gotPayload.PkScript)
}

// TestPendingIntentAnchorRebindSweepsOrphan verifies that a newer intent
// claiming an older intent's anchors rebinds them, and that an older parent
// left with zero anchors is swept in the same transaction.
func TestPendingIntentAnchorRebindSweepsOrphan(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	h := newPendingIntentStoreForTest(t)

	opA := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 0}
	opB := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 1}

	older := makePendingIntent(boardPayload(1), 100, opA, opB)
	require.NoError(t, h.store.UpsertPendingIntent(ctx, older))

	// A newer board intent (different payload, hence different ID)
	// claims both anchors: the older parent must vanish.
	newer := makePendingIntent(boardPayload(2), 200, opA, opB)
	require.NoError(t, h.store.UpsertPendingIntent(ctx, newer))

	got, err := h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindBoard,
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, newer.ID, got[0].ID)
	require.ElementsMatch(t, []wire.OutPoint{opA, opB}, got[0].Anchors)
}

// TestPendingIntentDelete verifies single-intent deletion removes the parent
// and its anchors while leaving other intents untouched.
func TestPendingIntentDelete(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	h := newPendingIntentStoreForTest(t)

	opA := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 0}
	opB := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 1}

	first := makePendingIntent(sendPayload(1), 100, opA)
	second := makePendingIntent(sendPayload(2), 200, opB)
	require.NoError(t, h.store.UpsertPendingIntent(ctx, first))
	require.NoError(t, h.store.UpsertPendingIntent(ctx, second))

	require.NoError(t, h.store.DeletePendingIntent(ctx, first.ID))

	got, err := h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindSendOnChain,
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, second.ID, got[0].ID)

	// Deleting an already-deleted intent is a benign no-op.
	require.NoError(t, h.store.DeletePendingIntent(ctx, first.ID))
}

// TestPendingIntentClearByKind verifies the bulk stale sweep only touches
// the requested kind.
func TestPendingIntentClearByKind(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	h := newPendingIntentStoreForTest(t)

	opA := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 0}
	opB := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 1}

	board := makePendingIntent(boardPayload(1), 100, opA)
	send := makePendingIntent(sendPayload(2), 200, opB)
	require.NoError(t, h.store.UpsertPendingIntent(ctx, board))
	require.NoError(t, h.store.UpsertPendingIntent(ctx, send))

	require.NoError(
		t, h.store.ClearPendingIntentsByKind(
			ctx, wallet.PendingIntentKindBoard,
		),
	)

	gotBoard, err := h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindBoard,
	)
	require.NoError(t, err)
	require.Empty(t, gotBoard)

	gotSend, err := h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindSendOnChain,
	)
	require.NoError(t, err)
	require.Len(t, gotSend, 1)
}

// TestPendingIntentAnchorClearByOutpoint exercises the CommitState-side
// query surface directly: clearing anchors one outpoint at a time, then
// sweeping orphaned parents, must remove a fully-adopted intent while
// leaving partially-adopted intents listable with their surviving anchors.
func TestPendingIntentAnchorClearByOutpoint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	h := newPendingIntentStoreForTest(t)

	opA := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 0}
	opB := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 1}

	intent := makePendingIntent(boardPayload(1), 100, opA, opB)
	require.NoError(t, h.store.UpsertPendingIntent(ctx, intent))

	clearAnchor := func(op wire.OutPoint) {
		err := h.raw.ExecTx(
			ctx, WriteTxOption(),
			func(q PendingIntentStore) error {
				err := q.ClearPendingIntentAnchorByOutpoint(
					ctx,
					//nolint:ll
					sqlc.ClearPendingIntentAnchorByOutpointParams{
						OutpointHash: op.Hash[:],
						OutpointIndex: int32(
							op.Index,
						),
					},
				)
				if err != nil {
					return err
				}

				// Mirror CommitState: sweep orphaned detail
				// rows before the now-anchorless headers they
				// foreign-key.
				err = q.DeleteOrphanedPendingBoardIntents(ctx)
				if err != nil {
					return err
				}

				err = q.DeleteOrphanedPendingSendIntents(ctx)
				if err != nil {
					return err
				}

				return q.DeleteOrphanedPendingIntents(ctx)
			},
		)
		require.NoError(t, err)
	}

	// Clearing one of two anchors leaves the intent listable with the
	// surviving anchor (partial adoption).
	clearAnchor(opA)

	got, err := h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindBoard,
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.ElementsMatch(t, []wire.OutPoint{opB}, got[0].Anchors)

	// Clearing the final anchor sweeps the parent row too.
	clearAnchor(opB)

	got, err = h.store.ListPendingIntents(
		ctx, wallet.PendingIntentKindBoard,
	)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestNewPendingIntentIDDeterminism verifies the derived ID is stable under
// anchor reordering and sensitive to kind, anchors, and payload fields.
func TestNewPendingIntentIDDeterminism(t *testing.T) {
	t.Parallel()

	opA := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 0}
	opB := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 1}

	idAB := wallet.NewPendingIntentID(
		boardPayload(1), []wire.OutPoint{opA, opB},
	)
	idBA := wallet.NewPendingIntentID(
		boardPayload(1), []wire.OutPoint{opB, opA},
	)
	require.Equal(t, idAB, idBA, "ID must be anchor-order independent")

	// Different kind (send vs board) over the same anchors → different ID.
	idOtherKind := wallet.NewPendingIntentID(
		sendPayload(1), []wire.OutPoint{opA, opB},
	)
	require.NotEqual(t, idAB, idOtherKind)

	// Different payload fields → different ID.
	idOtherPayload := wallet.NewPendingIntentID(
		boardPayload(2), []wire.OutPoint{opA, opB},
	)
	require.NotEqual(t, idAB, idOtherPayload)

	// Fewer anchors → different ID.
	idFewerAnchors := wallet.NewPendingIntentID(
		boardPayload(1), []wire.OutPoint{opA},
	)
	require.NotEqual(t, idAB, idFewerAnchors)
}
