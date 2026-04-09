package oor

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// makeTestP2TRScript returns a deterministic 34-byte P2TR pkScript seeded by
// the provided byte so DB-backed tests accept the script length.
func makeTestP2TRScript(seed byte) []byte {
	script := make([]byte, 34)
	script[0] = 0x51 // OP_1
	script[1] = 0x20 // PUSH 32
	script[2] = seed

	return script
}

// makeTestPSBT builds a compact PSBT fixture with one input/output.
func makeTestPSBT(t *testing.T, seed byte) *psbt.Packet {
	t.Helper()

	tx := wire.NewMsgTx(2)

	var hash chainhash.Hash
	hash[0] = seed

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  hash,
			Index: uint32(seed),
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(1000 + int(seed)),
		PkScript: makeTestP2TRScript(seed),
	})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	// Populate WitnessUtxo so callers can read input value/script.
	pkt.Inputs[0].WitnessUtxo = &wire.TxOut{
		Value:    1000,
		PkScript: makeTestP2TRScript(seed),
	}

	return pkt
}

// makeTestSessionID returns a deterministic session ID from an Ark PSBT.
func makeTestSessionID(arkPSBT *psbt.Packet) SessionID {
	return SessionID(arkPSBT.UnsignedTx.TxHash())
}

// newTestSessionStore creates a session store backed by a test DB.
func newTestSessionStore(t *testing.T) (*DBSessionStore,
	db.BatchedQuerier) {

	t.Helper()

	dbh := db.NewTestDB(t)

	return NewDBSessionStore(
		dbh, clock.NewDefaultClock(), btclog.Disabled,
	), dbh
}

// TestUpsertCoSignedBasic verifies basic UpsertCoSigned persistence and
// retrieval via LoadActiveSessions.
func TestUpsertCoSignedBasic(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 1)
	checkpoint := makeTestPSBT(t, 2)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	sessions, err := store.LoadActiveSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	s := sessions[0]
	require.Equal(t, sessionID, s.SessionID)
	require.Equal(t, oorStateCoSigned, s.State)
	require.Len(t, s.Inputs, 1)
	require.Equal(t, input, s.Inputs[0])
	require.NotNil(t, s.ArkPSBT)
	require.Len(t, s.CheckpointPSBTs, 1)
}

// TestUpsertCoSignedIdempotent verifies that UpsertCoSigned is idempotent
// when called with the same payload.
func TestUpsertCoSignedIdempotent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 10)
	checkpoint := makeTestPSBT(t, 11)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	for i := 0; i < 3; i++ {
		err := store.UpsertCoSigned(
			ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
			[]*psbt.Packet{checkpoint},
			time.Now().Add(DefaultSessionExpiry),
		)
		require.NoError(t, err)
	}

	sessions, err := store.LoadActiveSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
}

// TestUpsertCoSignedRejectsAwaitingNotifyRegression verifies that a session
// already advanced to awaiting_notify cannot be regressed to co-signed.
func TestUpsertCoSignedRejectsAwaitingNotifyRegression(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 12)
	checkpoint := makeTestPSBT(t, 13)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	finalCheckpoint := makeTestPSBT(t, 14)
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)

	err = store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.ErrorContains(t, err, "cannot upsert co-signed session")
	require.ErrorContains(t, err, string(oorStateAwaitingNotify))
}

// TestUpsertCoSignedRejectsFinalizedRegression verifies that a finalized
// session cannot be regressed to co-signed.
func TestUpsertCoSignedRejectsFinalizedRegression(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 15)
	checkpoint := makeTestPSBT(t, 16)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	finalCheckpoint := makeTestPSBT(t, 17)
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)

	err = store.MarkNotified(ctx, sessionID)
	require.NoError(t, err)

	err = store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.ErrorContains(t, err, "cannot upsert co-signed session")
	require.ErrorContains(t, err, string(oorStateFinalized))
}

// TestUpsertCoSignedRejectsArkPSBTMismatch verifies that upsert retries for an
// existing co-signed session must use the same Ark PSBT bytes.
func TestUpsertCoSignedRejectsArkPSBTMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 18)
	checkpoint := makeTestPSBT(t, 19)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	mismatchArk := makeTestPSBT(t, 20)
	err = store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, mismatchArk,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.ErrorContains(t, err, "co-signed ark psbt mismatch")
}

// TestApplyFinalizeFromCoSigned verifies the cosigned -> awaiting_notify
// transition.
func TestApplyFinalizeFromCoSigned(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 20)
	checkpoint := makeTestPSBT(t, 21)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	// Build a "finalized" checkpoint (use a different seed for
	// distinct bytes).
	finalCheckpoint := makeTestPSBT(t, 22)

	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)

	// Session should now be awaiting_notify.
	sessions, err := store.LoadActiveSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, oorStateAwaitingNotify, sessions[0].State)
}

// TestApplyFinalizeIdempotentSamePayload verifies repeat ApplyFinalize with
// the same checkpoint payload succeeds.
func TestApplyFinalizeIdempotentSamePayload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 30)
	checkpoint := makeTestPSBT(t, 31)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	finalCheckpoint := makeTestPSBT(t, 32)

	// First call transitions cosigned -> awaiting_notify.
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)

	// Second call with same payload is idempotent.
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)
}

// TestApplyFinalizeRejectsDifferentPayload verifies repeat ApplyFinalize with
// different checkpoint payload returns an error.
func TestApplyFinalizeRejectsDifferentPayload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 40)
	checkpoint := makeTestPSBT(t, 41)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	finalCheckpoint := makeTestPSBT(t, 42)

	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)

	// Different payload should fail.
	differentCheckpoint := makeTestPSBT(t, 43)
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{differentCheckpoint},
	)
	require.ErrorContains(t, err, "payload mismatch")
}

// TestApplyFinalizeOnAlreadyFinalizedSucceeds verifies that calling
// ApplyFinalize on a session already past awaiting_notify (i.e. finalized)
// returns success.
func TestApplyFinalizeOnAlreadyFinalizedSucceeds(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 50)
	checkpoint := makeTestPSBT(t, 51)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	finalCheckpoint := makeTestPSBT(t, 52)
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)

	err = store.MarkNotified(ctx, sessionID)
	require.NoError(t, err)

	// ApplyFinalize on already-finalized returns success.
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)
}

// TestMarkNotifiedTransition verifies awaiting_notify -> finalized.
func TestMarkNotifiedTransition(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 60)
	checkpoint := makeTestPSBT(t, 61)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	finalCheckpoint := makeTestPSBT(t, 62)
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)

	err = store.MarkNotified(ctx, sessionID)
	require.NoError(t, err)

	// After MarkNotified, session should no longer be in active list.
	sessions, err := store.LoadActiveSessions(ctx)
	require.NoError(t, err)
	require.Empty(t, sessions)
}

// TestMarkNotifiedIdempotent verifies MarkNotified is idempotent when
// already finalized.
func TestMarkNotifiedIdempotent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 70)
	checkpoint := makeTestPSBT(t, 71)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	finalCheckpoint := makeTestPSBT(t, 72)
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)

	// First MarkNotified succeeds.
	err = store.MarkNotified(ctx, sessionID)
	require.NoError(t, err)

	// Second MarkNotified is idempotent.
	err = store.MarkNotified(ctx, sessionID)
	require.NoError(t, err)
}

// TestLoadFinalizedPackage verifies the finalized package can be loaded
// after ApplyFinalize.
func TestLoadFinalizedPackage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store, _ := newTestSessionStore(t)

	arkPSBT := makeTestPSBT(t, 80)
	checkpoint := makeTestPSBT(t, 81)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	err := store.UpsertCoSigned(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
	)
	require.NoError(t, err)

	finalCheckpoint := makeTestPSBT(t, 82)
	err = store.ApplyFinalize(
		ctx, sessionID, []*psbt.Packet{finalCheckpoint},
	)
	require.NoError(t, err)

	pkg, err := store.LoadFinalizedPackage(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, pkg)
	require.NotNil(t, pkg.ArkPSBT)
	require.Len(t, pkg.FinalCheckpointPSBTs, 1)

	// Verify Ark txid matches.
	expectedTxid := arkPSBT.UnsignedTx.TxHash()
	actualTxid := pkg.ArkPSBT.UnsignedTx.TxHash()
	require.Equal(t, expectedTxid, actualTxid)
}

// TestAtomicCoSignedAndMarkInFlight verifies atomic session+lock persistence.
func TestAtomicCoSignedAndMarkInFlight(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	dbh := db.NewTestDB(t)
	dbStore := db.NewStore(
		dbh.DB, dbh.Queries, dbh.Backend(), btclog.Disabled,
		clock.NewDefaultClock(),
	)

	store := NewDBSessionStore(
		dbStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	recordStore := dbStore.NewVTXORecordStore()

	arkPSBT := makeTestPSBT(t, 90)
	checkpoint := makeTestPSBT(t, 91)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	// Create the VTXO record so it can be locked.
	err := recordStore.Create(ctx, &vtxo.Record{
		Outpoint: input,
		Value:    checkpoint.Inputs[0].WitnessUtxo.Value,
		PkScript: checkpoint.Inputs[0].WitnessUtxo.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	owner := vtxo.OORLockOwner(sessionID.String())
	err = store.UpsertCoSignedAndMarkInFlight(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry), owner,
	)
	require.NoError(t, err)

	// Verify session persisted.
	row, err := dbh.Queries.GetOORSession(
		ctx, sessionIDBytes(sessionID),
	)
	require.NoError(t, err)
	require.Equal(t, string(oorStateCoSigned), row.State)

	// Verify VTXO locked.
	record, err := recordStore.Get(ctx, input)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, vtxo.StatusInFlight, record.Status)
	require.Equal(t, owner, record.InFlightOwner)
}

// TestAtomicCoSignedRollbackOnLockFailure verifies session is rolled back when
// VTXO lock fails (e.g. unknown VTXO).
func TestAtomicCoSignedRollbackOnLockFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	dbh := db.NewTestDB(t)
	dbStore := db.NewStore(
		dbh.DB, dbh.Queries, dbh.Backend(), btclog.Disabled,
		clock.NewDefaultClock(),
	)

	store := NewDBSessionStore(
		dbStore, clock.NewDefaultClock(), btclog.Disabled,
	)

	arkPSBT := makeTestPSBT(t, 95)
	checkpoint := makeTestPSBT(t, 96)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)

	// Don't create the VTXO record — lock should fail.
	err := store.UpsertCoSignedAndMarkInFlight(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry),
		vtxo.OORLockOwner(sessionID.String()),
	)
	require.Error(t, err)

	// Session should not be persisted.
	_, err = dbh.Queries.GetOORSession(
		ctx, sessionIDBytes(sessionID),
	)
	require.True(t, errors.Is(err, sql.ErrNoRows))
}

// TestAtomicFinalizeAndMaterialize verifies that finalize persists the session
// transition and the VTXO set mutations in one DB transaction.
func TestAtomicFinalizeAndMaterialize(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	dbh := db.NewTestDB(t)
	dbStore := db.NewStore(
		dbh.DB, dbh.Queries, dbh.Backend(), btclog.Disabled,
		clock.NewDefaultClock(),
	)

	store := NewDBSessionStore(
		dbStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	recordStore := dbStore.NewVTXORecordStore()

	arkPSBT := makeTestPSBT(t, 100)
	checkpoint := makeTestPSBT(t, 101)
	finalCheckpoint := makeTestPSBT(t, 102)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)
	owner := vtxo.OORLockOwner(sessionID.String())

	err := recordStore.Create(ctx, &vtxo.Record{
		Outpoint: input,
		Value:    checkpoint.Inputs[0].WitnessUtxo.Value,
		PkScript: checkpoint.Inputs[0].WitnessUtxo.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	err = store.UpsertCoSignedAndMarkInFlight(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry), owner,
	)
	require.NoError(t, err)

	output := &vtxo.Record{
		Outpoint: wire.OutPoint{
			Hash:  arkPSBT.UnsignedTx.TxHash(),
			Index: 0,
		},
		Value: arkPSBT.UnsignedTx.TxOut[0].Value,
		PkScript: append(
			[]byte(nil), arkPSBT.UnsignedTx.TxOut[0].PkScript...,
		),
		Status: vtxo.StatusLive,
	}

	err = store.ApplyFinalizeAndMaterialize(
		ctx, sessionID, []wire.OutPoint{input},
		[]*psbt.Packet{finalCheckpoint}, []*vtxo.Record{output},
	)
	require.NoError(t, err)

	state, found, err := store.GetSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, oorStateAwaitingNotify, state)

	inputRecord, err := recordStore.Get(ctx, input)
	require.NoError(t, err)
	require.NotNil(t, inputRecord)
	require.Equal(t, vtxo.StatusSpent, inputRecord.Status)

	outputRecord, err := recordStore.Get(ctx, output.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, outputRecord)
	require.Equal(t, vtxo.StatusLive, outputRecord.Status)
	require.Equal(t, output.Value, outputRecord.Value)
	require.Equal(t, output.PkScript, outputRecord.PkScript)
}

// TestAtomicFinalizeRollbackOnCreateFailure verifies that a failing output
// materialization rolls back the spent-input mutation and session transition.
func TestAtomicFinalizeRollbackOnCreateFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	dbh := db.NewTestDB(t)
	dbStore := db.NewStore(
		dbh.DB, dbh.Queries, dbh.Backend(), btclog.Disabled,
		clock.NewDefaultClock(),
	)

	store := NewDBSessionStore(
		dbStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	recordStore := dbStore.NewVTXORecordStore()

	arkPSBT := makeTestPSBT(t, 103)
	checkpoint := makeTestPSBT(t, 104)
	finalCheckpoint := makeTestPSBT(t, 105)
	input := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
	sessionID := makeTestSessionID(arkPSBT)
	owner := vtxo.OORLockOwner(sessionID.String())

	err := recordStore.Create(ctx, &vtxo.Record{
		Outpoint: input,
		Value:    checkpoint.Inputs[0].WitnessUtxo.Value,
		PkScript: checkpoint.Inputs[0].WitnessUtxo.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	err = store.UpsertCoSignedAndMarkInFlight(
		ctx, sessionID, []wire.OutPoint{input}, arkPSBT,
		[]*psbt.Packet{checkpoint},
		time.Now().Add(DefaultSessionExpiry), owner,
	)
	require.NoError(t, err)

	outputOutpoint := wire.OutPoint{
		Hash:  arkPSBT.UnsignedTx.TxHash(),
		Index: 0,
	}
	err = recordStore.Create(ctx, &vtxo.Record{
		Outpoint: outputOutpoint,
		Value:    arkPSBT.UnsignedTx.TxOut[0].Value + 1,
		PkScript: append(
			[]byte(nil), arkPSBT.UnsignedTx.TxOut[0].PkScript...,
		),
		Status: vtxo.StatusLive,
	})
	require.NoError(t, err)

	err = store.ApplyFinalizeAndMaterialize(
		ctx, sessionID, []wire.OutPoint{input},
		[]*psbt.Packet{finalCheckpoint}, []*vtxo.Record{{
			Outpoint: outputOutpoint,
			Value:    arkPSBT.UnsignedTx.TxOut[0].Value,
			PkScript: append(
				[]byte(nil),
				arkPSBT.UnsignedTx.TxOut[0].PkScript...,
			),
			Status: vtxo.StatusLive,
		}},
	)
	require.ErrorContains(t, err, "different value")

	state, found, err := store.GetSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, oorStateCoSigned, state)

	inputRecord, err := recordStore.Get(ctx, input)
	require.NoError(t, err)
	require.NotNil(t, inputRecord)
	require.Equal(t, vtxo.StatusInFlight, inputRecord.Status)
	require.Equal(t, owner, inputRecord.InFlightOwner)
}
