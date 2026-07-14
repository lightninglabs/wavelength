package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// newLedgerEmitActor builds a RoundClientActor shell wired to a
// mock ledger sink, stripped of everything emitVTXOsReceived
// does not touch (no FSM, no store, no wallet). Used only by the
// emission-path tests so each case is a small, readable setup.
func newLedgerEmitActor(t *testing.T, sink ledger.Sink) *RoundClientActor {
	t.Helper()

	return &RoundClientActor{
		cfg: &RoundClientConfig{
			LedgerSink: fn.Some(sink),
		},
		log: btclog.Disabled,
	}
}

// drainLedgerMessages pulls every queued ledger message off the
// capture ref without blocking. Returns them in receive order so
// assertions on emission order are deterministic.
func drainLedgerMessages(t *testing.T,
	sink *actor.ChannelTellOnlyRef[ledger.LedgerMsg],
) []ledger.LedgerMsg {

	t.Helper()

	var msgs []ledger.LedgerMsg
	for {
		select {
		case m := <-sink.Messages():
			msgs = append(msgs, m)

		default:
			return msgs
		}
	}
}

// TestEmitVTXOsReceivedBoardingOrigin confirms a boarding-origin
// VTXO dispatches a single VTXOReceivedMsg stamped with
// SourceRoundBoarding. That routing is what credits wallet_balance
// on the ledger side; mis-routing it to SourceRoundTransfer would
// silently corrupt the chart of accounts.
func TestEmitVTXOsReceivedBoardingOrigin(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"round-ledger", 8,
	)
	a := newLedgerEmitActor(t, sink)

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
		},
		Index: 1,
	}
	roundUUID := uuid.New()
	a.emitVTXOsReceived(t.Context(), &VTXOCreatedNotification{
		RoundID: roundUUID.String(),
		VTXOs: []*ClientVTXO{{
			Outpoint: outpoint,
			Amount:   btcutil.Amount(50_000),
			Origin:   types.VTXOOriginRoundBoarding,
		}},
	})

	msgs := drainLedgerMessages(t, sink)
	require.Len(t, msgs, 1)

	recv, ok := msgs[0].(*ledger.VTXOReceivedMsg)
	require.True(t, ok, "expected VTXOReceivedMsg, got %T", msgs[0])
	require.Equal(
		t, ledger.SourceRoundBoarding, recv.Source,
	)
	require.Equal(t, int64(50_000), recv.AmountSat)
	require.Equal(t, [32]byte(outpoint.Hash), recv.OutpointHash)
	require.Equal(t, outpoint.Index, recv.OutpointIndex)
	require.Equal(t, [16]byte(roundUUID[:]), recv.RoundID)
}

// TestEmitVTXOsReceivedTransferOrigin confirms a transfer-origin
// VTXO (another participant's in-round send to this client)
// dispatches a single VTXOReceivedMsg stamped with
// SourceRoundTransfer. Prior to task A this was the default for
// every VTXO, which misclassified boarding outputs.
func TestEmitVTXOsReceivedTransferOrigin(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"round-ledger", 8,
	)
	a := newLedgerEmitActor(t, sink)

	a.emitVTXOsReceived(t.Context(), &VTXOCreatedNotification{
		RoundID: uuid.New().String(),
		VTXOs: []*ClientVTXO{{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{0x22},
			},
			Amount: btcutil.Amount(15_000),
			Origin: types.VTXOOriginRoundTransfer,
		}},
	})

	msgs := drainLedgerMessages(t, sink)
	require.Len(t, msgs, 1)

	recv, ok := msgs[0].(*ledger.VTXOReceivedMsg)
	require.True(t, ok)
	require.Equal(
		t, ledger.SourceRoundTransfer, recv.Source,
	)
}

// TestEmitVTXOsReceivedRefreshEmitsPair confirms a refresh-origin
// VTXO emits BOTH a VTXOSentMsg (for the gross forfeited value)
// and a VTXOReceivedMsg with Source=SourceRoundRefresh, in that
// order, and that both carry the same outpoint. The outpoint on
// VTXOSentMsg is what lets handleVTXOSent stamp a per-VTXO
// idempotency key instead of colliding on
// idx_client_ledger_idempotent_round with other sends in the
// same round.
func TestEmitVTXOsReceivedRefreshEmitsPair(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"round-ledger", 8,
	)
	a := newLedgerEmitActor(t, sink)

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x33,
		},
		Index: 2,
	}
	roundUUID := uuid.New()
	a.emitVTXOsReceived(t.Context(), &VTXOCreatedNotification{
		RoundID: roundUUID.String(),
		VTXOs: []*ClientVTXO{{
			Outpoint: outpoint,
			Amount:   btcutil.Amount(40_000),
			Origin:   types.VTXOOriginRoundRefresh,
		}},
	})

	msgs := drainLedgerMessages(t, sink)
	require.Len(t, msgs, 2)

	sent, ok := msgs[0].(*ledger.VTXOSentMsg)
	require.True(
		t, ok, "first emission must be VTXOSentMsg, got %T", msgs[0],
	)
	require.Equal(t, outpoint, sent.Outpoint)
	require.Equal(t, int64(40_000), sent.AmountSat)
	require.Equal(t, [16]byte(roundUUID[:]), sent.RoundID)

	recv, ok := msgs[1].(*ledger.VTXOReceivedMsg)
	require.True(
		t, ok, "second emission must be VTXOReceivedMsg, got %T",
		msgs[1],
	)
	require.Equal(
		t, ledger.SourceRoundRefresh, recv.Source,
	)
	require.Equal(t, int64(40_000), recv.AmountSat)
	require.Equal(t, [32]byte(outpoint.Hash), recv.OutpointHash)
	require.Equal(t, outpoint.Index, recv.OutpointIndex)
}

// TestEmitVTXOsReceivedUnknownOriginIsNoOp is a defensive test:
// if the wallet or a legacy composition path forgets to tag
// origin, the emission must skip the VTXO rather than falling
// back to a default source that would misclassify. A
// misclassification here silently corrupts the chart of
// accounts, so strict no-op is the intended behavior.
func TestEmitVTXOsReceivedUnknownOriginIsNoOp(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"round-ledger", 4,
	)
	a := newLedgerEmitActor(t, sink)

	a.emitVTXOsReceived(t.Context(), &VTXOCreatedNotification{
		RoundID: uuid.New().String(),
		VTXOs: []*ClientVTXO{{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{0x44},
			},
			Amount: btcutil.Amount(7_000),
			Origin: types.VTXOOriginUnknown,
		}},
	})

	require.Empty(
		t, drainLedgerMessages(t, sink),
		"Unknown origin must not emit a ledger message (would risk "+
			"misclassifying the entry)",
	)
}

// TestEmitVTXOsReceivedRefreshEmitsFeePaidMsg confirms the round
// actor appends a single FeePaidMsg{FeeType=refresh} after the
// paired VTXOSent+VTXOReceived legs when the round has
// OperatorFeeSat > 0 and at least one refresh-origin VTXO. This
// is the net of the refresh-round accounting: transfers_out
// legs cancel, vtxo_balance drops by exactly the fee, fees_paid
// rises by exactly the fee.
func TestEmitVTXOsReceivedRefreshEmitsFeePaidMsg(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"round-ledger", 8,
	)
	a := newLedgerEmitActor(t, sink)

	roundUUID := uuid.New()
	a.emitVTXOsReceived(t.Context(), &VTXOCreatedNotification{
		RoundID:        roundUUID.String(),
		OperatorFeeSat: 850,
		CreatedHeight:  800_111,
		VTXOs: []*ClientVTXO{{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{0x55},
			},
			Amount: btcutil.Amount(30_000),
			Origin: types.VTXOOriginRoundRefresh,
		}},
	})

	msgs := drainLedgerMessages(t, sink)
	require.Len(
		t, msgs, 3,
		"expected VTXOSent + VTXOReceived(refresh) + FeePaidMsg",
	)

	_, ok := msgs[0].(*ledger.VTXOSentMsg)
	require.True(t, ok, "first emission must be VTXOSentMsg")
	_, ok = msgs[1].(*ledger.VTXOReceivedMsg)
	require.True(t, ok, "second emission must be VTXOReceivedMsg")

	fee, ok := msgs[2].(*ledger.FeePaidMsg)
	require.True(
		t, ok, "third emission must be FeePaidMsg, got %T", msgs[2],
	)
	require.Equal(t, ledger.FeeTypeRefresh, fee.FeeType)
	require.Equal(t, int64(850), fee.AmountSat)
	require.Equal(t, [16]byte(roundUUID[:]), fee.RoundID)
	require.Equal(t, uint32(800_111), fee.BlockHeight)
}

// TestEmitVTXOsReceivedNoFeeWhenZero covers the common "free
// operation" case: if OperatorFeeSat is zero the round actor
// must not emit a FeePaidMsg. Emitting a zero-amount message
// would be rejected by handleFeePaid's positivity guard and
// dead-letter the whole dispatch.
func TestEmitVTXOsReceivedNoFeeWhenZero(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"round-ledger", 4,
	)
	a := newLedgerEmitActor(t, sink)

	a.emitVTXOsReceived(t.Context(), &VTXOCreatedNotification{
		RoundID:        uuid.New().String(),
		OperatorFeeSat: 0,
		VTXOs: []*ClientVTXO{{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{0x66},
			},
			Amount: btcutil.Amount(20_000),
			Origin: types.VTXOOriginRoundRefresh,
		}},
	})

	for _, m := range drainLedgerMessages(t, sink) {
		_, ok := m.(*ledger.FeePaidMsg)
		require.False(
			t, ok, "FeePaidMsg must not be emitted when "+
				"OperatorFeeSat is zero, got %#v", m,
		)
	}
}

// TestEmitVTXOsReceivedNoFeeForBoardingOnly locks in the deferral
// TestEmitVTXOsReceivedBoardingFee confirms a pure-boarding round
// with a non-zero OperatorFeeSat emits a boarding FeePaidMsg.
func TestEmitVTXOsReceivedBoardingFee(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"round-ledger", 4,
	)
	a := newLedgerEmitActor(t, sink)

	a.emitVTXOsReceived(t.Context(), &VTXOCreatedNotification{
		RoundID:         uuid.New().String(),
		OperatorFeeSat:  500,
		OperatorFeeType: ledger.FeeTypeBoarding,
		VTXOs: []*ClientVTXO{{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{0x77},
			},
			Amount: btcutil.Amount(10_000),
			Origin: types.VTXOOriginRoundBoarding,
		}},
	})

	msgs := drainLedgerMessages(t, sink)
	require.Len(t, msgs, 2)

	fee, ok := msgs[1].(*ledger.FeePaidMsg)
	require.True(t, ok, "expected FeePaidMsg, got %T", msgs[1])
	require.Equal(t, int64(500), fee.AmountSat)
	require.Equal(t, ledger.FeeTypeBoarding, fee.FeeType)
}

// TestEmitVTXOsReceivedMixedBatch verifies a single notification
// carrying a batch of VTXOs with heterogenous origins routes
// each one correctly: a boarding VTXO + a transfer VTXO + a
// refresh VTXO produces exactly one VTXOReceived(Boarding),
// one VTXOReceived(Transfer), and a paired
// VTXOSent + VTXOReceived(Refresh). This locks in the scenario
// most likely to regress (directed-send round where the sender
// produces self-change refresh + recipient transfer + wallet
// boarding change all in one round).
func TestEmitVTXOsReceivedMixedBatch(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"round-ledger", 16,
	)
	a := newLedgerEmitActor(t, sink)

	a.emitVTXOsReceived(t.Context(), &VTXOCreatedNotification{
		RoundID: uuid.New().String(),
		VTXOs: []*ClientVTXO{
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{0xa1},
				},
				Amount: btcutil.Amount(10_000),
				Origin: types.VTXOOriginRoundBoarding,
			},
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{0xa2},
				},
				Amount: btcutil.Amount(20_000),
				Origin: types.VTXOOriginRoundTransfer,
			},
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{0xa3},
				},
				Amount: btcutil.Amount(30_000),
				Origin: types.VTXOOriginRoundRefresh,
			},
		},
	})

	msgs := drainLedgerMessages(t, sink)
	require.Len(
		t, msgs, 4, "1 boarding + 1 transfer + (1 sent + 1 "+
			"received) for refresh = 4 messages",
	)

	// 1st: boarding receive.
	recv, ok := msgs[0].(*ledger.VTXOReceivedMsg)
	require.True(t, ok)
	require.Equal(
		t, ledger.SourceRoundBoarding, recv.Source,
	)
	require.Equal(t, int64(10_000), recv.AmountSat)

	// 2nd: transfer receive.
	recv, ok = msgs[1].(*ledger.VTXOReceivedMsg)
	require.True(t, ok)
	require.Equal(
		t, ledger.SourceRoundTransfer, recv.Source,
	)
	require.Equal(t, int64(20_000), recv.AmountSat)

	// 3rd + 4th: refresh pair.
	sent, ok := msgs[2].(*ledger.VTXOSentMsg)
	require.True(t, ok)
	require.Equal(t, int64(30_000), sent.AmountSat)

	recv, ok = msgs[3].(*ledger.VTXOReceivedMsg)
	require.True(t, ok)
	require.Equal(
		t, ledger.SourceRoundRefresh, recv.Source,
	)
	require.Equal(t, int64(30_000), recv.AmountSat)
}

// TestRoundIDBytesValidUUID verifies that a canonical RoundID
// string round-trips to its 16-byte form intact. uuid.Parse is
// the authoritative parser; the test pins the behaviour so a
// future switch to a different form factor (hex, etc.) does not
// silently change what gets stored in the ledger.
func TestRoundIDBytesValidUUID(t *testing.T) {
	t.Parallel()

	id := uuid.New()

	got := roundIDBytes(id.String())
	require.Equal(t, [16]byte(id[:]), got)
}

// TestRoundIDBytesInvalidReturnsZero covers the fallback path
// where a malformed RoundID string decays to the zero array.
// The ledger handler stores zero as NULL via roundIDOrNil so a
// bad input degrades to a non-round-tagged entry rather than
// rejecting the whole message.
func TestRoundIDBytesInvalidReturnsZero(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"not-a-uuid",
		"abcdef",
		"12345678-1234-1234-1234-1234567890ZZ", // bad hex char
	}

	var zero [16]byte
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			require.Equal(t, zero, roundIDBytes(in))
		})
	}
}
