package round

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// buildEchoTestIntents returns a deterministic intent pairing for
// echo-validation tests: one non-change recipient VTXO, one change
// VTXO, and one non-change leave. Target amounts are fixed so the
// adversarial mutations below can pick distinct values and still
// highlight the rejection path. The returned operatorPub drives
// EffectivePkScript derivation so the quote's echoed PkScript can
// be computed identically from the intent.
func buildEchoTestIntents(t *testing.T) (Intents, *btcec.PublicKey) {
	t.Helper()

	opPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	op := opPriv.PubKey()

	reqA := mkReq(t, op, 0x10, true) // Non-change recipient.
	reqA.req.Amount = 40_000
	reqA.req.IsChange = false

	reqB := mkReq(t, op, 0x20, true) // Change output.
	reqB.req.Amount = 60_000
	reqB.req.IsChange = true

	leavePkScript := []byte{0x00, 0x14, 0xAB, 0xCD, 0x01}
	leave := &types.LeaveRequest{
		Output: &wire.TxOut{
			PkScript: leavePkScript,
			Value:    25_000,
		},
		IsChange: false,
	}

	return Intents{
		VTXOs: []types.VTXORequest{
			reqA.req,
			reqB.req,
		},
		Leaves: []*types.LeaveRequest{
			leave,
		},
	}, op
}

// quoteFromIntents builds a faithfully-echoed ClientQuote from the
// supplied intents. Callers then mutate the returned quote to
// simulate adversarial server behavior for rejection assertions.
func quoteFromIntents(t *testing.T, intents Intents,
	operatorFeeSat int64) *ClientQuote {

	t.Helper()

	vtxoQuotes := make([]VTXOQuoteEntry, len(intents.VTXOs))
	for i := range intents.VTXOs {
		req := intents.VTXOs[i]
		script, err := req.EffectivePkScript()
		require.NoError(t, err)

		recipientKey := req.SigningKey.PubKey.SerializeCompressed()
		vtxoQuotes[i] = VTXOQuoteEntry{
			PkScript:     script,
			AmountSat:    int64(req.Amount),
			RecipientKey: recipientKey,
		}
	}

	leaveQuotes := make([]LeaveQuoteEntry, len(intents.Leaves))
	for i, l := range intents.Leaves {
		leaveQuotes[i] = LeaveQuoteEntry{
			PkScript:  l.Output.PkScript,
			AmountSat: l.Output.Value,
		}
	}

	var quoteID [32]byte
	for i := range quoteID {
		quoteID[i] = byte(i + 1)
	}

	return &ClientQuote{
		QuoteID:        quoteID,
		OperatorFeeSat: operatorFeeSat,
		VTXOQuotes:     vtxoQuotes,
		LeaveQuotes:    leaveQuotes,
	}
}

// TestEvaluateQuoteEchoAcceptsFaithfulQuote covers the happy path:
// a quote whose echoes match the intent verbatim accepts.
func TestEvaluateQuoteEchoAcceptsFaithfulQuote(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)
	env := quoteReceivedTestEnv(10_000)

	decision := evaluateQuote(env, RoundID{}, intents, quote)
	_, ok := decision.(*QuoteAccepted)
	require.True(t, ok, "faithful echo should accept")
}

// TestEvaluateQuoteEchoAcceptsChangeDeviation verifies that amount
// deviation is permitted for the single IsChange=true VTXO output —
// the residual sink is server-decided by design.
func TestEvaluateQuoteEchoAcceptsChangeDeviation(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	// Change entry is intents.VTXOs[1]; server chooses a
	// different residual. Must still accept.
	quote.VTXOQuotes[1].AmountSat = 42_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	_, ok := decision.(*QuoteAccepted)
	require.True(t, ok, "change-output deviation must be permitted")
}

// TestEvaluateQuoteEchoRejectsVTXOLengthMismatch verifies that a
// truncated VTXOQuotes slice (server sends fewer entries than the
// intent) is rejected. Without this the old positional-fallback
// path let the server mask a missing change-leaf.
func TestEvaluateQuoteEchoRejectsVTXOLengthMismatch(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)
	quote.VTXOQuotes = quote.VTXOQuotes[:1]

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rej.Reason, "vtxo entries")
}

// TestEvaluateQuoteEchoRejectsLeaveLengthMismatch verifies that a
// truncated LeaveQuotes slice is rejected.
func TestEvaluateQuoteEchoRejectsLeaveLengthMismatch(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)
	quote.LeaveQuotes = nil

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rej.Reason, "leave entries")
}

// TestEvaluateQuoteEchoRejectsVTXOPkScriptMismatch verifies that
// altering the echoed pkScript for any VTXO entry rejects.
func TestEvaluateQuoteEchoRejectsVTXOPkScriptMismatch(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	quote.VTXOQuotes[0].PkScript = append(
		[]byte(nil), quote.VTXOQuotes[0].PkScript...,
	)
	quote.VTXOQuotes[0].PkScript[0] ^= 0xFF

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rej.Reason, "pkScript echo mismatch")
}

// TestEvaluateQuoteEchoRejectsRecipientKeyMismatch verifies that
// altering the echoed MuSig2 recipient key rejects.
func TestEvaluateQuoteEchoRejectsRecipientKeyMismatch(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	quote.VTXOQuotes[0].RecipientKey = append(
		[]byte(nil), quote.VTXOQuotes[0].RecipientKey...,
	)
	quote.VTXOQuotes[0].RecipientKey[1] ^= 0xFF

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rej.Reason, "recipient key echo mismatch")
}

// TestEvaluateQuoteEchoRejectsNonChangeVTXOAmountDrift verifies
// that a deviation on a non-change VTXO amount rejects. This is
// the primary defense against fee-shifting onto a recipient.
func TestEvaluateQuoteEchoRejectsNonChangeVTXOAmountDrift(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	// intents.VTXOs[0] is the non-change recipient. Server
	// shaves it down, rebalancing into the change output to
	// keep total fee ≤ cap. Must reject.
	quote.VTXOQuotes[0].AmountSat = 35_000
	quote.VTXOQuotes[1].AmountSat = 65_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rej.Reason, "non-change amount")
}

// TestEvaluateQuoteEchoRejectsNonChangeLeaveAmountDrift verifies
// the same rule for LeaveRequest outputs.
func TestEvaluateQuoteEchoRejectsNonChangeLeaveAmountDrift(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	quote.LeaveQuotes[0].AmountSat = 24_999

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rej.Reason, "leave")
	require.Contains(t, rej.Reason, "non-change amount")
}

// TestFromProtoRejectsUnknownRejectReason verifies the decoder
// refuses a JoinRoundQuote whose reject_reason sits outside the
// proto enum. Previously the narrowing uint32(...) cast let a
// hostile server inject arbitrary numeric noise into the
// user-visible rejection text; now it fails at decode time.
func TestFromProtoRejectsUnknownRejectReason(t *testing.T) {
	t.Parallel()

	roundID := testRoundIDTr("round-unknown-reject")

	var quoteID [32]byte
	for i := range quoteID {
		quoteID[i] = byte(i + 1)
	}

	pb := &roundpb.JoinRoundQuote{
		RoundId:      roundID.String(),
		QuoteId:      quoteID[:],
		RejectReason: roundpb.QuoteReason(9999),
	}

	var got JoinRoundQuoteReceived
	err := got.FromProto(pb)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reject_reason")
}

// TestEvaluateQuoteRendersActionableRejectReason verifies error rendering on
// server-rejected quotes preserves the named enum while explaining the
// operator action needed to recover.
func TestEvaluateQuoteRendersActionableRejectReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		reason   roundpb.QuoteReason
		contains []string
	}{{
		name:   "insufficient residual",
		reason: roundpb.QuoteReason_INSUFFICIENT_RESIDUAL,
		contains: []string{
			"INSUFFICIENT_RESIDUAL",
			"not enough value remains",
			"use a larger input",
			"reduce fixed outputs",
		},
	}, {
		name:   "invalid change designation",
		reason: roundpb.QuoteReason_INVALID_CHANGE_DESIGNATION,
		contains: []string{
			"INVALID_CHANGE_DESIGNATION",
			"exactly one change output",
			"should be reported",
		},
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			intents, _ := buildEchoTestIntents(t)
			quote := quoteFromIntents(t, intents, 5_000)
			quote.RejectReason = test.reason

			env := quoteReceivedTestEnv(10_000)
			decision := evaluateQuote(
				env, RoundID{}, intents, quote,
			)
			rej, ok := decision.(*QuoteRejected)
			require.True(t, ok)
			for _, fragment := range test.contains {
				require.Contains(t, rej.Reason, fragment)
			}
		})
	}

	// Sanity: a proto marshal+unmarshal cycle preserves the
	// enum name round-trip by keeping the value.
	pb := &roundpb.JoinRoundQuote{
		RejectReason: roundpb.QuoteReason_INSUFFICIENT_RESIDUAL,
	}
	raw, err := proto.Marshal(pb)
	require.NoError(t, err)
	var decoded roundpb.JoinRoundQuote
	require.NoError(t, proto.Unmarshal(raw, &decoded))
	require.Equal(
		t, roundpb.QuoteReason_INSUFFICIENT_RESIDUAL,
		decoded.GetRejectReason(),
	)
}

// TestEvaluateQuoteRejectsExpiredQuote verifies that a quote whose
// `quote_expires_at` has already passed local time is rejected
// without signing. The spec (#270 "Quote expiry races") requires
// the client to handle this locally so a late delivery does not
// strand the FSM in RoundJoinedState waiting for a commitment tx
// the server has already abandoned.
func TestEvaluateQuoteRejectsExpiredQuote(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	// Inject a frozen clock five seconds after the quote expiry.
	expiry := time.Unix(1_700_000_000, 0)
	quote.QuoteExpiresAt = expiry.Unix()

	env := quoteReceivedTestEnv(10_000)
	env.Now = func() time.Time {
		return expiry.Add(5 * time.Second)
	}

	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "expired quote must reject")
	require.Contains(t, rej.Reason, "expired")
}

// TestEvaluateQuoteAcceptsFreshQuoteBeforeExpiry verifies the
// positive path: a quote with an expiry still in the future is
// accepted (when all other invariants hold).
func TestEvaluateQuoteAcceptsFreshQuoteBeforeExpiry(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	now := time.Unix(1_700_000_000, 0)
	quote.QuoteExpiresAt = now.Add(10 * time.Second).Unix()

	env := quoteReceivedTestEnv(10_000)
	env.Now = func() time.Time { return now }

	decision := evaluateQuote(env, RoundID{}, intents, quote)
	_, ok := decision.(*QuoteAccepted)
	require.True(t, ok)
}

// TestQuoteReceivedReplacesOnReseal verifies the reseal path: a
// fresh JoinRoundQuoteReceived with a higher seal_pass_number
// replaces the stored quote and re-evaluates, rather than being
// silently dropped. The previously-visible failure was that a
// reseal would leave the FSM stuck on the stale quote; the server
// drops the eventual accept (quote_id binding), and no fresh
// commitment ever arrives.
func TestQuoteReceivedReplacesOnReseal(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)

	// First pass: well-formed quote.
	first := quoteFromIntents(t, intents, 5_000)
	first.SealPass = 0

	s := &QuoteReceivedState{
		RoundID: RoundID{},
		Quote:   first,
		Intents: intents,
	}

	env := quoteReceivedTestEnv(10_000)

	// Second pass: higher seal_pass with a different change
	// amount the server chose under updated chain state.
	second := quoteFromIntents(t, intents, 6_000)
	second.SealPass = 1
	second.VTXOQuotes[1].AmountSat = 58_000 // Change leg shifted.

	tr, err := s.ProcessEvent(
		context.Background(),
		&JoinRoundQuoteReceived{
			RoundID: RoundID{},
			Quote:   second,
		},
		env,
	)
	require.NoError(t, err)

	// FSM must land on a fresh QuoteReceivedState whose quote is
	// the new pass, and emit an internal QuoteAccepted event.
	next, ok := tr.NextState.(*QuoteReceivedState)
	require.True(t, ok, "reseal must stay in QuoteReceivedState")
	require.Equal(t, uint32(1), next.Quote.SealPass)

	require.True(t, tr.NewEvents.IsSome())
	internal := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).InternalEvent
	require.Len(t, internal, 1)
	_, isAccept := internal[0].(*QuoteAccepted)
	require.True(t, isAccept)
}

// TestQuoteReceivedIgnoresStaleReseal verifies that a redelivered
// quote with an equal-or-lower seal_pass is silently dropped and
// the FSM stays in QuoteReceivedState. Without this the durable
// mailbox could redeliver the first pass after the FSM has already
// advanced, and we would retrigger the accept path on a stale
// quote_id.
func TestQuoteReceivedIgnoresStaleReseal(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)

	first := quoteFromIntents(t, intents, 5_000)
	first.SealPass = 3

	s := &QuoteReceivedState{
		RoundID: RoundID{},
		Quote:   first,
		Intents: intents,
	}

	env := quoteReceivedTestEnv(10_000)

	stale := quoteFromIntents(t, intents, 5_000)
	stale.SealPass = 3 // Equal.

	tr, err := s.ProcessEvent(
		context.Background(),
		&JoinRoundQuoteReceived{
			RoundID: RoundID{},
			Quote:   stale,
		},
		env,
	)
	require.NoError(t, err)

	// Self-loop: same state, no events emitted.
	next, ok := tr.NextState.(*QuoteReceivedState)
	require.True(t, ok)
	require.Equal(t, uint32(3), next.Quote.SealPass)
	require.False(t, tr.NewEvents.IsSome())
}

// TestEvaluateQuoteRejectsZeroCap verifies that an unset
// env.MaxOperatorFee (zero-value btcutil.Amount) fails closed:
// every quote is rejected with a diagnostic reason regardless of
// whether the echoed shape is otherwise valid. This is the primary
// defense against lazy-integrator fail-open — leaving
// MaxOperatorFee at its default must not silently accept an
// unbounded server fee.
func TestEvaluateQuoteRejectsZeroCap(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 1) // Small positive fee.

	env := quoteReceivedTestEnv(0) // Unset cap.
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "zero cap must reject")
	require.Contains(t, rej.Reason, "cap is unset")
}

// TestEvaluateQuoteRejectsNegativeOperatorFee verifies that a
// server returning a negative operator_fee_sat is rejected even
// though it passes the cap comparison trivially.
func TestEvaluateQuoteRejectsNegativeOperatorFee(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, -1)

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rej.Reason, "negative")

	// Sanity: ProcessEvent on the wrapper QuoteRejected should
	// flip the FSM into ClientFailedState with a reject outbox.
	s := &QuoteReceivedState{
		RoundID: RoundID{},
		Quote:   quote,
	}
	tr, err := s.ProcessEvent(context.Background(), rej, env)
	require.NoError(t, err)
	_, isFail := tr.NextState.(*ClientFailedState)
	require.True(t, isFail)
}

// buildSingleVTXOIntents returns a deterministic intent carrying a
// single non-change VTXORequest. This mirrors the wire shape produced
// by:
//
//   - a single-recipient directed send whose coin selection covered
//     the target exactly (no self-change),
//   - a single-VTXO refresh,
//   - or a single-input boarding flow.
//
// All four flows ship one output with IsChange=false; the server then
// treats the lone slot as implicit change. Issue #378 reported that
// the client previously skipped the amount echo entirely in this
// case, leaving the lone output's value at the operator's discretion.
func buildSingleVTXOIntents(t *testing.T) Intents {
	t.Helper()

	opPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	op := opPriv.PubKey()

	req := mkReq(t, op, 0x30, true)
	req.req.Amount = 100_000
	req.req.IsChange = false

	return Intents{
		VTXOs: []types.VTXORequest{
			req.req,
		},
	}
}

// buildSingleLeaveIntents returns a deterministic intent carrying a
// single non-change LeaveRequest. Mirrors a single-VTXO offboard.
func buildSingleLeaveIntents() Intents {
	// A valid P2WPKH script is OP_0 <20-byte-hash>, total 22 bytes.
	leavePkScript := append(
		[]byte{0x00, 0x14}, bytes.Repeat([]byte{0xab}, 20)...,
	)

	return Intents{
		Leaves: []*types.LeaveRequest{{
			Output: &wire.TxOut{
				PkScript: leavePkScript,
				Value:    100_000,
			},
			IsChange: false,
		}},
	}
}

// quoteFromSingleVTXOWithFee builds a quote echoing the lone VTXO
// entry with its amount reduced by the supplied operator fee. This
// matches the honest server's behaviour for a single-output intent:
// residual = Σin − Σ(fixed) − fee, stamped on the lone (implicit-
// change) slot.
func quoteFromSingleVTXOWithFee(t *testing.T, intents Intents,
	operatorFeeSat int64) *ClientQuote {

	t.Helper()
	quote := quoteFromIntents(t, intents, operatorFeeSat)
	quote.VTXOQuotes[0].AmountSat -= operatorFeeSat

	return quote
}

// TestEvaluateQuoteEchoAcceptsSingleVTXOImplicitChangeFee verifies
// the honest single-output path: server echoes (Amount − fee) on the
// lone slot and the client accepts. Guards against over-tightening
// the #378 fix into rejecting honest single-output refresh / leave /
// boarding flows.
func TestEvaluateQuoteEchoAcceptsSingleVTXOImplicitChangeFee(t *testing.T) {
	t.Parallel()

	intents := buildSingleVTXOIntents(t)
	quote := quoteFromSingleVTXOWithFee(t, intents, 2_500)

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	_, ok := decision.(*QuoteAccepted)
	require.True(
		t, ok, "honest single-output fee deduction must accept",
	)
}

// TestEvaluateQuoteEchoRejectsSingleVTXOUnderpayment is the primary
// regression test for issue #378. Before the fix, a server that
// echoed an arbitrary smaller amount on a single non-change output
// was accepted because the implicitChange shortcut skipped the
// amount-equality check entirely. After the fix, only the exact
// (Amount − OperatorFeeSat) deviation is permitted; anything else
// rejects.
//
// This test MUST fail without the fix.
func TestEvaluateQuoteEchoRejectsSingleVTXOUnderpayment(t *testing.T) {
	t.Parallel()

	intents := buildSingleVTXOIntents(t)
	quote := quoteFromIntents(t, intents, 1_000)

	// Adversarial: operator claims a 1_000 sat fee but shaves
	// 50_000 sat off the lone recipient. With the implicitChange
	// shortcut this was silently accepted (fund theft); the
	// tightened rule requires entry == Amount − OperatorFeeSat.
	quote.VTXOQuotes[0].AmountSat = 50_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "single-output underpayment must reject")
	require.Contains(t, rej.Reason, "implicit-change amount")
}

// TestEvaluateQuoteEchoRejectsSingleLeaveUnderpayment is the leave-
// channel mirror of the #378 regression. A single-output offboard
// that the server shaves beyond the quoted operator fee must reject.
//
// This test MUST fail without the fix.
func TestEvaluateQuoteEchoRejectsSingleLeaveUnderpayment(t *testing.T) {
	t.Parallel()

	intents := buildSingleLeaveIntents()
	quote := quoteFromIntents(t, intents, 1_000)
	quote.LeaveQuotes[0].AmountSat = 50_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "single-leave underpayment must reject")
	require.Contains(t, rej.Reason, "implicit-change amount")
}

// TestEvaluateQuoteEchoAcceptsSingleLeaveImplicitChangeFee verifies
// the honest single-leave path: server echoes (Value − fee) on the
// lone slot and the client accepts.
func TestEvaluateQuoteEchoAcceptsSingleLeaveImplicitChangeFee(t *testing.T) {
	t.Parallel()

	intents := buildSingleLeaveIntents()
	quote := quoteFromIntents(t, intents, 2_500)
	quote.LeaveQuotes[0].AmountSat -= quote.OperatorFeeSat

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	_, ok := decision.(*QuoteAccepted)
	require.True(t, ok, "honest single-leave fee deduction must "+
		"accept")
}

// TestEvaluateQuoteEchoRejectsSingleVTXOOverpayment guards against a
// hostile server INCREASING the lone output's amount (e.g. to inflate
// the client's claimed balance prior to a follow-on attack). The
// tightened rule requires an exact match.
func TestEvaluateQuoteEchoRejectsSingleVTXOOverpayment(t *testing.T) {
	t.Parallel()

	intents := buildSingleVTXOIntents(t)
	quote := quoteFromIntents(t, intents, 1_000)

	// Operator inflates the lone slot above (Amount − fee).
	quote.VTXOQuotes[0].AmountSat = int64(intents.VTXOs[0].Amount) +
		10_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "single-output overpayment must reject")
	require.Contains(t, rej.Reason, "implicit-change amount")
}

// TestEvaluateQuoteEchoRejectsSingleVTXOMissingFeeDeduction guards
// the boundary case where the server claims a non-zero operator fee
// but echoes the lone slot at the full intent target (i.e. shifts
// the fee somewhere else, like an off-tree mint). The tightened rule
// rejects: the residual must be stamped on the implicit-change slot.
func TestEvaluateQuoteEchoRejectsSingleVTXOMissingFeeDeduction(t *testing.T) {
	t.Parallel()

	intents := buildSingleVTXOIntents(t)
	quote := quoteFromIntents(t, intents, 1_000)
	// Intentionally do not subtract the fee: echo == Amount.

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(env, RoundID{}, intents, quote)
	rej, ok := decision.(*QuoteRejected)
	require.True(
		t, ok, "single-output missing fee deduction must reject",
	)
	require.Contains(t, rej.Reason, "implicit-change amount")
}
