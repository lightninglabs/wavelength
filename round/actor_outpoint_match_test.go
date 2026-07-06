package round

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// minBoardingIntent returns a minimally-populated wallet.BoardingIntent
// carrying just the outpoint — the only field boardingMatches reads.
func minBoardingIntent(o wire.OutPoint) wallet.BoardingIntent {
	return wallet.BoardingIntent{Outpoint: o}
}

// op builds a deterministic wire.OutPoint for the table tests.
func op(seed byte, idx uint32) wire.OutPoint {
	return wire.OutPoint{
		Hash: chainhash.Hash{
			seed,
		},
		Index: idx,
	}
}

// TestBoardingMatchesExactSet verifies boardingMatches rejects
// supersets, subsets, disjoint sets, and empty-vs-nonempty; and
// accepts exact matches (including the empty set, which is the
// VTXO-only-round shape the pre-fix code over-matched on).
func TestBoardingMatchesExactSet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		intents  []BoardingIntent
		set      []wire.OutPoint
		expected bool
	}{
		{
			name:     "empty_matches_empty",
			intents:  nil,
			set:      nil,
			expected: true,
		},
		{
			name:    "empty_intent_vs_non_empty_set",
			intents: nil,
			set: []wire.OutPoint{
				op(0x01, 0),
			},
			expected: false,
		},
		{
			name: "non_empty_intent_vs_empty_set",
			intents: []BoardingIntent{
				{
					Request: types.BoardingRequest{},
				},
			},
			set:      nil,
			expected: false,
		},
		{
			name: "exact_single_match",
			intents: []BoardingIntent{{
				BoardingIntent: minBoardingIntent(op(0x01, 0)),
			}},
			set: []wire.OutPoint{
				op(0x01, 0),
			},
			expected: true,
		},
		{
			name: "disjoint_rejects",
			intents: []BoardingIntent{{
				BoardingIntent: minBoardingIntent(op(0x01, 0)),
			}},
			set: []wire.OutPoint{
				op(0x02, 0),
			},
			expected: false,
		},
		{
			name: "subset_rejects",
			intents: []BoardingIntent{
				{BoardingIntent: minBoardingIntent(
					op(0x01, 0),
				)},
				{BoardingIntent: minBoardingIntent(
					op(0x02, 0),
				)},
			},
			set: []wire.OutPoint{
				op(0x01, 0),
			},
			expected: false,
		},
		{
			name: "superset_rejects",
			intents: []BoardingIntent{
				{BoardingIntent: minBoardingIntent(
					op(0x01, 0),
				)},
			},
			set: []wire.OutPoint{
				op(0x01, 0),
				op(0x02, 0),
			},
			expected: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := boardingMatches(
				tc.intents, fn.NewSet(tc.set...),
			)
			require.Equal(t, tc.expected, got)
		})
	}
}

// TestForfeitsMatchExactSet verifies the same strict-set-equality
// contract for forfeit VTXO outpoints. This is the fix for the
// pre-#270 bug where findRoundByOutpoints ignored the forfeit
// set and let concurrent VTXO-only rounds re-key ambiguously.
func TestForfeitsMatchExactSet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		forfeits []types.ForfeitRequest
		set      []wire.OutPoint
		expected bool
	}{
		{
			name:     "empty_matches_empty",
			forfeits: nil,
			set:      nil,
			expected: true,
		},
		{
			name:     "empty_forfeits_vs_non_empty_set",
			forfeits: nil,
			set: []wire.OutPoint{
				op(0x10, 1),
			},
			expected: false,
		},
		{
			name: "exact_single_match",
			forfeits: []types.ForfeitRequest{
				{
					VTXOOutpoint: opPtr(op(0x10, 1)),
				},
			},
			set: []wire.OutPoint{
				op(0x10, 1),
			},
			expected: true,
		},
		{
			name: "nil_outpoint_rejects",
			forfeits: []types.ForfeitRequest{
				{
					VTXOOutpoint: nil,
				},
			},
			set: []wire.OutPoint{
				op(0x10, 1),
			},
			expected: false,
		},
		{
			name: "two_round_collision_case_a_only",
			forfeits: []types.ForfeitRequest{
				{
					VTXOOutpoint: opPtr(op(0xAA, 0)),
				},
			},
			set: []wire.OutPoint{
				op(0xBB, 0),
			},
			expected: false,
		},
		{
			name: "superset_rejects",
			forfeits: []types.ForfeitRequest{
				{
					VTXOOutpoint: opPtr(op(0xAA, 0)),
				},
			},
			set: []wire.OutPoint{
				op(0xAA, 0),
				op(0xBB, 0),
			},
			expected: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := forfeitsMatch(
				tc.forfeits, fn.NewSet(tc.set...),
			)
			require.Equal(t, tc.expected, got)
		})
	}
}

// opPtr returns a heap-allocated copy of the given outpoint. The
// ForfeitRequest.VTXOOutpoint is a pointer so matching logic can
// detect a nil-sentinel separate from the empty set.
func opPtr(o wire.OutPoint) *wire.OutPoint { return &o }
