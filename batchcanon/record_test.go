package batchcanon

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestEffectiveExpiryNoneWhenUnconfirmed verifies that a batch with no
// current confirmation observation has no effective expiry — the structural
// guarantee that expiry is not a standalone terminal fact.
func TestEffectiveExpiryNoneWhenUnconfirmed(t *testing.T) {
	t.Parallel()

	rec := &Record{
		BatchTxID: chainhash.Hash{
			0x01,
		},
		State:              StateUnseen,
		ConfirmationHeight: fn.None[int32](),
		CSVExpiryDelta:     144,
	}

	require.True(t, rec.EffectiveExpiry().IsNone())
}

// TestEffectiveExpiryDerivesFromConfirmation verifies the effective expiry is
// the confirmation height plus the CSV-relative delta.
func TestEffectiveExpiryDerivesFromConfirmation(t *testing.T) {
	t.Parallel()

	rec := &Record{
		BatchTxID: chainhash.Hash{
			0x02,
		},
		State:              StateProvisional,
		ConfirmationHeight: fn.Some[int32](100),
		CSVExpiryDelta:     144,
	}

	got := rec.EffectiveExpiry()
	require.True(t, got.IsSome())
	require.Equal(t, int32(244), got.UnwrapOr(0))
}

// TestEffectiveExpiryRecomputesAfterReconfirm verifies that re-confirming the
// same batch at a different height (as happens after a reorg) yields a fresh
// effective expiry rather than a value frozen at first confirmation.
func TestEffectiveExpiryRecomputesAfterReconfirm(t *testing.T) {
	t.Parallel()

	rec := &Record{
		BatchTxID: chainhash.Hash{
			0x03,
		},
		ConfirmationHeight: fn.Some[int32](100),
		CSVExpiryDelta:     144,
	}
	require.Equal(t, int32(244), rec.EffectiveExpiry().UnwrapOr(0))

	// Reorg: the confirmation leaves the best chain.
	rec.ConfirmationHeight = fn.None[int32]()
	require.True(t, rec.EffectiveExpiry().IsNone())

	// Reconfirmation at a higher height on the new best chain.
	rec.ConfirmationHeight = fn.Some[int32](103)
	require.Equal(t, int32(247), rec.EffectiveExpiry().UnwrapOr(0))
}
