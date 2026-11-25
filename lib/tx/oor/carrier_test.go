package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestPlanAssetCarriersScenarios covers boarded-only, OOR-only, mixed,
// consolidation, exact-consumption, and rejection cases.
func TestPlanAssetCarriersScenarios(t *testing.T) {
	t.Parallel()

	const floor = btcutil.Amount(1_000)

	tests := []struct {
		name    string
		req     CarrierPlanRequest
		want    CarrierPlan
		wantErr string
	}{
		{
			name: "first OOR partial send from boarded leaf",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: 2_500,
					Origin: CarrierOriginRound,
				}},
				NewAssetLeafCount: 2,
				OutputFloor:       floor,
				LeaseValue:        10_000,
			},
			want: CarrierPlan{
				NewAssetLeafCarrier: floor,
				NewAssetLeafTotal:   2_000,
				SenderChange:        2_500,
				ReclaimedCarriers:   0,
				OperatorChange:      8_000,
			},
		},
		{
			name: "subsequent OOR full send reclaims float carrier",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: floor,
					Origin: CarrierOriginOOR,
				}},
				NewAssetLeafCount: 1,
				OutputFloor:       floor,
				LeaseValue:        5_000,
			},
			want: CarrierPlan{
				NewAssetLeafCarrier: floor,
				NewAssetLeafTotal:   1_000,
				SenderChange:        0,
				ReclaimedCarriers:   1_000,
				OperatorChange:      5_000,
			},
		},
		{
			name: "multi-input consolidation grows float",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{
					{
						Amount: floor,
						Origin: CarrierOriginOOR,
					},
					{
						Amount: floor,
						Origin: CarrierOriginOOR,
					},
					{
						Amount: floor,
						Origin: CarrierOriginOOR,
					},
				},
				NewAssetLeafCount: 1,
				OutputFloor:       floor,
				LeaseValue:        1_000,
			},
			want: CarrierPlan{
				NewAssetLeafCarrier: floor,
				NewAssetLeafTotal:   1_000,
				SenderChange:        0,
				ReclaimedCarriers:   3_000,
				OperatorChange:      3_000,
			},
		},
		{
			name: "mixed boarded and OOR inputs",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{
					{
						Amount: 1_500,
						Origin: CarrierOriginRound,
					},
					{
						Amount: floor,
						Origin: CarrierOriginOOR,
					},
					{
						Amount: 2_000,
						Origin: CarrierOriginRound,
					},
				},
				NewAssetLeafCount: 2,
				OutputFloor:       floor,
				LeaseValue:        6_000,
			},
			want: CarrierPlan{
				NewAssetLeafCarrier: floor,
				NewAssetLeafTotal:   2_000,
				SenderChange:        3_500,
				ReclaimedCarriers:   1_000,
				OperatorChange:      5_000,
			},
		},
		{
			name: "exact lease leaves zero operator change",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: 2_000,
					Origin: CarrierOriginRound,
				}},
				NewAssetLeafCount: 2,
				OutputFloor:       floor,
				LeaseValue:        2_000,
			},
			want: CarrierPlan{
				NewAssetLeafCarrier: floor,
				NewAssetLeafTotal:   2_000,
				SenderChange:        2_000,
				ReclaimedCarriers:   0,
				OperatorChange:      0,
			},
		},
		{
			name: "reject unspecified carrier origin",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: floor,
					Origin: CarrierOriginUnspecified,
				}},
				NewAssetLeafCount: 1,
				OutputFloor:       floor,
				LeaseValue:        5_000,
			},
			wantErr: "carrier origin 0 is invalid",
		},
		{
			name: "reject dust sender change",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: floor - 1,
					Origin: CarrierOriginRound,
				}},
				NewAssetLeafCount: 1,
				OutputFloor:       floor,
				LeaseValue:        5_000,
			},
			wantErr: "sender Bitcoin change 999 sat is below",
		},
		{
			name: "reject lease below required floors",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: floor,
					Origin: CarrierOriginOOR,
				}},
				NewAssetLeafCount: 2,
				OutputFloor:       floor,
				LeaseValue:        1_999,
			},
			wantErr: "below the 2000 sat needed",
		},
		{
			name: "reject dust operator change",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: 2_000,
					Origin: CarrierOriginRound,
				}},
				NewAssetLeafCount: 2,
				OutputFloor:       floor,
				LeaseValue:        2_500,
			},
			wantErr: "below output floor",
		},
		{
			name: "reject missing inputs",
			req: CarrierPlanRequest{
				NewAssetLeafCount: 1,
				OutputFloor:       floor,
				LeaseValue:        5_000,
			},
			wantErr: "at least one asset input carrier",
		},
		{
			name: "reject zero leaf count",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: floor,
					Origin: CarrierOriginRound,
				}},
				NewAssetLeafCount: 0,
				OutputFloor:       floor,
				LeaseValue:        5_000,
			},
			wantErr: "new asset leaf count must be positive",
		},
		{
			name: "reject zero floor",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: floor,
					Origin: CarrierOriginRound,
				}},
				NewAssetLeafCount: 1,
				OutputFloor:       0,
				LeaseValue:        5_000,
			},
			wantErr: "carrier output floor",
		},
		{
			name: "reject non-positive input carrier",
			req: CarrierPlanRequest{
				Inputs: []CarrierInput{{
					Amount: 0,
					Origin: CarrierOriginRound,
				}},
				NewAssetLeafCount: 1,
				OutputFloor:       floor,
				LeaseValue:        5_000,
			},
			wantErr: "carrier value 0 is invalid",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := PlanAssetCarriers(tc.req)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)

			var totalInputs btcutil.Amount
			for _, in := range tc.req.Inputs {
				totalInputs += in.Amount
			}
			require.Equal(
				t, totalInputs+tc.req.LeaseValue,
				got.NewAssetLeafTotal+got.SenderChange+
					got.OperatorChange,
			)
		})
	}
}

// TestPlanAssetCarriersConservationProperty asserts exact satoshi conservation
// and non-dust operator change across generated carrier inputs and leases.
func TestPlanAssetCarriersConservationProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		floor := btcutil.Amount(
			rapid.Int64Range(330, 50_000).Draw(rt, "floor"),
		)
		leafCount := rapid.IntRange(1, 2).Draw(rt, "leaf_count")
		numInputs := rapid.IntRange(1, 8).Draw(rt, "num_inputs")

		inputs := make([]CarrierInput, numInputs)
		var totalInputCarriers btcutil.Amount
		for i := range inputs {
			roundCreated := rapid.Bool().Draw(rt, "round_created")
			origin := CarrierOriginOOR
			amt := floor
			if roundCreated {
				origin = CarrierOriginRound
				amt = btcutil.Amount(
					rapid.Int64Range(
						int64(floor), 1_000_000,
					).Draw(rt, "round_carrier"),
				)
			}
			inputs[i] = CarrierInput{
				Amount: amt,
				Origin: origin,
			}
			totalInputCarriers += amt
		}

		required, err := RequiredCarrierLeaseSat(leafCount, floor)
		require.NoError(rt, err)

		extra := btcutil.Amount(
			rapid.Int64Range(0, 500_000).Draw(rt, "extra"),
		)
		leaseValue := required + extra

		plan, err := PlanAssetCarriers(CarrierPlanRequest{
			Inputs:            inputs,
			NewAssetLeafCount: leafCount,
			OutputFloor:       floor,
			LeaseValue:        leaseValue,
		})
		if err != nil {
			// The only valid rejection for well-formed inputs with
			// leaseValue >= required is sub-floor operator change.
			var reclaimed btcutil.Amount
			for _, in := range inputs {
				if in.Origin == CarrierOriginOOR {
					reclaimed += in.Amount
				}
			}
			opChange := extra + reclaimed
			require.Greater(rt, opChange, btcutil.Amount(0))
			require.Less(rt, opChange, floor)

			return
		}

		require.Equal(rt, floor, plan.NewAssetLeafCarrier)
		require.Equal(rt, required, plan.NewAssetLeafTotal)
		require.Equal(
			rt, totalInputCarriers+leaseValue,
			plan.NewAssetLeafTotal+plan.SenderChange+
				plan.OperatorChange,
		)
		if plan.SenderChange != 0 {
			require.GreaterOrEqual(rt, plan.SenderChange, floor)
		}
		if plan.OperatorChange != 0 {
			require.GreaterOrEqual(rt, plan.OperatorChange, floor)
		}
	})
}

// TestParseCarrierPubKey verifies 33-byte compressed key parsing and rejects
// empty, x-only, uncompressed, and invalid curve bytes.
func TestParseCarrierPubKey(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	compressed := priv.PubKey().SerializeCompressed()
	parsed, err := ParseCarrierPubKey(compressed)
	require.NoError(t, err)
	require.True(t, parsed.IsEqual(priv.PubKey()))

	_, err = ParseCarrierPubKey(nil)
	require.ErrorContains(t, err, "33 compressed bytes")

	_, err = ParseCarrierPubKey(compressed[1:])
	require.ErrorContains(t, err, "33 compressed bytes")

	_, err = ParseCarrierPubKey(priv.PubKey().SerializeUncompressed())
	require.ErrorContains(t, err, "33 compressed bytes")

	bad := make([]byte, btcec.PubKeyBytesLenCompressed)
	bad[0] = 0x05
	_, err = ParseCarrierPubKey(bad)
	require.ErrorContains(t, err, "parse carrier pubkey")
}

// TestValidateCarrierFloatKeys verifies nil, identical, and opposite-parity
// x-only twin keys are rejected while distinct keys pass.
func TestValidateCarrierFloatKeys(t *testing.T) {
	t.Parallel()

	floatPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	opPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	require.NoError(
		t,
		ValidateCarrierFloatKeys(
			floatPriv.PubKey(), opPriv.PubKey(),
		),
	)
	require.ErrorContains(
		t,
		ValidateCarrierFloatKeys(
			nil, opPriv.PubKey(),
		),
		"float owner key",
	)
	require.ErrorContains(
		t,
		ValidateCarrierFloatKeys(
			floatPriv.PubKey(), nil,
		),
		"operator key",
	)
	require.ErrorContains(
		t,
		ValidateCarrierFloatKeys(
			opPriv.PubKey(), opPriv.PubKey(),
		),
		"must differ",
	)

	// Flipping the compressed parity prefix produces the opposite-parity
	// point with the same x-only key, which must also be rejected because
	// Tapscript CHECKSIG uses x-only keys.
	twinBytes := opPriv.PubKey().SerializeCompressed()
	twinBytes[0] ^= 0x01
	twinKey, err := btcec.ParsePubKey(twinBytes)
	require.NoError(t, err)
	require.ErrorContains(
		t,
		ValidateCarrierFloatKeys(
			twinKey, opPriv.PubKey(),
		),
		"must differ",
	)
}
