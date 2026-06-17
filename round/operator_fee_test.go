package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/stretchr/testify/require"
)

// newBoardingIntent is a local helper that constructs a
// round.BoardingIntent with the given on-chain amount and a
// harmless default for the remaining fields. The fee math only
// reads ChainInfo.Amount so the rest can stay at zero.
func newBoardingIntent(amount btcutil.Amount) BoardingIntent {
	return BoardingIntent{
		BoardingIntent: wallet.BoardingIntent{
			ChainInfo: wallet.BoardingChainInfo{
				Amount: amount,
			},
		},
	}
}

// TestComputeClientOperatorFee exercises the operator-fee calculator across
// its full set of accounting paths. Every case is data-only: a set of intents
// plus owned output VTXOs in, the expected fee out. The named cases pin,
// respectively, the canonical boarding admission fee, the refresh
// forfeit-for-new-VTXO difference, the additive boarding+forfeit input side,
// the cooperative leave output (counted like an owned VTXO), the
// directed-send-with-change contract (the recipient VTXO is filtered out by
// buildClientVTXOs before the calculator sees it, so the fee absorbs the
// recipient value plus the operator cut), the zero-contribution recipient-only
// slot, the negative clamp guarding against an outputs-exceed-inputs
// accounting bug, and the defensive nil-entry tolerance for a partially
// serialized round state.
func TestComputeClientOperatorFee(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		intents  Intents
		vtxos    []*ClientVTXO
		expected int64
	}{
		{
			name: "pure boarding",
			intents: Intents{
				Boarding: []BoardingIntent{
					newBoardingIntent(100_000),
				},
			},
			vtxos: []*ClientVTXO{
				{
					Amount: btcutil.Amount(99_500),
				},
			},
			expected: 500,
		},
		{
			name: "pure refresh",
			intents: Intents{
				Forfeits: []types.ForfeitRequest{
					{
						VTXOOutpoint: &wire.OutPoint{},
						Amount: btcutil.Amount(
							50_000,
						),
					},
				},
			},
			vtxos: []*ClientVTXO{
				{
					Amount: btcutil.Amount(49_800),
				},
			},
			expected: 200,
		},
		{
			name: "mixed boarding and refresh",
			intents: Intents{
				Boarding: []BoardingIntent{
					newBoardingIntent(80_000),
				},
				Forfeits: []types.ForfeitRequest{
					{
						Amount: btcutil.Amount(20_000),
					},
				},
			},
			vtxos: []*ClientVTXO{
				{
					Amount: btcutil.Amount(50_000),
				},
				{
					Amount: btcutil.Amount(49_200),
				},
			},
			expected: 800,
		},
		{
			name: "cooperative leave",
			intents: Intents{
				Forfeits: []types.ForfeitRequest{
					{
						Amount: btcutil.Amount(60_000),
					},
				},
				Leaves: []*types.LeaveRequest{
					{
						Output: &wire.TxOut{
							Value: 59_400,
						},
					},
				},
			},
			expected: 600,
		},
		{
			name: "directed send with change",
			intents: Intents{
				Forfeits: []types.ForfeitRequest{
					{
						Amount: btcutil.Amount(100_000),
					},
				},
			},
			vtxos: []*ClientVTXO{
				{
					Amount: btcutil.Amount(59_500),
				},
			},
			expected: 40_500,
		},
		{
			name:     "zero when no contribution",
			intents:  Intents{},
			expected: 0,
		},
		{
			name: "clamps negative",
			intents: Intents{
				Forfeits: []types.ForfeitRequest{
					{
						Amount: btcutil.Amount(100),
					},
				},
			},
			vtxos: []*ClientVTXO{
				{
					Amount: btcutil.Amount(200),
				},
			},
			expected: 0,
		},
		{
			name: "ignores nil entries",
			intents: Intents{
				Boarding: []BoardingIntent{
					newBoardingIntent(10_000),
				},
				Leaves: []*types.LeaveRequest{
					nil,
					{
						Output: nil,
					},
					{
						Output: &wire.TxOut{
							Value: 5_000,
						},
					},
				},
			},
			vtxos: []*ClientVTXO{
				nil,
				{
					Amount: btcutil.Amount(4_500),
				},
			},
			expected: 500,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, tc.expected,
				computeClientOperatorFee(tc.intents, tc.vtxos),
			)
		})
	}
}
