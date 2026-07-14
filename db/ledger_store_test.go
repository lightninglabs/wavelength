package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/stretchr/testify/require"
)

// newLedgerStoreForTest creates a LedgerStoreDB backed by a fresh test
// database with all migrations applied.
func newLedgerStoreForTest(t *testing.T) *LedgerStoreDB {
	t.Helper()

	store, _ := newLedgerStoreAndDBForTest(t)

	return store
}

// newLedgerStoreAndDBForTest creates a LedgerStoreDB and returns its backing
// database so tests can exercise storage-layer edge cases directly.
func newLedgerStoreAndDBForTest(t *testing.T) (*LedgerStoreDB, *BaseDB) {
	t.Helper()

	db := NewTestDB(t)

	txExec := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return &LedgerStoreDB{
		TransactionExecutor: txExec,
	}, db.BaseDB
}

// makeLedgerEntry is a test helper that creates a ledger.LedgerEntry with the
// given parameters and sensible defaults for the remaining fields.
func makeLedgerEntry(debit, credit string, amount int64, eventType string,
	roundID []byte, ts int64) ledger.LedgerEntry {

	return ledger.LedgerEntry{
		DebitAccount:  debit,
		CreditAccount: credit,
		AmountSat:     amount,
		RoundID:       roundID,
		EventType:     eventType,
		Description:   eventType + " test entry",
		CreatedAt:     ts,
	}
}

// testBytes returns a deterministic byte slice with the requested length.
func testBytes(length int, seed byte) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = seed + byte(i)
	}

	return out
}

// insertTestInternalKey registers an internal_keys row with a deterministic
// 33-byte pubkey and returns the FK id boarding rows reference.
func insertTestInternalKey(ctx context.Context, t *testing.T, db *BaseDB,
	seed byte) sql.NullInt64 {

	t.Helper()

	id, err := db.UpsertInternalKey(
		ctx, sqlc.UpsertInternalKeyParams{
			Pubkey:    testBytes(33, seed),
			KeyFamily: 0,
			KeyIndex:  int64(seed),
			CreatedAt: 0,
		},
	)
	require.NoError(t, err)

	return sql.NullInt64{Int64: id, Valid: true}
}

// testHash32 returns a deterministic 32-byte hash for tests.
func testHash32(seed byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = seed + byte(i)
	}

	return out
}

// testInt32Ptr returns a pointer to v for optional int32 test fields.
func testInt32Ptr(v int32) *int32 {
	return &v
}

// insertTransactionHistorySweepRow inserts a minimal boarding_sweeps row for
// transaction-history tests. The history query does not need sweep inputs.
func insertTransactionHistorySweepRow(t *testing.T, db *BaseDB, txid []byte,
	amount, fee, createdAt int64) {

	t.Helper()

	query := `INSERT INTO boarding_sweeps (
		txid, raw_tx, destination_address, total_amount,
		fee_amount, fee_rate_sat_per_vbyte, vbytes, status,
		created_height, created_time
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if db.Backend() == sqlc.BackendTypePostgres {
		query = `INSERT INTO boarding_sweeps (
			txid, raw_tx, destination_address, total_amount,
			fee_amount, fee_rate_sat_per_vbyte, vbytes, status,
			created_height, created_time
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	}

	_, err := db.ExecContext(
		t.Context(), query, txid, []byte{0x01}, "bcrt1test", amount,
		fee, int64(2), int64(120), wallet.BoardingSweepStatusPublished,
		int32(700), createdAt,
	)
	require.NoError(t, err)
}

// insertTransactionHistoryOOROutput inserts the minimum package, VTXO, and
// binding rows needed for the transaction-history query to resolve an OOR
// created output.
func insertTransactionHistoryOOROutput(t *testing.T, db *BaseDB, sessionID,
	outpointHash []byte, outpointIndex int32, amount, createdAt int64,
	seed byte) {

	t.Helper()

	ctx := t.Context()
	roundID := fmt.Sprintf("019e4bc6-d95f-7caf-93c3-409e854bbb%02x", seed)

	require.NoError(
		t,
		db.InsertRound(
			ctx, sqlc.InsertRoundParams{
				RoundID:        roundID,
				Status:         "confirmed",
				CreationTime:   createdAt,
				LastUpdateTime: createdAt,
			},
		),
	)
	_, err := db.UpsertOORPackage(
		ctx, sqlc.UpsertOORPackageParams{
			SessionID: sessionID,
			Direction: oorPackageDirectionOutgoingCode,
			ArkPsbt:   testBytes(8, seed+1),
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
	)
	require.NoError(t, err)
	require.NoError(
		t,
		db.InsertVTXO(
			ctx, sqlc.InsertVTXOParams{
				OutpointHash:  outpointHash,
				OutpointIndex: outpointIndex,
				RoundID:       roundID,
				Amount:        amount,
				PkScript:      testBytes(34, seed+2),
				Expiry:        144,
				ClientKeyID: insertTestInternalKey(
					ctx, t, db, seed+3,
				),
				OperatorPubkey: testBytes(33, seed+4),
				CommitmentTxid: testBytes(32, seed+5),
				CreationTime:   createdAt,
				LastUpdateTime: createdAt,
			},
		),
	)
	_, err = db.UpsertOORVTXOBinding(
		ctx, sqlc.UpsertOORVTXOBindingParams{
			OutpointHash:  outpointHash,
			OutpointIndex: outpointIndex,
			SessionID:     sessionID,
			OutputIndex:   outpointIndex,
			LinkKind:      oorPackageLinkKindCreatedOutputCode,
			CreatedAt:     createdAt,
			UpdatedAt:     createdAt,
		},
	)
	require.NoError(t, err)
}

// TestLedgerStoreInsertAndRetrieve verifies that a single ledger entry
// can be inserted and retrieved via ListLedgerEntries.
func TestLedgerStoreInsertAndRetrieve(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()
	roundID := []byte("round-001")

	entry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000, "boarding_fee_paid",
		roundID, now,
	)
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	// Retrieve the entry.
	entries, err := store.ListLedgerEntries(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	got := entries[0]
	require.Equal(t, entry.DebitAccount, got.DebitAccount)
	require.Equal(t, entry.CreditAccount, got.CreditAccount)
	require.Equal(t, entry.AmountSat, got.AmountSat)
	require.Equal(t, entry.RoundID, got.RoundID)
	require.Equal(t, entry.EventType, got.EventType)
	require.Equal(t, entry.Description, got.Description)
	require.Equal(t, entry.CreatedAt, got.CreatedAt)
}

// TestLedgerStoreTransactionHistoryFiltersBeforePagination verifies the
// unified transaction-history query applies type and date filters before
// LIMIT/OFFSET. A filtered page should find matching older rows instead of
// returning empty just because newer rows have different types.
func TestLedgerStoreTransactionHistoryFiltersBeforePagination(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db := newLedgerStoreAndDBForTest(t)

	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:  ledger.AccountTransfersOut,
				CreditAccount: ledger.AccountVTXOBalance,
				AmountSat:     1_000,
				SessionID:     testBytes(32, 1),
				EventType:     ledger.EventVTXOSent,
				Description:   "oor send",
				CreatedAt:     100,
			},
		),
	)
	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:  ledger.AccountVTXOBalance,
				CreditAccount: ledger.AccountWalletBalance,
				AmountSat:     2_000,
				RoundID:       testBytes(16, 2),
				EventType:     ledger.EventVTXOReceived,
				Description:   "round receive",
				CreatedAt:     200,
			},
		),
	)
	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:   ledger.AccountWalletBalance,
				CreditAccount:  ledger.AccountOpeningBalance,
				AmountSat:      3_000,
				EventType:      ledger.EventWalletUTXOCreated,
				Description:    "boarding deposit",
				CreatedAt:      300,
				IdempotencyKey: testBytes(36, 3),
			},
		),
	)
	insertTransactionHistorySweepRow(
		t, db, testBytes(32, 4), 4_000, 40, 400,
	)

	oorRows, err := store.ListTransactionHistory(ctx, "oor", 0, 0, 1, 0)
	require.NoError(t, err)
	require.Len(t, oorRows, 1)
	require.Equal(t, "oor", oorRows[0].TransactionType)
	require.Equal(t, int64(100), oorRows[0].CreatedAt)

	windowRows, err := store.ListTransactionHistory(
		ctx, "", 150, 350, 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, windowRows, 2)
	require.Equal(t, []string{"boarding", "round"}, []string{
		windowRows[0].TransactionType,
		windowRows[1].TransactionType,
	})

	sweepRows, err := store.ListTransactionHistory(
		ctx, "sweep", 0, 0, 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, sweepRows, 1)
	require.Equal(t, "boarding_sweep", sweepRows[0].Source)
	require.Equal(t, int64(4_000), sweepRows[0].AmountSat)
	require.Equal(t, int64(40), sweepRows[0].FeeSat)
}

// TestLedgerStoreTransactionHistorySurfacesBoardingAddress verifies the
// history query resolves a confirmed boarding-deposit row back to its
// allocated boarding address (via boarding_intents -> boarding_addresses), so
// the client can key the confirmed DEPOSIT row by the same id as its pending
// row. Non-deposit rows carry no boarding address.
func TestLedgerStoreTransactionHistorySurfacesBoardingAddress(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db := newLedgerStoreAndDBForTest(t)

	// Seed the durable outpoint -> address link: internal key ->
	// boarding_addresses -> boarding_intents for a confirmed outpoint.
	keyID := insertTestInternalKey(ctx, t, db, 0x10)
	pkScript := testBytes(34, 0x20)
	const addr = "bcrt1qtestboardingaddr"
	outpointHash := testBytes(32, 0x30)
	var outpointIndex int32

	require.NoError(
		t,
		db.InsertBoardingAddress(
			ctx, sqlc.InsertBoardingAddressParams{
				PkScript:       pkScript,
				AddressString:  addr,
				ClientKeyID:    keyID,
				OperatorPubkey: testBytes(33, 0x40),
				ExitDelay:      144,
			},
		),
	)
	require.NoError(
		t,
		db.InsertBoardingIntent(
			ctx, sqlc.InsertBoardingIntentParams{
				OutpointHash:  outpointHash,
				OutpointIndex: outpointIndex,
				PkScript:      pkScript,
				Amount:        100_000,
				ConfHeight:    500,
				ConfHash:      testBytes(32, 0x70),
				ConfTx:        testBytes(64, 0x80),
				Status:        "confirmed",
			},
		),
	)

	// The confirmed boarding-deposit ledger row, keyed by the same
	// outpoint.
	idx := outpointIndex
	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:   ledger.AccountWalletBalance,
				CreditAccount:  ledger.AccountOpeningBalance,
				AmountSat:      100_000,
				EventType:      ledger.EventWalletUTXOCreated,
				Description:    "boarding deposit",
				CreatedAt:      300,
				IdempotencyKey: testBytes(36, 0x50),
				ChainTxid:      outpointHash,
				ChainVout:      &idx,
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "boarding", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, ledger.EventWalletUTXOCreated, rows[0].Subtype)
	require.Equal(
		t, addr, rows[0].BoardingAddress,
		"confirmed boarding deposit must surface its allocated address",
	)

	// A non-deposit row must not carry a boarding address.
	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:  ledger.AccountTransfersOut,
				CreditAccount: ledger.AccountVTXOBalance,
				AmountSat:     1_000,
				SessionID:     testBytes(32, 0x60),
				EventType:     ledger.EventVTXOSent,
				Description:   "oor send",
				CreatedAt:     400,
			},
		),
	)
	oorRows, err := store.ListTransactionHistory(ctx, "oor", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, oorRows, 1)
	require.Empty(t, oorRows[0].BoardingAddress)
}

// TestLedgerStoreTransactionHistoryClassifiesOORReceiveWithoutSessionID
// verifies OOR receive rows are user-visible OOR history even though the
// ledger cannot stamp session_id on them without colliding on multi-output
// receives from the same session.
func TestLedgerStoreTransactionHistoryClassifiesOORReceiveWithoutSessionID(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, db := newLedgerStoreAndDBForTest(t)
	sessionID := testBytes(32, 0x43)
	outpointHash := testBytes(32, 0x44)
	outpointIndex := int32(2)
	insertTransactionHistoryOOROutput(
		t, db, sessionID, outpointHash, outpointIndex, 2_500, 99, 0x46,
	)

	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:   ledger.AccountVTXOBalance,
				CreditAccount:  ledger.AccountTransfersIn,
				AmountSat:      2_500,
				EventType:      ledger.EventVTXOReceived,
				Description:    "oor receive",
				CreatedAt:      100,
				IdempotencyKey: testBytes(36, 0x45),
				ChainTxid:      outpointHash,
				ChainVout:      &outpointIndex,
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "oor", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "oor", rows[0].TransactionType)
	require.Equal(t, ledger.EventVTXOReceived, rows[0].Subtype)
	require.Equal(t, outpointHash, rows[0].Txid)
	require.Equal(t, outpointIndex, rows[0].OutputIndex)
	require.Equal(t, sessionID, rows[0].SessionID)
}

// TestLedgerStoreTransactionHistorySynthesizesOORReceiveFromBinding verifies
// old ledger rows that predate structured outpoint fields can still be paired
// through the OOR binding table without parsing descriptions.
func TestLedgerStoreTransactionHistorySynthesizesOORReceiveFromBinding(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, db := newLedgerStoreAndDBForTest(t)
	sessionID := testBytes(32, 0x50)
	outpointHash := testBytes(32, 0x51)
	outpointIndex := int32(1)
	insertTransactionHistoryOOROutput(
		t, db, sessionID, outpointHash, outpointIndex, 998_511, 99,
		0x52,
	)

	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:  ledger.AccountVTXOBalance,
				CreditAccount: ledger.AccountTransfersIn,
				AmountSat:     998_511,
				EventType:     ledger.EventVTXOReceived,
				Description:   "old unstructured OOR receive",
				CreatedAt:     100,
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "oor", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "oor_binding", rows[0].Source)
	require.Equal(t, ledger.EventVTXOReceived, rows[0].Subtype)
	require.Equal(t, int64(998_511), rows[0].AmountSat)
	require.Equal(t, outpointHash, rows[0].Txid)
	require.Equal(t, outpointIndex, rows[0].OutputIndex)
	require.Equal(t, sessionID, rows[0].SessionID)
}

// TestLedgerStoreTransactionHistoryKeepsUnstructuredReceiveRound verifies an
// old or malformed receive row without structured outpoint fields does not
// become OOR history merely because its accounts look like a transfer.
func TestLedgerStoreTransactionHistoryKeepsUnstructuredReceiveRound(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, _ := newLedgerStoreAndDBForTest(t)

	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:  ledger.AccountVTXOBalance,
				CreditAccount: ledger.AccountTransfersIn,
				AmountSat:     2_500,
				EventType:     ledger.EventVTXOReceived,
				Description:   "unstructured receive",
				CreatedAt:     100,
			},
		),
	)

	oorRows, err := store.ListTransactionHistory(ctx, "oor", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Empty(t, oorRows)

	rows, err := store.ListTransactionHistory(ctx, "", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "round", rows[0].TransactionType)
}

// TestLedgerStoreTransactionHistoryWalletUTXOCreatedChainFields verifies
// wallet UTXO creation ledger rows expose the chain transaction id and
// confirmation height in the unified transaction history.
func TestLedgerStoreTransactionHistoryWalletUTXOCreatedChainFields(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, db := newLedgerStoreAndDBForTest(t)

	outpointHash := testHash32(0x42)
	const (
		outpointIndex      = uint32(7)
		confirmationHeight = int32(304_081)
		depositAmount      = int64(100_001)
		vtxoAmount         = int64(99_746)
		createdAt          = int64(1_700_000_501)
	)
	roundID := "019e4bc6-d95f-7caf-93c3-409e854bbb9f"
	pkScript := testBytes(34, 0x01)

	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:   ledger.AccountWalletBalance,
				CreditAccount:  ledger.AccountOpeningBalance,
				AmountSat:      depositAmount,
				EventType:      ledger.EventWalletUTXOCreated,
				Description:    "boarding deposit",
				CreatedAt:      createdAt,
				IdempotencyKey: testBytes(36, 0x51),
				ChainTxid:      outpointHash[:],
				ChainVout: testInt32Ptr(
					int32(outpointIndex),
				),
				ConfirmationHeight: testInt32Ptr(
					confirmationHeight,
				),
			},
		),
	)

	require.NoError(
		t,
		db.InsertBoardingAddress(
			ctx, sqlc.InsertBoardingAddressParams{
				PkScript:      pkScript,
				AddressString: "bcrt1ptest",
				ClientKeyID: insertTestInternalKey(
					ctx, t, db, 0x11,
				),
				OperatorPubkey: testBytes(33, 0x22),
				ExitDelay:      144,
				CreationTime:   createdAt,
			},
		),
	)
	require.NoError(
		t,
		db.InsertBoardingIntent(
			ctx, sqlc.InsertBoardingIntentParams{
				OutpointHash:   outpointHash[:],
				OutpointIndex:  int32(outpointIndex),
				PkScript:       pkScript,
				Amount:         depositAmount,
				ConfHeight:     confirmationHeight,
				ConfHash:       testBytes(32, 0x02),
				ConfTx:         testBytes(64, 0x03),
				TxProof:        testBytes(64, 0x04),
				Status:         "confirmed",
				CreationTime:   createdAt,
				LastUpdateTime: createdAt,
			},
		),
	)
	require.NoError(
		t,
		db.InsertRound(
			ctx, sqlc.InsertRoundParams{
				RoundID:        roundID,
				Status:         "confirmed",
				CreationTime:   createdAt,
				LastUpdateTime: createdAt,
			},
		),
	)
	require.NoError(
		t,
		db.InsertRoundBoardingIntent(
			ctx, sqlc.InsertRoundBoardingIntentParams{
				RoundID:       roundID,
				OutpointHash:  outpointHash[:],
				OutpointIndex: int32(outpointIndex),
				ClientKey:     testBytes(33, 0x11),
				OperatorKey:   testBytes(33, 0x22),
				ExitDelay:     144,
			},
		),
	)
	require.NoError(
		t,
		db.InsertVTXO(
			ctx, sqlc.InsertVTXOParams{
				OutpointHash:  testBytes(32, 0x31),
				OutpointIndex: 0,
				RoundID:       roundID,
				Amount:        vtxoAmount,
				PkScript:      testBytes(34, 0x41),
				Expiry:        144,
				ClientKeyID: insertTestInternalKey(
					ctx, t, db, 0x51,
				),
				OperatorPubkey: testBytes(33, 0x61),
				CommitmentTxid: testBytes(32, 0x71),
				CreationTime:   createdAt,
				LastUpdateTime: createdAt,
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "boarding", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	row := rows[0]
	require.Equal(t, "ledger", row.Source)
	require.Equal(t, "boarding", row.TransactionType)
	require.Equal(t, ledger.EventWalletUTXOCreated, row.Subtype)
	require.Equal(t, "confirmed", row.Status)
	require.Equal(t, outpointHash[:], row.Txid)
	require.Equal(t, confirmationHeight, row.ConfirmationHeight)
	require.Equal(t, depositAmount-vtxoAmount, row.FeeSat)
}

// TestLedgerStoreTransactionHistoryWalletUTXOCreatedBoardingStatus confirms
// a chain-confirmed deposit is not reported as complete until its boarding
// outpoint has moved through a confirmed round and produced a spendable VTXO.
func TestLedgerStoreTransactionHistoryWalletUTXOCreatedBoardingStatus(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, db := newLedgerStoreAndDBForTest(t)

	outpointHash := testHash32(0x52)
	const (
		outpointIndex      = uint32(1)
		confirmationHeight = int32(304_091)
		depositAmount      = int64(75_000)
		createdAt          = int64(1_700_000_601)
	)
	pkScript := testBytes(34, 0x09)

	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:   ledger.AccountWalletBalance,
				CreditAccount:  ledger.AccountOpeningBalance,
				AmountSat:      depositAmount,
				EventType:      ledger.EventWalletUTXOCreated,
				Description:    "boarding deposit",
				CreatedAt:      createdAt,
				IdempotencyKey: testBytes(36, 0x61),
				ChainTxid:      outpointHash[:],
				ChainVout: testInt32Ptr(
					int32(outpointIndex),
				),
				ConfirmationHeight: testInt32Ptr(
					confirmationHeight,
				),
			},
		),
	)

	require.NoError(
		t,
		db.InsertBoardingAddress(
			ctx, sqlc.InsertBoardingAddressParams{
				PkScript:      pkScript,
				AddressString: "bcrt1pboarding",
				ClientKeyID: insertTestInternalKey(
					ctx, t, db, 0x12,
				),
				OperatorPubkey: testBytes(33, 0x23),
				ExitDelay:      144,
				CreationTime:   createdAt,
			},
		),
	)
	require.NoError(
		t,
		db.InsertBoardingIntent(
			ctx, sqlc.InsertBoardingIntentParams{
				OutpointHash:   outpointHash[:],
				OutpointIndex:  int32(outpointIndex),
				PkScript:       pkScript,
				Amount:         depositAmount,
				ConfHeight:     confirmationHeight,
				ConfHash:       testBytes(32, 0x03),
				ConfTx:         testBytes(64, 0x04),
				TxProof:        testBytes(64, 0x05),
				Status:         "confirmed",
				CreationTime:   createdAt,
				LastUpdateTime: createdAt,
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "boarding", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	row := rows[0]
	require.Equal(t, ledger.EventWalletUTXOCreated, row.Subtype)
	require.Equal(t, "boarding", row.Status)
	require.Equal(t, outpointHash[:], row.Txid)
	require.Equal(t, confirmationHeight, row.ConfirmationHeight)
	require.Zero(t, row.FeeSat)
}

// TestLedgerStoreTransactionHistoryWalletUTXOCreatedNonBoardingStatus confirms
// a chain-confirmed wallet UTXO that is not backed by a boarding intent stays
// confirmed. Sweep returns and future non-deposit wallet UTXOs should not be
// shown as boarding forever just because they have wallet_utxo_created rows.
func TestLedgerStoreTransactionHistoryWalletUTXOCreatedNonBoardingStatus(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	outpointHash := testHash32(0x53)
	const (
		outpointIndex      = uint32(0)
		confirmationHeight = int32(304_101)
		amount             = int64(50_000)
		createdAt          = int64(1_700_000_602)
	)

	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:   ledger.AccountWalletBalance,
				CreditAccount:  ledger.AccountOpeningBalance,
				AmountSat:      amount,
				EventType:      ledger.EventWalletUTXOCreated,
				Description:    "boarding sweep return",
				CreatedAt:      createdAt,
				IdempotencyKey: testBytes(36, 0x62),
				ChainTxid:      outpointHash[:],
				ChainVout: testInt32Ptr(
					int32(outpointIndex),
				),
				ConfirmationHeight: testInt32Ptr(
					confirmationHeight,
				),
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "boarding", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	row := rows[0]
	require.Equal(t, ledger.EventWalletUTXOCreated, row.Subtype)
	require.Equal(t, "confirmed", row.Status)
	require.Equal(t, outpointHash[:], row.Txid)
	require.Equal(t, int32(outpointIndex), row.OutputIndex)
	require.Equal(t, confirmationHeight, row.ConfirmationHeight)
}

// TestLedgerStoreTransactionHistoryWalletUTXOCreatedMultiInputFee verifies
// multi-input boarding rounds allocate the aggregate round fee across input
// deposit rows proportionally for display.
func TestLedgerStoreTransactionHistoryWalletUTXOCreatedMultiInputFee(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, db := newLedgerStoreAndDBForTest(t)

	const (
		outpointIndex = int32(0)
		createdAt     = int64(1_700_000_900)
	)
	roundID := "019e4bc6-d95f-7caf-93c3-409e854bbb9f"
	firstOutpoint := testHash32(0x10)
	secondOutpoint := testHash32(0x20)

	require.NoError(
		t,
		db.InsertRound(
			ctx, sqlc.InsertRoundParams{
				RoundID:        roundID,
				Status:         "confirmed",
				CreationTime:   createdAt,
				LastUpdateTime: createdAt,
			},
		),
	)

	insertBoardingInput := func(outpoint [32]byte, amount, createdAt int64,
		pkScriptSeed byte) {

		pkScript := testBytes(34, pkScriptSeed)
		require.NoError(
			t,
			db.InsertBoardingAddress(
				ctx, sqlc.InsertBoardingAddressParams{
					PkScript:      pkScript,
					AddressString: "bcrt1ptest",
					ClientKeyID: insertTestInternalKey(
						ctx, t, db, pkScriptSeed,
					),
					OperatorPubkey: testBytes(33, 0x22),
					ExitDelay:      144,
					CreationTime:   createdAt,
				},
			),
		)
		require.NoError(
			t,
			db.InsertBoardingIntent(
				ctx, sqlc.InsertBoardingIntentParams{
					OutpointHash:  outpoint[:],
					OutpointIndex: outpointIndex,
					PkScript:      pkScript,
					Amount:        amount,
					ConfHeight:    304_081,
					ConfHash:      testBytes(32, 0x02),
					ConfTx: testBytes(
						64, pkScriptSeed,
					),
					TxProof: testBytes(
						64, pkScriptSeed+1,
					),
					Status:         "confirmed",
					CreationTime:   createdAt,
					LastUpdateTime: createdAt,
				},
			),
		)
		require.NoError(
			t,
			db.InsertRoundBoardingIntent(
				ctx, sqlc.InsertRoundBoardingIntentParams{
					RoundID:       roundID,
					OutpointHash:  outpoint[:],
					OutpointIndex: outpointIndex,
					ClientKey:     testBytes(33, 0x11),
					OperatorKey:   testBytes(33, 0x22),
					ExitDelay:     144,
				},
			),
		)
		require.NoError(
			t,
			store.InsertLedgerEntry(
				ctx, ledger.LedgerEntry{
					DebitAccount: ledger.
						AccountWalletBalance,
					CreditAccount: ledger.
						AccountOpeningBalance,
					AmountSat: amount,
					EventType: ledger.
						EventWalletUTXOCreated,
					Description: "boarding deposit",
					CreatedAt:   createdAt,
					IdempotencyKey: testBytes(
						36, pkScriptSeed+2,
					),
					ChainTxid: outpoint[:],
					ChainVout: testInt32Ptr(outpointIndex),
				},
			),
		)
	}

	insertBoardingInput(firstOutpoint, 10_000, createdAt, 0x31)
	insertBoardingInput(secondOutpoint, 20_000, createdAt+1, 0x41)

	require.NoError(
		t,
		db.InsertVTXO(
			ctx, sqlc.InsertVTXOParams{
				OutpointHash:  testBytes(32, 0x51),
				OutpointIndex: 0,
				RoundID:       roundID,
				Amount:        29_995,
				PkScript:      testBytes(34, 0x52),
				Expiry:        144,
				ClientKeyID: insertTestInternalKey(
					ctx, t, db, 0x53,
				),
				OperatorPubkey: testBytes(33, 0x54),
				CommitmentTxid: testBytes(32, 0x55),
				CreationTime:   createdAt,
				LastUpdateTime: createdAt,
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "boarding", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	fees := map[string]int64{}
	for _, row := range rows {
		fees[string(row.Txid)] = row.FeeSat
	}
	require.Equal(t, int64(2), fees[string(firstOutpoint[:])])
	require.Equal(t, int64(3), fees[string(secondOutpoint[:])])
	require.Equal(
		t, int64(5), fees[string(firstOutpoint[:])]+
			fees[string(secondOutpoint[:])],
	)

	require.NoError(
		t,
		db.InsertVTXO(
			ctx, sqlc.InsertVTXOParams{
				OutpointHash:  testBytes(32, 0x61),
				OutpointIndex: 0,
				RoundID:       roundID,
				Amount:        10,
				PkScript:      testBytes(34, 0x62),
				Expiry:        144,
				ClientKeyID: insertTestInternalKey(
					ctx, t, db, 0x63,
				),
				OperatorPubkey: testBytes(33, 0x64),
				CommitmentTxid: testBytes(32, 0x65),
				CreationTime:   createdAt,
				LastUpdateTime: createdAt,
			},
		),
	)

	rows, err = store.ListTransactionHistory(ctx, "boarding", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	for _, row := range rows {
		require.Zero(
			t, row.FeeSat,
			"ambiguous round outputs should not be shown as fees",
		)
	}
}

// TestLedgerStoreTransactionHistoryWalletUTXOCreatedMissingHeight verifies a
// wallet UTXO creation ledger row with a chain txid but no stored confirmation
// height keeps the txid and falls back to an unknown height.
func TestLedgerStoreTransactionHistoryWalletUTXOCreatedMissingHeight(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	outpointHash := testHash32(0x62)
	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:   ledger.AccountWalletBalance,
				CreditAccount:  ledger.AccountOpeningBalance,
				AmountSat:      100_001,
				EventType:      ledger.EventWalletUTXOCreated,
				Description:    "boarding deposit no audit",
				CreatedAt:      1_700_000_600,
				IdempotencyKey: testBytes(36, 0x61),
				ChainTxid:      outpointHash[:],
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "boarding", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	row := rows[0]
	require.Equal(t, "ledger", row.Source)
	require.Equal(t, "boarding", row.TransactionType)
	require.Equal(t, ledger.EventWalletUTXOCreated, row.Subtype)
	require.Equal(t, "confirmed", row.Status)
	require.Equal(t, outpointHash[:], row.Txid)
	require.Zero(t, row.ConfirmationHeight)
}

// TestLedgerStoreTransactionHistoryBoardingFeePaidHasNoChainFields verifies
// boarding fee ledger rows keep their recorded status and empty chain fields.
func TestLedgerStoreTransactionHistoryBoardingFeePaidHasNoChainFields(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:  ledger.AccountFeesPaid,
				CreditAccount: ledger.AccountWalletBalance,
				AmountSat:     1_000,
				EventType:     ledger.EventBoardingFeePaid,
				Description:   "boarding fee paid",
				CreatedAt:     1_700_000_700,
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "boarding", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	row := rows[0]
	require.Equal(t, "ledger", row.Source)
	require.Equal(t, "boarding", row.TransactionType)
	require.Equal(t, ledger.EventBoardingFeePaid, row.Subtype)
	require.Equal(t, "recorded", row.Status)
	require.Nil(t, row.Txid)
	require.Zero(t, row.ConfirmationHeight)
}

// TestLedgerStoreTransactionHistoryWalletUTXOCreatedNoChainFields verifies
// wallet UTXO creation rows do not infer chain fields from legacy
// idempotency-key bytes during ordinary history reads.
func TestLedgerStoreTransactionHistoryWalletUTXOCreatedNoChainFields(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	require.NoError(
		t,
		store.InsertLedgerEntry(
			ctx, ledger.LedgerEntry{
				DebitAccount:   ledger.AccountWalletBalance,
				CreditAccount:  ledger.AccountOpeningBalance,
				AmountSat:      100_001,
				EventType:      ledger.EventWalletUTXOCreated,
				Description:    "boarding deposit bad key",
				CreatedAt:      1_700_000_800,
				IdempotencyKey: testBytes(32, 0x72),
			},
		),
	)

	rows, err := store.ListTransactionHistory(ctx, "boarding", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	row := rows[0]
	require.Equal(t, "ledger", row.Source)
	require.Equal(t, "boarding", row.TransactionType)
	require.Equal(t, ledger.EventWalletUTXOCreated, row.Subtype)
	require.Equal(t, "confirmed", row.Status)
	require.Nil(t, row.Txid)
	require.Zero(t, row.ConfirmationHeight)
}

// TestLedgerStoreEntryIDsDoNotReuseAfterDelete verifies ledger entry IDs
// remain monotonic even if the current maximum row is deleted.
func TestLedgerStoreEntryIDsDoNotReuseAfterDelete(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db := newLedgerStoreAndDBForTest(t)
	now := time.Now().Unix()

	first := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000, "boarding_fee_paid",
		[]byte("round-rowid-1"), now,
	)
	require.NoError(t, store.InsertLedgerEntry(ctx, first))

	entries, err := store.ListLedgerEntries(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	firstID := entries[0].EntryID
	// There is intentionally no production delete query for this
	// append-only log. The raw SQL keeps this regression test limited to
	// the impossible-in-production row deletion shape that triggers ROWID
	// reuse.
	query := "DELETE FROM ledger_entries WHERE entry_id = ?"
	if db.Backend() == sqlc.BackendTypePostgres {
		query = "DELETE FROM ledger_entries WHERE entry_id = $1"
	}

	_, err = db.ExecContext(ctx, query, firstID)
	require.NoError(t, err)

	second := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000, "boarding_fee_paid",
		[]byte("round-rowid-2"), now+1,
	)
	require.NoError(t, store.InsertLedgerEntry(ctx, second))

	entries, err = store.ListLedgerEntries(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, firstID+1, entries[0].EntryID)
}

// TestLedgerStoreAccountBalance verifies that GetAccountBalance
// correctly computes net balance (debits minus credits) for an account.
func TestLedgerStoreAccountBalance(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Insert a debit of 5000 to fees_paid from wallet_balance.
	entry1 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 5000, "boarding_fee_paid",
		[]byte("round-a"), now,
	)
	require.NoError(t, store.InsertLedgerEntry(ctx, entry1))

	// Insert a credit to fees_paid (debit from vtxo_balance).
	entry2 := makeLedgerEntry(
		"vtxo_balance", "fees_paid", 2000, "refresh_fee_paid",
		[]byte("round-b"), now+1,
	)
	require.NoError(t, store.InsertLedgerEntry(ctx, entry2))

	// fees_paid: debits=5000, credits=2000 => balance=3000.
	balance, err := store.GetAccountBalance(ctx, "fees_paid")
	require.NoError(t, err)
	require.Equal(t, int64(3000), balance)

	// wallet_balance: debits=0, credits=5000 => balance=-5000.
	balance, err = store.GetAccountBalance(ctx, "wallet_balance")
	require.NoError(t, err)
	require.Equal(t, int64(-5000), balance)

	// vtxo_balance: debits=2000, credits=0 => balance=2000.
	balance, err = store.GetAccountBalance(ctx, "vtxo_balance")
	require.NoError(t, err)
	require.Equal(t, int64(2000), balance)
}

// TestLedgerStoreAccountBalanceEmpty verifies that querying the balance
// of an account with no entries returns zero.
func TestLedgerStoreAccountBalanceEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	balance, err := store.GetAccountBalance(ctx, "fees_paid")
	require.NoError(t, err)
	require.Equal(t, int64(0), balance)
}

// TestLedgerStoreTotalOperatorFeesPaid verifies that
// GetTotalOperatorFeesPaid sums only entries debited to the fees_paid
// account.
func TestLedgerStoreTotalOperatorFeesPaid(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Two fee entries debiting fees_paid.
	e1 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 3000, "boarding_fee_paid",
		[]byte("round-1"), now,
	)
	e2 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 7000, "refresh_fee_paid",
		[]byte("round-2"), now+1,
	)

	// An unrelated entry that should not be counted.
	e3 := makeLedgerEntry(
		"vtxo_balance", "wallet_balance", 500, "vtxo_received",
		[]byte("round-3"), now+2,
	)

	require.NoError(t, store.InsertLedgerEntry(ctx, e1))
	require.NoError(t, store.InsertLedgerEntry(ctx, e2))
	require.NoError(t, store.InsertLedgerEntry(ctx, e3))

	total, err := store.GetTotalOperatorFeesPaid(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(10000), total)
}

// TestLedgerStoreTotalOperatorFeesPaidEmpty verifies that
// GetTotalOperatorFeesPaid returns zero when no entries exist.
func TestLedgerStoreTotalOperatorFeesPaidEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	total, err := store.GetTotalOperatorFeesPaid(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), total)
}

// TestLedgerStoreListEntriesPagination verifies that ListLedgerEntries
// respects limit and offset, returning entries in descending created_at
// order.
func TestLedgerStoreListEntriesPagination(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	baseTime := time.Now().Unix()

	// Insert 5 entries with distinct timestamps.
	for i := range 5 {
		e := makeLedgerEntry(
			"fees_paid", "wallet_balance", int64(1000*(i+1)),
			"boarding_fee_paid", []byte{byte(i)}, baseTime+int64(i),
		)
		require.NoError(t, store.InsertLedgerEntry(ctx, e))
	}

	// Fetch first page (2 entries).
	page1, err := store.ListLedgerEntries(ctx, 2, 0)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	// Should be newest first (index 4, then 3).
	require.Equal(t, baseTime+4, page1[0].CreatedAt)
	require.Equal(t, baseTime+3, page1[1].CreatedAt)

	// Fetch second page.
	page2, err := store.ListLedgerEntries(ctx, 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	require.Equal(t, baseTime+2, page2[0].CreatedAt)
	require.Equal(t, baseTime+1, page2[1].CreatedAt)

	// Fetch third page.
	page3, err := store.ListLedgerEntries(ctx, 2, 4)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	require.Equal(t, baseTime, page3[0].CreatedAt)
}

// TestLedgerStoreListEntriesByType verifies filtering by event type.
func TestLedgerStoreListEntriesByType(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Insert entries with different event types.
	boarding := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000, "boarding_fee_paid",
		[]byte("r-1"), now,
	)
	refresh := makeLedgerEntry(
		"fees_paid", "wallet_balance", 2000, "refresh_fee_paid",
		[]byte("r-2"), now+1,
	)
	boarding2 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 3000, "boarding_fee_paid",
		[]byte("r-3"), now+2,
	)

	require.NoError(t, store.InsertLedgerEntry(ctx, boarding))
	require.NoError(t, store.InsertLedgerEntry(ctx, refresh))
	require.NoError(t, store.InsertLedgerEntry(ctx, boarding2))

	// Filter by boarding_fee_paid.
	entries, err := store.ListLedgerEntriesByType(
		ctx, "boarding_fee_paid", 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	// Should be newest first.
	require.Equal(t, int64(3000), entries[0].AmountSat)
	require.Equal(t, int64(1000), entries[1].AmountSat)

	// Filter by refresh_fee_paid.
	entries, err = store.ListLedgerEntriesByType(
		ctx, "refresh_fee_paid", 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int64(2000), entries[0].AmountSat)
}

// TestLedgerStoreListEntriesByTypePagination verifies that the type
// filter correctly applies limit and offset.
func TestLedgerStoreListEntriesByTypePagination(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	baseTime := time.Now().Unix()

	// Insert 4 entries of the same type.
	for i := range 4 {
		e := makeLedgerEntry(
			"fees_paid", "wallet_balance", int64(100*(i+1)),
			"boarding_fee_paid", []byte{byte(i)}, baseTime+int64(i),
		)
		require.NoError(t, store.InsertLedgerEntry(ctx, e))
	}

	// Page through with limit 2.
	page1, err := store.ListLedgerEntriesByType(
		ctx, "boarding_fee_paid", 2, 0,
	)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	page2, err := store.ListLedgerEntriesByType(
		ctx, "boarding_fee_paid", 2, 2,
	)
	require.NoError(t, err)
	require.Len(t, page2, 2)

	// No overlap between pages.
	require.NotEqual(t, page1[0].EntryID, page2[0].EntryID)
	require.NotEqual(t, page1[1].EntryID, page2[1].EntryID)
}

// TestLedgerStoreCountEntries verifies the count query returns the
// correct total.
func TestLedgerStoreCountEntries(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	// Start with zero entries.
	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), count)

	now := time.Now().Unix()

	// Insert 3 entries.
	for i := range 3 {
		e := makeLedgerEntry(
			"fees_paid", "wallet_balance", 1000,
			"boarding_fee_paid", []byte{byte(i)}, now+int64(i),
		)
		require.NoError(t, store.InsertLedgerEntry(ctx, e))
	}

	count, err = store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
}

// TestLedgerStoreListAccounts verifies that the migration seed data for
// the chart of accounts is returned correctly, including the
// opening_balance equity account that acts as the source-of-funds
// counterparty for wallet UTXO confirmations.
func TestLedgerStoreListAccounts(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	accounts, err := store.ListAccounts(ctx)
	require.NoError(t, err)

	// The migrations seed 8 accounts: wallet_balance, vtxo_balance,
	// fees_paid, onchain_fees, transfers_in, transfers_out,
	// opening_balance, wallet_clearing.
	require.Len(t, accounts, 8)

	// Build a map for easier assertions.
	byID := make(map[string]sqlc.Account, len(accounts))
	for _, a := range accounts {
		byID[a.AccountID] = a
	}

	// Verify a few key accounts.
	require.Equal(t, "asset", byID["wallet_balance"].AccountType)
	require.Equal(t, "Wallet Balance", byID["wallet_balance"].AccountName)

	require.Equal(t, "expense", byID["fees_paid"].AccountType)
	require.Equal(t, "Fees Paid", byID["fees_paid"].AccountName)

	require.Equal(t, "revenue", byID["transfers_in"].AccountType)
	require.Equal(t, "Transfers In", byID["transfers_in"].AccountName)

	require.Equal(t, "expense", byID["transfers_out"].AccountType)
	require.Equal(t, "Transfers Out", byID["transfers_out"].AccountName)

	// opening_balance is the equity source-of-funds account for
	// wallet UTXO deposits. Without it, wallet_balance would drift
	// negative on every boarding because SourceRoundBoarding only
	// ever credits it.
	require.Equal(t, "equity", byID["opening_balance"].AccountType)
	require.Equal(
		t, "Opening Balance", byID["opening_balance"].AccountName,
	)

	require.Equal(t, "asset", byID["wallet_clearing"].AccountType)
	require.Equal(
		t, "Wallet Sweep Clearing", byID["wallet_clearing"].AccountName,
	)
}

// TestLedgerStoreIdempotentInsert verifies that a redelivered
// message resolves to a silent no-op: the partial unique index on
// (round_id, event_type, debit_account, credit_account) combined
// with ON CONFLICT DO NOTHING on InsertClientLedgerEntry swallows
// the duplicate. The call returns nil (so durable-actor replay
// does not nack) and the row count stays at one.
func TestLedgerStoreIdempotentInsert(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()
	roundID := []byte("round-dup")

	entry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000, "boarding_fee_paid",
		roundID, now,
	)

	// First insert succeeds.
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	// Second insert with the same (round_id, event_type,
	// debit_account, credit_account) is swallowed by
	// ON CONFLICT DO NOTHING rather than surfacing a
	// constraint violation. Returning an error here would
	// drive an infinite durable-actor retry loop on a
	// permanent condition.
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	// Only one entry should exist.
	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestLedgerStoreNilRoundIDAllowsDuplicates verifies that entries with
// NULL round_id are not subject to the idempotency constraint, since
// the unique index uses a WHERE round_id IS NOT NULL filter.
func TestLedgerStoreNilRoundIDAllowsDuplicates(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	entry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 500, "onchain_fee_paid", nil,
		now,
	)

	// Both inserts should succeed because round_id is NULL.
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	entry.CreatedAt = now + 1
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}

// TestLedgerStoreIdempotentInsertBySession verifies that a
// redelivered OOR VTXO-sent message is deduped silently: the
// partial unique index idx_client_ledger_idempotent_session
// combined with ON CONFLICT DO NOTHING on
// InsertClientLedgerEntry treats the duplicate as a no-op
// instead of surfacing a constraint error.
func TestLedgerStoreIdempotentInsertBySession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()
	sessionID := []byte("session-abcdefghijklmnopqrstuvwx")

	entry := ledger.LedgerEntry{
		DebitAccount:  "transfers_out",
		CreditAccount: "vtxo_balance",
		AmountSat:     5_000,
		SessionID:     sessionID,
		EventType:     "vtxo_sent",
		Description:   "duplicate session send test",
		CreatedAt:     now,
	}

	// First insert succeeds.
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	// Replay of the same (session_id, event_type, debit,
	// credit) tuple is swallowed by ON CONFLICT DO NOTHING
	// and returns nil so the durable actor can ack the
	// redelivery instead of nacking forever.
	entry.CreatedAt = now + 1
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestLedgerStoreCheckConstraintSameAccount verifies that the CHECK
// constraint preventing debit_account == credit_account is enforced.
func TestLedgerStoreCheckConstraintSameAccount(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	entry := makeLedgerEntry(
		"fees_paid", "fees_paid", 1000, "boarding_fee_paid",
		[]byte("round-x"), time.Now().Unix(),
	)

	err := store.InsertLedgerEntry(ctx, entry)
	require.Error(t, err)
}

// TestLedgerStoreCheckConstraintPositiveAmount verifies that the CHECK
// constraint enforcing amount_sat > 0 rejects zero and negative amounts.
func TestLedgerStoreCheckConstraintPositiveAmount(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Zero amount should be rejected.
	zeroEntry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 0, "boarding_fee_paid",
		[]byte("round-zero"), now,
	)
	err := store.InsertLedgerEntry(ctx, zeroEntry)
	require.Error(t, err)

	// Negative amount should also be rejected.
	negEntry := makeLedgerEntry(
		"fees_paid", "wallet_balance", -100, "boarding_fee_paid",
		[]byte("round-neg"), now+1,
	)
	err = store.InsertLedgerEntry(ctx, negEntry)
	require.Error(t, err)
}

// TestLedgerStoreForeignKeyEventType verifies that inserting an entry
// with an invalid event type fails the FK constraint.
func TestLedgerStoreForeignKeyEventType(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	entry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000, "invalid_event_type",
		[]byte("round-fk"), time.Now().Unix(),
	)

	err := store.InsertLedgerEntry(ctx, entry)
	require.Error(t, err)
}

// TestLedgerStoreForeignKeyAccount verifies that inserting an entry
// with an invalid account ID fails the FK constraint.
func TestLedgerStoreForeignKeyAccount(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	entry := makeLedgerEntry(
		"nonexistent_account", "wallet_balance", 1000,
		"boarding_fee_paid", []byte("round-fk2"), time.Now().Unix(),
	)

	err := store.InsertLedgerEntry(ctx, entry)
	require.Error(t, err)
}

// TestLedgerStoreMultipleAccountBalances verifies balance computation
// across several accounts with many entries to exercise the aggregate
// query paths.
func TestLedgerStoreMultipleAccountBalances(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Simulate a boarding fee: wallet_balance -> fees_paid.
	e1 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 5000, "boarding_fee_paid",
		[]byte("r-01"), now,
	)

	// Simulate receiving a VTXO: vtxo_balance <- transfers_in.
	e2 := makeLedgerEntry(
		"vtxo_balance", "transfers_in", 20000, "vtxo_received",
		[]byte("r-02"), now+1,
	)

	// Simulate sending a VTXO: fees_paid <- vtxo_balance.
	e3 := makeLedgerEntry(
		"fees_paid", "vtxo_balance", 1000, "vtxo_sent", []byte("r-03"),
		now+2,
	)

	// Simulate on-chain fee: onchain_fees <- wallet_balance.
	e4 := makeLedgerEntry(
		"onchain_fees", "wallet_balance", 250, "onchain_fee_paid",
		[]byte("r-04"), now+3,
	)

	for _, e := range []ledger.LedgerEntry{e1, e2, e3, e4} {
		require.NoError(t, store.InsertLedgerEntry(ctx, e))
	}

	// wallet_balance: debits=0, credits=5000+250=5250 => -5250.
	bal, err := store.GetAccountBalance(ctx, "wallet_balance")
	require.NoError(t, err)
	require.Equal(t, int64(-5250), bal)

	// fees_paid: debits=5000+1000=6000, credits=0 => 6000.
	bal, err = store.GetAccountBalance(ctx, "fees_paid")
	require.NoError(t, err)
	require.Equal(t, int64(6000), bal)

	// vtxo_balance: debits=20000, credits=1000 => 19000.
	bal, err = store.GetAccountBalance(ctx, "vtxo_balance")
	require.NoError(t, err)
	require.Equal(t, int64(19000), bal)

	// transfers_in: debits=0, credits=20000 => -20000.
	bal, err = store.GetAccountBalance(ctx, "transfers_in")
	require.NoError(t, err)
	require.Equal(t, int64(-20000), bal)

	// onchain_fees: debits=250, credits=0 => 250.
	bal, err = store.GetAccountBalance(ctx, "onchain_fees")
	require.NoError(t, err)
	require.Equal(t, int64(250), bal)

	// Total operator fees paid = debits to fees_paid = 6000.
	total, err := store.GetTotalOperatorFeesPaid(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(6000), total)
}
