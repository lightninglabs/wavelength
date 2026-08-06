package vtxo

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestCheckForfeitAdmission asserts the admissible set for a new cooperative
// forfeit commitment across every VTXO lifecycle status, so a status added
// later cannot silently inherit the permissive default.
func TestCheckForfeitAdmission(t *testing.T) {
	t.Parallel()

	outpoint := wire.OutPoint{Hash: chainhash.Hash{0x01}, Index: 0}

	tests := []struct {
		name    string
		status  VTXOStatus
		roundID string
		wantErr error
	}{{
		name:   "live is the ordinary admission",
		status: VTXOStatusLive,
	}, {
		// The only way to recover an expired VTXO's value is to
		// forfeit it into an ordinary round, so refusing it here
		// would strand the coin.
		name:   "expired stays reclaimable",
		status: VTXOStatusExpired,
	}, {
		name:    "pending forfeit is already committed",
		status:  VTXOStatusPendingForfeit,
		wantErr: ErrForfeitInFlight,
	}, {
		name:    "forfeiting is already committed",
		status:  VTXOStatusForfeiting,
		roundID: "019fd94b-a1e6-7422-a625-e90c75599e72",
		wantErr: ErrForfeitInFlight,
	}, {
		name:    "spending is owned by an OOR claim",
		status:  VTXOStatusSpending,
		wantErr: ErrVTXOLiquidityLocked,
	}, {
		name:    "unilateral exit is owned by the chain resolver",
		status:  VTXOStatusUnilateralExit,
		wantErr: ErrExitInFlight,
	}, {
		name:    "forfeited is terminal",
		status:  VTXOStatusForfeited,
		wantErr: ErrVTXOTerminal,
	}, {
		name:    "spent is terminal",
		status:  VTXOStatusSpent,
		wantErr: ErrVTXOTerminal,
	}, {
		name:    "failed is terminal",
		status:  VTXOStatusFailed,
		wantErr: ErrVTXOTerminal,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := CheckForfeitAdmission(&Descriptor{
				Outpoint:       outpoint,
				Status:         test.status,
				ForfeitRoundID: test.roundID,
			})

			if test.wantErr == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, test.wantErr)

			// Every rejection must name the coin it is about, so
			// a batch leave's per-outpoint errors stay
			// attributable once they are collected together.
			require.Contains(t, err.Error(), outpoint.String())
		})
	}
}

// TestCheckForfeitAdmissionNamesRound asserts a committed VTXO's rejection
// points at the round holding it, which is the only handle a user has for
// looking up why their request was refused.
func TestCheckForfeitAdmissionNamesRound(t *testing.T) {
	t.Parallel()

	const roundID = "019fd94b-a1e6-7422-a625-e90c75599e72"

	err := CheckForfeitAdmission(&Descriptor{
		Outpoint:       wire.OutPoint{Hash: chainhash.Hash{0x02}},
		Status:         VTXOStatusForfeiting,
		ForfeitRoundID: roundID,
	})
	require.ErrorIs(t, err, ErrForfeitInFlight)
	require.Contains(t, err.Error(), roundID)

	// A VTXO reserved before the round supplied forfeit details has no
	// round to name yet; the message must degrade rather than print an
	// empty round.
	err = CheckForfeitAdmission(&Descriptor{
		Outpoint: wire.OutPoint{Hash: chainhash.Hash{0x03}},
		Status:   VTXOStatusPendingForfeit,
	})
	require.ErrorIs(t, err, ErrForfeitInFlight)
	require.NotContains(t, err.Error(), ", round ")
	require.True(t, strings.Contains(err.Error(), "pending_forfeit"))
}

// TestCheckForfeitAdmissionNilDescriptor asserts a missing descriptor is
// refused rather than admitted by the zero value, which would otherwise read
// as VTXOStatusLive.
func TestCheckForfeitAdmissionNilDescriptor(t *testing.T) {
	t.Parallel()

	require.ErrorIs(t, CheckForfeitAdmission(nil), ErrVTXOTerminal)
}
