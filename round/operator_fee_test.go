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

// TestComputeClientOperatorFeePureBoarding confirms a round with
// a single boarding input and a single owned output VTXO yields
// fee = inputs - outputs. This is the canonical "client pays
// operator for ark admission" case.
func TestComputeClientOperatorFeePureBoarding(t *testing.T) {
	t.Parallel()

	intents := Intents{
		Boarding: []BoardingIntent{
			newBoardingIntent(100_000),
		},
	}
	vtxos := []*ClientVTXO{
		{
			Amount: btcutil.Amount(99_500),
		},
	}

	require.Equal(
		t, int64(500), computeClientOperatorFee(intents, vtxos),
	)
}

// TestComputeClientOperatorFeePureRefresh covers the refresh
// path: the client forfeits one VTXO, receives a new VTXO at
// slightly lower value (difference = operator fee). No boarding
// inputs, no leave outputs.
func TestComputeClientOperatorFeePureRefresh(t *testing.T) {
	t.Parallel()

	intents := Intents{
		Forfeits: []types.ForfeitRequest{
			{
				VTXOOutpoint: &wire.OutPoint{},
				Amount:       btcutil.Amount(50_000),
			},
		},
	}
	vtxos := []*ClientVTXO{
		{
			Amount: btcutil.Amount(49_800),
		},
	}

	require.Equal(
		t, int64(200), computeClientOperatorFee(intents, vtxos),
	)
}

// TestComputeClientOperatorFeeMixedBoardingAndRefresh locks in
// the additive input side: a round with both boarding and
// forfeit inputs sums them before subtracting outputs.
func TestComputeClientOperatorFeeMixedBoardingAndRefresh(t *testing.T) {
	t.Parallel()

	intents := Intents{
		Boarding: []BoardingIntent{
			newBoardingIntent(80_000),
		},
		Forfeits: []types.ForfeitRequest{
			{
				Amount: btcutil.Amount(20_000),
			},
		},
	}
	vtxos := []*ClientVTXO{
		{
			Amount: btcutil.Amount(50_000),
		},
		{
			Amount: btcutil.Amount(49_200),
		},
	}

	// 80_000 + 20_000 = 100_000 contributed, 50_000 + 49_200 =
	// 99_200 received back as owned VTXOs, fee = 800.
	require.Equal(
		t, int64(800), computeClientOperatorFee(intents, vtxos),
	)
}

// TestComputeClientOperatorFeeLeave covers the cooperative leave
// path: forfeit feeds an on-chain leave output rather than a new
// VTXO. The leave output value counts against inputs exactly
// the same way as an owned VTXO output, so the fee surfaces
// the same way.
func TestComputeClientOperatorFeeLeave(t *testing.T) {
	t.Parallel()

	intents := Intents{
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
	}

	require.Equal(
		t, int64(600),
		computeClientOperatorFee(intents, nil),
	)
}

// TestComputeClientOperatorFeeDirectedSendWithChange covers the
// directed-send-with-change case: the client forfeits one VTXO,
// produces a recipient VTXO (foreign, skipped by
// buildClientVTXOs), and keeps the change. From this client's
// view the only owned output is the change VTXO, so the fee
// absorbs the recipient value plus the operator cut. The
// calculator does not know about the recipient VTXO because
// buildClientVTXOs filtered it out before passing ownedVTXOs
// to us -- this test pins that contract.
func TestComputeClientOperatorFeeDirectedSendWithChange(t *testing.T) {
	t.Parallel()

	intents := Intents{
		Forfeits: []types.ForfeitRequest{
			{
				Amount: btcutil.Amount(100_000),
			},
		},
	}
	// Only the client's own change VTXO: recipient's 40_000 is
	// foreign and was filtered out by HasLocalOwner before the
	// fee calculator sees it.
	vtxos := []*ClientVTXO{
		{
			Amount: btcutil.Amount(59_500),
		},
	}

	// 100_000 - 59_500 = 40_500 "flowed out" of this client's
	// side -- 40_000 to the recipient + 500 to the operator.
	// The fee math can't distinguish the recipient portion; it
	// surfaces the total client outflow. Caller responsibility
	// (tracked in task follow-ups) is to emit a VTXOSentMsg for
	// the recipient portion before reading this number.
	require.Equal(
		t, int64(40_500), computeClientOperatorFee(intents, vtxos),
	)
}

// TestComputeClientOperatorFeeZeroWhenNoContribution covers the
// degenerate case of a round where this client contributed
// nothing: a remote recipient-only slot. Fee is zero.
func TestComputeClientOperatorFeeZeroWhenNoContribution(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, int64(0),
		computeClientOperatorFee(Intents{}, nil),
	)
}

// TestComputeClientOperatorFeeClampsNegative guards against a
// pathological state where outputs exceed inputs (wallet was
// already paid but no input was booked, accounting bug
// upstream). Returning a negative number would generate a
// nonsensical FeePaidMsg that the ledger handler would reject,
// silently dropping the whole notification. Clamping to zero is
// strictly safer and surfaces the upstream bug via a missing
// fee row rather than a broken ledger.
func TestComputeClientOperatorFeeClampsNegative(t *testing.T) {
	t.Parallel()

	intents := Intents{
		Forfeits: []types.ForfeitRequest{
			{
				Amount: btcutil.Amount(100),
			},
		},
	}
	vtxos := []*ClientVTXO{
		{
			Amount: btcutil.Amount(200),
		},
	}

	require.Equal(
		t, int64(0),
		computeClientOperatorFee(intents, vtxos),
	)
}

// TestComputeClientOperatorFeeIgnoresNilEntries is a defensive
// test: the calculator must survive nil entries in the input
// slices without panicking. The wallet should never produce
// those, but a future persistence layer resuming a partially-
// serialized round state could surface nils. Ignoring them is
// safer than crashing.
func TestComputeClientOperatorFeeIgnoresNilEntries(t *testing.T) {
	t.Parallel()

	intents := Intents{
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
	}
	vtxos := []*ClientVTXO{
		nil,
		{
			Amount: btcutil.Amount(4_500),
		},
	}

	// Inputs: 10_000. Outputs: 5_000 (leave) + 4_500 (vtxo)
	// = 9_500. Fee: 500.
	require.Equal(
		t, int64(500), computeClientOperatorFee(intents, vtxos),
	)
}
