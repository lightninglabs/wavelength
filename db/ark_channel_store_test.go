package db

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestArkChannelStoreRoundTrip preserves source identity, unsigned SCIDs and
// preparation timing through revisioned SQL writes and reloads.
func TestArkChannelStoreRoundTrip(t *testing.T) {
	t.Parallel()
	store := newArkChannelStoreForTest(t)
	coordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	terms := testArkChannelTerms(t, arkchannel.KindPromotion, 1)
	terms.ReservedSCID = lnwire.ShortChannelID{
		BlockHeight: 0x800001, TxIndex: 1,
	}.ToUint64()
	requested, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	require.False(t, requested.CreatedAt.IsZero())
	require.False(t, requested.UpdatedAt.IsZero())
	require.True(t, requested.PrePONRStartedAt.IsZero())
	armed, _, err := coordinator.Apply(
		t.Context(), terms.ID, &arkchannel.OORPreparationStarted{},
	)
	require.NoError(t, err)
	require.True(t, armed.Snapshot.OORPreparationStarted)
	require.Nil(t, armed.Snapshot.Source)
	require.False(t, armed.PrePONRStartedAt.IsZero())
	binding := testArkChannelBinding(terms)
	bound, _, err := coordinator.Apply(
		t.Context(), terms.ID, &arkchannel.BindVTXO{
			Binding: binding,
		},
	)
	require.NoError(t, err)
	loaded, err := store.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, bound, loaded)
	require.Equal(t, binding, *loaded.Snapshot.Source)
	require.Equal(t, armed.PrePONRStartedAt, loaded.PrePONRStartedAt)
	require.Equal(t, terms.ReservedSCID, loaded.Snapshot.Terms.ReservedSCID)
	byPending, err := store.GetByPendingChannelID(
		t.Context(), terms.PendingChannelID,
	)
	require.NoError(t, err)
	require.Equal(t, loaded, byPending)
	_, err = store.CompareAndSwap(
		t.Context(), terms.ID, requested.Revision, requested.Snapshot,
	)
	require.ErrorIs(t, err, arkchannel.ErrConflict)
	remaining, err := store.ListNonTerminal(t.Context())
	require.NoError(t, err)
	require.Len(t, remaining, 1)
}

// TestArkChannelStoreRejectsDuplicateProtocolBindings proves a virtual SCID
// and prepared OOR output cannot identify more than one channel.
func TestArkChannelStoreRejectsDuplicateProtocolBindings(t *testing.T) {
	t.Parallel()

	t.Run("reserved SCID", func(t *testing.T) {
		store := newArkChannelStoreForTest(t)
		coordinator, err := arkchannel.NewCoordinator(store)
		require.NoError(t, err)
		first := testArkChannelTerms(t, arkchannel.KindPromotion, 20)
		second := testArkChannelTerms(t, arkchannel.KindPromotion, 21)
		second.ReservedSCID = first.ReservedSCID

		_, err = coordinator.Request(t.Context(), first)
		require.NoError(t, err)
		_, err = coordinator.Request(t.Context(), second)
		require.Error(t, err)
		require.True(t, IsUniqueConstraintViolation(err), err)
	})

	t.Run("prepared source", func(t *testing.T) {
		store := newArkChannelStoreForTest(t)
		coordinator, err := arkchannel.NewCoordinator(store)
		require.NoError(t, err)
		first := testArkChannelTerms(t, arkchannel.KindPromotion, 22)
		second := first.Clone()
		second.ID[0]++
		second.PendingChannelID[0]++
		second.ReservedSCID += 1 << 16

		_, err = coordinator.Request(t.Context(), first)
		require.NoError(t, err)
		_, err = coordinator.Request(t.Context(), second)
		require.NoError(t, err)
		binding := testArkChannelBinding(first)
		_, _, err = coordinator.Apply(
			t.Context(), first.ID, &arkchannel.BindVTXO{
				Binding: binding,
			},
		)
		require.NoError(t, err)
		_, _, err = coordinator.Apply(
			t.Context(), second.ID, &arkchannel.BindVTXO{
				Binding: binding,
			},
		)
		require.Error(t, err)
		require.True(t, IsUniqueConstraintViolation(err), err)
	})
}

// TestArkChannelStoreNotFound verifies the shared coordinator sentinel.
func TestArkChannelStoreNotFound(t *testing.T) {
	t.Parallel()

	store := newArkChannelStoreForTest(t)
	_, err := store.Get(t.Context(), arkchannel.ID{99})
	require.ErrorIs(t, err, arkchannel.ErrNotFound)
	_, err = store.GetByChannelPoint(
		t.Context(), wire.OutPoint{
			Hash: chainhash.Hash{99},
		},
	)
	require.ErrorIs(t, err, arkchannel.ErrNotFound)
}

// newArkChannelStoreForTest creates a migrated SQL-backed channel store.
func newArkChannelStoreForTest(t *testing.T) *ArkChannelStoreDB {
	t.Helper()

	raw := NewTestDB(t)
	store := NewStore(
		raw.DB, raw.Queries, raw.Backend(), btclog.Disabled,
	)

	return NewArkChannelStore(store, clock.NewDefaultClock())
}

// testArkChannelTerms creates valid immutable SQL fixtures.
func testArkChannelTerms(t *testing.T, kind arkchannel.Kind,
	seed byte) arkchannel.Terms {

	t.Helper()
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hubKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	newPolicyKey := func(role byte) [33]byte {
		key := testArkChannelPolicyKey(seed, role)
		var serialized [33]byte
		copy(serialized[:], key.PubKey().SerializeCompressed())

		return serialized
	}

	var clientNodeKey, hubNodeKey [33]byte
	copy(clientNodeKey[:], clientKey.PubKey().SerializeCompressed())
	copy(hubNodeKey[:], hubKey.PubKey().SerializeCompressed())

	terms := arkchannel.Terms{
		ID: arkchannel.ID{
			seed,
			1,
		},
		Kind:   kind,
		Funder: arkchannel.PartyClient,
		PendingChannelID: [32]byte{
			seed,
			2,
		},
		Capacity: btcutil.Amount(100_000),
		ReservedSCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_000 + uint32(seed),
			TxIndex:     uint32(seed),
		}.ToUint64(),
		ClientNodeKey: clientNodeKey,
		HubNodeKey:    hubNodeKey,
		VTXO: arkchannel.VTXOTerms{
			ClientArkKey:     newPolicyKey(1),
			HubArkKey:        newPolicyKey(2),
			ArkOperatorKey:   newPolicyKey(3),
			ClientChannelKey: newPolicyKey(4),
			HubChannelKey:    newPolicyKey(5),
			FunderKey:        newPolicyKey(6),
			ChannelDelay:     144,
			FunderDelay:      576,
			MinExitDelay:     144,
		},
	}

	return terms
}

// testArkChannelPolicyKey returns a deterministic policy key for one SQL
// fixture role.
func testArkChannelPolicyKey(seed, role byte) *btcec.PrivateKey {
	keyBytes := bytes.Repeat([]byte{role}, 32)
	keyBytes[0] ^= seed
	key, _ := btcec.PrivKeyFromBytes(keyBytes)

	return key
}

// testArkChannelBinding creates an exact VTXO binding fixture.
func testArkChannelBinding(terms arkchannel.Terms) arkchannel.VTXOBinding {
	policy, pkScript, err := terms.VTXO.Artifacts()
	if err != nil {
		panic(err)
	}

	amount := terms.Capacity + 1_000
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{terms.ID[0], 5},
		},
	})
	for i := 0; i < 3; i++ {
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(i + 1),
			PkScript: []byte{0x51},
		})
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(amount),
		PkScript: pkScript,
	})
	var arkTransaction bytes.Buffer
	if err := tx.Serialize(&arkTransaction); err != nil {
		panic(err)
	}
	sessionID := [32]byte(tx.TxHash())

	return arkchannel.VTXOBinding{
		OORSessionID: sessionID,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash(sessionID),
			Index: 3,
		},
		Amount:         amount,
		ArkTransaction: arkTransaction.Bytes(),
		PolicyTemplate: policy,
		PkScript:       pkScript,
	}
}
