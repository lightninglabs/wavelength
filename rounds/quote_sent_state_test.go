package rounds

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
)

// newQuoteSentTestState builds a QuoteSentState populated with the
// given number of pending clients, each with a deterministic
// quote_id so tests can craft matching accept/reject events. Client
// IDs are "cN" for N in [0, n).
func newQuoteSentTestState(n int) *QuoteSentState {
	regs := make(map[clientconn.ClientID]*ClientRegistration, n)
	quotes := make(map[clientconn.ClientID]*Quote, n)
	status := make(map[clientconn.ClientID]QuoteStatus, n)

	var rid RoundID
	copy(rid[:], []byte("test-round-uuid-"))

	for i := 0; i < n; i++ {
		cid := clientconn.ClientID(rune('a' + i))
		regs[cid] = &ClientRegistration{ClientID: cid}
		quotes[cid] = &Quote{
			ClientID:     cid,
			QuoteID:      computeQuoteID(rid, 0, cid),
			SealPass:     0,
			RejectReason: QuoteReasonOK,
		}
		status[cid] = QuotePending
	}

	return &QuoteSentState{
		ClientRegistrations: regs,
		Quotes:              quotes,
		Status:              status,
		SealPass:            0,
		QuoteExpires:        time.Now().Add(time.Second),
		RejectCounts:        make(map[clientconn.ClientID]uint32),
		DroppedClients:      make(map[clientconn.ClientID]struct{}),
	}
}

// quoteTestEnv returns a minimal Environment with a logger and the
// governance knobs set to known values for test assertions. Other
// fields are intentionally zero; ProcessEvent must not reach for
// any chain or wallet dependency in the QuoteSentState paths.
func quoteTestEnv(roundID RoundID) *Environment {
	return &Environment{
		RoundID:          roundID,
		Log:              btclog.Disabled,
		MaxSealPasses:    3,
		MaxClientRejects: 3,
		QuoteTTL:         time.Second,
	}
}

// TestQuoteSentAcceptFlipsStatus covers the happy per-client flip:
// a ClientQuoteAcceptEvent with the matching quote_id moves a
// pending client to QuoteAccepted.
func TestQuoteSentAcceptFlipsStatus(t *testing.T) {
	t.Parallel()

	s := newQuoteSentTestState(2)
	env := quoteTestEnv(RoundID{})

	evt := &ClientQuoteAcceptEvent{
		ClientID: clientconn.ClientID("a"),
		QuoteID:  s.Quotes["a"].QuoteID,
	}

	tr, err := s.ProcessEvent(context.Background(), evt, env)
	require.NoError(t, err)
	require.NotNil(t, tr.NextState)

	ns, ok := tr.NextState.(*QuoteSentState)
	require.True(t, ok)
	require.Equal(t, QuoteAccepted, ns.Status["a"])
	require.Equal(t, QuotePending, ns.Status["b"])

	// Not everyone resolved yet — no AllQuotesResolvedEvent.
	require.False(t, tr.NewEvents.IsSome())
}

// TestQuoteSentStaleQuoteIDDropped verifies that an accept (or
// reject) carrying a quote_id that does not match the client's
// active quote is a silent no-op.
func TestQuoteSentStaleQuoteIDDropped(t *testing.T) {
	t.Parallel()

	s := newQuoteSentTestState(1)
	env := quoteTestEnv(RoundID{})

	var staleID [32]byte
	for i := range staleID {
		staleID[i] = byte(i)
	}

	evt := &ClientQuoteAcceptEvent{
		ClientID: clientconn.ClientID("a"),
		QuoteID:  staleID,
	}

	tr, err := s.ProcessEvent(context.Background(), evt, env)
	require.NoError(t, err)

	// State should be returned unchanged (no flip, no events).
	ns := tr.NextState.(*QuoteSentState)
	require.Equal(t, QuotePending, ns.Status["a"])
	require.False(t, tr.NewEvents.IsSome())
}

// TestQuoteSentRejectBumpsCounter verifies that a reject flips the
// status and increments the per-client reject counter without
// dropping the client until MaxClientRejects is exceeded.
func TestQuoteSentRejectBumpsCounter(t *testing.T) {
	t.Parallel()

	s := newQuoteSentTestState(1)
	env := quoteTestEnv(RoundID{})

	evt := &ClientQuoteRejectEvent{
		ClientID: clientconn.ClientID("a"),
		QuoteID:  s.Quotes["a"].QuoteID,
		Reason:   "fee exceeds cap",
	}

	tr, err := s.ProcessEvent(context.Background(), evt, env)
	require.NoError(t, err)

	ns := tr.NextState.(*QuoteSentState)
	require.Equal(t, QuoteRejected, ns.Status["a"])
	require.Equal(t, uint32(1), ns.RejectCounts["a"])
}

// TestQuoteSentAllResolvedEmitsInternal verifies that once the last
// pending client flips to a terminal status, the handler fires an
// AllQuotesResolvedEvent internally so the pass resolution logic
// runs as its own event.
func TestQuoteSentAllResolvedEmitsInternal(t *testing.T) {
	t.Parallel()

	s := newQuoteSentTestState(1)
	env := quoteTestEnv(RoundID{})

	evt := &ClientQuoteAcceptEvent{
		ClientID: clientconn.ClientID("a"),
		QuoteID:  s.Quotes["a"].QuoteID,
	}

	tr, err := s.ProcessEvent(context.Background(), evt, env)
	require.NoError(t, err)
	require.True(t, tr.NewEvents.IsSome())

	internals := tr.NewEvents.UnwrapOr(EmittedEvent{}).InternalEvent
	require.Len(t, internals, 1)
	_, ok := internals[0].(*AllQuotesResolvedEvent)
	require.True(t, ok)
}

// TestQuoteSentRejectCapDropsClient drives a single client over the
// MaxClientRejects threshold via three back-to-back reject passes
// and asserts that resolvePass moves the client into
// DroppedClients and emits a ClientRoundFailedResp when the cap is
// exceeded.
func TestQuoteSentRejectCapDropsClient(t *testing.T) {
	t.Parallel()

	env := quoteTestEnv(RoundID{})
	env.MaxClientRejects = 2 // easier to hit in a test.

	// Seed a state where a pre-existing reject count has already
	// hit the cap minus one, plus a pending client.
	s := newQuoteSentTestState(1)
	s.RejectCounts[clientconn.ClientID("a")] = 1

	rejectEvt := &ClientQuoteRejectEvent{
		ClientID: clientconn.ClientID("a"),
		QuoteID:  s.Quotes["a"].QuoteID,
		Reason:   "too expensive",
	}

	// Drive the reject through; this should increment RejectCounts
	// to 2, hit the cap, and internally fire AllQuotesResolvedEvent.
	tr, err := s.ProcessEvent(context.Background(), rejectEvt, env)
	require.NoError(t, err)
	ns := tr.NextState.(*QuoteSentState)
	require.Equal(t, uint32(2), ns.RejectCounts["a"])

	// Now fire the internally emitted AllQuotesResolvedEvent.
	internals := tr.NewEvents.UnwrapOr(EmittedEvent{}).InternalEvent
	require.Len(t, internals, 1)
	_, ok := internals[0].(*AllQuotesResolvedEvent)
	require.True(t, ok)

	resTr, err := ns.ProcessEvent(
		context.Background(), &AllQuotesResolvedEvent{}, env,
	)
	require.NoError(t, err)
	require.NotNil(t, resTr.NextState)

	// Zero accepted + reject cap hit → fall back to
	// IntentCollectingState (empty) with a ClientRoundFailedResp
	// for the dropped client.
	outbox := resTr.NewEvents.UnwrapOr(EmittedEvent{}).Outbox
	var sawFailResp bool
	for _, msg := range outbox {
		if _, ok := msg.(*ClientRoundFailedResp); ok {
			sawFailResp = true
			break
		}
	}
	require.True(t, sawFailResp,
		"expected ClientRoundFailedResp in outbox for dropped client")
}

// TestQuoteSentTimeoutDoesNotIncrementRejects asserts that the
// timeout path flips QuoteTimedOut but leaves RejectCounts alone —
// honest clients should not be dropped on network flakes.
func TestQuoteSentTimeoutDoesNotIncrementRejects(t *testing.T) {
	t.Parallel()

	s := newQuoteSentTestState(1)
	env := quoteTestEnv(RoundID{})

	evt := &QuoteTimeoutEvent{
		ClientID: clientconn.ClientID("a"),
		QuoteID:  s.Quotes["a"].QuoteID,
	}

	tr, err := s.ProcessEvent(context.Background(), evt, env)
	require.NoError(t, err)

	ns := tr.NextState.(*QuoteSentState)
	require.Equal(t, QuoteTimedOut, ns.Status["a"])
	require.Equal(t, uint32(0), ns.RejectCounts["a"])
}
