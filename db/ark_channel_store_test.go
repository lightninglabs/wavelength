package db

import (
	"bytes"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestArkChannelStoreRoundTrip verifies revisions, optional backing fields,
// unsigned SCIDs, and non-terminal recovery scans.
func TestArkChannelStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := newArkChannelStoreForTest(t)
	coordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	terms := testArkChannelTerms(t, arkchannel.KindReceiveIntent, 1)
	terms.ReservedSCID = math.MaxUint64

	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	binding := testArkChannelBinding(terms)
	bound, _, err := coordinator.Apply(
		t.Context(), terms.ID, &arkchannel.BindVTXO{
			Binding: binding,
		},
	)
	require.NoError(t, err)
	backing := testArkChannelBacking(t, terms, binding)
	for _, event := range []arkchannel.Event{
		&arkchannel.BackingSigned{
			Backing: backing,
		},
		&arkchannel.FundingFinalized{
			Party: arkchannel.PartyClient,
		},
		&arkchannel.FundingFinalized{
			Party: arkchannel.PartyHub,
		},
	} {
		_, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}

	loaded, err := store.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseBackingReady, loaded.Snapshot.Phase)
	require.Equal(
		t, uint64(math.MaxUint64), loaded.Snapshot.Terms.ReservedSCID,
	)
	require.Equal(t, backing, *loaded.Snapshot.Backing)
	require.Equal(t, binding, *loaded.Snapshot.Source)

	_, err = store.CompareAndSwap(
		t.Context(), terms.ID, bound.Revision, bound.Snapshot,
	)
	require.ErrorIs(t, err, arkchannel.ErrConflict)

	failedTerms := testArkChannelTerms(t, arkchannel.KindPromotion, 2)
	_, err = coordinator.Request(t.Context(), failedTerms)
	require.NoError(t, err)
	_, _, err = coordinator.Apply(
		t.Context(), failedTerms.ID, &arkchannel.Fail{
			Reason: "negotiation rejected",
		},
	)
	require.NoError(t, err)

	records, err := store.ListNonTerminal(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, terms.ID, records[0].Snapshot.Terms.ID)
}

// TestArkChannelStoreNotFound verifies the shared coordinator sentinel.
func TestArkChannelStoreNotFound(t *testing.T) {
	t.Parallel()

	store := newArkChannelStoreForTest(t)
	_, err := store.Get(t.Context(), arkchannel.ID{99})
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
	newPolicyKey := func() [33]byte {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)

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
		Capacity:      btcutil.Amount(100_000),
		ClientNodeKey: clientNodeKey,
		HubNodeKey:    hubNodeKey,
		VTXO: arkchannel.VTXOTerms{
			ClientArkKey:     newPolicyKey(),
			HubArkKey:        newPolicyKey(),
			ArkOperatorKey:   newPolicyKey(),
			ClientChannelKey: newPolicyKey(),
			HubChannelKey:    newPolicyKey(),
			FunderKey:        newPolicyKey(),
			ChannelDelay:     144,
			FunderDelay:      576,
			MinExitDelay:     144,
		},
	}
	if kind == arkchannel.KindReceiveIntent {
		terms.Funder = arkchannel.PartyHub
		terms.ReservedSCID = uint64(seed)
		terms.PaymentHash = [32]byte{seed, 4}
	}

	return terms
}

// testArkChannelBinding creates an exact VTXO binding fixture.
func testArkChannelBinding(terms arkchannel.Terms) arkchannel.VTXOBinding {
	policy, pkScript, err := terms.VTXO.Artifacts()
	if err != nil {
		panic(err)
	}

	return arkchannel.VTXOBinding{
		OutPoint: wire.OutPoint{
			Hash: chainhash.Hash{
				terms.ID[0],
				5,
			},
			Index: 3,
		},
		Amount:  terms.Capacity + 1_000,
		RoundID: "round-store-test",
		CommitmentTxID: chainhash.Hash{
			terms.ID[0],
			6,
		},
		PolicyTemplate: policy,
		PkScript:       pkScript,
	}
}

// testArkChannelBacking creates a signed funding transaction fixture.
func testArkChannelBacking(t *testing.T, terms arkchannel.Terms,
	binding arkchannel.VTXOBinding) arkchannel.Backing {

	t.Helper()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: binding.OutPoint,
		Witness:          wire.TxWitness{bytes.Repeat([]byte{1}, 64)},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(terms.Capacity),
		PkScript: []byte{0x51, 0x20, 7},
	})

	var encoded bytes.Buffer
	require.NoError(t, tx.Serialize(&encoded))

	return arkchannel.Backing{
		Transaction: encoded.Bytes(),
		ChannelPoint: wire.OutPoint{
			Hash: tx.TxHash(),
		},
	}
}
