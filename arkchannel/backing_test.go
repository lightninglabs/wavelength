package arkchannel

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestBackingTemplateCompletesChannelSpend proves the backing transaction is
// a valid spend of the exact prepared OOR output into lnd's funding output.
func TestBackingTemplateCompletesChannelSpend(t *testing.T) {
	t.Parallel()

	terms, source, clientKey, hubKey := testBackingTerms(t)
	fundingOutput := &wire.TxOut{
		Value: int64(terms.Capacity),
		PkScript: append(
			[]byte{txscript.OP_1, 32}, bytes.Repeat(
				[]byte{2}, 32,
			)...,
		),
	}
	packet, err := psbt.New(
		nil, []*wire.TxOut{fundingOutput}, 2, 0, nil,
	)
	require.NoError(t, err)

	template, err := NewBackingTemplate(packet, terms, source)
	require.NoError(t, err)
	require.Equal(
		t, source.OutPoint,
		template.Transaction().TxIn[0].PreviousOutPoint,
	)
	require.Equal(t, fundingOutput,
		template.Transaction().TxOut[0])
	require.Equal(
		t, template.Transaction().TxHash(),
		template.ChannelPoint().Hash,
	)
	require.NoError(t, template.ValidateFundingOutput(fundingOutput))

	clientSig := signBacking(t, template, terms, PartyClient, clientKey)
	hubSig := signBacking(t, template, terms, PartyHub, hubKey)
	backing, err := template.Complete(
		terms, source, clientSig, hubSig,
	)
	require.NoError(t, err)
	require.NoError(t, backing.Validate(terms, source))
	require.Equal(t, template.ChannelPoint(), backing.ChannelPoint)
}

// TestBackingTemplateRejectsDifferentFundingOutput proves a peer cannot be
// induced to sign a backing transaction for a different lnd reservation.
func TestBackingTemplateRejectsDifferentFundingOutput(t *testing.T) {
	t.Parallel()

	terms, source, _, _ := testBackingTerms(t)
	fundingOutput := &wire.TxOut{
		Value: int64(terms.Capacity),
		PkScript: append(
			[]byte{txscript.OP_1, 32}, bytes.Repeat(
				[]byte{3}, 32,
			)...,
		),
	}
	packet, err := psbt.New(
		nil, []*wire.TxOut{fundingOutput}, 2, 0, nil,
	)
	require.NoError(t, err)
	template, err := NewBackingTemplate(packet, terms, source)
	require.NoError(t, err)

	differentScript := &wire.TxOut{
		Value:    fundingOutput.Value,
		PkScript: bytes.Clone(fundingOutput.PkScript),
	}
	differentScript.PkScript[len(differentScript.PkScript)-1] ^= 1
	require.ErrorContains(
		t, template.ValidateFundingOutput(differentScript),
		"does not fund the local lnd reservation",
	)

	differentAmount := &wire.TxOut{
		Value:    fundingOutput.Value,
		PkScript: bytes.Clone(fundingOutput.PkScript),
	}
	differentAmount.Value--
	require.ErrorContains(
		t, template.ValidateFundingOutput(differentAmount),
		"does not fund the local lnd reservation",
	)
}

// TestBackingTemplateRejectsWrongChannelKey proves role signatures cannot be
// substituted with an unrelated Ark or Lightning key.
func TestBackingTemplateRejectsWrongChannelKey(t *testing.T) {
	t.Parallel()

	terms, source, _, _ := testBackingTerms(t)
	channelScript := append(
		[]byte{txscript.OP_1, 32}, make([]byte, 32)...,
	)
	packet, err := psbt.New(nil, []*wire.TxOut{{
		Value:    int64(terms.Capacity),
		PkScript: channelScript,
	}}, 2, 0, nil)
	require.NoError(t, err)
	template, err := NewBackingTemplate(packet, terms, source)
	require.NoError(t, err)

	wrongKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	_, err = template.SignDescriptor(
		terms, PartyClient, keychain.KeyDescriptor{
			PubKey: wrongKey.PubKey(),
		},
	)
	require.ErrorContains(t, err, "does not match client channel key")
}

// testBackingTerms creates channel terms whose materialization keys are
// available to this test.
func testBackingTerms(t *testing.T) (Terms, VTXOBinding, *btcec.PrivateKey,
	*btcec.PrivateKey) {

	t.Helper()

	terms := testTerms(t, KindPromotion)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hubKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	terms.VTXO.ClientChannelKey = compressedKey(clientKey)
	terms.VTXO.HubChannelKey = compressedKey(hubKey)

	return terms, testBinding(terms), clientKey, hubKey
}

// compressedKey converts a private key to the durable channel key encoding.
func compressedKey(key *btcec.PrivateKey) [33]byte {
	var serialized [33]byte
	copy(serialized[:], key.PubKey().SerializeCompressed())

	return serialized
}

// signBacking signs one endpoint's tapscript branch with lnd's signer API.
func signBacking(t *testing.T, template *BackingTemplate, terms Terms,
	party Party, key *btcec.PrivateKey) input.Signature {

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
		template.Packet().UnsignedTx, desc,
	)
	require.NoError(t, err)

	return sig
}
