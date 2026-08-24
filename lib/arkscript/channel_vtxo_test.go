package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestChannelVTXOPolicyPaths verifies each spend path has the intended signer
// set and relative delay.
func TestChannelVTXOPolicyPaths(t *testing.T) {
	t.Parallel()

	params := testChannelVTXOParams(t)
	policy, err := NewChannelVTXOPolicy(params)
	require.NoError(t, err)

	cooperative, err := policy.CooperativeSpendPath()
	require.NoError(t, err)
	require.Equal(t, ^uint32(0), cooperative.RequiredSequence)
	cooperativeNode, ok := policy.CooperativeClosure.(*Multisig)
	require.True(t, ok)
	require.Len(t, cooperativeNode.Keys, 3)

	channel, err := policy.ChannelSpendPath()
	require.NoError(t, err)
	require.Equal(
		t, blockchain.LockTimeToSequence(
			false, params.ChannelDelay,
		),
		channel.RequiredSequence,
	)
	channelCSV, ok := policy.ChannelClosure.(*CSV)
	require.True(t, ok)
	channelNode, ok := channelCSV.Inner.(*Multisig)
	require.True(t, ok)
	require.Len(t, channelNode.Keys, 2)

	funder, err := policy.FunderSpendPath()
	require.NoError(t, err)
	require.Equal(
		t, blockchain.LockTimeToSequence(
			false, params.FunderDelay,
		),
		funder.RequiredSequence,
	)
	funderCSV, ok := policy.FunderClosure.(*CSV)
	require.True(t, ok)
	funderNode, ok := funderCSV.Inner.(*Multisig)
	require.True(t, ok)
	require.Len(t, funderNode.Keys, 1)
}

// TestChannelVTXOPolicyReactionWindow rejects a refund path that can overtake
// the channel endpoints before they have time to materialize.
func TestChannelVTXOPolicyReactionWindow(t *testing.T) {
	t.Parallel()

	params := testChannelVTXOParams(t)
	params.FunderDelay = params.ChannelDelay +
		DefaultChannelReactionWindow - 1

	_, err := NewChannelVTXOPolicy(params)
	require.ErrorContains(t, err, "preserve the reaction window")
}

// TestValidateChannelVTXOTemplate detects any change to the persisted channel
// contract while accepting a canonical encode/decode round trip.
func TestValidateChannelVTXOTemplate(t *testing.T) {
	t.Parallel()

	params := testChannelVTXOParams(t)
	raw, pkScript, err := EncodeChannelVTXOArtifacts(params)
	require.NoError(t, err)
	require.NotEmpty(t, pkScript)

	template, err := DecodePolicyTemplate(raw)
	require.NoError(t, err)
	require.NoError(t, ValidateChannelVTXOTemplate(template, params))

	otherKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	params.HubChannelKey = otherKey.PubKey()
	require.Error(t, ValidateChannelVTXOTemplate(template, params))
}

// testChannelVTXOParams returns independent keys and valid channel delays.
func testChannelVTXOParams(t *testing.T) ChannelVTXOParams {
	t.Helper()

	keys := make([]*btcec.PrivateKey, 6)
	for i := range keys {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		keys[i] = key
	}

	return ChannelVTXOParams{
		ClientArkKey:     keys[0].PubKey(),
		HubArkKey:        keys[1].PubKey(),
		ArkOperatorKey:   keys[2].PubKey(),
		ClientChannelKey: keys[3].PubKey(),
		HubChannelKey:    keys[4].PubKey(),
		FunderKey:        keys[5].PubKey(),
		ChannelDelay:     144,
		FunderDelay:      144 + DefaultChannelReactionWindow,
		MinExitDelay:     144,
	}
}
