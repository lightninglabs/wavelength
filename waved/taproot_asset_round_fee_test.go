package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// feeFundingOutpoint builds a deterministic outpoint whose txid is filled
// with the given byte, so ordering between two candidates is predictable.
func feeFundingOutpoint(fill byte, index uint32) wire.OutPoint {
	var hash chainhash.Hash
	for i := range hash {
		hash[i] = fill
	}

	return wire.OutPoint{Hash: hash, Index: index}
}

// flatFeeQuoter prices every candidate the same, which isolates the
// selection rule from the quote.
func flatFeeQuoter(fee btcutil.Amount) feeFundingQuoter {
	return func(context.Context, feeFundingCandidate) (btcutil.Amount,
		error) {

		return fee, nil
	}
}

// TestSelectFeeFundingVTXOSmallestSufficient asserts the rule is
// least-waste: among the candidates that clear the requirement the
// smallest is taken, so a large VTXO is never churned when a small one
// suffices.
func TestSelectFeeFundingVTXOSmallestSufficient(t *testing.T) {
	t.Parallel()

	small := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x01, 0),
		amount:   20_000,
	}
	large := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x02, 0),
		amount:   500_000,
	}

	// The larger candidate is listed first so a "first sufficient"
	// implementation over the input order would pick it.
	selected, err := selectFeeFundingVTXO(
		t.Context(), []feeFundingCandidate{large, small}, 1_000,
		flatFeeQuoter(500),
	)
	require.NoError(t, err)
	require.Equal(t, small, selected)
}

// TestSelectFeeFundingVTXORespectsFloor asserts a candidate that could
// cover the fee alone but would leave the change output under the
// operator's minimum is skipped in favour of one that clears both. The
// operator rejects the whole intent when the residual falls below the
// floor, so covering only the fee is not enough.
func TestSelectFeeFundingVTXORespectsFloor(t *testing.T) {
	t.Parallel()

	const (
		fee   btcutil.Amount = 800
		floor btcutil.Amount = 1_000
	)

	// 1_500 covers the fee but leaves 700, which is below the floor.
	tooTight := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x01, 0),
		amount:   1_500,
	}
	sufficient := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x02, 0),
		amount:   1_900,
	}

	selected, err := selectFeeFundingVTXO(
		t.Context(), []feeFundingCandidate{tooTight, sufficient}, floor,
		flatFeeQuoter(fee),
	)
	require.NoError(t, err)
	require.Equal(t, sufficient, selected)

	// Exactly meeting fee + floor is enough: the residual lands on the
	// floor rather than below it.
	exact := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x03, 0),
		amount:   fee + floor,
	}
	selected, err = selectFeeFundingVTXO(
		t.Context(), []feeFundingCandidate{sufficient, exact}, floor,
		flatFeeQuoter(fee),
	)
	require.NoError(t, err)
	require.Equal(t, exact, selected)
}

// TestSelectFeeFundingVTXOTieBreak asserts equal-value candidates resolve
// deterministically on outpoint, so the choice does not drift with the
// order the live-VTXO listing happens to return.
func TestSelectFeeFundingVTXOTieBreak(t *testing.T) {
	t.Parallel()

	lower := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x01, 7),
		amount:   50_000,
	}
	higherHash := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x02, 0),
		amount:   50_000,
	}
	higherIndex := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x01, 9),
		amount:   50_000,
	}

	// Every permutation must resolve to the same winner.
	orders := [][]feeFundingCandidate{
		{
			lower,
			higherHash,
			higherIndex,
		},
		{
			higherIndex,
			lower,
			higherHash,
		},
		{
			higherHash,
			higherIndex,
			lower,
		},
	}
	for _, candidates := range orders {
		selected, err := selectFeeFundingVTXO(
			t.Context(), candidates, 1_000, flatFeeQuoter(0),
		)
		require.NoError(t, err)
		require.Equal(t, lower, selected)
	}
}

// TestSelectFeeFundingVTXONoCandidate asserts both an empty and an
// all-too-small candidate set surface the actionable sentinel rather than
// a zero value the caller could mistake for a selection.
func TestSelectFeeFundingVTXONoCandidate(t *testing.T) {
	t.Parallel()

	_, err := selectFeeFundingVTXO(
		t.Context(), nil, 1_000, flatFeeQuoter(0),
	)
	require.ErrorIs(t, err, ErrNoFeeFundingVTXO)

	// Both clear the floor on their own but neither survives the fee.
	tooSmall := []feeFundingCandidate{{
		outpoint: feeFundingOutpoint(0x01, 0),
		amount:   1_400,
	}, {
		outpoint: feeFundingOutpoint(0x02, 0),
		amount:   1_200,
	}}
	_, err = selectFeeFundingVTXO(
		t.Context(), tooSmall, 1_000, flatFeeQuoter(500),
	)
	require.ErrorIs(t, err, ErrNoFeeFundingVTXO)
}

// TestSelectFeeFundingVTXOZeroFee asserts the zero-quote case: the whole
// input returns as change, so any candidate at or above the floor
// qualifies and the smallest still wins.
func TestSelectFeeFundingVTXOZeroFee(t *testing.T) {
	t.Parallel()

	atFloor := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x01, 0),
		amount:   1_000,
	}
	larger := feeFundingCandidate{
		outpoint: feeFundingOutpoint(0x02, 0),
		amount:   9_000,
	}

	selected, err := selectFeeFundingVTXO(
		t.Context(), []feeFundingCandidate{larger, atFloor}, 1_000,
		flatFeeQuoter(0),
	)
	require.NoError(t, err)
	require.Equal(t, atFloor, selected)
}

// TestSelectFeeFundingVTXOQuoteError asserts a quote failure surfaces
// rather than being read as "this candidate does not qualify" — the
// production quoter degrades to a flat fallback, so an error reaching
// here means something the caller must not paper over.
func TestSelectFeeFundingVTXOQuoteError(t *testing.T) {
	t.Parallel()

	quoteErr := errors.New("quote unavailable")
	candidates := []feeFundingCandidate{{
		outpoint: feeFundingOutpoint(0x01, 0),
		amount:   50_000,
	}}

	_, err := selectFeeFundingVTXO(
		t.Context(), candidates, 1_000,
		func(context.Context, feeFundingCandidate) (btcutil.Amount,
			error) {

			return 0, quoteErr
		},
	)
	require.ErrorIs(t, err, quoteErr)
}

// TestHasFeeFundingSlot asserts the skip-when-a-slot-exists predicate: an
// asset-only intent has no slot of its own because asset carriers are
// fixed, while an intent that already boards or refreshes Bitcoin does and
// must not be given a second fee payer.
func TestHasFeeFundingSlot(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ownerKey := keychain.KeyDescriptor{
		PubKey: privKey.PubKey(),
	}

	assetCarrier := types.VTXORequest{
		Amount:      30_000,
		AssetRef:    "asset-ref",
		AssetAmount: 900,
		FixedAmount: true,
		OwnerKey:    ownerKey,
	}
	bitcoinChange := types.VTXORequest{
		Amount:   100_000,
		OwnerKey: ownerKey,
	}
	foreignRecipient := types.VTXORequest{
		Amount: 100_000,
	}

	require.False(t, hasFeeFundingSlot(nil))

	// An asset boarding on its own: one fixed carrier, no slot.
	require.False(
		t,
		hasFeeFundingSlot(
			[]types.VTXORequest{
				assetCarrier,
			},
		),
	)

	// A directed send's recipient output is non-fixed but not the
	// client's, so it cannot absorb the client's fee.
	require.False(
		t,
		hasFeeFundingSlot(
			[]types.VTXORequest{
				assetCarrier, foreignRecipient,
			},
		),
	)

	// Boarding Bitcoin in the same round, or refreshing one into it,
	// already supplies the slot.
	require.True(
		t,
		hasFeeFundingSlot(
			[]types.VTXORequest{
				assetCarrier, bitcoinChange,
			},
		),
	)
	require.True(
		t,
		hasFeeFundingSlot(
			[]types.VTXORequest{
				bitcoinChange,
			},
		),
	)
}
