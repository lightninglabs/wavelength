package round

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// TestDurableEnvironment keeps admission policy fixed across a later restart.
func TestDurableEnvironment(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	deadline := time.Date(2026, 9, 10, 12, 0, 0, 123, time.UTC)
	original := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{
			PubKey:            key.PubKey(),
			BoardingExitDelay: 1, VTXOExitDelay: 2,
			DustLimit: 3, MinVTXOAmount: 4, MinBoardingAmount: 5,
			MaxVTXOAmount: 6, MaxUserBalance: 7, FeeRate: 8,
			MinOperatorFee: 9, FreeRefreshWindowBlocks: 10,
			MinConfirmations: 11, VTXOConfirmations: 12,
			MaxOORLineageVBytes: 13,
		},
		ParticipationDeadline: deadline, RoundKey: "temp:original",
		StartHeight: 101, MaxOperatorFee: 102,
		AutoRefreshFeeFloor: 103, AutoRefreshFeeRatePPM: 104,
		ForfeitCollectionTimeout: 5 * time.Second,
		RegistrationTimeout:      -time.Second,
		StatusReconcileTimeout:   7 * time.Second,
		DisableJoinRequestAuth:   true,
	}
	raw, err := encodeDurableEnvironment(original)
	require.NoError(t, err)
	now := deadline.Add(time.Hour)
	query := func(context.Context) (uint32, error) { return 999, nil }
	base := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{
			MaxVTXOAmount: 999,
		},
		Now: func() time.Time { return now }, QueryBestHeight: query,
		MaxOperatorFee: 999, StartHeight: 999,
	}
	restored, err := decodeDurableEnvironment(raw, base)
	require.NoError(t, err)
	require.Equal(t, original.OperatorTerms, restored.OperatorTerms)
	require.NotSame(t, original.OperatorTerms, restored.OperatorTerms)
	require.True(t, restored.ParticipationDeadline.Equal(deadline))
	require.True(t, restored.now().After(restored.ParticipationDeadline))
	height, err := restored.QueryBestHeight(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 999, height)
	require.EqualValues(t, 101, restored.StartHeight)
	require.EqualValues(t, 102, restored.MaxOperatorFee)
	require.Equal(t, -time.Second, restored.RegistrationTimeout)
	require.EqualValues(t, 999, base.MaxOperatorFee)
	again, err := encodeDurableEnvironment(restored)
	require.NoError(t, err)
	require.Equal(t, raw, again)
}

// TestDurableEnvironmentZeroDeadline preserves an unadmitted round's deadline.
func TestDurableEnvironmentZeroDeadline(t *testing.T) {
	raw, err := encodeDurableEnvironment(&ClientEnvironment{})
	require.NoError(t, err)
	restored, err := decodeDurableEnvironment(raw, &ClientEnvironment{})
	require.NoError(t, err)
	require.True(t, restored.ParticipationDeadline.IsZero())
	require.Nil(t, restored.OperatorTerms)
	_, err = decodeDurableEnvironment(
		raw[:len(raw)-1], &ClientEnvironment{},
	)
	require.Error(t, err)
}
