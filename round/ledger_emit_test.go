package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// newLedgerEmitActor builds a RoundClientActor shell wired to a
// mock ledger sink, stripped of everything emitVTXOsReceived
// does not touch (no FSM, no store, no wallet). Used only by the
// emission-path tests so each case is a small, readable setup.
func newLedgerEmitActor(t *testing.T,
	sink ledger.Sink) *RoundClientActor {

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
		Hash:  chainhash.Hash{0x11},
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
		Hash:  chainhash.Hash{0x33},
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
		t, ok, "first emission must be VTXOSentMsg, got %T",
		msgs[0],
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

	require.Empty(t, drainLedgerMessages(t, sink),
		"Unknown origin must not emit a ledger message "+
			"(would risk misclassifying the entry)")
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
	require.Len(t, msgs, 4,
		"1 boarding + 1 transfer + (1 sent + 1 received) "+
			"for refresh = 4 messages")

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
