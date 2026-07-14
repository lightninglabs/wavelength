package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestNormalizeCheckpointOwnerLeavesBindsSessionKey proves that for a standard
// collaborative input the checkpoint OUTPUT owner leaf is rebound to the
// session operator key rather than the spent input VTXO's (possibly
// pre-rotation) operator key. This is the client half of the
// operator-key-rotation OOR fix: the server rebuilds + co-signs the checkpoint
// output under the session key, so the client must commit the output owner leaf
// to that same key.
func TestNormalizeCheckpointOwnerLeavesBindsSessionKey(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// X: the operator key the input VTXO was created under (historical).
	inputOperatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Y: the operator's current key, carried by the session policy.
	sessionOperatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputs := []TransferInput{{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					1,
				},
			},
			Amount: btcutil.Amount(50_000),
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
			},
			OperatorKey: inputOperatorKey.PubKey(),
		},
	}}

	policy := arkscript.CheckpointPolicy{
		OperatorKey: sessionOperatorKey.PubKey(),
		CSVDelay:    10,
	}

	require.NoError(t, normalizeCheckpointOwnerLeaves(policy, inputs))

	leaf, err := arkscript.DecodeLeafTemplate(inputs[0].OwnerLeafPolicy)
	require.NoError(t, err)

	// The owner leaf commits to the session key + the client key, and NOT
	// to the spent input's historical operator key.
	require.True(
		t,
		arkscript.ContainsKey(
			leaf.Node, sessionOperatorKey.PubKey(),
		),
		"owner leaf must commit to the session operator key",
	)
	require.True(t, arkscript.ContainsKey(leaf.Node, clientKey.PubKey()))
	require.False(
		t,
		arkscript.ContainsKey(
			leaf.Node, inputOperatorKey.PubKey(),
		),
		"owner leaf must not commit to the input's historical key",
	)

	// Script and policy are a consistent pair derived from the session key.
	wantLeaf, _, err := defaultOwnerLeaf(
		clientKey.PubKey(), sessionOperatorKey.PubKey(),
	)
	require.NoError(t, err)
	require.Equal(t, wantLeaf, inputs[0].OwnerLeafScript)

	// The spend side is untouched: the input VTXO's operator key, from
	// which the historical collaborative spend path is derived, still
	// equals X. Only the checkpoint OUTPUT owner leaf was rebound.
	require.Equal(
		t, inputOperatorKey.PubKey(), inputs[0].VTXO.OperatorKey,
		"normalization must not touch the input's spend-side key",
	)
}

// TestNormalizeCheckpointOwnerLeavesSkipsIncompleteInput asserts that an input
// without a VTXO or without a client pubkey is skipped (not an error and not a
// keyless leaf), since its owner leaf cannot be derived.
func TestNormalizeCheckpointOwnerLeavesSkipsIncompleteInput(t *testing.T) {
	t.Parallel()

	sessionOperatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	policy := arkscript.CheckpointPolicy{
		OperatorKey: sessionOperatorKey.PubKey(),
		CSVDelay:    10,
	}

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputs := []TransferInput{
		// No VTXO at all.
		{},
		// VTXO present but no client pubkey (operator key set just to
		// show it is the missing client key, not a missing VTXO, that
		// triggers the skip).
		{VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					3,
				},
			},
			Amount:      btcutil.Amount(50_000),
			OperatorKey: clientKey.PubKey(),
		}},
	}

	require.NoError(t, normalizeCheckpointOwnerLeaves(policy, inputs))

	for i := range inputs {
		require.Empty(t, inputs[i].OwnerLeafScript)
		require.Empty(t, inputs[i].OwnerLeafPolicy)
	}
}

// TestNormalizeCheckpointOwnerLeavesSkipsCustomSpend asserts that a custom
// spend input (e.g. vHTLC), which carries its own explicit owner leaf, is left
// untouched by the session-key normalization.
func TestNormalizeCheckpointOwnerLeavesSkipsCustomSpend(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	inputOperatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	sessionOperatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	customLeaf := []byte{0xde, 0xad, 0xbe, 0xef}
	customPolicy := []byte{0xca, 0xfe}

	inputs := []TransferInput{{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					2,
				},
			},
			Amount: btcutil.Amount(50_000),
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
			},
			OperatorKey: inputOperatorKey.PubKey(),
		},
		OwnerLeafScript: customLeaf,
		OwnerLeafPolicy: customPolicy,
		CustomSpend:     &arkscript.SpendPath{},
	}}

	policy := arkscript.CheckpointPolicy{
		OperatorKey: sessionOperatorKey.PubKey(),
		CSVDelay:    10,
	}

	require.NoError(t, normalizeCheckpointOwnerLeaves(policy, inputs))

	require.Equal(t, customLeaf, inputs[0].OwnerLeafScript)
	require.Equal(t, customPolicy, inputs[0].OwnerLeafPolicy)
}

// TestNormalizeCheckpointOwnerLeavesRequiresPolicyKey asserts a nil session
// operator key is rejected rather than silently producing a keyless leaf.
func TestNormalizeCheckpointOwnerLeavesRequiresPolicyKey(t *testing.T) {
	t.Parallel()

	require.Error(
		t,
		normalizeCheckpointOwnerLeaves(
			arkscript.CheckpointPolicy{
				CSVDelay: 10,
			},
			nil,
		),
	)
}
