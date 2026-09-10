package round

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func TestValidateAssetRequest(t *testing.T) {
	t.Parallel()

	assetRef := tapsdk.AssetRefFromAssetID(tapsdk.AssetID{1}).String()
	tests := []struct {
		name    string
		request *types.VTXORequest
		errText string
	}{
		{
			name:    "bitcoin request",
			request: &types.VTXORequest{},
		},
		{
			name: "asset request",
			request: &types.VTXORequest{
				AssetRef:    assetRef,
				AssetAmount: 1,
				FixedAmount: true,
			},
		},
		{
			name: "invalid reference",
			request: &types.VTXORequest{
				AssetRef:    "invalid",
				AssetAmount: 1,
				FixedAmount: true,
			},
			errText: "asset reference",
		},
		{
			name: "noncanonical reference",
			request: &types.VTXORequest{
				AssetRef:    strings.ToUpper(assetRef),
				AssetAmount: 1,
				FixedAmount: true,
			},
			errText: "canonical encoding",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateAssetRequest(test.request)
			if test.errText == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, test.errText)
		})
	}
}

func TestAssetVTXORequestProtoRoundTrip(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	signingPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy, err := arkscript.EncodeStandardVTXOTemplate(
		clientPriv.PubKey(), operatorPriv.PubKey(), 144,
	)
	require.NoError(t, err)

	assetRef := tapsdk.AssetRefFromAssetID(tapsdk.AssetID{2}).String()
	request := JoinRoundRequest{
		VTXORequests: []types.VTXORequest{{
			Amount:         330,
			FixedAmount:    true,
			AssetRef:       assetRef,
			AssetAmount:    42,
			PolicyTemplate: policy,
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingPriv.PubKey(),
			},
		}},
	}

	message := request.ToProto().UnwrapOrFail(t)
	pbRequest, ok := message.(*roundpb.JoinRoundRequest)
	require.True(t, ok)

	var decoded JoinRoundRequest
	require.NoError(t, decoded.FromProto(pbRequest))
	require.Len(t, decoded.VTXORequests, 1)
	require.Equal(t, request.VTXORequests[0], decoded.VTXORequests[0])
}
