package swaps

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	swapsqlc "github.com/lightninglabs/darepo-client/sdk/swaps/sqlc"
	"github.com/stretchr/testify/require"
)

// newTestSwapStore opens one isolated swap SQLite database in a temp
// directory and closes it automatically when the test ends.
func newTestSwapStore(t *testing.T) *Store {
	t.Helper()

	store, err := NewSqliteStore(&SqliteStoreConfig{
		DatabaseFileName: filepath.Join(
			t.TempDir(), DefaultSqliteDatabaseFileName,
		),
	}, btclog.Disabled)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	return store
}

// sqliteTableExists reports whether one sqlite table exists in the test store.
func sqliteTableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()

	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master "+
			"WHERE type = 'table' AND name = ?",
		table,
	).Scan(&count)
	require.NoError(t, err)

	return count == 1
}

// TestSessionMutateAndPersistRollsBackMutateError verifies failed transition
// closures do not leave partially-applied in-memory state behind.
func TestSessionMutateAndPersistRollsBackMutateError(t *testing.T) {
	t.Parallel()

	failErr := errors.New("transition failed")

	receive := &ReceiveSession{
		state:       ReceiveStateCreated,
		vhtlcAmount: 1,
	}
	err := receive.mutateAndPersist(t.Context(), func() error {
		receive.state = ReceiveStateInvoiceCreated
		receive.vhtlcAmount = 2

		return failErr
	})
	require.ErrorIs(t, err, failErr)
	require.Equal(t, ReceiveStateCreated, receive.state)
	require.EqualValues(t, 1, receive.vhtlcAmount)

	pay := &paySession{
		state:       PayStateCreated,
		vhtlcAmount: 1,
	}
	err = pay.mutateAndPersist(t.Context(), func() error {
		pay.state = PayStateSwapCreated
		pay.vhtlcAmount = 2

		return failErr
	})
	require.ErrorIs(t, err, failErr)
	require.Equal(t, PayStateCreated, pay.state)
	require.EqualValues(t, 1, pay.vhtlcAmount)
}

// TestSwapSqliteStoreRunsMigrations verifies that the isolated swap store
// creates its own schema and migration bookkeeping table.
func TestSwapSqliteStoreRunsMigrations(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	require.True(t, sqliteTableExists(
		t, store.DB(), "receive_swaps",
	))
	require.True(t, sqliteTableExists(
		t, store.DB(), "pay_swaps",
	))
	require.True(t, sqliteTableExists(
		t, store.DB(), DefaultMigrationsTable,
	))
	require.True(t, sqliteTableExists(
		t, store.DB(), "receive_auth_keys",
	))
}

// TestReceiveAuthKeyPersistsAcrossRestart verifies the locally generated
// receive-auth key is durable and reused by a new client instance.
func TestReceiveAuthKeyPersistsAcrossRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), DefaultSqliteDatabaseFileName)

	store, err := NewSqliteStore(&SqliteStoreConfig{
		DatabaseFileName: dbPath,
	}, btclog.Disabled)
	require.NoError(t, err)

	client := NewSwapClientWithStore(nil, nil, nil, nil, store)
	key, err := client.receiveAuthKey(ctx)
	require.NoError(t, err)
	firstPubKey := key.PubKey().SerializeCompressed()

	row, err := store.queries.GetReceiveAuthKey(ctx, receiveAuthKeyID)
	require.NoError(t, err)
	require.Len(t, row, receiveAuthKeyLen)
	require.NoError(t, store.Close())

	store, err = NewSqliteStore(&SqliteStoreConfig{
		DatabaseFileName: dbPath,
	}, btclog.Disabled)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	client = NewSwapClientWithStore(nil, nil, nil, nil, store)
	key, err = client.receiveAuthKey(ctx)
	require.NoError(t, err)
	require.Equal(t, firstPubKey, key.PubKey().SerializeCompressed())
}

// TestListSwapSummariesIncludesFeesAndPendingFilter verifies the public list
// API returns both directions, exposes pay fees, and can filter to resumable
// sessions.
func TestListSwapSummariesIncludesFeesAndPendingFilter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newTestSwapStore(t)

	client := NewSwapClientWithStore(nil, nil, nil, nil, store)

	payHash := testHash(1)
	receiveHash := testHash(2)
	completedHash := testHash(3)
	receivePreimage := testHash(4)
	fundingState := PayStateFundingInitiated.String()
	receiveState := ReceiveStateInvoiceCreated.String()
	expiryUnix := time.Unix(1_700, 0).Unix()

	err := store.queries.UpsertPaySwap(ctx, swapsqlc.UpsertPaySwapParams{
		PaymentHash:                          payHash[:],
		Invoice:                              "ln-pay",
		MaxFeeSat:                            999,
		State:                                fundingState,
		AmountSat:                            42_000,
		FeeSat:                               123,
		ExpiryUnix:                           expiryUnix,
		ClientPubkey:                         testPubKeyBytes(2),
		OperatorPubkey:                       testPubKeyBytes(3),
		ServerPubkey:                         testPubKeyBytes(4),
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		VhtlcPkscript:                        []byte{0x51, 0x20},
		VhtlcPolicyTemplate:                  []byte{0x01},
		CreatedAtUnix:                        time.Unix(10, 0).Unix(),
		UpdatedAtUnix:                        time.Unix(20, 0).Unix(),
	})
	require.NoError(t, err)

	err = store.queries.UpsertReceiveSwap(
		ctx, swapsqlc.UpsertReceiveSwapParams{
			PaymentHash:         receiveHash[:],
			AmountSat:           21_000,
			State:               receiveState,
			Invoice:             "ln-receive",
			Preimage:            receivePreimage[:],
			DeadlineUnix:        time.Unix(1_800, 0).Unix(),
			ClientPubkey:        testPubKeyBytes(5),
			PaymentAddr:         []byte{},
			OperatorPubkey:      testPubKeyBytes(6),
			SwapServerPubkey:    testPubKeyBytes(7),
			RefundLocktime:      155,
			VhtlcPkscript:       []byte{0x51, 0x21},
			VhtlcPolicyTemplate: []byte{0x02},
			CreatedAtUnix:       time.Unix(11, 0).Unix(),
			UpdatedAtUnix:       time.Unix(21, 0).Unix(),
		},
	)
	require.NoError(t, err)

	err = store.queries.UpsertPaySwap(ctx, swapsqlc.UpsertPaySwapParams{
		PaymentHash:         completedHash[:],
		Invoice:             "ln-complete",
		State:               PayStateCompleted.String(),
		AmountSat:           50_000,
		FeeSat:              321,
		ExpiryUnix:          time.Unix(1_900, 0).Unix(),
		ClientPubkey:        testPubKeyBytes(8),
		OperatorPubkey:      testPubKeyBytes(9),
		ServerPubkey:        testPubKeyBytes(10),
		VhtlcPkscript:       []byte{0x51, 0x22},
		VhtlcPolicyTemplate: []byte{0x02},
		CreatedAtUnix:       time.Unix(12, 0).Unix(),
		UpdatedAtUnix:       time.Unix(22, 0).Unix(),
	})
	require.NoError(t, err)

	all, err := client.ListSwapSummaries(ctx, false)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, SwapDirectionPay, all[0].Direction)
	require.EqualValues(t, 123, all[0].FeeSat)
	require.EqualValues(t, 999, all[0].MaxFeeSat)
	require.True(t, all[0].Pending)
	require.Equal(t, SwapDirectionReceive, all[1].Direction)
	require.EqualValues(t, 0, all[1].FeeSat)
	require.True(t, all[1].Pending)
	require.False(t, all[2].Pending)

	pending, err := client.ListSwapSummaries(ctx, true)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	require.True(t, pending[0].Pending)
	require.True(t, pending[1].Pending)

	paySummary, err := client.GetSwapSummary(ctx, payHash)
	require.NoError(t, err)
	require.Equal(t, SwapDirectionPay, paySummary.Direction)
	require.Equal(t, fundingState, paySummary.State)

	receiveSummary, err := client.GetSwapSummary(ctx, receiveHash)
	require.NoError(t, err)
	require.Equal(t, SwapDirectionReceive, receiveSummary.Direction)
	require.Equal(t, receiveState, receiveSummary.State)
}

// testHash returns a deterministic 32-byte hash-like value.
func testHash(seed byte) [32]byte {
	var hash [32]byte
	for i := range hash {
		hash[i] = seed
	}

	return hash
}

// testPubKeyBytes returns deterministic compressed-pubkey-shaped bytes.
func testPubKeyBytes(seed byte) []byte {
	pubkey := make([]byte, 33)
	pubkey[0] = 2
	for i := 1; i < len(pubkey); i++ {
		pubkey[i] = seed
	}

	return pubkey
}
