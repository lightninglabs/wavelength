package wallet

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// takeListUnspent removes the fixture's default ListUnspent expectation so a
// test can pin the exact confirmation depth the wallet queries at. It returns
// the UTXO that expectation served.
func takeListUnspent(t *testing.T, fix *eagerBoardFixture) *Utxo {
	t.Helper()

	var utxo *Utxo

	kept := fix.backend.ExpectedCalls[:0]
	for _, call := range fix.backend.ExpectedCalls {
		if call.Method != "ListUnspent" {
			kept = append(kept, call)

			continue
		}

		utxos, ok := call.ReturnArguments.Get(0).([]*Utxo)
		require.True(t, ok)
		utxo = utxos[0]
	}
	fix.backend.ExpectedCalls = kept

	return utxo
}

// withTerms makes the wallet read the given operator terms.
func withTerms(fix *eagerBoardFixture, terms *types.OperatorTerms, err error) {
	fix.wallet.fetchOperatorTerms = func(context.Context) (
		*types.OperatorTerms, error) {

		return terms, err
	}
}

// tick drives one tip-tick pass at the given height.
func tick(t *testing.T, fix *eagerBoardFixture, height int32) {
	t.Helper()

	epoch := fix.epoch
	epoch.Height = height
	require.True(t, fix.wallet.handleBlockEpoch(t.Context(), epoch).IsOk())
	res := fix.wallet.handleProcessTipTick(t.Context())
	require.True(t, res.IsOk(), "tip tick failed: %v", res.Err())
}

// TestBoardingIntentsRecordedAtOperatorDepth pins the invariant that a
// boarding UTXO only becomes a confirmed intent once it has the operator's
// MinConfirmations. Before the fix the wallet listed UTXOs at depth 1 and
// recorded them as confirmed, so handleBoard shipped them to an operator that
// then rejected the join with "insufficient confirmations".
func TestBoardingIntentsRecordedAtOperatorDepth(t *testing.T) {
	t.Parallel()

	fix := newEagerBoardFixture(t, false)
	utxo := takeListUnspent(t, fix)
	withTerms(fix, &types.OperatorTerms{MinConfirmations: 6}, nil)

	// At depth 6 the backend reports nothing yet. Any other depth is an
	// unexpected call and fails the test.
	fix.backend.On(
		"ListUnspent", mock.Anything, int32(6),
		int32(MaxConfsForListUnspent),
	).Return([]*Utxo(nil), nil).Once()

	tick(t, fix, 101)
	fix.store.AssertNotCalled(
		t, "InsertBoardingIntents", mock.Anything, mock.Anything,
	)
	require.False(
		t,
		fix.wallet.seenUtxos.Contains(
			NewUtxoKey(utxo.Outpoint),
		),
	)
	fix.backend.AssertNumberOfCalls(t, "ListUnspent", 1)

	// Once the deposit reaches the operator depth it is recorded.
	fix.backend.On(
		"ListUnspent", mock.Anything, int32(6),
		int32(MaxConfsForListUnspent),
	).Return([]*Utxo{utxo}, nil).Once()

	tick(t, fix, 102)
	fix.store.AssertNumberOfCalls(t, "InsertBoardingIntents", 1)
	require.True(
		t,
		fix.wallet.seenUtxos.Contains(
			NewUtxoKey(utxo.Outpoint),
		),
	)
}

// TestBoardingDepth covers how the operator terms map to the depth the
// wallet requires.
func TestBoardingDepth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		terms *types.OperatorTerms
		err   error
		fetch bool
		want  int32
		fails bool
	}{
		{
			name: "no terms source",
			want: MinBoardingConfs,
		},
		{
			name: "nil terms", fetch: true,
			want: MinBoardingConfs,
		},
		{
			name: "zero advertised", fetch: true,
			terms: &types.OperatorTerms{},
			want:  MinBoardingConfs,
		},
		{
			name: "operator depth", fetch: true,
			terms: &types.OperatorTerms{
				MinConfirmations: 6,
			},
			want: 6,
		},
		{
			name: "fetch error", fetch: true,
			err: errors.New("boom"), fails: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fix := newEagerBoardFixture(t, false)
			if tc.fetch {
				withTerms(fix, tc.terms, tc.err)
			}

			got, err := fix.wallet.boardingDepth(t.Context())
			if tc.fails {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestBoardingTipNotAdvancedWithoutTerms checks that a failed terms lookup
// leaves the tip unprocessed, so the next tick retries instead of recording
// intents at an unknown depth.
func TestBoardingTipNotAdvancedWithoutTerms(t *testing.T) {
	t.Parallel()

	fix := newEagerBoardFixture(t, false)
	takeListUnspent(t, fix)
	withTerms(fix, nil, errors.New("operator unreachable"))

	tick(t, fix, 101)
	require.Zero(t, fix.wallet.processedTipHeight.Load())
	fix.store.AssertNotCalled(
		t, "InsertBoardingIntents", mock.Anything, mock.Anything,
	)
}

// TestUnconfirmedBoardingBalanceBelowOperatorDepth checks that deposits below
// the operator depth are reported as unconfirmed rather than ready to board.
func TestUnconfirmedBoardingBalanceBelowOperatorDepth(t *testing.T) {
	t.Parallel()

	fix := newEagerBoardFixture(t, false)
	deep := takeListUnspent(t, fix)
	withTerms(fix, &types.OperatorTerms{MinConfirmations: 6}, nil)

	shallow := *deep
	shallow.Confirmations = 1
	shallow.Amount = btcutil.Amount(7_000)

	deep.Confirmations = 6
	fix.backend.On(
		"ListUnspent", mock.Anything, int32(0),
		int32(MaxConfsForListUnspent),
	).Return([]*Utxo{&shallow, deep}, nil)

	total, count, err := fix.wallet.unconfirmedBoardingBalance(
		t.Context(),
	)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(7_000), total)
	require.Equal(t, 1, count)
}
