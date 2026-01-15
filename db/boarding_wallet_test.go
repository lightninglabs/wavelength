package db

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newBoardingStoreForTest creates a new BoardingWalletStore using the
// transaction executor pattern for testing.
func newBoardingStoreForTest(
	t *testing.T) (*BoardingWalletStore, *BaseDB) {

	db := NewTestDB(t)

	boardingDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) BoardingStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	store := NewBoardingWalletStore(
		boardingDB, &chaincfg.RegressionNetParams,
		clock.NewDefaultClock(),
	)

	return store, db.BaseDB
}

// createTestBoardingAddress creates a test boarding address with proper
// tapscript construction using scripts.VTXOTapScript.
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
	tapscript, err := scripts.VTXOTapScript(
		clientPubKey, operatorPubKey, exitDelay,
	)
	require.NoError(t, err)

	// Create the taproot address from the tapscript.
	taprootKey := txscript.ComputeTaprootOutputKey(
		&scripts.ARKNUMSKey, tapscript.RootHash,
	)
	address, err := btcutil.NewAddressTaproot(
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
		Hash:  chainhash.Hash{0xaa, 0xbb},
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
	}

	for i, status := range statuses {
		outpoint := wire.OutPoint{
			Hash:  chainhash.Hash{byte(i)},
			Index: 0,
		}

		intent := wallet.BoardingIntent{
			Address:  *boardingAddr,
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: int32(100 + i),
				ConfHash:   chainhash.Hash{byte(i + 0x10)},
				OutPoint:   outpoint,
				Amount:     10000,
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
			Hash:  chainhash.Hash{byte(i)},
			Index: 0,
		}

		intent := wallet.BoardingIntent{
			Address:  *boardingAddr,
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: int32(100 + i),
				ConfHash:   chainhash.Hash{byte(i + 0x10)},
				OutPoint:   outpoint,
				Amount:     10000,
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
		Hash:  chainhash.Hash{0x99, 0x88},
		Index: 1,
	}

	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 150,
			ConfHash:   chainhash.Hash{0xaa},
			OutPoint:   outpoint,
			Amount:     20000,
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
			Hash:  chainhash.Hash{byte(0x10 + i)},
			Index: uint32(i),
		}

		intents[i] = wallet.BoardingIntent{
			Address:  *boardingAddr,
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: int32(200 + i),
				ConfHash:   chainhash.Hash{byte(0x20 + i)},
				OutPoint:   outpoint,
				Amount:     btcutil.Amount(10000 + i*1000),
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
		Hash:  chainhash.Hash{0xff, 0xee},
		Index: 0,
	}

	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 300,
			ConfHash:   chainhash.Hash{0xdd, 0xcc},
			ConfTx:     confTx,
			OutPoint:   outpoint,
			Amount:     75000,
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
		Hash:  chainhash.Hash{0xab, 0xcd},
		Index: 0,
	}

	// Create intent without ConfTx.
	intent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: 400,
			ConfHash:   chainhash.Hash{0xef, 0x01},
			OutPoint:   outpoint,
			Amount:     30000,

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
