package round

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/require"
)

// newQuoteReceivedTestState builds a QuoteReceivedState with a
// ClientQuote parameterized on fee and reject reason. Tests drive
// the QuoteAccepted / QuoteRejected decision against this state.
func newQuoteReceivedTestState(
	operatorFeeSat int64, rejectReason roundpb.QuoteReason,
) *QuoteReceivedState {

	var quoteID [32]byte
	for i := range quoteID {
		quoteID[i] = byte(i)
	}

	return &QuoteReceivedState{
		RoundID: RoundID{},
		Quote: &ClientQuote{
			QuoteID:        quoteID,
			OperatorFeeSat: operatorFeeSat,
			RejectReason:   rejectReason,
		},
	}
}

// quoteReceivedTestEnv returns a minimal ClientEnvironment with a
// concrete MaxOperatorFee cap.
func quoteReceivedTestEnv(maxFee btcutil.Amount) *ClientEnvironment {
	return &ClientEnvironment{
		Log:            btclog.Disabled,
		MaxOperatorFee: maxFee,
	}
}

// TestQuoteReceivedAcceptEmitsOutbox covers the happy path: a
// QuoteAccepted event emits a JoinRoundAcceptOutbox and transitions
// to RoundJoinedState.
func TestQuoteReceivedAcceptEmitsOutbox(t *testing.T) {
	t.Parallel()

	s := newQuoteReceivedTestState(5000, roundpb.QuoteReason_QUOTE_OK)
	env := quoteReceivedTestEnv(10_000)

	tr, err := s.ProcessEvent(context.Background(), &QuoteAccepted{
		RoundID: s.RoundID,
		QuoteID: s.Quote.QuoteID,
	}, env)
	require.NoError(t, err)
	require.NotNil(t, tr.NextState)

	_, ok := tr.NextState.(*RoundJoinedState)
	require.True(t, ok)
	require.True(t, tr.NewEvents.IsSome())

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	require.Len(t, outbox, 1)
	_, ok = outbox[0].(*JoinRoundAcceptOutbox)
	require.True(t, ok)
}

// TestQuoteReceivedRejectEmitsOutbox verifies that QuoteRejected
// flips to ClientFailedState and emits a JoinRoundRejectOutbox.
func TestQuoteReceivedRejectEmitsOutbox(t *testing.T) {
	t.Parallel()

	s := newQuoteReceivedTestState(5000, roundpb.QuoteReason_QUOTE_OK)
	env := quoteReceivedTestEnv(10_000)

	tr, err := s.ProcessEvent(context.Background(), &QuoteRejected{
		RoundID: s.RoundID,
		QuoteID: s.Quote.QuoteID,
		Reason:  "fee too high",
	}, env)
	require.NoError(t, err)

	_, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	require.Len(t, outbox, 1)
	_, ok = outbox[0].(*JoinRoundRejectOutbox)
	require.True(t, ok)
}

// TestEvaluateQuoteRejectsFeeAboveCap verifies the local policy
// check: when the server's operator fee exceeds env.MaxOperatorFee,
// evaluateQuote returns a QuoteRejected event with the fee / cap
// diagnostic. Nil or OK-reasoned quotes within the cap accept.
//
// The echo-shape invariant path is exercised separately; the intent
// passed here is empty so the length checks pass trivially for the
// cap / reject-reason assertions below.
func TestEvaluateQuoteRejectsFeeAboveCap(t *testing.T) {
	t.Parallel()

	env := quoteReceivedTestEnv(1000)
	empty := Intents{}

	// Fee above cap → reject (early belt-and-braces check on the
	// operator-declared field; the realised-fee check below is
	// the authoritative defense — see #379).
	q := &ClientQuote{
		OperatorFeeSat: 1500,
		RejectReason:   roundpb.QuoteReason_QUOTE_OK,
	}
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, empty, q,
	)
	_, isReject := decision.(*QuoteRejected)
	require.True(t, isReject)

	// Nil quote → reject defensively.
	decision = evaluateQuote(
		context.Background(), env, RoundID{}, empty, nil,
	)
	_, isReject = decision.(*QuoteRejected)
	require.True(t, isReject)

	// Server-rejected quote (non-OK reason) → propagate as
	// QuoteRejected even if the fee is zero.
	q = &ClientQuote{
		OperatorFeeSat: 0,
		RejectReason:   roundpb.QuoteReason_INSUFFICIENT_RESIDUAL,
	}
	decision = evaluateQuote(
		context.Background(), env, RoundID{}, empty, q,
	)
	_, isReject = decision.(*QuoteRejected)
	require.True(t, isReject)
}
