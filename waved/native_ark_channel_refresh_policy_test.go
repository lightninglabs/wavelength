package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// TestRefreshPayoutPolicy checks operator limits before either endpoint's
// authorization can make an otherwise unspendable payout irreversible.
func TestRefreshPayoutPolicy(t *testing.T) {
	t.Parallel()
	delivery := &arkChannelRefreshDelivery{minimum: 330, maximum: 100_000}
	for _, test := range []struct {
		name        string
		client, hub btcutil.Amount
		valid       bool
	}{
		{
			name:   "both",
			client: 330,
			hub:    100_000,
			valid:  true,
		},
		{
			name:   "one output",
			client: 100_000,
			valid:  true,
		},
		{
			name:   "client dust",
			client: 329,
			hub:    1_000,
		},
		{
			name:   "hub dust",
			client: 1_000,
			hub:    1,
		},
		{
			name:   "over cap",
			client: 100_001,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := delivery.ValidateCooperativeCloseOutputs(
				arkchannel.CooperativeCloseProposal{
					ClientOutput: test.client,
					HubOutput:    test.hub,
				},
			)
			if test.valid {
				require.NoError(t, err)

				return
			}
			require.ErrorContains(t, err, "outside limits")
		})
	}
}

// TestRefreshDeliveryRegistersRotatedPolicy checks new quotes against the
// registered policy and signed replay against the immutable owner alone.
func TestRefreshDeliveryRegistersRotatedPolicy(t *testing.T) {
	t.Parallel()
	raw, coordinator := testArkChannelResumeStore(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	terms := testPrePONRTerms(t)
	_, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	source, err := btcec.ParsePubKey(terms.VTXO.ArkOperatorKey[:])
	require.NoError(t, err)
	current := &types.OperatorTerms{
		PubKey: source, VTXOExitDelay: terms.VTXO.MinExitDelay,
	}
	owner := testKeyDescriptor(t, 7).PubKey
	var registrationErr error
	// Use the same static opening terms and live registration hook that
	// SwapDK supplies to each native hub endpoint.
	controller := &NativeArkChannelController{
		coordinator: coordinator,
		cfg: ArkChannelControllerConfig{
			OORDestination: owner,
			OperatorTerms: &types.OperatorTerms{
				PubKey:        source,
				VTXOExitDelay: current.VTXOExitDelay,
			},
			RegisterRefreshPolicy: func(context.Context) (
				*types.OperatorTerms, error) {

				return current, registrationErr
			},
		},
	}
	d, err := controller.refreshDelivery()
	require.NoError(t, err)
	original, err := d.CooperativeCloseDelivery(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, owner.SerializeCompressed(), original)
	current.PubKey = testKeyDescriptor(t, 8).PubKey
	quoted, err := d.CooperativeCloseDelivery(t.Context(), terms.ID)
	require.NoError(t, err)
	want, err := arkscript.EncodeStandardVTXOTemplate(
		owner, current.PubKey, current.VTXOExitDelay,
	)
	require.NoError(t, err)
	require.Equal(t, want, quoted)
	registrationErr = errors.New("registration unavailable")
	_, err = d.CooperativeCloseDelivery(t.Context(), terms.ID)
	require.ErrorIs(t, err, registrationErr)
	for _, encoded := range [][]byte{original, quoted} {
		require.NoError(
			t,
			d.ValidateCooperativeCloseDelivery(
				t.Context(), terms.ID, encoded,
			),
		)
	}
}

// TestRefreshDeliveryUsesClientPolicySource covers clients without the static
// hub-only snapshot, including startup before operator negotiation completes.
func TestRefreshDeliveryUsesClientPolicySource(t *testing.T) {
	t.Parallel()
	var policy *types.OperatorTerms
	controller := &NativeArkChannelController{
		cfg: ArkChannelControllerConfig{
			OperatorTermsSource: func() *types.OperatorTerms {
				return policy
			},
		},
	}
	delivery, err := controller.refreshDelivery()
	require.NoError(t, err)
	proposal := arkchannel.CooperativeCloseProposal{ClientOutput: 100_000}
	require.ErrorContains(
		t, delivery.ValidateCooperativeCloseOutputs(
			proposal,
		),
		"policy is unavailable",
	)
	policy = &types.OperatorTerms{MaxVTXOAmount: 100_000}
	require.NoError(t, delivery.ValidateCooperativeCloseOutputs(proposal))
	policy = &types.OperatorTerms{MaxVTXOAmount: 99_999}
	require.ErrorContains(
		t, delivery.ValidateCooperativeCloseOutputs(
			proposal,
		),
		"outside limits",
	)
}
