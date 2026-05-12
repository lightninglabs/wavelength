package rounds

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestJoinRoundQuoteOutboxToProtoPositionalOrder is a regression test
// for the map-iteration bug: Quote.VTXOAmounts used to be
// map[SigningKeyHex]btcutil.Amount, and ToProto iterated the map in
// Go's randomized order to build the VtxoQuotes wire slice. The
// client reads the slice positionally (by IntentVTXOReqs index), so
// any intent with 2+ VTXO outputs would hit mismatched amounts on
// client-side leaf validation whenever the server's map iteration
// order diverged from the intent order. After the refactor to
// positional []btcutil.Amount slices, the emitted proto order must
// match the Quote's slice order byte-for-byte on every invocation.
//
// Drive the test with a 5-output intent so any position drift is
// obvious, and re-marshal the proto 50 times to shake out any latent
// nondeterminism (Go map iteration randomizes per-iteration, not
// per-run).
func TestJoinRoundQuoteOutboxToProtoPositionalOrder(t *testing.T) {
	t.Parallel()

	const numOutputs = 5
	vtxoAmounts := make([]btcutil.Amount, numOutputs)
	for i := range vtxoAmounts {
		// Use values that encode position so mismatches stand out:
		// position i gets amount 10_000 + i, making a positional
		// shuffle visible in the first failure message.
		vtxoAmounts[i] = btcutil.Amount(10_000 + i)
	}

	leaveAmounts := make([]btcutil.Amount, 3)
	for i := range leaveAmounts {
		leaveAmounts[i] = btcutil.Amount(500_000 + i*100)
	}

	msg := &JoinRoundQuoteOutbox{
		Client: clientconn.ClientID("alice"),
		RoundID: RoundID{
			0x01,
			0x02,
			0x03,
		},
		Quote: &Quote{
			ClientID:     clientconn.ClientID("alice"),
			SealPass:     1,
			VTXOAmounts:  vtxoAmounts,
			LeaveAmounts: leaveAmounts,
			OperatorFee:  btcutil.Amount(255),
			RejectReason: QuoteReasonOK,
		},
		QuoteExpiresAt: 1_700_000_000,
	}

	// Reconverting the outbox to proto 50 times must yield the
	// exact same wire slice order every pass — no map iteration
	// noise permitted.
	for iter := 0; iter < 50; iter++ {
		pb, ok := msg.ToProto().(*roundpb.JoinRoundQuote)
		require.True(
			t, ok, "ToProto must return *roundpb.JoinRoundQuote",
		)

		require.Len(
			t, pb.GetVtxoQuotes(), numOutputs,
			"wire slice length must equal intent position count",
		)

		// Position-for-position match against the source slice.
		for i, wireQuote := range pb.GetVtxoQuotes() {
			require.Equal(
				t, int64(vtxoAmounts[i]),
				wireQuote.GetAmountSat(),
				"iter=%d position=%d: wire amount drifted "+
					"from Quote.VTXOAmounts slice order",
				iter, i,
			)
		}

		require.Len(t, pb.GetLeaveQuotes(), len(leaveAmounts))
		for i, wireLeave := range pb.GetLeaveQuotes() {
			require.Equal(
				t, int64(leaveAmounts[i]),
				wireLeave.GetAmountSat(),
				"iter=%d position=%d: leave wire amount "+
					"drifted from slice order", iter, i,
			)
		}
	}
}

// TestJoinRoundQuoteOutboxToProtoProtoMarshalDeterministic verifies
// that the proto serialized bytes are identical across invocations
// when the Quote is identical. This is stronger than just asserting
// positional slice order: it catches any field ordering drift at
// the proto layer (e.g. if a future change introduced a second map
// somewhere in the chain). Deterministic wire encoding is required
// for durable-mailbox replay and idempotency-key derivation.
func TestJoinRoundQuoteOutboxToProtoProtoMarshalDeterministic(t *testing.T) {
	t.Parallel()

	vtxoAmounts := []btcutil.Amount{
		btcutil.Amount(11_111),
		btcutil.Amount(22_222),
		btcutil.Amount(33_333),
	}

	msg := &JoinRoundQuoteOutbox{
		Client: clientconn.ClientID("bob"),
		RoundID: RoundID{
			0xff,
			0xee,
			0xdd,
		},
		Quote: &Quote{
			ClientID:     clientconn.ClientID("bob"),
			SealPass:     2,
			VTXOAmounts:  vtxoAmounts,
			OperatorFee:  btcutil.Amount(512),
			RejectReason: QuoteReasonOK,
		},
		QuoteExpiresAt: 1_700_000_042,
	}

	var firstBytes []byte
	for iter := 0; iter < 20; iter++ {
		pb := msg.ToProto()

		bytesOut, err := proto.MarshalOptions{
			Deterministic: true,
		}.Marshal(
			pb,
		)
		require.NoError(t, err)

		if iter == 0 {
			firstBytes = bytesOut
			continue
		}

		require.Equal(
			t, firstBytes, bytesOut, "iter=%d: ToProto emitted "+
				"a different byte sequence — wire encoding "+
				"must be deterministic for durable replay",
			iter,
		)
	}
}

// TestJoinRoundQuoteOutboxToProtoRejectSkipsAmounts verifies the
// reject-quote short-circuit: when RejectReason is non-OK the
// output amounts and breakdown are intentionally omitted from the
// wire (the client reads reject_reason and stops there). Guards
// against a future regression where someone removes the
// short-circuit and sends amounts for rejected quotes, which would
// confuse the client's positional validation path.
func TestJoinRoundQuoteOutboxToProtoRejectSkipsAmounts(t *testing.T) {
	t.Parallel()

	msg := &JoinRoundQuoteOutbox{
		Client: clientconn.ClientID("eve"),
		RoundID: RoundID{
			0xaa,
		},
		Quote: &Quote{
			ClientID: clientconn.ClientID("eve"),
			SealPass: 0,
			VTXOAmounts: []btcutil.Amount{
				btcutil.Amount(42),
			},
			RejectReason: QuoteReasonInsufficientResidual,
		},
	}

	pb, ok := msg.ToProto().(*roundpb.JoinRoundQuote)
	require.True(t, ok)
	require.Empty(
		t, pb.GetVtxoQuotes(),
		"reject quotes must not carry binding amounts",
	)
	require.Empty(
		t, pb.GetLeaveQuotes(),
		"reject quotes must not carry leave amounts",
	)
	require.Nil(
		t, pb.GetBreakdown(),
		"reject quotes must not carry a fee breakdown",
	)
	require.NotEqual(t, roundpb.QuoteReason_QUOTE_OK,
		pb.GetRejectReason())
}
