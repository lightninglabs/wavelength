package db

import (
	"database/sql"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newBoardingStoreForTest creates a new BoardingWalletStore using the
// transaction executor pattern for testing.
func newBoardingStoreForTest(t *testing.T) (*BoardingWalletStore, *BaseDB) {
	db := NewTestDB(t)

	boardingDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) BoardingStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	intentDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) PendingIntentStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	store := NewBoardingWalletStore(
		boardingDB, intentDB, &chaincfg.RegressionNetParams,
		clock.NewDefaultClock(),
	)

	return store, db.BaseDB
}

// createTestBoardingAddress creates a test boarding address with proper
// tapscript construction using arkscript.VTXOTapScript.
func createTestBoardingAddress(t *testing.T,
	keyIndex uint32) (*wallet.BoardingAddress, *btcec.PrivateKey) {

	clientPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientPubKey := clientPrivKey.PubKey()

	operatorPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPubKey := operatorPrivKey.PubKey()

	exitDelay := uint32(144)

	// Build the tapscript using the proper VTXO construction.
	tapscript, err := arkscript.VTXOTapScript(
		clientPubKey, operatorPubKey, exitDelay,
	)
	require.NoError(t, err)

	// Create the taproot address from the tapscript.
	taprootKey := txscript.ComputeTaprootOutputKey(
		&arkscript.ARKNUMSKey, tapscript.RootHash,
	)
	address, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	boardingAddr := &wallet.BoardingAddress{
		Address:   address,
		Tapscript: tapscript,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(42),
				Index:  keyIndex,
			},
		},
		OperatorKey: operatorPubKey,
		ExitDelay:   exitDelay,
	}

	return boardingAddr, clientPrivKey
}

// createSweepStoreIntent inserts one confirmed boarding intent for sweep store
// tests.
func createSweepStoreIntent(t *testing.T,
	store *BoardingWalletStore) wallet.BoardingIntent {

	t.Helper()

	return createSweepStoreIntentWithSeed(t, store, 0x99)
}

// createSweepStoreIntentWithSeed inserts one confirmed boarding intent using a
// caller-selected seed for tests that need multiple distinct outpoints.
func createSweepStoreIntentWithSeed(t *testing.T, store *BoardingWalletStore,
	seed byte) wallet.BoardingIntent {

	t.Helper()

	ctx := t.Context()
	boardingAddr, _ := createTestBoardingAddress(t, uint32(seed))
	require.NoError(t, store.InsertBoardingAddress(ctx, boardingAddr))

	pkScript, err := txscript.PayToAddrScript(boardingAddr.Address)
	require.NoError(t, err)

	confTx := wire.NewMsgTx(2)
	confTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{seed},
			Index: 0,
		},
	})
	confTx.AddTxOut(&wire.TxOut{
		Value:    10_000,
		PkScript: pkScript,
	})

	outpoint := wire.OutPoint{
		Hash:  confTx.TxHash(),
		Index: 0,
	}

	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 100,
			ConfHash: chainhash.Hash{
				0xaa,
			},
			ConfTx:   confTx,
			OutPoint: outpoint,
			Amount:   10_000,
		},
		Status: wallet.BoardingStatusConfirmed,
	}
	require.NoError(t, store.InsertBoardingIntents(ctx, intent))

	return intent
}

// insertRoundBoardingIntentForTest links an intent to a persisted round.
func insertRoundBoardingIntentForTest(t *testing.T, db *BaseDB, roundID,
	roundStatus string, intent wallet.BoardingIntent) {

	t.Helper()

	ctx := t.Context()
	require.NoError(
		t,
		db.InsertRound(
			ctx, sqlc.InsertRoundParams{
				RoundID:        roundID,
				Status:         roundStatus,
				CreationTime:   100,
				LastUpdateTime: 100,
			},
		),
	)
	require.NoError(
		t,
		db.InsertRoundBoardingIntent(
			ctx, sqlc.InsertRoundBoardingIntentParams{
				RoundID:       roundID,
				OutpointHash:  intent.Outpoint.Hash[:],
				OutpointIndex: int32(intent.Outpoint.Index),
				ClientKey:     testBytes(33, 0x11),
				OperatorKey:   testBytes(33, 0x22),
				ExitDelay:     144,
			},
		),
	)
}

// dbSweepInputStatus returns the only input status in a one-input sweep test.
func dbSweepInputStatus(t *testing.T,
	inputs []wallet.BoardingSweepInputRecord) string {

	t.Helper()

	require.Len(t, inputs, 1)

	return inputs[0].Status
}

// TestBoardingAddressRoundTrip tests inserting and retrieving a boarding
// address.
func TestBoardingAddressRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	// Create a test boarding address.
	boardingAddr, _ := createTestBoardingAddress(t, 10)

	// Insert the boarding address.
	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	// Inserting again should be idempotent (no error).
	err = store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	// Retrieve the boarding address by pkScript.
	pkScript, err := txscript.PayToAddrScript(boardingAddr.Address)
	require.NoError(t, err)

	retrievedAddr, err := store.LookupBoardingAddress(ctx, pkScript)
	require.NoError(t, err)
	require.NotNil(t, retrievedAddr)

	// Verify all fields match.
	require.Equal(
		t, boardingAddr.Address.String(),
		retrievedAddr.Address.String(),
	)
	require.Equal(
		t, boardingAddr.KeyDesc.PubKey.SerializeCompressed(),
		retrievedAddr.KeyDesc.PubKey.SerializeCompressed(),
	)
	require.Equal(
		t, boardingAddr.KeyDesc.Family, retrievedAddr.KeyDesc.Family,
	)
	require.Equal(
		t, boardingAddr.KeyDesc.Index, retrievedAddr.KeyDesc.Index,
	)
	require.Equal(
		t, boardingAddr.OperatorKey.SerializeCompressed(),
		retrievedAddr.OperatorKey.SerializeCompressed(),
	)
	require.Equal(t, boardingAddr.ExitDelay, retrievedAddr.ExitDelay)

	// Verify tapscript reconstruction produces equivalent result.
	require.Equal(
		t, boardingAddr.Tapscript.Type, retrievedAddr.Tapscript.Type,
	)
	require.Equal(
		t, boardingAddr.Tapscript.RootHash,
		retrievedAddr.Tapscript.RootHash,
	)
}

// TestBoardingAddressNotFound tests error handling when address doesn't exist.
func TestBoardingAddressNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	// Try to retrieve a non-existent address.
	fakePkScript := []byte{0xff, 0xff, 0xff}
	addr, err := store.LookupBoardingAddress(ctx, fakePkScript)
	require.Error(t, err)
	require.Nil(t, addr)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestListAllBoardingAddresses tests listing all boarding addresses.
func TestListAllBoardingAddresses(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	// Initially, there should be no addresses.
	addresses, err := store.ListAllBoardingAddresses(ctx)
	require.NoError(t, err)
	require.Empty(t, addresses)

	// Insert multiple boarding addresses.
	numAddresses := 5
	for i := 0; i < numAddresses; i++ {
		boardingAddr, _ := createTestBoardingAddress(t, uint32(i))
		err = store.InsertBoardingAddress(ctx, boardingAddr)
		require.NoError(t, err)
	}

	// List all addresses.
	addresses, err = store.ListAllBoardingAddresses(ctx)
	require.NoError(t, err)
	require.Len(t, addresses, numAddresses)

	// Verify each address has valid data.
	for _, addr := range addresses {
		require.NotNil(t, addr.Address)
		require.NotNil(t, addr.KeyDesc.PubKey)
		require.NotNil(t, addr.OperatorKey)
		require.NotNil(t, addr.Tapscript)
	}
}

// TestBoardingIntentLifecycle tests the full lifecycle of a boarding intent.
func TestBoardingIntentLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	// Create a test boarding address first.
	boardingAddr, _ := createTestBoardingAddress(t, 0)

	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	// Create a test boarding intent in confirmed status.
	// (BoardingIntents are only created after on-chain confirmation.)
	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
		},
		Index: 0,
	}

	confHash := chainhash.Hash{0xcc, 0xdd}
	confTx := wire.NewMsgTx(2)
	confTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x11},
			Index: 0,
		},
	})
	confTx.AddTxOut(&wire.TxOut{
		Value:    100000,
		PkScript: []byte{0x51, 0x20},
	})

	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 100,
			ConfHash:   confHash,
			ConfTx:     confTx,
			OutPoint:   outpoint,
			Amount:     100000,
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	// Insert the intent.
	err = store.InsertBoardingIntents(ctx, intent)
	require.NoError(t, err)

	// Retrieve the intent by outpoint.
	retrievedIntent, err := store.GetIntent(ctx, outpoint)
	require.NoError(t, err)
	require.NotNil(t, retrievedIntent)

	// Verify fields.
	require.Equal(t, intent.Outpoint.Hash, retrievedIntent.Outpoint.Hash)
	require.Equal(t, intent.Outpoint.Index, retrievedIntent.Outpoint.Index)
	require.Equal(t, intent.Status, retrievedIntent.Status)
	require.Equal(t, int32(100), retrievedIntent.ChainInfo.ConfHeight)
	require.Equal(t, confHash, retrievedIntent.ChainInfo.ConfHash)
	require.Equal(
		t, btcutil.Amount(100000), retrievedIntent.ChainInfo.Amount,
	)
	require.NotNil(t, retrievedIntent.ChainInfo.ConfTx)
	require.Len(t, retrievedIntent.ChainInfo.ConfTx.TxIn, 1)
	require.Len(t, retrievedIntent.ChainInfo.ConfTx.TxOut, 1)

	// Update the intent status (simulating adoption).
	intent.Status = wallet.BoardingStatusAdopted

	// Re-insert (upsert) with updated status.
	err = store.InsertBoardingIntents(ctx, intent)
	require.NoError(t, err)

	// Retrieve again and verify status update.
	retrievedIntent, err = store.GetIntent(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, wallet.BoardingStatusAdopted, retrievedIntent.Status)
}

// TestBoardingIntentConfirmedReplayDoesNotRegressStatus verifies that repeated
// boarding UTXO detection cannot move an already-adopted intent back to
// confirmed.
func TestBoardingIntentConfirmedReplayDoesNotRegressStatus(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)
	intent := createSweepStoreIntent(t, store)

	require.NoError(
		t, store.UpdateBoardingIntentStatus(
			ctx, intent.Outpoint, wallet.BoardingStatusAdopted,
		),
	)

	intent.Status = wallet.BoardingStatusConfirmed
	require.NoError(t, store.InsertBoardingIntents(ctx, intent))

	retrievedIntent, err := store.GetIntent(ctx, intent.Outpoint)
	require.NoError(t, err)
	require.Equal(t, wallet.BoardingStatusAdopted, retrievedIntent.Status)
}

// TestConfirmedBoardingIntentQueriesIgnoreRoundAdoptedRows verifies stale
// confirmed rows that already have round boarding metadata cannot contribute to
// pending boarding balance or board replay.
func TestConfirmedBoardingIntentQueriesIgnoreRoundAdoptedRows(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db := newBoardingStoreForTest(t)
	intent := createSweepStoreIntent(t, store)

	insertRoundBoardingIntentForTest(
		t, db, "round-issue-564", "confirmed", intent,
	)

	confirmed, err := store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusConfirmed,
	)
	require.NoError(t, err)
	require.Empty(t, confirmed)

	backlog, err := store.FetchBoardingIntentsByStatusAndMinHeight(
		ctx, wallet.BoardingStatusConfirmed, 0,
	)
	require.NoError(t, err)
	require.Empty(t, backlog)

	sweepable, err := store.FetchBoardingIntentsBySweepableStatuses(
		ctx, []wallet.BoardingStatus{
			wallet.BoardingStatusConfirmed,
			wallet.BoardingStatusFailed,
			wallet.BoardingStatusExpired,
		},
	)
	require.NoError(t, err)
	require.Empty(t, sweepable)
}

// TestConfirmedBoardingIntentQueriesKeepFailedRoundRows verifies confirmed
// intents linked only to failed rounds remain visible for retry/recovery.
func TestConfirmedBoardingIntentQueriesKeepFailedRoundRows(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db := newBoardingStoreForTest(t)
	intent := createSweepStoreIntent(t, store)

	insertRoundBoardingIntentForTest(
		t, db, "round-failed", "failed", intent,
	)

	confirmed, err := store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusConfirmed,
	)
	require.NoError(t, err)
	require.Len(t, confirmed, 1)
	require.Equal(t, intent.Outpoint, confirmed[0].Outpoint)

	backlog, err := store.FetchBoardingIntentsByStatusAndMinHeight(
		ctx, wallet.BoardingStatusConfirmed, 0,
	)
	require.NoError(t, err)
	require.Len(t, backlog, 1)
	require.Equal(t, intent.Outpoint, backlog[0].Outpoint)

	sweepable, err := store.FetchBoardingIntentsBySweepableStatuses(
		ctx, []wallet.BoardingStatus{
			wallet.BoardingStatusConfirmed,
			wallet.BoardingStatusFailed,
			wallet.BoardingStatusExpired,
		},
	)
	require.NoError(t, err)
	require.Len(t, sweepable, 1)
	require.Equal(t, intent.Outpoint, sweepable[0].Outpoint)
}

// TestAdoptedBoardingIntentQueriesIgnoreConfirmedRoundRows verifies adopted
// boarding intents stop contributing to pending boarding balance once their
// linked commitment round is confirmed.
func TestAdoptedBoardingIntentQueriesIgnoreConfirmedRoundRows(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db := newBoardingStoreForTest(t)
	intent := createSweepStoreIntent(t, store)

	require.NoError(
		t, store.UpdateBoardingIntentStatus(
			ctx, intent.Outpoint, wallet.BoardingStatusAdopted,
		),
	)

	insertRoundBoardingIntentForTest(
		t, db, "round-input-sig", "input_sig_sent", intent,
	)

	adopted, err := store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusAdopted,
	)
	require.NoError(t, err)
	require.Len(t, adopted, 1)

	require.NoError(
		t,
		db.UpdateRoundStatus(
			ctx, sqlc.UpdateRoundStatusParams{
				RoundID:        "round-input-sig",
				Status:         "confirmed",
				LastUpdateTime: 200,
			},
		),
	)

	adopted, err = store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusAdopted,
	)
	require.NoError(t, err)
	require.Empty(t, adopted)
}

// TestUpdateBoardingIntentStatus verifies the narrow status update helper used
// by recovery flows that do not rewrite the full boarding intent.
func TestUpdateBoardingIntentStatus(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)

	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
		},
		Index: 0,
	}
	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 100,
			ConfHash: chainhash.Hash{
				0xcc,
				0xdd,
			},
			OutPoint: outpoint,
			Amount:   100000,
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	err = store.InsertBoardingIntents(ctx, intent)
	require.NoError(t, err)

	err = store.UpdateBoardingIntentStatus(
		ctx, outpoint, wallet.BoardingStatusSwept,
	)
	require.NoError(t, err)

	retrievedIntent, err := store.GetIntent(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, wallet.BoardingStatusSwept, retrievedIntent.Status)
}

// TestFetchBoardingIntentsByStatus tests filtering intents by status.
func TestFetchBoardingIntentsByStatus(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)

	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	statuses := []wallet.BoardingStatus{
		wallet.BoardingStatusConfirmed,
		// Include two confirmed statuses to test count filtering.
		wallet.BoardingStatusConfirmed,
		wallet.BoardingStatusAdopted,
		wallet.BoardingStatusFailed,
		wallet.BoardingStatusExpired,
		wallet.BoardingStatusSwept,
		wallet.BoardingStatusSweepPending,
	}

	for i, status := range statuses {
		outpoint := wire.OutPoint{
			Hash: chainhash.Hash{
				byte(i),
			},
			Index: 0,
		}

		intent := wallet.BoardingIntent{
			Address:  *boardingAddr,
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: int32(100 + i),
				ConfHash: chainhash.Hash{
					byte(i + 0x10),
				},
				OutPoint: outpoint,
				Amount:   10000,
			},
			Status: status,
		}

		err = store.InsertBoardingIntents(ctx, intent)
		require.NoError(t, err)
	}

	confirmedIntents, err := store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusConfirmed,
	)
	require.NoError(t, err)
	require.Len(t, confirmedIntents, 2)

	for _, intent := range confirmedIntents {
		require.Equal(t, wallet.BoardingStatusConfirmed, intent.Status)
	}

	adoptedIntents, err := store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusAdopted,
	)
	require.NoError(t, err)
	require.Len(t, adoptedIntents, 1)

	expiredIntents, err := store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusExpired,
	)
	require.NoError(t, err)
	require.Len(t, expiredIntents, 1)

	sweptIntents, err := store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusSwept,
	)
	require.NoError(t, err)
	require.Len(t, sweptIntents, 1)

	pendingIntents, err := store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusSweepPending,
	)
	require.NoError(t, err)
	require.Len(t, pendingIntents, 1)
}

// TestBoardingSweepStoreTracksPendingAndResolved verifies the durable sweep
// lifecycle moves intents to pending and only marks them swept after a
// confirmed spend.
func TestBoardingSweepStoreTracksPendingAndResolved(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	intent := createSweepStoreIntent(t, store)
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: intent.Outpoint,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    int64(intent.ChainInfo.Amount - 500),
		PkScript: []byte{txscript.OP_TRUE},
	})
	sweepTxid := sweepTx.TxHash()

	err := store.CreatePendingBoardingSweep(ctx, wallet.NewBoardingSweep{
		Tx:                 sweepTx,
		DestinationAddress: "bcrt1test",
		TotalAmount:        intent.ChainInfo.Amount,
		FeeAmount:          500,
		FeeRateSatPerVByte: 2,
		VBytes:             250,
		CreatedHeight:      200,
		Inputs: []wallet.NewBoardingSweepInput{{
			Outpoint:       intent.Outpoint,
			Amount:         intent.ChainInfo.Amount,
			PreviousStatus: intent.Status,
		}},
	})
	require.NoError(t, err)

	updated, err := store.GetIntent(ctx, intent.Outpoint)
	require.NoError(t, err)
	require.Equal(t, wallet.BoardingStatusSweepPending, updated.Status)

	pending, err := store.ListPendingBoardingSweeps(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(
		t, dbSweepInputStatus(t, pending[0].Inputs),
		wallet.BoardingSweepInputStatusPending,
	)
	require.Equal(t, btcutil.Amount(500), pending[0].FeeAmount)
	require.Equal(t, int64(250), pending[0].VBytes)

	allSweeps, err := store.ListBoardingSweeps(ctx, "", 10, 0)
	require.NoError(t, err)
	require.Len(t, allSweeps, 1)
	require.Equal(t, "bcrt1test", allSweeps[0].DestinationAddress)
	require.Equal(t, btcutil.Amount(500), allSweeps[0].FeeAmount)
	require.Equal(t, int64(2), allSweeps[0].FeeRateSatPerVByte)

	filteredSweeps, err := store.ListBoardingSweeps(
		ctx, wallet.BoardingSweepStatusPublished, 10, 0,
	)
	require.NoError(t, err)
	require.Empty(t, filteredSweeps)

	err = store.MarkBoardingSweepPublished(ctx, sweepTxid)
	require.NoError(t, err)

	filteredSweeps, err = store.ListBoardingSweeps(
		ctx, wallet.BoardingSweepStatusPublished, 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, filteredSweeps, 1)

	pending, err = store.ListPendingBoardingSweeps(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(
		t, dbSweepInputStatus(t, pending[0].Inputs),
		wallet.BoardingSweepInputStatusPublished,
	)

	resolved, err := store.MarkBoardingSweepInputSpent(
		ctx, intent.Outpoint, sweepTxid, 222,
	)
	require.NoError(t, err)
	require.True(t, resolved)

	pending, err = store.ListPendingBoardingSweeps(ctx)
	require.NoError(t, err)
	require.Empty(t, pending)

	filteredSweeps, err = store.ListBoardingSweeps(
		ctx, wallet.BoardingSweepStatusConfirmed, 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, filteredSweeps, 1)
	require.True(t, filteredSweeps[0].ConfirmedHeight.Valid)
	require.Equal(t, int32(222), filteredSweeps[0].ConfirmedHeight.Int32)
	require.Equal(
		t, wallet.BoardingSweepInputStatusSpent,
		dbSweepInputStatus(t, filteredSweeps[0].Inputs),
	)

	updated, err = store.GetIntent(ctx, intent.Outpoint)
	require.NoError(t, err)
	require.Equal(t, wallet.BoardingStatusSwept, updated.Status)
}

// TestBoardingSweepFailedRestoresIntent verifies failed broadcasts put inputs
// back into their pre-sweep boarding status.
func TestBoardingSweepFailedRestoresIntent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	intent := createSweepStoreIntent(t, store)
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: intent.Outpoint,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    int64(intent.ChainInfo.Amount - 500),
		PkScript: []byte{txscript.OP_TRUE},
	})
	sweepTxid := sweepTx.TxHash()

	err := store.CreatePendingBoardingSweep(ctx, wallet.NewBoardingSweep{
		Tx:                 sweepTx,
		DestinationAddress: "",
		TotalAmount:        intent.ChainInfo.Amount,
		FeeAmount:          500,
		FeeRateSatPerVByte: 2,
		VBytes:             250,
		CreatedHeight:      200,
		Inputs: []wallet.NewBoardingSweepInput{{
			Outpoint:       intent.Outpoint,
			Amount:         intent.ChainInfo.Amount,
			PreviousStatus: intent.Status,
		}},
	})
	require.NoError(t, err)

	err = store.MarkBoardingSweepFailed(
		ctx, sweepTxid, sql.ErrConnDone,
	)
	require.NoError(t, err)

	updated, err := store.GetIntent(ctx, intent.Outpoint)
	require.NoError(t, err)
	require.Equal(t, wallet.BoardingStatusConfirmed, updated.Status)
}

// TestBoardingSweepExternalSpendResolvesSeparately verifies an externally
// spent sweep input does not make the aggregate look confirmed by our tx.
func TestBoardingSweepExternalSpendResolvesSeparately(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	intent := createSweepStoreIntent(t, store)
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: intent.Outpoint,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    int64(intent.ChainInfo.Amount - 500),
		PkScript: []byte{txscript.OP_TRUE},
	})
	sweepTxid := sweepTx.TxHash()

	err := store.CreatePendingBoardingSweep(ctx, wallet.NewBoardingSweep{
		Tx:                 sweepTx,
		TotalAmount:        intent.ChainInfo.Amount,
		FeeAmount:          500,
		FeeRateSatPerVByte: 2,
		VBytes:             250,
		CreatedHeight:      200,
		Inputs: []wallet.NewBoardingSweepInput{{
			Outpoint:       intent.Outpoint,
			Amount:         intent.ChainInfo.Amount,
			PreviousStatus: intent.Status,
		}},
	})
	require.NoError(t, err)

	externalTxid := chainhash.Hash{0xee}
	resolved, err := store.MarkBoardingSweepInputSpent(
		ctx, intent.Outpoint, externalTxid, 333,
	)
	require.NoError(t, err)
	require.True(t, resolved)
	require.NotEqual(t, sweepTxid, externalTxid)

	external, err := store.ListBoardingSweeps(
		ctx, wallet.BoardingSweepStatusExternalResolved, 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, external, 1)
	require.Equal(
		t, wallet.BoardingSweepInputStatusExternalSpent,
		dbSweepInputStatus(t, external[0].Inputs),
	)
	require.Equal(t, int32(333), external[0].ConfirmedHeight.Int32)

	confirmed, err := store.ListBoardingSweeps(
		ctx, wallet.BoardingSweepStatusConfirmed, 10, 0,
	)
	require.NoError(t, err)
	require.Empty(t, confirmed)
}

// TestActiveBoardingSweepInputUnique verifies the DB enforces that one
// boarding outpoint can only belong to one active sweep at a time.
func TestActiveBoardingSweepInputUnique(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	intent := createSweepStoreIntent(t, store)
	firstTx := wire.NewMsgTx(2)
	firstTx.AddTxIn(&wire.TxIn{PreviousOutPoint: intent.Outpoint})
	firstTx.AddTxOut(&wire.TxOut{
		Value:    9_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	err := store.CreatePendingBoardingSweep(ctx, wallet.NewBoardingSweep{
		Tx:                 firstTx,
		TotalAmount:        intent.ChainInfo.Amount,
		FeeAmount:          1_000,
		FeeRateSatPerVByte: 2,
		VBytes:             250,
		CreatedHeight:      200,
		Inputs: []wallet.NewBoardingSweepInput{{
			Outpoint:       intent.Outpoint,
			Amount:         intent.ChainInfo.Amount,
			PreviousStatus: intent.Status,
		}},
	})
	require.NoError(t, err)

	secondTx := wire.NewMsgTx(2)
	secondTx.AddTxIn(&wire.TxIn{PreviousOutPoint: intent.Outpoint})
	secondTx.AddTxOut(&wire.TxOut{
		Value:    8_000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	secondTx.LockTime = 1

	err = store.CreatePendingBoardingSweep(ctx, wallet.NewBoardingSweep{
		Tx:                 secondTx,
		TotalAmount:        intent.ChainInfo.Amount,
		FeeAmount:          2_000,
		FeeRateSatPerVByte: 2,
		VBytes:             250,
		CreatedHeight:      201,
		Inputs: []wallet.NewBoardingSweepInput{{
			Outpoint:       intent.Outpoint,
			Amount:         intent.ChainInfo.Amount,
			PreviousStatus: intent.Status,
		}},
	})
	require.Error(t, err)
}

// TestListBoardingSweepsPaginates verifies the store only loads the requested
// page window.
func TestListBoardingSweepsPaginates(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	for i := byte(1); i <= 3; i++ {
		intent := createSweepStoreIntentWithSeed(t, store, i)
		sweepTx := wire.NewMsgTx(2)
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: intent.Outpoint,
		})
		sweepTx.AddTxOut(&wire.TxOut{
			Value:    9_000,
			PkScript: []byte{txscript.OP_TRUE},
		})
		sweepTx.LockTime = uint32(i)

		err := store.CreatePendingBoardingSweep(
			ctx, wallet.NewBoardingSweep{
				Tx:                 sweepTx,
				TotalAmount:        intent.ChainInfo.Amount,
				FeeAmount:          1_000,
				FeeRateSatPerVByte: 2,
				VBytes:             250,
				CreatedHeight:      int32(200 + i),
				Inputs: []wallet.NewBoardingSweepInput{{
					Outpoint:       intent.Outpoint,
					Amount:         intent.ChainInfo.Amount,
					PreviousStatus: intent.Status,
				}},
			},
		)
		require.NoError(t, err)
	}

	firstPage, err := store.ListBoardingSweeps(ctx, "", 2, 0)
	require.NoError(t, err)
	require.Len(t, firstPage, 2)

	secondPage, err := store.ListBoardingSweeps(ctx, "", 2, 2)
	require.NoError(t, err)
	require.Len(t, secondPage, 1)
}

// TestFetchBoardingIntents tests fetching all intents.
func TestFetchBoardingIntents(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)

	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	// Insert 4 intents with different statuses.
	statuses := []wallet.BoardingStatus{
		wallet.BoardingStatusConfirmed,
		wallet.BoardingStatusConfirmed,
		wallet.BoardingStatusAdopted,
		wallet.BoardingStatusFailed,
	}

	for i, status := range statuses {
		outpoint := wire.OutPoint{
			Hash: chainhash.Hash{
				byte(i),
			},
			Index: 0,
		}

		intent := wallet.BoardingIntent{
			Address:  *boardingAddr,
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: int32(100 + i),
				ConfHash: chainhash.Hash{
					byte(i + 0x10),
				},
				OutPoint: outpoint,
				Amount:   10000,
			},
			Status: status,
		}

		err = store.InsertBoardingIntents(ctx, intent)
		require.NoError(t, err)
	}

	intents, err := store.FetchBoardingIntents(ctx)
	require.NoError(t, err)

	require.Len(t, intents, 4)
}

// TestLookupIntentByScript tests looking up an intent by pkScript.
func TestLookupIntentByScript(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)

	pkScript, err := txscript.PayToAddrScript(boardingAddr.Address)
	require.NoError(t, err)

	err = store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x99,
			0x88,
		},
		Index: 1,
	}

	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 150,
			ConfHash: chainhash.Hash{
				0xaa,
			},
			OutPoint: outpoint,
			Amount:   20000,
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	err = store.InsertBoardingIntents(ctx, intent)
	require.NoError(t, err)

	retrievedIntent, err := store.LookupIntentByScript(ctx, pkScript)
	require.NoError(t, err)
	require.NotNil(t, retrievedIntent)

	require.Equal(t, outpoint.Hash, retrievedIntent.Outpoint.Hash)
	require.Equal(t, outpoint.Index, retrievedIntent.Outpoint.Index)
}

// TestLookupIntentByScriptNotFound tests error handling when no intent exists.
func TestLookupIntentByScriptNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	// Try to lookup intent for non-existent pkScript.
	fakePkScript := []byte{0xff, 0xff, 0xff}
	intent, err := store.LookupIntentByScript(ctx, fakePkScript)
	require.Error(t, err)
	require.Nil(t, intent)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestInsertMultipleBoardingIntents tests inserting multiple intents in a
// single transaction.
func TestInsertMultipleBoardingIntents(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)

	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	intents := make([]wallet.BoardingIntent, 3)
	for i := 0; i < 3; i++ {
		outpoint := wire.OutPoint{
			Hash: chainhash.Hash{
				byte(0x10 + i),
			},
			Index: uint32(i),
		}

		intents[i] = wallet.BoardingIntent{
			Address:  *boardingAddr,
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: int32(200 + i),
				ConfHash: chainhash.Hash{
					byte(0x20 + i),
				},
				OutPoint: outpoint,
				Amount:   btcutil.Amount(10000 + i*1000),
			},
			Status: wallet.BoardingStatusConfirmed,
		}
	}

	// Insert all intents in a single transaction.
	err = store.InsertBoardingIntents(ctx, intents...)
	require.NoError(t, err)

	// Verify all were inserted.
	for _, intent := range intents {
		retrieved, err := store.GetIntent(ctx, intent.Outpoint)
		require.NoError(t, err)
		require.NotNil(t, retrieved)
		require.Equal(
			t, wallet.BoardingStatusConfirmed, retrieved.Status,
		)
		require.Equal(
			t, intent.ChainInfo.Amount, retrieved.ChainInfo.Amount,
		)
	}
}

// TestIntentWithConfTx tests storing and retrieving an intent with a
// confirmation transaction.
func TestIntentWithConfTx(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)

	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	// Create a more complex confirmation transaction.
	confTx := wire.NewMsgTx(2)
	confTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x11, 0x22},
			Index: 0,
		},
		Sequence: 0xfffffffe,
	})
	confTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x33, 0x44},
			Index: 1,
		},
		Sequence: 0xfffffffe,
	})
	confTx.AddTxOut(&wire.TxOut{
		Value:    50000,
		PkScript: []byte{0x51, 0x20, 0x01, 0x02},
	})
	confTx.AddTxOut(&wire.TxOut{
		Value:    25000,
		PkScript: []byte{0x51, 0x20, 0x03, 0x04},
	})
	confTx.LockTime = 800000

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xff,
			0xee,
		},
		Index: 0,
	}

	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 300,
			ConfHash: chainhash.Hash{
				0xdd,
				0xcc,
			},
			ConfTx:   confTx,
			OutPoint: outpoint,
			Amount:   75000,
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	err = store.InsertBoardingIntents(ctx, intent)
	require.NoError(t, err)

	// Retrieve and verify the transaction.
	retrieved, err := store.GetIntent(ctx, outpoint)
	require.NoError(t, err)
	require.NotNil(t, retrieved.ChainInfo.ConfTx)

	// Verify transaction structure.
	require.Len(t, retrieved.ChainInfo.ConfTx.TxIn, 2)
	require.Len(t, retrieved.ChainInfo.ConfTx.TxOut, 2)
	require.Equal(t, confTx.LockTime, retrieved.ChainInfo.ConfTx.LockTime)
	require.Equal(
		t, confTx.TxOut[0].Value,
		retrieved.ChainInfo.ConfTx.TxOut[0].Value,
	)
	require.Equal(
		t, confTx.TxOut[1].Value,
		retrieved.ChainInfo.ConfTx.TxOut[1].Value,
	)
}

// TestIntentWithoutConfTx tests storing and retrieving an intent without a
// confirmation transaction.
func TestIntentWithoutConfTx(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)

	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xab,
			0xcd,
		},
		Index: 0,
	}

	// Create intent without ConfTx.
	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 400,
			ConfHash: chainhash.Hash{
				0xef,
				0x01,
			},
			OutPoint: outpoint,
			Amount:   30000,

			// ConfTx is intentionally nil to test handling of
			// intents without a confirmation transaction.
			ConfTx: nil,
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	err = store.InsertBoardingIntents(ctx, intent)
	require.NoError(t, err)

	// Retrieve and verify ConfTx is nil.
	retrieved, err := store.GetIntent(ctx, outpoint)
	require.NoError(t, err)
	require.Nil(t, retrieved.ChainInfo.ConfTx)
	require.Equal(t, int32(400), retrieved.ChainInfo.ConfHeight)
	require.Equal(t, btcutil.Amount(30000), retrieved.ChainInfo.Amount)
}

// TestIntentTxProofRoundTrip exercises the migration-000010 column: an intent
// inserted with a populated TxProof must round-trip through the DB and come
// back with byte-identical proof contents (block header, height, merkle
// proof, claimed outpoint, internal key, merkle root). Without the column,
// the intent reload silently dropped the proof — which is what produced the
// post-restart "TxProof is required" failure in lwwallet mode.
func TestIntentTxProofRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)
	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	// Build a minimal but valid TxProof: a single-tx block lets us
	// derive a real merkle proof rather than crafting one by hand.
	confTx := wire.NewMsgTx(2)
	confTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x11, 0x22},
			Index: 0,
		},
	})
	confTx.AddTxOut(&wire.TxOut{
		Value:    100000,
		PkScript: []byte{0x51, 0x20, 0x03, 0x04},
	})
	merkleProof, err := proof.NewTxMerkleProof(
		[]*wire.MsgTx{confTx}, 0,
	)
	require.NoError(t, err)

	outpoint := wire.OutPoint{Hash: confTx.TxHash(), Index: 0}
	originalProof := proof.TxProof{
		MsgTx: *confTx,
		BlockHeader: wire.BlockHeader{
			Version: 4,
			Bits:    0x1d00ffff,
		},
		BlockHeight:     500,
		MerkleProof:     *merkleProof,
		ClaimedOutPoint: outpoint,
		InternalKey:     *boardingAddr.OperatorKey,
		MerkleRoot: []byte{
			0xaa,
			0xbb,
			0xcc,
			0xdd,
		},
	}

	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 500,
			ConfHash: chainhash.Hash{
				0xab,
				0xcd,
			},
			ConfTx:   confTx,
			OutPoint: outpoint,
			Amount:   100000,
			TxProof:  fn.Some(originalProof),
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	err = store.InsertBoardingIntents(ctx, intent)
	require.NoError(t, err)

	retrieved, err := store.GetIntent(ctx, outpoint)
	require.NoError(t, err)
	require.True(
		t, retrieved.ChainInfo.TxProof.IsSome(),
		"TxProof must survive the DB round-trip",
	)

	got := retrieved.ChainInfo.TxProof.UnsafeFromSome()
	require.Equal(t, originalProof.BlockHeight, got.BlockHeight)
	require.Equal(t, originalProof.ClaimedOutPoint, got.ClaimedOutPoint)
	require.Equal(t, originalProof.MerkleRoot, got.MerkleRoot)
	require.Equal(t, originalProof.MsgTx.TxHash(), got.MsgTx.TxHash())
	require.True(t, originalProof.InternalKey.IsEqual(&got.InternalKey))

	// Also verify the bulk-listing path returns the proof.
	listed, err := store.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusConfirmed,
	)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.True(t, listed[0].ChainInfo.TxProof.IsSome())
}

// TestIntentTxProofMissingDecodesAsNone verifies the legacy-row path: an
// intent inserted before migration-000010 (or written without a proof) loads
// back with TxProof.IsNone() and does not error. The wallet's sendBacklog
// rebuild fallback covers reconstruction; the read path itself must remain
// permissive.
func TestIntentTxProofMissingDecodesAsNone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)
	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xfe,
			0xed,
		},
		Index: 0,
	}
	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 600,
			ConfHash: chainhash.Hash{
				0x12,
				0x34,
			},
			OutPoint: outpoint,
			Amount:   50000,
			// TxProof intentionally None.
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	err = store.InsertBoardingIntents(ctx, intent)
	require.NoError(t, err)

	retrieved, err := store.GetIntent(ctx, outpoint)
	require.NoError(t, err)
	require.True(t, retrieved.ChainInfo.TxProof.IsNone())
}

// TestIntentTxProofCorruptDecodesAsNone verifies the corrupt-blob branch of
// dbIntentToDomainIntent: a non-NULL but malformed tx_proof column must
// decode as fn.None without erroring (the rebuild path in
// wallet.maybeRebuildBoardingProof is the recovery mechanism). This is a
// narrower contract than the NULL case: corruption is unexpected, so the
// store logs at Warn — but observability is the goal, not a hard fail. A
// future change to DeserializeTxProof that makes it stricter (or one that
// introduces a partial-decode bug) without updating this fall-through
// would cause the load path to start erroring; this test pins it down.
func TestIntentTxProofCorruptDecodesAsNone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, baseDB := newBoardingStoreForTest(t)

	boardingAddr, _ := createTestBoardingAddress(t, 0)
	err := store.InsertBoardingAddress(ctx, boardingAddr)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xc0,
			0x01,
		},
		Index: 0,
	}
	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 700,
			ConfHash: chainhash.Hash{
				0xab,
				0xcd,
			},
			OutPoint: outpoint,
			Amount:   50000,
			// Insert with TxProof=None so the row exists; we
			// then UPDATE the column out-of-band with garbage
			// bytes to simulate on-disk corruption.
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	err = store.InsertBoardingIntents(ctx, intent)
	require.NoError(t, err)

	// Inject a malformed TLV blob directly into the tx_proof column.
	// `0xde 0xad 0xbe 0xef` is not a valid TLV record stream for
	// proof.TxProof, so DeserializeTxProof must error. Switch
	// placeholder style on the backend dialect: SQLite uses `?` while
	// Postgres requires `$N`.
	garbage := []byte{0xde, 0xad, 0xbe, 0xef}
	updateQuery := "UPDATE boarding_intents SET tx_proof = ? " +
		"WHERE outpoint_hash = ? AND outpoint_index = ?"
	if baseDB.Backend() == sqlc.BackendTypePostgres {
		updateQuery = "UPDATE boarding_intents SET tx_proof = $1 " +
			"WHERE outpoint_hash = $2 AND outpoint_index = $3"
	}
	_, err = baseDB.ExecContext(
		ctx, updateQuery, garbage, outpoint.Hash[:],
		int32(outpoint.Index),
	)
	require.NoError(t, err)

	// The read path must NOT error: corrupt blobs fall through to
	// None so the rebuild fallback can recover.
	retrieved, err := store.GetIntent(ctx, outpoint)
	require.NoError(t, err)
	require.True(
		t, retrieved.ChainInfo.TxProof.IsNone(),
		"corrupt blob must decode as None, not error",
	)
}
