package arkchannel

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestChannelTermsRejectNonZeroFundingPosition keeps the advertised alias
// consistent with the single output in every channel backing transaction.
func TestChannelTermsRejectNonZeroFundingPosition(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	scid := lnwire.NewShortChanIDFromInt(terms.ReservedSCID)
	scid.TxPosition = 1
	terms.ReservedSCID = scid.ToUint64()

	err := terms.Validate()
	require.ErrorContains(t, err, "funding output position must be zero")
}

// TestChannelTermsRejectUnsafeRefundDelay verifies a funder cannot reclaim the
// VTXO before the channel parties have the configured reaction window.
func TestChannelTermsRejectUnsafeRefundDelay(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	terms.VTXO.FunderDelay = terms.VTXO.ChannelDelay + 431

	err := terms.Validate()
	require.ErrorContains(t, err, "preserve the reaction window")
}

// TestBindingRejectsDifferentChannelPolicy verifies an OOR preparation cannot
// bind an arbitrary output while retaining otherwise valid channel terms.
func TestBindingRejectsDifferentChannelPolicy(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	binding := testBinding(terms)
	binding.PolicyTemplate[0] ^= 1

	err := binding.Validate(terms)
	require.ErrorContains(t, err, "policy does not match")
}

// TestBackingMustBeSignedAndBound rejects a transaction that lnd could not
// safely activate against the exact VTXO.
func TestBackingMustBeSignedAndBound(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	binding := testBinding(terms)
	backing := testBacking(t, terms, binding)

	tx := wire.NewMsgTx(2)
	require.NoError(t, tx.Deserialize(bytes.NewReader(backing.Transaction)))
	tx.TxIn[0].Witness = nil
	backing.Transaction = serializeTx(t, tx)
	backing.ChannelPoint.Hash = tx.TxHash()

	err := backing.Validate(terms, binding)
	require.ErrorContains(t, err, "not fully signed")
}

// testTerms creates valid immutable terms for one channel kind.
func testTerms(t *testing.T, kind Kind) Terms {
	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hubKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	newPolicyKey := func(role byte) [33]byte {
		key := testPolicyPrivateKey(kind, role)
		var serialized [33]byte
		copy(serialized[:], key.PubKey().SerializeCompressed())

		return serialized
	}

	var clientNodeKey, hubNodeKey [33]byte
	copy(clientNodeKey[:], clientKey.PubKey().SerializeCompressed())
	copy(hubNodeKey[:], hubKey.PubKey().SerializeCompressed())

	terms := Terms{
		ID: ID{
			1,
			2,
			byte(kind),
		},
		Kind:   kind,
		Funder: PartyClient,
		PendingChannelID: [32]byte{
			4,
			5,
			byte(kind),
		},
		Capacity: btcutil.Amount(100_000),
		ReservedSCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_000 + uint32(kind),
			TxIndex:     uint32(kind),
		}.ToUint64(),
		ClientNodeKey: clientNodeKey,
		HubNodeKey:    hubNodeKey,
		VTXO: VTXOTerms{
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

// testPolicyPrivateKey returns stable channel-policy keys so generic lifecycle
// fixtures can construct real signed backing transactions.
func testPolicyPrivateKey(kind Kind, role byte) *btcec.PrivateKey {
	seed := bytes.Repeat([]byte{role}, 32)
	seed[0] ^= byte(kind)
	key, _ := btcec.PrivKeyFromBytes(seed)

	return key
}

// testBinding creates one exact prepared OOR output for channel terms.
func testBinding(terms Terms) VTXOBinding {
	policy, pkScript, err := terms.VTXO.Artifacts()
	if err != nil {
		panic(err)
	}
	amount := terms.Capacity + 1_000
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{10, byte(terms.Kind)},
		},
	})
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	tx.AddTxOut(&wire.TxOut{Value: 2, PkScript: []byte{0x51}})
	tx.AddTxOut(&wire.TxOut{Value: int64(amount), PkScript: pkScript})
	var arkTransaction bytes.Buffer
	if err := tx.Serialize(&arkTransaction); err != nil {
		panic(err)
	}
	sessionID := [32]byte(tx.TxHash())

	return VTXOBinding{
		OORSessionID: sessionID,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash(sessionID),
			Index: 2,
		},
		Amount:         amount,
		ArkTransaction: arkTransaction.Bytes(),
		PolicyTemplate: policy,
		PkScript:       pkScript,
	}
}

// testBacking creates a signed single-input funding transaction.
func testBacking(t *testing.T, terms Terms, binding VTXOBinding) Backing {
	t.Helper()

	packet, err := psbt.New(nil, []*wire.TxOut{{
		Value:    int64(terms.Capacity),
		PkScript: []byte{0x51, 0x20, 1},
	}}, 2, 0, nil)
	require.NoError(t, err)
	template, err := NewBackingTemplate(packet, terms, binding)
	require.NoError(t, err)
	clientSig := signBacking(
		t, template, terms, PartyClient,
		testPolicyPrivateKey(terms.Kind, 4),
	)
	hubSig := signBacking(
		t, template, terms, PartyHub,
		testPolicyPrivateKey(terms.Kind, 5),
	)
	backing, err := template.Complete(terms, binding, clientSig, hubSig)
	require.NoError(t, err)

	return backing
}

// serializeTx serializes one witness transaction for a test fixture.
func serializeTx(t *testing.T, tx *wire.MsgTx) []byte {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, tx.Serialize(&buf))

	return buf.Bytes()
}
