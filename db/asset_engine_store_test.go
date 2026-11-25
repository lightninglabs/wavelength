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

// testAssetAddress returns an address record that differs per seed.
func testAssetAddress(seed byte, createdAt time.Time) embedded.AddressRecord {
	addr := embedded.AddressRecord{
		Encoded:        "taprt1" + string(rune('a'+seed)),
		CreationHeight: 100 + uint32(seed),
		CreatedAt:      createdAt,
	}
	addr.ScriptKey[0] = 0x02
	addr.ScriptKey[1] = seed
	addr.InternalKey[0] = 0x03
	addr.InternalKey[1] = seed

	return addr
}

// testAssetReceive returns the record of an announcement of outpoint to
// addr.
func testAssetReceive(outpoint wire.OutPoint, addr [33]byte, announcement byte,
	createdAt time.Time) embedded.ReceiveRecord {

	return embedded.ReceiveRecord{
		Outpoint: outpoint,
		Address:  addr,
		Announcement: [32]byte{
			announcement,
		},
		State:     embedded.ReceiveDetected,
		CreatedAt: createdAt,
		Payload: []byte{
			announcement,
		},
	}
}

// TestAssetEngineAddresses records addresses and lists them oldest first,
// with addresses of the same creation time in the order of their insertion.
func TestAssetEngineAddresses(t *testing.T) {
	t.Parallel()

	store := newTestAssetEngineStore(t)
	ctx := t.Context()

	addrs, err := store.ListAddresses(ctx)
	require.NoError(t, err)
	require.Empty(t, addrs)

	// The script keys sort against the insertion order, so that only the
	// insertion can break the tie of the shared creation time.
	created := time.Unix(1_700_000_000, 7)
	second := testAssetAddress(9, created)
	third := testAssetAddress(1, created)
	first := testAssetAddress(5, created.Add(-time.Second))
	for _, addr := range []embedded.AddressRecord{second, third, first} {
		require.NoError(t, store.InsertAddress(ctx, addr))
	}

	addrs, err = store.ListAddresses(ctx)
	require.NoError(t, err)
	require.Equal(t, []embedded.AddressRecord{first, second, third}, addrs)

	// A recorded script key keeps its address.
	replaced := second
	replaced.Encoded = "taprt1other"
	err = store.InsertAddress(ctx, replaced)
	require.ErrorIs(t, err, embedded.ErrAddressExists)
	addrs, err = store.ListAddresses(ctx)
	require.NoError(t, err)
	require.Equal(t, []embedded.AddressRecord{first, second, third}, addrs)
}

// TestAssetEngineReceives records several announcements of one output,
// filters them, confirms one with its proofs, and takes it through a re-org
// of its block to the final state.
func TestAssetEngineReceives(t *testing.T) {
	t.Parallel()

	store := newTestAssetEngineStore(t)
	ctx := t.Context()

	created := time.Unix(1_700_000_000, 3)
	addr := testAssetAddress(1, created)
	other := testAssetAddress(2, created)
	outpoint := wire.OutPoint{Hash: chainhash.Hash{7}, Index: 1}
	elsewhere := wire.OutPoint{Hash: chainhash.Hash{7}, Index: 2}

	forged := testAssetReceive(outpoint, addr.ScriptKey, 9, created)
	honest := testAssetReceive(outpoint, addr.ScriptKey, 1, created)
	foreign := testAssetReceive(outpoint, other.ScriptKey, 1, created)
	earlier := testAssetReceive(
		elsewhere, addr.ScriptKey, 1, created.Add(-time.Second),
	)
	for _, receive := range []embedded.ReceiveRecord{
		forged, honest, foreign, earlier,
	} {
		stored, err := store.InsertReceive(ctx, receive)
		require.NoError(t, err)
		require.Equal(t, receive, *stored)
	}

	// An announcement that is recorded stays as it is.
	again := honest
	again.State = embedded.ReceiveFailed
	again.Payload = []byte("replayed")
	again.CreatedAt = created.Add(time.Hour)
	stored, err := store.InsertReceive(ctx, again)
	require.NoError(t, err)
	require.Equal(t, honest, *stored)

	all, err := store.ListReceives(ctx, embedded.ReceiveFilter{})
	require.NoError(t, err)
	require.Equal(
		t, []embedded.ReceiveRecord{earlier, forged, honest, foreign},
		all,
	)

	ofOutput, err := store.ListReceives(ctx, embedded.ReceiveFilter{
		Outpoint: &outpoint,
		Address:  &addr.ScriptKey,
	})
	require.NoError(t, err)
	require.Equal(t, []embedded.ReceiveRecord{forged, honest}, ofOutput)

	ofAddress, err := store.ListReceives(ctx, embedded.ReceiveFilter{
		Address: &other.ScriptKey,
	})
	require.NoError(t, err)
	require.Equal(t, []embedded.ReceiveRecord{foreign}, ofAddress)

	// Confirming stores the proofs with the new state, and keeps the
	// time the receive was recorded.
	received := testAssetProof(4, true)
	received.Outpoint = outpoint
	honest.State = embedded.ReceiveConfirmed
	honest.BlockHash = chainhash.Hash{0xaa}
	honest.BlockHeight = 101
	honest.Payload = []byte("confirmed")
	update := honest
	update.CreatedAt = created.Add(time.Hour)
	err = store.UpdateReceive(
		ctx, update, []embedded.AssetProof{received},
	)
	require.NoError(t, err)

	state := embedded.ReceiveConfirmed
	confirmed, err := store.ListReceives(ctx, embedded.ReceiveFilter{
		State: &state,
	})
	require.NoError(t, err)
	require.Equal(t, []embedded.ReceiveRecord{honest}, confirmed)
	owned, err := store.ListOwnedProofs(ctx, false)
	require.NoError(t, err)
	require.Equal(t, []embedded.AssetProof{received}, owned)

	// An update of an announcement that is not recorded stores nothing.
	missing := testAssetReceive(elsewhere, addr.ScriptKey, 5, created)
	err = store.UpdateReceive(
		ctx, missing, []embedded.AssetProof{testAssetProof(5, true)},
	)
	require.ErrorIs(t, err, embedded.ErrNotFound)
	owned, err = store.ListOwnedProofs(ctx, false)
	require.NoError(t, err)
	require.Equal(t, []embedded.AssetProof{received}, owned)

	// A transfer spends the received output.
	err = store.InsertTransfer(ctx, embedded.Transfer{
		AnchorTxid: chainhash.Hash{0xcc},
		CreatedAt:  created,
		Payload:    []byte("spend"),
	}, []embedded.ProofLocator{received.ProofLocator})
	require.NoError(t, err)
	spent := received
	spent.Spent = true
	owned, err = store.ListOwnedProofs(ctx, true)
	require.NoError(t, err)
	require.Equal(t, []embedded.AssetProof{spent}, owned)

	// When the block of the receive leaves the chain, the receive returns
	// to detected and its proof is kept without being owned.
	unowned := received
	unowned.Owned = false
	honest.State = embedded.ReceiveDetected
	honest.BlockHash = chainhash.Hash{}
	honest.BlockHeight = 0
	honest.Payload = []byte("block left the chain")
	err = store.UpdateReceive(ctx, honest, []embedded.AssetProof{unowned})
	require.NoError(t, err)

	ofOutput, err = store.ListReceives(ctx, embedded.ReceiveFilter{
		Outpoint: &outpoint,
		Address:  &addr.ScriptKey,
	})
	require.NoError(t, err)
	require.Equal(t, []embedded.ReceiveRecord{forged, honest}, ofOutput)
	owned, err = store.ListOwnedProofs(ctx, true)
	require.NoError(t, err)
	require.Empty(t, owned)

	// Once the confirmation is final the output is owned again, and it
	// is still spent.
	honest.State = embedded.ReceiveFinal
	honest.BlockHash = chainhash.Hash{0xbb}
	honest.BlockHeight = 102
	honest.Payload = []byte("final")
	err = store.UpdateReceive(ctx, honest, []embedded.AssetProof{received})
	require.NoError(t, err)

	state = embedded.ReceiveFinal
	final, err := store.ListReceives(ctx, embedded.ReceiveFilter{
		State: &state,
	})
	require.NoError(t, err)
	require.Equal(t, []embedded.ReceiveRecord{honest}, final)
	owned, err = store.ListOwnedProofs(ctx, true)
	require.NoError(t, err)
	require.Equal(t, []embedded.AssetProof{spent}, owned)
	owned, err = store.ListOwnedProofs(ctx, false)
	require.NoError(t, err)
	require.Empty(t, owned)
}
