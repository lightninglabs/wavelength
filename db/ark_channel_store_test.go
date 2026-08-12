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
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
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
	terms.ReservedSCID = lnwire.ShortChannelID{
		BlockHeight: 0x800001,
		TxIndex:     1,
	}.ToUint64()

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
	require.Equal(t, terms.ReservedSCID, loaded.Snapshot.Terms.ReservedSCID)
	require.Equal(t, backing, *loaded.Snapshot.Backing)
	require.Equal(t, binding, *loaded.Snapshot.Source)
	byPendingID, err := store.GetByPendingChannelID(
		t.Context(), terms.PendingChannelID,
	)
	require.NoError(t, err)
	require.Equal(t, loaded, byPendingID)
	byChannelPoint, err := store.GetByChannelPoint(
		t.Context(), backing.ChannelPoint,
	)
	require.NoError(t, err)
	require.Equal(t, loaded, byChannelPoint)

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

// TestArkChannelStoreCooperativeCloseRoundTrip verifies the compact SQL facts
// reconstruct and script-validate the exact signed direct VTXO settlement.
func TestArkChannelStoreCooperativeCloseRoundTrip(t *testing.T) {
	t.Parallel()

	store := newArkChannelStoreForTest(t)
	coordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	terms := testArkChannelTerms(t, arkchannel.KindPromotion, 8)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hubKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	terms.VTXO.ClientArkKey = testArkChannelCompressedKey(clientKey)
	terms.VTXO.HubArkKey = testArkChannelCompressedKey(hubKey)
	terms.VTXO.ArkOperatorKey = testArkChannelCompressedKey(operatorKey)
	binding := testArkChannelBinding(terms)
	backing := testArkChannelBacking(t, terms, binding)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	for _, event := range []arkchannel.Event{
		&arkchannel.BindVTXO{
			Binding: binding,
		},
		&arkchannel.BackingSigned{
			Backing: backing,
		},
		&arkchannel.FundingFinalized{
			Party: arkchannel.PartyClient,
		},
		&arkchannel.FundingFinalized{
			Party: arkchannel.PartyHub,
		},
		&arkchannel.OORFinalized{
			SessionID: binding.OORSessionID,
		},
		&arkchannel.RecoveryPackageInstalled{},
		&arkchannel.ChannelActive{
			ChannelPointHash:  backing.ChannelPoint.Hash,
			ChannelPointIndex: backing.ChannelPoint.Index,
		},
	} {
		_, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}
	request := arkchannel.CooperativeCloseRequest{
		Initiator: arkchannel.PartyClient,
		ClientDeliveryScript: append(
			[]byte{0x00, 0x14}, bytes.Repeat([]byte{1}, 20)...,
		),
		HubDeliveryScript: append(
			[]byte{0x00, 0x14}, bytes.Repeat([]byte{2}, 20)...,
		),
		FeeRate: chainfee.SatPerKWeight(1_000),
	}
	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &arkchannel.RequestCooperativeClose{
			Request: request,
		},
	)
	require.NoError(t, err)
	template, err := arkchannel.NewCooperativeCloseTemplate(
		terms, binding, request, 70_000, 30_000, 9,
	)
	require.NoError(t, err)
	clientSig := testArkChannelCloseSignature(
		t, template, terms, arkchannel.PartyClient, clientKey,
	)
	hubSig := testArkChannelCloseSignature(
		t, template, terms, arkchannel.PartyHub, hubKey,
	)
	operatorDesc, err := template.OperatorSignDescriptor(
		terms, keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	)
	require.NoError(t, err)
	operatorSig, err := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	).SignOutputRaw(
		testArkChannelProposalTx(t, template.Proposal()), operatorDesc,
	)
	require.NoError(t, err)
	settlement, err := template.Complete(
		terms, binding, request, clientSig, hubSig, operatorSig,
	)
	require.NoError(t, err)
	for _, event := range []arkchannel.Event{
		&arkchannel.CooperativeCloseSigned{
			Close: settlement,
			Party: arkchannel.PartyClient,
		},
		&arkchannel.CooperativeCloseSigned{
			Close: settlement,
			Party: arkchannel.PartyHub,
		},
		&arkchannel.CooperativeClosePublished{
			TxID: settlement.TxID,
		},
		&arkchannel.CooperativeCloseFinalized{
			Party: arkchannel.PartyClient,
		},
	} {
		_, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}

	loaded, err := store.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(
		t, arkchannel.PhaseCoopClosePublished, loaded.Snapshot.Phase,
	)
	require.Equal(t, request, *loaded.Snapshot.CooperativeCloseRequest)
	require.Equal(t, settlement, *loaded.Snapshot.CooperativeClose)
	require.True(t, loaded.Snapshot.ClientCloseFinalized)
	require.False(t, loaded.Snapshot.HubCloseFinalized)

	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &arkchannel.CooperativeCloseFinalized{
			Party: arkchannel.PartyHub,
		},
	)
	require.NoError(t, err)
	loaded, err = store.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseClosed, loaded.Snapshot.Phase)
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

// TestArkChannelStoreReceiveClaimKind verifies the receive fallback channel
// kind survives the SQL constraint and domain round trip.
func TestArkChannelStoreReceiveClaimKind(t *testing.T) {
	t.Parallel()

	store := newArkChannelStoreForTest(t)
	coordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	terms := testArkChannelTerms(t, arkchannel.KindReceiveClaim, 9)

	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	loaded, err := store.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(
		t, arkchannel.KindReceiveClaim, loaded.Snapshot.Terms.Kind,
	)
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
		Capacity: btcutil.Amount(100_000),
		ReservedSCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_000 + uint32(seed),
			TxIndex:     uint32(seed),
		}.ToUint64(),
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
	}
	if kind == arkchannel.KindReceiveIntent ||
		kind == arkchannel.KindReceiveClaim {

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

// testArkChannelCompressedKey encodes one policy key.
func testArkChannelCompressedKey(key *btcec.PrivateKey) [33]byte {
	var encoded [33]byte
	copy(encoded[:], key.PubKey().SerializeCompressed())

	return encoded
}

// testArkChannelCloseSignature signs one endpoint's immediate VTXO role.
func testArkChannelCloseSignature(t *testing.T,
	template *arkchannel.CooperativeCloseTemplate, terms arkchannel.Terms,
	party arkchannel.Party, key *btcec.PrivateKey) input.Signature {

	t.Helper()
	desc, err := template.SignDescriptor(
		terms, party, keychain.KeyDescriptor{
			PubKey: key.PubKey(),
		},
	)
	require.NoError(t, err)
	sig, err := input.NewMockSigner(
		[]*btcec.PrivateKey{key}, nil,
	).SignOutputRaw(
		testArkChannelProposalTx(t, template.Proposal()), desc,
	)
	require.NoError(t, err)

	return sig
}

// testArkChannelProposalTx decodes one unsigned close proposal.
func testArkChannelProposalTx(t *testing.T,
	proposal arkchannel.CooperativeCloseProposal) *wire.MsgTx {

	t.Helper()
	tx := wire.NewMsgTx(2)
	require.NoError(
		t,
		tx.Deserialize(
			bytes.NewReader(proposal.Transaction),
		),
	)

	return tx
}
