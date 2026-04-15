package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// newLedgerSinkActor builds a durable-behavior instance bound to
// an in-memory ledger sink so tests can observe emitted messages
// without spinning up a real ledger actor.
func newLedgerSinkActor(
	t *testing.T,
	sink fn.Option[ledger.Sink]) *oorDurableBehavior {

	t.Helper()

	return &oorDurableBehavior{
		cfg: ClientActorCfg{
			Log:        fn.Some(btclog.Disabled),
			LedgerSink: sink,
		},
		sessions: map[SessionID]*sessionHandle{},
	}
}

// TestEmitVTXOSentSumsTransferInputs confirms emitVTXOSent
// totals every TransferInput's VTXO amount and posts one
// VTXOSentMsg tagged with the finalize session_id. OOR is
// fee-less on the wire so the sum equals what the counterparty
// receives.
func TestEmitVTXOSentSumsTransferInputs(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)
	b := newLedgerSinkActor(t, fn.Some[ledger.Sink](sink))

	sessionID := SessionID{0xaa}
	state := &AwaitingFinalizeAccepted{
		TransferInputs: []TransferInput{
			{VTXO: &vtxo.Descriptor{Amount: 10_000}},
			{VTXO: &vtxo.Descriptor{Amount: 15_500}},
			{VTXO: &vtxo.Descriptor{Amount: 4_500}},
		},
	}

	b.emitVTXOSent(t.Context(), sessionID, state)

	// Exactly one message, summed across all inputs.
	select {
	case raw := <-sink.Messages():
		msg, ok := raw.(*ledger.VTXOSentMsg)
		require.True(t, ok, "expected VTXOSentMsg, got %T", raw)
		require.Equal(t, int64(30_000), msg.AmountSat)
		require.Equal(t, [32]byte(sessionID), msg.SessionID)
	default:
		t.Fatalf("no ledger message emitted")
	}

	// No second message leaks out.
	select {
	case msg := <-sink.Messages():
		t.Fatalf("unexpected second message: %T", msg)
	default:
	}
}

// TestEmitVTXOSentSkipsZeroTotal covers the guard that prevents
// emitting a 0-amount message. OOR transfers that land with a
// degenerate (zero-sum or empty) TransferInputs slice produce
// no ledger entry rather than a message the handler rejects.
func TestEmitVTXOSentSkipsZeroTotal(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)
	b := newLedgerSinkActor(t, fn.Some[ledger.Sink](sink))

	cases := []struct {
		name  string
		state *AwaitingFinalizeAccepted
	}{
		{
			name:  "nil state",
			state: nil,
		},
		{
			name:  "empty inputs",
			state: &AwaitingFinalizeAccepted{},
		},
		{
			name: "zero-value input",
			state: &AwaitingFinalizeAccepted{
				TransferInputs: []TransferInput{
					{VTXO: &vtxo.Descriptor{Amount: 0}},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b.emitVTXOSent(t.Context(), SessionID{}, tc.state)

			select {
			case msg := <-sink.Messages():
				t.Fatalf(
					"unexpected ledger emission for %s: %T",
					tc.name, msg,
				)
			default:
			}
		})
	}
}

// TestEmitVTXOSentNoSink confirms the function is a silent
// no-op when LedgerSink is fn.None. Tests and embedded use
// cases that do not register a ledger actor must be able to
// drive the full OOR flow without wiring accounting.
func TestEmitVTXOSentNoSink(t *testing.T) {
	t.Parallel()

	b := newLedgerSinkActor(t, fn.None[ledger.Sink]())

	// Should not panic and should not reach any sink.
	b.emitVTXOSent(t.Context(), SessionID{0x01},
		&AwaitingFinalizeAccepted{
			TransferInputs: []TransferInput{
				{VTXO: &vtxo.Descriptor{Amount: 42}},
			},
		},
	)
}

// TestEmitVTXOsReceivedPerDescriptor confirms that each
// materialized VTXO descriptor produces one VTXOReceivedMsg
// with Source=SourceOOR and the on-wire amount verbatim.
func TestEmitVTXOsReceivedPerDescriptor(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 8,
	)
	b := newLedgerSinkActor(t, fn.Some[ledger.Sink](sink))

	h1 := chainhash.Hash{0x01}
	h2 := chainhash.Hash{0x02}
	descs := []*vtxo.Descriptor{
		{
			Outpoint: wire.OutPoint{
				Hash: h1, Index: 0,
			},
			Amount: btcutil.Amount(1_000),
		},
		nil, // nil entries must be skipped cleanly.
		{
			Outpoint: wire.OutPoint{
				Hash: h2, Index: 3,
			},
			Amount: btcutil.Amount(2_500),
		},
	}

	b.emitVTXOsReceived(t.Context(), descs)

	msgs := drainChannel(t, sink.Messages())
	require.Len(t, msgs, 2, "nil descriptor must be skipped")

	first, ok := msgs[0].(*ledger.VTXOReceivedMsg)
	require.True(t, ok)
	require.Equal(t, [32]byte(h1), first.OutpointHash)
	require.Equal(t, uint32(0), first.OutpointIndex)
	require.Equal(t, int64(1_000), first.AmountSat)
	require.Equal(t, ledger.SourceOOR, first.Source)

	second, ok := msgs[1].(*ledger.VTXOReceivedMsg)
	require.True(t, ok)
	require.Equal(t, [32]byte(h2), second.OutpointHash)
	require.Equal(t, uint32(3), second.OutpointIndex)
	require.Equal(t, int64(2_500), second.AmountSat)
	require.Equal(t, ledger.SourceOOR, second.Source)
}

// drainChannel collects every message currently buffered on the
// channel without blocking. Used by the tests above so a slow
// test harness does not mask a skipped-message bug with a
// timeout.
func drainChannel[M actor.Message](t *testing.T,
	ch <-chan M) []M {

	t.Helper()

	out := []M{}
	for {
		select {
		case msg := <-ch:
			out = append(out, msg)
		default:
			return out
		}
	}
}
