package wallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestBuildSendVTXORequestsAcceptsChannelPolicy verifies the operator can fund
// one exact channel VTXO while keeping the ordinary send API amount based.
func TestBuildSendVTXORequestsAcceptsChannelPolicy(t *testing.T) {
	t.Parallel()

	params := testWalletChannelPolicyParams(t)
	policy, pkScript, err := arkscript.EncodeChannelVTXOArtifacts(params)
	require.NoError(t, err)
	req := &SendVTXOsRequest{
		Recipients: []SendRecipient{
			{
				Amount:         btcutil.Amount(100_000),
				PkScript:       pkScript,
				PolicyTemplate: policy,
				FixedAmount:    true,
			},
		},
	}

	requests, err := (&Ark{}).buildSendVTXORequests(
		t.Context(), req, 0,
	)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	require.Equal(t, policy, requests[0].PolicyTemplate)
	require.Equal(t, pkScript, requests[0].PkScript)
	require.True(t, requests[0].FixedAmount)
}

// TestBuildSendVTXORequestsRejectsChannelScriptMismatch prevents an operator
// request from labelling another output with registered channel semantics.
func TestBuildSendVTXORequestsRejectsChannelScriptMismatch(t *testing.T) {
	t.Parallel()

	params := testWalletChannelPolicyParams(t)
	policy, _, err := arkscript.EncodeChannelVTXOArtifacts(params)
	require.NoError(t, err)
	req := &SendVTXOsRequest{
		Recipients: []SendRecipient{
			{
				Amount: btcutil.Amount(100_000),
				PkScript: []byte{
					0x51,
				},
				PolicyTemplate: policy,
			},
		},
	}

	_, err = (&Ark{}).buildSendVTXORequests(t.Context(), req, 0)
	require.ErrorContains(t, err, "script mismatch")
}

// testWalletChannelPolicyParams creates independent channel policy keys.
func testWalletChannelPolicyParams(t *testing.T) arkscript.ChannelVTXOParams {
	t.Helper()

	keys := make([]*btcec.PrivateKey, 6)
	for i := range keys {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		keys[i] = key
	}

	return arkscript.ChannelVTXOParams{
		ClientArkKey:     keys[0].PubKey(),
		HubArkKey:        keys[1].PubKey(),
		ArkOperatorKey:   keys[2].PubKey(),
		ClientChannelKey: keys[3].PubKey(),
		HubChannelKey:    keys[4].PubKey(),
		FunderKey:        keys[5].PubKey(),
		ChannelDelay:     144,
		FunderDelay: 144 +
			arkscript.DefaultChannelReactionWindow,
		MinExitDelay: 144,
	}
}
