package types

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestVTXOClaimAuthDigestBindsContext verifies the participant signature binds
// every operator-derived and client-selected claim field.
func TestVTXOClaimAuthDigestBindsContext(t *testing.T) {
	t.Parallel()

	participant, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	replacement, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	server, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	claim := &VTXOClaimInput{
		SourceOutpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				1,
				2,
				3,
			},
			Index: 7,
		},
		ParticipantPubKey: participant.PubKey(),
		ReplacementSigningKey: keychain.KeyDescriptor{
			PubKey: replacement.PubKey(),
		},
		ValidFrom:  800_000,
		ValidUntil: 800_144,
	}
	claim.Nonce[0] = 42

	roundID := []byte("round-id")
	amount := btcutil.Amount(50_000)
	policy := []byte("policy-template")
	pkScript := []byte{0x51, 0x20, 1, 2, 3}
	digest, err := VTXOClaimAuthDigest(
		claim, server.PubKey(), roundID, amount, policy, pkScript,
	)
	require.NoError(t, err)
	sig, err := schnorr.Sign(participant, digest[:])
	require.NoError(t, err)
	claim.Signature = sig.Serialize()

	require.NoError(
		t,
		VerifyVTXOClaimAuth(
			claim, server.PubKey(), roundID, amount, policy,
			pkScript,
		),
	)

	otherServer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	tests := []struct {
		name     string
		server   *btcec.PublicKey
		roundID  []byte
		amount   btcutil.Amount
		policy   []byte
		pkScript []byte
	}{
		{
			name: "server", server: otherServer.PubKey(),
			roundID: roundID, amount: amount, policy: policy,
			pkScript: pkScript,
		},
		{
			name: "round", server: server.PubKey(),
			roundID: []byte("other-round"), amount: amount,
			policy: policy, pkScript: pkScript,
		},
		{
			name:     "amount",
			server:   server.PubKey(),
			roundID:  roundID,
			amount:   amount + 1,
			policy:   policy,
			pkScript: pkScript,
		},
		{
			name:     "policy",
			server:   server.PubKey(),
			roundID:  roundID,
			amount:   amount,
			policy:   []byte("other-policy"),
			pkScript: pkScript,
		},
		{
			name:     "pkScript",
			server:   server.PubKey(),
			roundID:  roundID,
			amount:   amount,
			policy:   policy,
			pkScript: append(bytes.Clone(pkScript), 4),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := VerifyVTXOClaimAuth(
				claim, test.server, test.roundID, test.amount,
				test.policy, test.pkScript,
			)
			require.ErrorContains(t, err, "invalid claim signature")
		})
	}
}

// TestVTXOClaimAuthDigestExcludesSignature verifies the signature field is an
// envelope around, rather than an input to, the authorization digest.
func TestVTXOClaimAuthDigestExcludesSignature(t *testing.T) {
	t.Parallel()

	participant, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	replacement, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	server, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	claim := &VTXOClaimInput{
		ParticipantPubKey: participant.PubKey(),
		ReplacementSigningKey: keychain.KeyDescriptor{
			PubKey: replacement.PubKey(),
		},
		ValidFrom:  1,
		ValidUntil: 2,
	}
	claim.Nonce[0] = 1
	roundID := []byte("round-id")

	first, err := VTXOClaimAuthDigest(
		claim, server.PubKey(), roundID, 1, []byte{1}, []byte{2},
	)
	require.NoError(t, err)
	message, err := VTXOClaimAuthMessage(
		claim, server.PubKey(), roundID, 1, []byte{1}, []byte{2},
	)
	require.NoError(t, err)
	require.Equal(
		t, first[:], chainhash.TaggedHash(
			VTXOClaimAuthTag(), message,
		)[:],
	)
	claim.Signature = bytes.Repeat([]byte{3}, VTXOClaimSignatureSize)
	second, err := VTXOClaimAuthDigest(
		claim, server.PubKey(), roundID, 1, []byte{1}, []byte{2},
	)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

// TestVTXOClaimRequiresConcreteRound verifies an authorization cannot be
// created before the operator supplies the exact open round UUID.
func TestVTXOClaimRequiresConcreteRound(t *testing.T) {
	t.Parallel()

	participant, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	replacement, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	server, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	claim := &VTXOClaimInput{
		ParticipantPubKey: participant.PubKey(),
		ReplacementSigningKey: keychain.KeyDescriptor{
			PubKey: replacement.PubKey(),
		},
		ValidFrom:  10,
		ValidUntil: 20,
	}
	claim.Nonce[0] = 1

	_, err = VTXOClaimAuthDigest(
		claim, server.PubKey(), nil, 1, []byte{1}, []byte{2},
	)
	require.ErrorContains(t, err, "claim round ID must be provided")

	assigned, err := VTXOClaimAuthDigest(
		claim, server.PubKey(), []byte("assigned-round"), 1, []byte{1},
		[]byte{2},
	)
	require.NoError(t, err)
	require.NotEqual(t, [32]byte{}, assigned)
}
