package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
)

// newTestAssetEngineStore returns an engine store on a fresh database.
func newTestAssetEngineStore(t *testing.T) *AssetEngineStore {
	t.Helper()

	base := NewTestDB(t)

	return &AssetEngineStore{
		TransactionExecutor: NewTransactionExecutor(
			base.BaseDB,
			func(tx *sql.Tx) *sqlc.Queries {
				return base.WithTx(tx)
			},
			btclog.Disabled,
		),
	}
}

// testAssetProof returns a proof record that differs per seed.
func testAssetProof(seed byte, owned bool) embedded.AssetProof {
	proof := embedded.AssetProof{
		ProofFile: []byte{
			seed,
			1,
			2,
			3,
		},
		Owned: owned,
	}
	proof.AssetID[0] = seed
	proof.ScriptKey[0] = 0x02
	proof.ScriptKey[1] = seed
	proof.Outpoint = wire.OutPoint{
		Hash: chainhash.Hash{
			seed,
		},
		Index: uint32(seed),
	}

	return proof
}

// TestAssetEngineKeys stores derived keys and script keys and reads them
// back.
func TestAssetEngineKeys(t *testing.T) {
	t.Parallel()

	store := newTestAssetEngineStore(t)
	ctx := t.Context()

	key := embedded.KeyRecord{Family: 212, Index: 7}
	key.PubKey[0] = 0x03
	key.PubKey[5] = 9

	_, err := store.FetchInternalKey(ctx, key.PubKey)
	require.ErrorIs(t, err, embedded.ErrNotFound)

	require.NoError(t, store.InsertInternalKey(ctx, key))
	require.NoError(t, store.InsertInternalKey(ctx, key))
	stored, err := store.FetchInternalKey(ctx, key.PubKey)
	require.NoError(t, err)
	require.Equal(t, key, *stored)

	scriptKey := embedded.ScriptKeyRecord{RawKey: key, Type: 1}
	scriptKey.TweakedKey[0] = 0x02
	scriptKey.TweakedKey[9] = 4

	_, err = store.FetchScriptKey(ctx, scriptKey.TweakedKey)
	require.ErrorIs(t, err, embedded.ErrNotFound)

	require.NoError(t, store.InsertScriptKey(ctx, scriptKey))
	storedScriptKey, err := store.FetchScriptKey(ctx, scriptKey.TweakedKey)
	require.NoError(t, err)
	require.Equal(t, scriptKey, *storedScriptKey)

	// A second insert replaces the record.
	scriptKey.Tweak = []byte{1, 2, 3}
	scriptKey.Type = 2
	require.NoError(t, store.InsertScriptKey(ctx, scriptKey))
	storedScriptKey, err = store.FetchScriptKey(ctx, scriptKey.TweakedKey)
	require.NoError(t, err)
	require.Equal(t, scriptKey, *storedScriptKey)
}

// TestAssetEngineTransfers records a transfer that spends an owned output,
// confirms it with a new output and returns it to pending.
func TestAssetEngineTransfers(t *testing.T) {
	t.Parallel()

	store := newTestAssetEngineStore(t)
	ctx := t.Context()

	input := testAssetProof(1, true)
	kept := testAssetProof(2, false)
	require.NoError(t, store.InsertProof(ctx, input))
	require.NoError(t, store.InsertProof(ctx, kept))

	owned, err := store.ListOwnedProofs(ctx, false)
	require.NoError(t, err)
	require.Equal(t, []embedded.AssetProof{input}, owned)

	transfer := embedded.Transfer{
		AnchorTxid: chainhash.Hash{
			7,
		},
		State:      embedded.TransferPending,
		HeightHint: 100,
		CreatedAt:  time.Unix(1_700_000_000, 5),
		Payload:    []byte("pending"),
	}
	err = store.InsertTransfer(
		ctx, transfer, []embedded.ProofLocator{input.ProofLocator},
	)
	require.NoError(t, err)
	err = store.InsertTransfer(ctx, transfer, nil)
	require.ErrorIs(t, err, embedded.ErrTransferExists)

	// The spent input leaves the unspent listing and keeps its flag when
	// its file is stored again.
	owned, err = store.ListOwnedProofs(ctx, false)
	require.NoError(t, err)
	require.Empty(t, owned)
	require.NoError(t, store.InsertProof(ctx, input))
	owned, err = store.ListOwnedProofs(ctx, true)
	require.NoError(t, err)
	require.Len(t, owned, 1)
	require.True(t, owned[0].Spent)

	// A transfer that spends an unknown output records nothing.
	unknown := testAssetProof(9, true)
	err = store.InsertTransfer(ctx, embedded.Transfer{
		AnchorTxid: chainhash.Hash{8},
		CreatedAt:  time.Unix(1_700_000_001, 0),
		Payload:    []byte("orphan"),
	}, []embedded.ProofLocator{unknown.ProofLocator})
	require.ErrorIs(t, err, embedded.ErrNotFound)
	transfers, err := store.ListTransfers(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, []embedded.Transfer{transfer}, transfers)

	// Confirming stores the output proofs with the new state.
	change := testAssetProof(3, true)
	transfer.State = embedded.TransferConfirmed
	transfer.BlockHash = chainhash.Hash{0xaa}
	transfer.BlockHeight = 101
	transfer.Payload = []byte("confirmed")
	err = store.UpdateTransfer(
		ctx, transfer, []embedded.AssetProof{change}, nil,
	)
	require.NoError(t, err)

	transfers, err = store.ListTransfers(ctx, &transfer.AnchorTxid)
	require.NoError(t, err)
	require.Equal(t, []embedded.Transfer{transfer}, transfers)
	owned, err = store.ListOwnedProofs(ctx, false)
	require.NoError(t, err)
	require.Equal(t, []embedded.AssetProof{change}, owned)

	found, err := store.FetchProofs(
		ctx, change.AssetID, change.ScriptKey, nil,
	)
	require.NoError(t, err)
	require.Equal(t, []embedded.AssetProof{change}, found)
	found, err = store.FetchProofs(
		ctx, kept.AssetID, kept.ScriptKey, &kept.Outpoint,
	)
	require.NoError(t, err)
	require.Equal(t, []embedded.AssetProof{kept}, found)
	found, err = store.FetchProofs(
		ctx, kept.AssetID, kept.ScriptKey, &change.Outpoint,
	)
	require.NoError(t, err)
	require.Empty(t, found)

	// A re-org clears the block and removes the output.
	transfer.State = embedded.TransferPending
	transfer.BlockHash = chainhash.Hash{}
	transfer.BlockHeight = 0
	transfer.Payload = []byte("pending again")
	err = store.UpdateTransfer(
		ctx, transfer, nil,
		[]embedded.ProofLocator{change.ProofLocator},
	)
	require.NoError(t, err)
	transfers, err = store.ListTransfers(ctx, &transfer.AnchorTxid)
	require.NoError(t, err)
	require.Equal(t, []embedded.Transfer{transfer}, transfers)
	owned, err = store.ListOwnedProofs(ctx, false)
	require.NoError(t, err)
	require.Empty(t, owned)

	err = store.UpdateTransfer(ctx, embedded.Transfer{
		AnchorTxid: chainhash.Hash{0xee},
		Payload:    []byte("missing"),
	}, nil, nil)
	require.ErrorIs(t, err, embedded.ErrNotFound)
}

// TestAssetJournal stores and replaces a journal entry.
func TestAssetJournal(t *testing.T) {
	t.Parallel()

	store := newTestAssetEngineStore(t)
	ctx := t.Context()

	_, err := store.LoadJournalEntry(ctx, "deposit-1")
	require.ErrorIs(t, err, ErrAssetJournalEntryNotFound)

	require.NoError(t, store.StoreJournalEntry(ctx, "deposit-1", []byte{1}))
	require.NoError(t, store.StoreJournalEntry(ctx, "deposit-1", []byte{2}))
	value, err := store.LoadJournalEntry(ctx, "deposit-1")
	require.NoError(t, err)
	require.Equal(t, []byte{2}, value)

	require.Error(t, store.StoreJournalEntry(ctx, "deposit-2", nil))
}
