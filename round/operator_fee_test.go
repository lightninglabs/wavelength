package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/wallet"
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

// TestComputeClientOperatorFeeAssetDeposit checks that an asset deposit and
// the transfer-in VTXOs it funds stay out of the fee, which is booked only for
// the client's own inputs and typed by them.
func TestComputeClientOperatorFeeAssetDeposit(t *testing.T) {
	t.Parallel()

	deposit := newBoardingIntent(4_000)
	deposit.Request.AssetRef = "asset"
	depositVTXOs := []*ClientVTXO{
		{
			Amount: btcutil.Amount(1_000),
			Origin: types.VTXOOriginRoundTransfer,
		},
		{
			Amount: btcutil.Amount(2_000),
			Origin: types.VTXOOriginRoundTransfer,
		},
	}

	alone := Intents{Boarding: []BoardingIntent{deposit}}
	require.Zero(t, computeClientOperatorFee(alone, depositVTXOs))

	withRefresh := Intents{
		Boarding: []BoardingIntent{
			deposit,
		},
		Forfeits: []types.ForfeitRequest{
			{
				Amount: btcutil.Amount(50_000),
			},
		},
	}
	refreshed := append([]*ClientVTXO{{
		Amount: btcutil.Amount(49_800),
		Origin: types.VTXOOriginRoundRefresh,
	}}, depositVTXOs...)
	require.Equal(
		t, int64(200), computeClientOperatorFee(withRefresh, refreshed),
	)
	require.Equal(
		t, ledger.FeeTypeRefresh, roundOperatorFeeType(withRefresh),
	)

	withBoarding := Intents{
		Boarding: []BoardingIntent{
			deposit,
			newBoardingIntent(100_000),
		},
	}
	boarded := append([]*ClientVTXO{{
		Amount: btcutil.Amount(99_500),
		Origin: types.VTXOOriginRoundBoarding,
	}}, depositVTXOs...)
	require.Equal(
		t, int64(500), computeClientOperatorFee(withBoarding, boarded),
	)
	require.Equal(
		t, ledger.FeeTypeBoarding, roundOperatorFeeType(withBoarding),
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
// produces a recipient VTXO, keeps the change, and pays only the
// operator cut as a fee. Recipient value is accounted separately as
// a round outflow, so it must not be folded into OperatorFeeSat.
func TestComputeClientOperatorFeeDirectedSendWithChange(t *testing.T) {
	t.Parallel()

	intents := Intents{
		Forfeits: []types.ForfeitRequest{
			{
				Amount: btcutil.Amount(100_000),
			},
		},
		VTXOs: []types.VTXORequest{
			{
				Amount: btcutil.Amount(40_000),
			},
			{
				Amount: btcutil.Amount(59_500),
			},
		},
	}

	// 100_000 - (40_000 recipient + 59_500 change) = 500
	// operator fee. The 40_000 recipient value is emitted as a
	// separate VTXOSentMsg by roundLedgerOutflows.
	require.Equal(
		t, int64(500), computeClientOperatorFee(intents, nil),
	)
}

// TestRoundLedgerOutflowKeysIncludeRoundID ensures keyed round outflows
// remain unique across rounds. The ledger DB dedupes idempotency keys
// globally, so output-index-only keys would silently suppress later
// rounds with the same foreign VTXO or leave position.
func TestRoundLedgerOutflowKeysIncludeRoundID(t *testing.T) {
	t.Parallel()

	intents := Intents{
		VTXOs: []types.VTXORequest{
			{
				Amount: btcutil.Amount(10_000),
			},
		},
		Leaves: []*types.LeaveRequest{
			{
				Output: &wire.TxOut{
					Value: 5_000,
				},
			},
		},
	}
	roundID1 := testRoundID("round-outflow-1")
	roundID2 := testRoundID("round-outflow-2")

	outflows1 := roundLedgerOutflows(roundID1, intents, nil)
	outflows2 := roundLedgerOutflows(roundID2, intents, nil)

	require.Len(t, outflows1, 2)
	require.Len(t, outflows2, 2)
	for i := range outflows1 {
		require.Contains(
			t, string(outflows1[i].IdempotencyKey),
			roundID1.String(),
		)
		require.Contains(
			t, string(outflows2[i].IdempotencyKey),
			roundID2.String(),
		)
		require.NotEqual(
			t, outflows1[i].IdempotencyKey,
			outflows2[i].IdempotencyKey,
		)
	}
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

// commitmentPacket wraps a set of outputs in the minimal PSBT shape
// roundLedgerOutflows reads.
func commitmentPacket(outs ...*wire.TxOut) *psbt.Packet {
	return &psbt.Packet{
		UnsignedTx: &wire.MsgTx{
			TxOut: outs,
		},
	}
}

// TestRoundLedgerOutflowsCarryOwnWalletFlag proves the leave's own-wallet flag
// reaches the ledger outflow with the commitment output index it paid, that an
// unflagged leave stays an outflow, and that a foreign directed-send recipient
// output can never be flagged. The boarding-limit change leave is the
// unflagged case that matters most: it pays a boarding script whose later
// deposit leg already books the return, so flagging it would count the same
// satoshis onto wallet_balance twice.
func TestRoundLedgerOutflowsCarryOwnWalletFlag(t *testing.T) {
	t.Parallel()

	ownScript := []byte{0x51, 0x20, 0xaa}
	intents := Intents{
		VTXOs: []types.VTXORequest{
			{
				Amount: btcutil.Amount(10_000),
			},
		},
		Leaves: []*types.LeaveRequest{
			{
				Output: &wire.TxOut{
					Value:    5_000,
					PkScript: ownScript,
				},
				DestinationOwnWallet: true,
			},
			{
				// The boarding-limit change leave, and any
				// leave to a destination we cannot claim.
				Output: &wire.TxOut{
					Value: 3_000,
				},
			},
			nil,
		},
	}

	outflows := roundLedgerOutflows(
		testRoundID("round-own-wallet"), intents,
		commitmentPacket(
			&wire.TxOut{
				Value: 1_000,
			}, &wire.TxOut{
				Value:    5_000,
				PkScript: ownScript,
			},
		),
	)
	require.Len(t, outflows, 3)

	// The recipient VTXO output is somebody else's by definition.
	require.Equal(t, int64(10_000), outflows[0].AmountSat)
	require.False(t, outflows[0].ProceedsOwnWallet)

	require.Equal(t, int64(5_000), outflows[1].AmountSat)
	require.True(t, outflows[1].ProceedsOwnWallet)
	require.Equal(t, uint32(1), outflows[1].ProceedsVout)

	require.Equal(t, int64(3_000), outflows[2].AmountSat)
	require.False(t, outflows[2].ProceedsOwnWallet)
}

// TestRoundLedgerOutflowsDropFlagWithoutAnOutpoint proves the flag fails
// closed. A leave the commitment transaction cannot name unambiguously books
// as a plain outflow, because a proceeds credit with no on-chain identity
// could never be reversed when those coins are boarded again.
func TestRoundLedgerOutflowsDropFlagWithoutAnOutpoint(t *testing.T) {
	t.Parallel()

	ownScript := []byte{0x51, 0x20, 0xaa}
	leave := &types.LeaveRequest{
		Output: &wire.TxOut{
			Value:    5_000,
			PkScript: ownScript,
		},
		DestinationOwnWallet: true,
	}
	intents := Intents{Leaves: []*types.LeaveRequest{leave}}
	roundID := testRoundID("round-no-outpoint")

	cases := []struct {
		name string
		tx   *psbt.Packet
	}{
		{
			name: "no commitment transaction",
			tx:   nil,
		},
		{
			name: "output not present",
			tx: commitmentPacket(
				&wire.TxOut{Value: 5_000},
			),
		},
		{
			name: "two identical outputs",
			tx: commitmentPacket(
				&wire.TxOut{
					Value:    5_000,
					PkScript: ownScript,
				},
				&wire.TxOut{
					Value:    5_000,
					PkScript: ownScript,
				},
			),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			outflows := roundLedgerOutflows(roundID, intents, tc.tx)
			require.Len(t, outflows, 1)
			require.False(t, outflows[0].ProceedsOwnWallet)
		})
	}
}

// TestRoundLedgerOutflowsMatchQuotedLeaveValue proves a fee-bearing leave
// still resolves its commitment output. The seal-time quote shaves the fee
// off the requested output value, so the commitment transaction carries the
// quoted amount and not the intent target. Matching on the request value
// would find no output, drop the own-wallet flag, and leave the payment to
// our own wallet booked as an outflow with no reversible audit row, which a
// later re-board would then credit a second time.
func TestRoundLedgerOutflowsMatchQuotedLeaveValue(t *testing.T) {
	t.Parallel()

	ownScript := []byte{0x51, 0x20, 0xbb}
	newIntents := func() Intents {
		return Intents{
			Leaves: []*types.LeaveRequest{
				{
					Output: &wire.TxOut{
						Value:    5_000,
						PkScript: ownScript,
					},
					DestinationOwnWallet: true,
				},
			},

			// The quote nets a 250-sat fee out of the requested
			// 5_000-sat leave.
			QuotedLeaveAmounts: []int64{
				4_750,
			},
		}
	}

	// The commitment output carries the quoted value, so the vout
	// resolves and the proceeds keep an on-chain identity.
	outflows := roundLedgerOutflows(
		testRoundID("round-quoted-leave"), newIntents(),
		commitmentPacket(
			&wire.TxOut{
				Value: 1_000,
			}, &wire.TxOut{
				Value:    4_750,
				PkScript: ownScript,
			},
		),
	)
	require.Len(t, outflows, 1)
	require.Equal(t, int64(4_750), outflows[0].AmountSat)
	require.True(t, outflows[0].ProceedsOwnWallet)
	require.Equal(t, uint32(1), outflows[0].ProceedsVout)

	// A commitment transaction carrying neither the quoted nor the
	// requested value still fails closed.
	outflows = roundLedgerOutflows(
		testRoundID("round-quoted-leave"), newIntents(),
		commitmentPacket(
			&wire.TxOut{
				Value:    5_000,
				PkScript: ownScript,
			},
		),
	)
	require.Len(t, outflows, 1)
	require.Equal(t, int64(4_750), outflows[0].AmountSat)
	require.False(t, outflows[0].ProceedsOwnWallet)
}
