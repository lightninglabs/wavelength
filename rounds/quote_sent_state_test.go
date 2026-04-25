package rounds

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
)

// noopBoardingLocker is a BoardingInputLocker stub used by
// reseal-path tests. releaseResolvedNonAcceptors unlocks boarding
// inputs when clients are dropped; the production locker touches
// shared state that is irrelevant to the FSM decision logic we are
// exercising here, so the stub returns nil for every call.
type noopBoardingLocker struct{}

func (noopBoardingLocker) Lock(context.Context, *wire.OutPoint,
	RoundID) error {

	return nil
}

func (noopBoardingLocker) Unlock(context.Context, *wire.OutPoint,
	RoundID) error {

	return nil
}

func (noopBoardingLocker) IsLocked(context.Context,
	*wire.OutPoint) (bool, RoundID, error) {

	return false, RoundID{}, nil
}

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

// quoteResealEnv extends quoteTestEnv with the FeeCalculator + Terms
// combo required for sealRoundWithQuotes to run cleanly on the
// reseal path. Reseal tests drive a mixed accept / reject resolution
// that pushes the FSM through sealRoundWithQuotes with the surviving
// accepted set; this helper wires the dependencies that path needs.
// maxPasses overrides MaxSealPasses so reseal-cap tests can dial the
// cap down to 1 without needing to thread rejects through three
// synthetic passes.
func quoteResealEnv(t *testing.T, roundID RoundID,
	maxPasses uint32) *Environment {

	t.Helper()

	env := quoteTestEnv(roundID)
	env.MaxSealPasses = maxPasses
	env.FeeCalculator = newTestBuilderCalc(t)
	env.Terms = &batch.Terms{
		ConnectorDustAmount: btcutil.Amount(330),
	}
	env.StartHeight = 100
	env.BoardingInputLocker = noopBoardingLocker{}

	return env
}

// newQuoteSentResealableState seeds a QuoteSentState where every
// client carries the intent material (boarding input + change-marked
// VTXO request) needed for sealRoundWithQuotes to re-derive a quote
// during the reseal path. Client IDs are "cN" for N in [0, n). The
// boarding input value is 50_000 sats and the VTXO request is
// marked IsChange=true so the builder's change-designation check
// passes on every reseal pass.
func newQuoteSentResealableState(t *testing.T, n int) *QuoteSentState {
	t.Helper()

	s := newQuoteSentTestState(n)
	const boardingValue = btcutil.Amount(50_000)

	for cid, reg := range s.ClientRegistrations {
		reg.BoardingInputs = []*BoardingInput{
			newTestBoardingInput(t, boardingValue),
		}
		reg.IntentVTXOReqs = []*types.VTXORequest{
			newTestVTXORequest(t, boardingValue, true),
		}
		s.ClientRegistrations[cid] = reg
	}

	return s
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
	ns, ok := tr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected *QuoteSentState, got %T", tr.NextState)
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

	ns, ok := tr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected *QuoteSentState, got %T", tr.NextState)
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
	ns, ok := tr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected *QuoteSentState, got %T", tr.NextState)
	require.Equal(t, uint32(2), ns.RejectCounts["a"])

	// Now fire the internally emitted AllQuotesResolvedEvent.
	internals := tr.NewEvents.UnwrapOr(EmittedEvent{}).InternalEvent
	require.Len(t, internals, 1)
	_, isAllResolved := internals[0].(*AllQuotesResolvedEvent)
	require.True(t, isAllResolved)

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

	ns, ok := tr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected *QuoteSentState, got %T", tr.NextState)
	require.Equal(t, QuoteTimedOut, ns.Status["a"])
	require.Equal(t, uint32(0), ns.RejectCounts["a"])
}

// driveAllQuotesResolved fires AllQuotesResolvedEvent against state s
// in the supplied Environment and returns the resolution transition.
// Centralized so reseal tests do not duplicate the same three lines
// of plumbing.
func driveAllQuotesResolved(t *testing.T, s *QuoteSentState,
	env *Environment) *StateTransition {

	t.Helper()

	tr, err := s.ProcessEvent(
		context.Background(), &AllQuotesResolvedEvent{}, env,
	)
	require.NoError(t, err)
	require.NotNil(t, tr)

	return tr
}

// TestQuoteSentRejectTriggersReseal drives one client to accept and
// another to reject, then fires AllQuotesResolvedEvent. With nextPass
// below the reseal cap and at least one accepted client, the FSM
// should reseal over the surviving accepted set, producing a new
// QuoteSentState with SealPass=1, the rejecter dropped, and a fresh
// quote for the surviving client.
func TestQuoteSentRejectTriggersReseal(t *testing.T) {
	t.Parallel()

	env := quoteResealEnv(t, RoundID{}, 3)
	s := newQuoteSentResealableState(t, 2)

	// Client "a" accepts, "b" rejects.
	acceptEvt := &ClientQuoteAcceptEvent{
		ClientID: clientconn.ClientID("a"),
		QuoteID:  s.Quotes["a"].QuoteID,
	}
	acceptTr, err := s.ProcessEvent(
		context.Background(), acceptEvt, env,
	)
	require.NoError(t, err)
	afterAccept, ok := acceptTr.NextState.(*QuoteSentState)
	require.True(
		t, ok, "expected *QuoteSentState, got %T", acceptTr.NextState,
	)

	rejectEvt := &ClientQuoteRejectEvent{
		ClientID: clientconn.ClientID("b"),
		QuoteID:  afterAccept.Quotes["b"].QuoteID,
		Reason:   "fee too high",
	}
	rejectTr, err := afterAccept.ProcessEvent(
		context.Background(), rejectEvt, env,
	)
	require.NoError(t, err)
	afterReject, ok := rejectTr.NextState.(*QuoteSentState)
	require.True(
		t, ok, "expected *QuoteSentState, got %T", rejectTr.NextState,
	)

	// Second reject should have emitted AllQuotesResolvedEvent
	// internally — drive it.
	internals := rejectTr.NewEvents.UnwrapOr(EmittedEvent{}).InternalEvent
	require.Len(t, internals, 1)
	_, ok = internals[0].(*AllQuotesResolvedEvent)
	require.True(t, ok)

	resTr := driveAllQuotesResolved(t, afterReject, env)

	// Expected: a new QuoteSentState at pass 1, with only "a" still
	// a survivor.
	resealed, ok := resTr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected QuoteSentState after reseal, got %T",
		resTr.NextState)
	require.Equal(t, uint32(1), resealed.SealPass)
	require.Contains(
		t, resealed.ClientRegistrations, clientconn.ClientID("a"),
	)
	require.NotContains(t,
		resealed.ClientRegistrations, clientconn.ClientID("b"),
	)
	require.Equal(
		t, QuotePending, resealed.Status[clientconn.ClientID("a")],
	)

	// The quote for "a" on the reseal pass should carry a different
	// QuoteID than the first pass — the quote_id is bound to
	// sealPass, so a stale ack from the prior pass is rejected.
	require.NotEqual(
		t, afterReject.Quotes["a"].QuoteID,
		resealed.Quotes["a"].QuoteID,
	)
}

// TestQuoteSentTimeoutTriggersReseal mirrors the reject path but uses
// QuoteTimeoutEvent on the non-accepting client. Timeouts are reseal-
// eligible and must not count against the per-client reject cap.
func TestQuoteSentTimeoutTriggersReseal(t *testing.T) {
	t.Parallel()

	env := quoteResealEnv(t, RoundID{}, 3)
	s := newQuoteSentResealableState(t, 2)

	// Client "a" accepts.
	acceptTr, err := s.ProcessEvent(
		context.Background(), &ClientQuoteAcceptEvent{
			ClientID: clientconn.ClientID("a"),
			QuoteID:  s.Quotes["a"].QuoteID,
		}, env,
	)
	require.NoError(t, err)
	afterAccept, ok := acceptTr.NextState.(*QuoteSentState)
	require.True(
		t, ok, "expected *QuoteSentState, got %T", acceptTr.NextState,
	)

	// Client "b" times out.
	timeoutTr, err := afterAccept.ProcessEvent(
		context.Background(), &QuoteTimeoutEvent{
			ClientID: clientconn.ClientID("b"),
			QuoteID:  afterAccept.Quotes["b"].QuoteID,
		}, env,
	)
	require.NoError(t, err)
	afterTimeout, ok := timeoutTr.NextState.(*QuoteSentState)
	require.True(
		t, ok, "expected *QuoteSentState, got %T", timeoutTr.NextState,
	)

	// RejectCounts for b must still be zero — timeouts do not
	// incriminate the client.
	require.Equal(t,
		uint32(0), afterTimeout.RejectCounts[clientconn.ClientID("b")],
	)

	resTr := driveAllQuotesResolved(t, afterTimeout, env)
	resealed, ok := resTr.NextState.(*QuoteSentState)
	require.True(t, ok)
	require.Equal(t, uint32(1), resealed.SealPass)
	require.Contains(
		t, resealed.ClientRegistrations, clientconn.ClientID("a"),
	)
	require.NotContains(t,
		resealed.ClientRegistrations, clientconn.ClientID("b"),
	)
}

// TestQuoteSentMixedResolutionReseals drives a three-client round
// through every resolution shape in a single pass: accept, reject,
// timeout. With the accept surviving and two drops forcing a reseal,
// the FSM should emit ClientRoundFailedResp for both drops and hand
// a fresh quote to the survivor on pass 1.
func TestQuoteSentMixedResolutionReseals(t *testing.T) {
	t.Parallel()

	env := quoteResealEnv(t, RoundID{}, 3)
	s := newQuoteSentResealableState(t, 3)

	// Client "a" accepts.
	tr, err := s.ProcessEvent(
		context.Background(), &ClientQuoteAcceptEvent{
			ClientID: clientconn.ClientID("a"),
			QuoteID:  s.Quotes["a"].QuoteID,
		}, env,
	)
	require.NoError(t, err)
	state, ok := tr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected *QuoteSentState, got %T", tr.NextState)

	// Client "b" rejects.
	tr, err = state.ProcessEvent(
		context.Background(), &ClientQuoteRejectEvent{
			ClientID: clientconn.ClientID("b"),
			QuoteID:  state.Quotes["b"].QuoteID,
			Reason:   "fee too high",
		}, env,
	)
	require.NoError(t, err)
	state, ok = tr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected *QuoteSentState, got %T", tr.NextState)

	// Client "c" times out.
	tr, err = state.ProcessEvent(
		context.Background(), &QuoteTimeoutEvent{
			ClientID: clientconn.ClientID("c"),
			QuoteID:  state.Quotes["c"].QuoteID,
		}, env,
	)
	require.NoError(t, err)
	state, ok = tr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected *QuoteSentState, got %T", tr.NextState)

	// Confirm terminal statuses before firing the resolution.
	require.Equal(t, QuoteAccepted, state.Status[clientconn.ClientID("a")])
	require.Equal(t, QuoteRejected, state.Status[clientconn.ClientID("b")])
	require.Equal(t, QuoteTimedOut, state.Status[clientconn.ClientID("c")])

	resTr := driveAllQuotesResolved(t, state, env)
	resealed, ok := resTr.NextState.(*QuoteSentState)
	require.True(t, ok)
	require.Equal(t, uint32(1), resealed.SealPass)
	require.Len(t, resealed.ClientRegistrations, 1)
	require.Contains(
		t, resealed.ClientRegistrations, clientconn.ClientID("a"),
	)

	// Sub-cap reject counts don't emit a fail-resp (the client
	// already sent its reject and can re-engage on the reseal),
	// but timeouts DO emit a fail-resp so the client FSM doesn't
	// sit in RoundJoined awaiting a commitment tx that will never
	// arrive. Expect one fail-resp addressed to "c" (the timeout)
	// and exactly one fresh quote addressed to the surviving
	// accepter "a".
	outbox := resTr.NewEvents.UnwrapOr(EmittedEvent{}).Outbox
	var (
		failResps []*ClientRoundFailedResp
		quoteSent []*JoinRoundQuoteOutbox
	)
	for _, msg := range outbox {
		switch v := msg.(type) {
		case *ClientRoundFailedResp:
			failResps = append(failResps, v)
		case *JoinRoundQuoteOutbox:
			quoteSent = append(quoteSent, v)
		}
	}
	require.Len(t, failResps, 1,
		"quote timeout must surface to the client as "+
			"ClientRoundFailedResp so its FSM unwinds")
	require.Equal(t, clientconn.ClientID("c"), failResps[0].Client)
	require.Contains(t, failResps[0].Reason, "timeout",
		"timeout fail-resp must carry a timeout-shaped reason")
	require.Len(t, quoteSent, 1,
		"exactly one fresh quote should be fanned out to the "+
			"surviving accepter on the reseal pass")
	require.Equal(t, clientconn.ClientID("a"), quoteSent[0].Client)
}

// TestQuoteSentResealCapFinalizes dials MaxSealPasses down to 1 so
// that the very first reject-triggered reseal would cross the cap.
// With nextPass >= cap, the FSM must finalize with the accepted set
// by transitioning to BatchBuildingState instead of calling
// sealRoundWithQuotes again.
func TestQuoteSentResealCapFinalizes(t *testing.T) {
	t.Parallel()

	// cap=1 means nextPass=1 is exactly the cap — finalize path.
	env := quoteResealEnv(t, RoundID{}, 1)
	s := newQuoteSentResealableState(t, 2)

	// Client "a" accepts.
	tr, err := s.ProcessEvent(
		context.Background(), &ClientQuoteAcceptEvent{
			ClientID: clientconn.ClientID("a"),
			QuoteID:  s.Quotes["a"].QuoteID,
		}, env,
	)
	require.NoError(t, err)
	state, ok := tr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected *QuoteSentState, got %T", tr.NextState)

	// Client "b" rejects — would trigger a reseal under a normal
	// cap, but we configured cap=1 so the FSM must finalize instead.
	tr, err = state.ProcessEvent(
		context.Background(), &ClientQuoteRejectEvent{
			ClientID: clientconn.ClientID("b"),
			QuoteID:  state.Quotes["b"].QuoteID,
			Reason:   "still too expensive",
		}, env,
	)
	require.NoError(t, err)
	state, ok = tr.NextState.(*QuoteSentState)
	require.True(t, ok, "expected *QuoteSentState, got %T", tr.NextState)

	resTr := driveAllQuotesResolved(t, state, env)

	// Cap reached — finalize with accepted set.
	batch, ok := resTr.NextState.(*BatchBuildingState)
	require.True(t, ok,
		"expected BatchBuildingState after cap-reached finalize, "+
			"got %T",
		resTr.NextState)
	require.Len(t, batch.ClientRegistrations, 1)
	require.Contains(t,
		batch.ClientRegistrations, clientconn.ClientID("a"),
	)

	// BuildBatchTxEvent must be fired internally so the newly
	// entered BatchBuildingState can drive PSBT construction.
	internals := resTr.NewEvents.UnwrapOr(EmittedEvent{}).InternalEvent
	require.Len(t, internals, 1)
	_, ok = internals[0].(*BuildBatchTxEvent)
	require.True(t, ok)
}

// TestSealRoundWithQuotesAllRejectedFails covers the codex P2
// regression: when every client's intent fails admission at seal
// time, sealRoundWithQuotes must NOT emit RoundSealedReq (which
// would orphan the now-empty round in the actor's routing map) and
// MUST instead emit a RoundFailedReq + transition to FailedState
// so the actor cleans up the round and spawns a fresh one for any
// late-arriving registrations.
func TestSealRoundWithQuotesAllRejectedFails(t *testing.T) {
	t.Parallel()

	var rid RoundID
	copy(rid[:], []byte("seal-empty-test-"))

	env := quoteResealEnv(t, rid, 3)

	// Two clients, both with invalid change designation (zero
	// IsChange markers across a multi-output intent). The seal-time
	// builder will return both with QuoteReasonInvalidChangeDesignation
	// and sealRoundWithQuotes will drop both → zero survivors.
	const input = btcutil.Amount(50_000)
	regs := map[clientconn.ClientID]*ClientRegistration{
		"c1": {
			ClientID: "c1",
			BoardingInputs: []*BoardingInput{
				newTestBoardingInput(t, input),
			},
			IntentVTXOReqs: []*types.VTXORequest{
				newTestVTXORequest(t, input/2, false),
				newTestVTXORequest(t, input/2, false),
			},
		},
		"c2": {
			ClientID: "c2",
			BoardingInputs: []*BoardingInput{
				newTestBoardingInput(t, input),
			},
			IntentVTXOReqs: []*types.VTXORequest{
				newTestVTXORequest(t, input/2, false),
				newTestVTXORequest(t, input/2, false),
			},
		},
	}

	tr, err := sealRoundWithQuotes(t.Context(), env, regs, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)

	// FailedState (terminal) is the next state, NOT
	// IntentCollectingState. The pre-fix code parked the round in
	// IntentCollectingState (empty), which the actor never cleans
	// up — the orphan we are guarding against.
	failed, ok := tr.NextState.(*FailedState)
	require.Truef(
		t, ok, "next state should be FailedState, got %T",
		tr.NextState,
	)
	require.True(t, failed.IsTerminal())

	outbox := tr.NewEvents.UnwrapOr(EmittedEvent{}).Outbox

	// Sanity: per-client drop notifications were emitted for both
	// clients dropped at seal time.
	var dropResps int
	for _, msg := range outbox {
		if _, ok := msg.(*ClientRoundFailedResp); ok {
			dropResps++
		}
	}
	require.Equal(t, 2, dropResps)

	// The fix: NO RoundSealedReq. Emitting it on the empty-survivor
	// path is what created the orphan round in actor memory.
	for _, msg := range outbox {
		_, isSealed := msg.(*RoundSealedReq)
		require.Falsef(
			t, isSealed,
			"RoundSealedReq must not be emitted when zero "+
				"clients survive seal-time quoting",
		)
	}

	// The fix: a RoundFailedReq IS emitted. The actor's
	// RoundFailedReq handler in actor.go untracks the round and
	// spawns a replacement, restoring the steady state that the
	// pre-fix flow leaked.
	var failedReqs int
	for _, msg := range outbox {
		if r, ok := msg.(*RoundFailedReq); ok {
			require.Equal(t, rid, r.FailedRoundID)
			require.Contains(t, r.Reason, "seal time")
			failedReqs++
		}
	}
	require.Equal(t, 1, failedReqs)
}

// TestSealRoundWithQuotesSurvivorEmitsSealed verifies the positive
// case after the P2 fix: when at least one client survives quoting,
// RoundSealedReq is still emitted on pass 0 so the actor spawns a
// fresh round for late-arriving registrations. The deferral only
// affects the empty-survivor path; the steady-state flow is
// unchanged.
func TestSealRoundWithQuotesSurvivorEmitsSealed(t *testing.T) {
	t.Parallel()

	var rid RoundID
	copy(rid[:], []byte("seal-okay-test--"))

	env := quoteResealEnv(t, rid, 3)
	env.RoundID = rid

	// One healthy client (well-formed implicit-change intent) plus
	// one invalid-designation client. The healthy client survives,
	// so RoundSealedReq must fire; the invalid client gets a drop
	// notification.
	healthyInput := btcutil.Amount(50_000)
	regs := map[clientconn.ClientID]*ClientRegistration{
		"healthy": {
			ClientID: "healthy",
			BoardingInputs: []*BoardingInput{
				newTestBoardingInput(t, healthyInput),
			},
			IntentVTXOReqs: []*types.VTXORequest{
				newTestVTXORequest(t, healthyInput, false),
			},
		},
		"bad": {
			ClientID: "bad",
			BoardingInputs: []*BoardingInput{
				newTestBoardingInput(t, healthyInput),
			},
			IntentVTXOReqs: []*types.VTXORequest{
				newTestVTXORequest(t, healthyInput/2, false),
				newTestVTXORequest(t, healthyInput/2, false),
			},
		},
	}

	tr, err := sealRoundWithQuotes(t.Context(), env, regs, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)

	// Survivor present → land in QuoteSentState.
	_, ok := tr.NextState.(*QuoteSentState)
	require.Truef(
		t, ok, "next state should be QuoteSentState, got %T",
		tr.NextState,
	)

	outbox := tr.NewEvents.UnwrapOr(EmittedEvent{}).Outbox

	// RoundSealedReq fires for the actor to spawn a new round.
	var sealedReqs int
	for _, msg := range outbox {
		if r, ok := msg.(*RoundSealedReq); ok {
			require.Equal(t, rid, r.SealedRoundID)
			sealedReqs++
		}
	}
	require.Equal(t, 1, sealedReqs)

	// And no RoundFailedReq, since at least one client survived.
	for _, msg := range outbox {
		_, isFailed := msg.(*RoundFailedReq)
		require.Falsef(
			t, isFailed,
			"RoundFailedReq must not be emitted when a "+
				"client survives seal-time quoting",
		)
	}
}

// TestSealRoundWithQuotesResealNoSealedReq verifies that reseals
// (sealPass > 0) never emit RoundSealedReq even with survivors —
// the original round is already known to the actor and needs no
// replacement spawn.
func TestSealRoundWithQuotesResealNoSealedReq(t *testing.T) {
	t.Parallel()

	var rid RoundID
	copy(rid[:], []byte("seal-reseal-test"))

	env := quoteResealEnv(t, rid, 3)
	env.RoundID = rid

	healthyInput := btcutil.Amount(50_000)
	regs := map[clientconn.ClientID]*ClientRegistration{
		"healthy": {
			ClientID: "healthy",
			BoardingInputs: []*BoardingInput{
				newTestBoardingInput(t, healthyInput),
			},
			IntentVTXOReqs: []*types.VTXORequest{
				newTestVTXORequest(t, healthyInput, false),
			},
		},
	}

	// Pass 1 (a reseal) should NOT emit RoundSealedReq.
	tr, err := sealRoundWithQuotes(t.Context(), env, regs, 1, nil)
	require.NoError(t, err)

	outbox := tr.NewEvents.UnwrapOr(EmittedEvent{}).Outbox
	for _, msg := range outbox {
		_, isSealed := msg.(*RoundSealedReq)
		require.Falsef(
			t, isSealed,
			"reseal pass must not emit RoundSealedReq",
		)
	}
}
