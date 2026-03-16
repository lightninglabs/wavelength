package darepod

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/db"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// testReceiveScriptStore is a minimal in-memory owned receive-script store used
// by receive-script unit tests.
type testReceiveScriptStore struct {
	records []db.OwnedReceiveScriptRecord
}

// UpsertOwnedReceiveScript stores or replaces one owned receive-script record.
func (s *testReceiveScriptStore) UpsertOwnedReceiveScript(_ context.Context,
	rec db.OwnedReceiveScriptRecord) error {

	for i := range s.records {
		if string(s.records[i].PkScript) == string(rec.PkScript) {
			s.records[i] = rec

			return nil
		}
	}

	s.records = append(s.records, rec)

	return nil
}

// ListOwnedReceiveScripts returns all tracked owned receive-script records.
func (s *testReceiveScriptStore) ListOwnedReceiveScripts(_ context.Context) (
	[]db.OwnedReceiveScriptRecord, error,
) {

	records := make([]db.OwnedReceiveScriptRecord, len(s.records))
	copy(records, s.records)

	return records, nil
}

// TestEnsureDefaultOORReceiveKeyReusesPersistedKey verifies that the helper
// prefers the most recent persisted wallet-managed receive key.
func TestEnsureDefaultOORReceiveKeyReusesPersistedKey(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	olderKey := testKeyDescriptor(t, 1)
	newerKey := testKeyDescriptor(t, 2)
	store := &testReceiveScriptStore{
		records: []db.OwnedReceiveScriptRecord{
			{
				PkScript:   []byte{0x51},
				ClientKey:  olderKey,
				Source:     db.OwnedReceiveScriptSourceWallet,
				CreatedAt:  time.Unix(10, 0),
				LastUsedAt: fn.None[time.Time](),
			},
			{
				PkScript:   []byte{0x52},
				ClientKey:  newerKey,
				Source:     db.OwnedReceiveScriptSourceWallet,
				CreatedAt:  time.Unix(20, 0),
				LastUsedAt: fn.None[time.Time](),
			},
		},
	}

	derived := false
	keyDesc, err := EnsureDefaultOORReceiveKey(
		ctx, store, func(context.Context) (*keychain.KeyDescriptor, error) {
			derived = true

			return nil, nil
		},
	)
	require.NoError(t, err)
	require.False(t, derived)
	require.NotNil(t, keyDesc)
	require.Equal(
		t,
		newerKey.PubKey.SerializeCompressed(),
		keyDesc.PubKey.SerializeCompressed(),
	)
}

// TestEnsureDefaultOORReceiveKeyDerivesWhenMissing verifies that the helper
// falls back to the provided wallet derivation path when no stored key exists.
func TestEnsureDefaultOORReceiveKeyDerivesWhenMissing(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := &testReceiveScriptStore{}
	expected := testKeyDescriptor(t, 3)

	keyDesc, err := EnsureDefaultOORReceiveKey(
		ctx, store, func(context.Context) (*keychain.KeyDescriptor, error) {
			return &expected, nil
		},
	)
	require.NoError(t, err)
	require.NotNil(t, keyDesc)
	require.Equal(
		t,
		expected.PubKey.SerializeCompressed(),
		keyDesc.PubKey.SerializeCompressed(),
	)
}

// testKeyDescriptor creates a deterministic test key descriptor.
func testKeyDescriptor(t *testing.T, seed byte) keychain.KeyDescriptor {
	t.Helper()

	privKey, _ := btcec.PrivKeyFromBytes(
		[]byte{
			seed, seed, seed, seed, seed, seed, seed, seed,
			seed, seed, seed, seed, seed, seed, seed, seed,
			seed, seed, seed, seed, seed, seed, seed, seed,
			seed, seed, seed, seed, seed, seed, seed, seed,
		},
	)

	return keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: DefaultOORReceiveKeyFamily(),
			Index:  uint32(seed),
		},
		PubKey: privKey.PubKey(),
	}
}
