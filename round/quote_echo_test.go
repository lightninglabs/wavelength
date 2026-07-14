package round

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/mock"
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
//
// A single boarding input of 130_000 sat is included so the
// realised-fee check in evaluateQuote (#379) sees Σinputs −
// Σoutputs == 5_000, which matches the default OperatorFeeSat
// passed by quoteFromIntents. Tests that mutate fee or output
// amounts may need to compensate.
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

	// Boarding input value covers Σ(quoted outputs) + the default
	// 5_000 sat fee that quoteFromIntents stamps. The chain info
	// amount is what realisedQuoteFee sums into Σinputs.
	boarding := BoardingIntent{
		BoardingIntent: WalletBoardingIntent{
			ChainInfo: BoardingChainInfo{
				Amount: 130_000,
			},
		},
	}

	return Intents{
		Boarding: []BoardingIntent{
			boarding,
		},
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

	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	_, ok := decision.(*QuoteAccepted)
	require.True(t, ok, "faithful echo should accept")
}

// TestEvaluateQuoteEchoAcceptsChangeDeviation verifies that amount
// deviation is permitted for the single IsChange=true VTXO output —
// the residual sink is server-decided by design — as long as the
// resulting realised fee stays within env.MaxOperatorFee (#379).
func TestEvaluateQuoteEchoAcceptsChangeDeviation(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	// Change entry is intents.VTXOs[1]; server chooses a
	// different residual. Σinputs=130_000, other outputs sum to
	// 40_000+25_000=65_000, so the new change=57_000 pushes the
	// realised fee to 8_000 — still under the 10_000 cap.
	// quote.OperatorFeeSat must agree with the realised value so
	// the dishonesty check passes.
	quote.VTXOQuotes[1].AmountSat = 57_000
	quote.OperatorFeeSat = 8_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	_, ok := decision.(*QuoteAccepted)
	require.True(t, ok, "change-output deviation must be permitted")
}

// TestEvaluateQuoteEchoRejectsFixedChangeOutput verifies a fixed contract VTXO
// cannot be the mutable change/residual slot even when the quote echoes its
// amount exactly.
func TestEvaluateQuoteEchoRejectsFixedChangeOutput(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	intents.VTXOs[1].FixedAmount = true
	quote := quoteFromIntents(t, intents, 5_000)

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "fixed change output must reject")
	require.Contains(t, rej.Reason, "fixed amount cannot be change")
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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
				context.Background(), env, RoundID{}, intents,
				quote,
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

	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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

	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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
	// amount the server chose under updated chain state. Σinputs
	// =130_000; with change=59_000 plus the unchanged 40_000 and
	// 25_000 outputs the realised fee is 6_000 — matches the
	// declared OperatorFeeSat and stays within the 10_000 cap
	// (#379).
	second := quoteFromIntents(t, intents, 6_000)
	second.SealPass = 1
	second.VTXOQuotes[1].AmountSat = 59_000 // Change leg shifted.

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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
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

// TestEvaluateQuoteRejectsChangeUnderpaymentBypass covers the #379
// fee-cap bypass: a malicious operator quotes an OperatorFeeSat
// that sits comfortably under env.MaxOperatorFee while shaving the
// IsChange=true VTXO output by a much larger delta. The echo
// validation intentionally permits change-output amount deviation,
// so without the realised-fee recomputation the client would
// accept and sign a round whose actual economic fee exceeds the
// cap. The realised-fee check in evaluateQuote must catch this.
func TestEvaluateQuoteRejectsChangeUnderpaymentBypass(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 1_000)

	// Σinputs=130_000; honest outputs sum to 125_000 so the
	// declared 1_000-sat fee is a lie unless the change output
	// is shaved. Drop the change leg by 20_000 sat (60_000 →
	// 40_000) so Σoutputs=105_000 and the realised fee is
	// 25_000 — well above the 10_000 cap.
	quote.VTXOQuotes[1].AmountSat = 40_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(
		t, ok, "change underpayment must be rejected, got %T", decision,
	)
	// Either the cap-exceeded message or the dishonesty-mismatch
	// message is acceptable — both protect the client.
	require.True(
		t,
		containsAny(
			rej.Reason, []string{
				"realised operator fee",
				"disagrees with realised fee",
			},
		),
		"unexpected rejection reason: %q",
		rej.Reason,
	)
}

// TestEvaluateQuoteRejectsChangeUnderpaymentBelowCap covers the
// subtler #379 variant: the change shave keeps the realised fee
// just within MaxOperatorFee, but the operator still lies about
// what they took. The dishonesty mismatch check must reject so
// the FeePaidMsg accounting cannot diverge from on-chain reality
// (the confirmation-time accounting trusts OperatorFeeSat as
// authoritative — see computeClientOperatorFee).
func TestEvaluateQuoteRejectsChangeUnderpaymentBelowCap(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	// Σinputs=130_000; declared fee=5_000. Shave the change leg
	// from 60_000 to 57_000 so Σoutputs=122_000 and realised
	// fee=8_000. That sits under the 10_000 cap but disagrees
	// with the declared 5_000.
	quote.VTXOQuotes[1].AmountSat = 57_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(
		t, ok, "dishonest fee declaration must be rejected, got %T",
		decision,
	)
	require.Contains(t, rej.Reason, "disagrees with realised fee")
}

// TestEvaluateQuoteRejectsSingleOutputUnderpayment covers the #379
// variant on single-output intents. With totalOutputs==1 the echo
// validator skips the per-output equality check (treating the lone
// output as implicit change), so the only protection against the
// operator silently lowering that output is the realised-fee
// recomputation. Build a single-output intent, have the operator
// declare a small fee while quoting a much smaller output amount,
// and assert rejection.
func TestEvaluateQuoteRejectsSingleOutputUnderpayment(t *testing.T) {
	t.Parallel()

	opPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	op := opPriv.PubKey()

	req := mkReq(t, op, 0x30, true)
	req.req.Amount = 100_000
	req.req.IsChange = false

	intents := Intents{
		Boarding: []BoardingIntent{{
			BoardingIntent: WalletBoardingIntent{
				ChainInfo: BoardingChainInfo{
					Amount: 100_000,
				},
			},
		}},
		VTXOs: []types.VTXORequest{
			req.req,
		},
	}

	// Operator declares a tiny 500-sat fee while quoting the
	// lone VTXO output down by 20_000 sat. Without #379's
	// realised-fee check, echo validation passes (single-output
	// intent is implicit change) and the client signs a round
	// paying 20_000 in fees against a 10_000 cap.
	script, err := req.req.EffectivePkScript()
	require.NoError(t, err)

	var quoteID [32]byte
	for i := range quoteID {
		quoteID[i] = byte(i + 1)
	}

	recipientKey := req.req.SigningKey.PubKey.SerializeCompressed()
	quote := &ClientQuote{
		QuoteID:        quoteID,
		OperatorFeeSat: 500,
		VTXOQuotes: []VTXOQuoteEntry{{
			PkScript: script,
			// 20_000 sat underpayment.
			AmountSat:    80_000,
			RecipientKey: recipientKey,
		}},
	}

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(
		t, ok, "single-output underpayment must be rejected, got %T",
		decision,
	)
	require.True(
		t,
		containsAny(
			rej.Reason, []string{
				"realised operator fee",
				"disagrees with realised fee",
			},
		),
		"unexpected rejection reason: %q",
		rej.Reason,
	)
}

// TestEvaluateQuoteRejectsRealisedFeeNegative covers the symmetric
// case: the operator quotes outputs that exceed the available
// inputs (i.e. the quote claims to mint value). The realised-fee
// check must reject — the existing balance guard at intent
// composition catches this on the input side, but a malicious
// operator could inflate a non-change output post-quote, and the
// FSM has no second balance gate before signing.
func TestEvaluateQuoteRejectsRealisedFeeNegative(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	// Σinputs=130_000; inflate the change leg from 60_000 to
	// 200_000 so Σoutputs=265_000 and realised fee is −135_000.
	quote.VTXOQuotes[1].AmountSat = 200_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rej.Reason, "realised fee is negative")
}

// TestEvaluateQuoteAcceptsExactlyAtCap covers the edge where the
// realised fee equals the cap exactly. The check is "exceeds cap"
// so equality must accept.
func TestEvaluateQuoteAcceptsExactlyAtCap(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 10_000)

	// Σinputs=130_000; shave change from 60_000 to 55_000 so
	// Σoutputs=120_000 and realised fee=10_000, exactly at cap.
	quote.VTXOQuotes[1].AmountSat = 55_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	_, ok := decision.(*QuoteAccepted)
	require.True(t, ok, "exactly-at-cap realised fee must accept")
}

// TestEvaluateQuoteRealisedFeeUsesForfeitStore covers the
// forfeit-only flow (refresh rounds): inputs come from VTXOStore
// lookups rather than boarding ChainInfo. A malicious operator
// that understates the change output must still be caught when
// the inputs are sourced from the store.
func TestEvaluateQuoteRealisedFeeUsesForfeitStore(t *testing.T) {
	t.Parallel()

	opPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	op := opPriv.PubKey()

	// Two recipient VTXOs and one change VTXO, no boarding.
	reqA := mkReq(t, op, 0x40, true)
	reqA.req.Amount = 30_000
	reqA.req.IsChange = false

	reqB := mkReq(t, op, 0x50, true)
	reqB.req.Amount = 60_000
	reqB.req.IsChange = true

	outpoint := wire.OutPoint{Index: 0}
	forfeit := types.ForfeitRequest{
		VTXOOutpoint: &outpoint,
		// Fallback used when store is nil.
		Amount: 100_000,
	}

	intents := Intents{
		VTXOs: []types.VTXORequest{
			reqA.req,
			reqB.req,
		},
		Forfeits: []types.ForfeitRequest{
			forfeit,
		},
	}

	scriptA, err := reqA.req.EffectivePkScript()
	require.NoError(t, err)
	scriptB, err := reqB.req.EffectivePkScript()
	require.NoError(t, err)

	var quoteID [32]byte
	for i := range quoteID {
		quoteID[i] = byte(i + 1)
	}

	// Honest fee would be 100_000−90_000=10_000 (exactly at
	// cap). Malicious operator declares 1_000 while shaving the
	// change leg from 60_000 to 30_000 (Σoutputs=60_000,
	// realised=40_000).
	keyA := reqA.req.SigningKey.PubKey.SerializeCompressed()
	keyB := reqB.req.SigningKey.PubKey.SerializeCompressed()
	quote := &ClientQuote{
		QuoteID:        quoteID,
		OperatorFeeSat: 1_000,
		VTXOQuotes: []VTXOQuoteEntry{
			{
				PkScript:     scriptA,
				AmountSat:    30_000,
				RecipientKey: keyA,
			},
			{
				PkScript:     scriptB,
				AmountSat:    30_000,
				RecipientKey: keyB,
			},
		},
	}

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(
		t, ok, "refresh-round change underpayment must be rejected, "+
			"got %T", decision,
	)
	require.True(
		t,
		containsAny(
			rej.Reason, []string{
				"realised operator fee",
				"disagrees with realised fee",
			},
		),
		"unexpected rejection reason: %q",
		rej.Reason,
	)
}

// TestRealisedFeeIncludesZeroValueOutputs verifies that a quote
// whose echoed amounts include a zero-value entry contributes that
// zero to the realised-fee sum (i.e. the loop no longer filters
// zero). The honest case is constructed so the realised fee matches
// the declared OperatorFeeSat exactly; a stray filter or arithmetic
// drift would push realised != declared and trigger the dishonesty
// rejection. We exercise a zero-value leave entry because that is
// the only on-chain shape (e.g. OP_RETURN markers) where zero is
// arguably legitimate; the VTXO side is rejected upstream as dust,
// but the realised-fee sum is shape-agnostic by design.
func TestRealisedFeeIncludesZeroValueOutputs(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)

	// Replace the leave intent's value with zero so the echo and
	// the realised sum both see a 0-sat output. Σinputs=130_000;
	// VTXO outputs sum to 40_000+60_000=100_000; leave sum drops
	// from 25_000 to 0, so the realised fee climbs from 5_000 to
	// 30_000. Declare 30_000 to keep the dishonesty check happy
	// and bump the cap accordingly.
	intents.Leaves[0].Output.Value = 0

	quote := quoteFromIntents(t, intents, 30_000)
	env := quoteReceivedTestEnv(30_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	_, ok := decision.(*QuoteAccepted)
	require.True(
		t, ok, "zero-value leave output must be summed into "+
			"realised fee, got %T", decision,
	)
}

// TestRealisedFeeRejectsNegativeOutput verifies that a quote with
// a negative AmountSat is rejected at the realised-fee computation
// step, rather than being silently filtered. A negative output
// would subtract a positive value from Σoutputs and inflate the
// realised fee in the operator's favor; the previous `if amt > 0`
// filter masked this by treating the entry as zero.
func TestRealisedFeeRejectsNegativeOutput(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	// Inject a negative leave-output amount. The echo validator
	// runs first; rebuild the intent's leave value to match so we
	// exercise the realisedQuoteFee branch rather than tripping
	// the non-change-amount echo check.
	intents.Leaves[0].Output.Value = -1
	quote.LeaveQuotes[0].AmountSat = -1

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(
		t, ok, "negative output must be rejected, got %T", decision,
	)
	require.Contains(t, rej.Reason, "negative")
}

// containsAny reports whether any of the substrings appears in s.
func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}

	return false
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
//
// A matching boarding input is included so that the realised-fee
// check added by #379 sees Σinputs = Σoutputs + fee for the honest
// path. (Without an input source the realised-fee check would
// reject every single-output intent built by this helper.)
func buildSingleVTXOIntents(t *testing.T) Intents {
	t.Helper()

	opPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	op := opPriv.PubKey()

	req := mkReq(t, op, 0x30, true)
	req.req.Amount = 100_000
	req.req.IsChange = false

	return Intents{
		Boarding: []BoardingIntent{{
			BoardingIntent: WalletBoardingIntent{
				ChainInfo: BoardingChainInfo{
					Amount: 100_000,
				},
			},
		}},
		VTXOs: []types.VTXORequest{
			req.req,
		},
	}
}

// buildSingleLeaveIntents returns a deterministic intent carrying a
// single non-change LeaveRequest. Mirrors a single-VTXO offboard. A
// matching boarding input is included so the realised-fee check
// added by #379 has a corresponding input source on the honest path.
func buildSingleLeaveIntents() Intents {
	// A valid P2WPKH script is OP_0 <20-byte-hash>, total 22 bytes.
	leavePkScript := append(
		[]byte{0x00, 0x14}, bytes.Repeat([]byte{0xab}, 20)...,
	)

	return Intents{
		Boarding: []BoardingIntent{{
			BoardingIntent: WalletBoardingIntent{
				ChainInfo: BoardingChainInfo{
					Amount: 100_000,
				},
			},
		}},
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	_, ok := decision.(*QuoteAccepted)
	require.True(
		t, ok, "honest single-output fee deduction must accept",
	)
}

// TestEvaluateQuoteEchoRejectsSingleVTXOUnderpayment is the primary
// regression test for issue #378. Before the realised-fee check, a
// server that echoed an arbitrary smaller amount on a single implicit-
// change output was accepted because the quote echo check intentionally
// leaves that server-stamped slot flexible. The realised fee must now
// match OperatorFeeSat and remain under the client's cap.
//
// This test MUST fail without the fix.
func TestEvaluateQuoteEchoRejectsSingleVTXOUnderpayment(t *testing.T) {
	t.Parallel()

	intents := buildSingleVTXOIntents(t)
	quote := quoteFromIntents(t, intents, 1_000)

	// Adversarial: operator claims a 1_000 sat fee but shaves
	// 50_000 sat off the lone recipient. With the implicitChange
	// shortcut alone this was silently accepted (fund theft); the
	// realised-fee check catches the mismatch.
	quote.VTXOQuotes[0].AmountSat = 50_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "single-output underpayment must reject")
	require.True(
		t,
		containsAny(
			rej.Reason, []string{
				"realised operator fee",
				"disagrees with realised fee",
			},
		),
		"unexpected rejection reason: %q",
		rej.Reason,
	)
}

// TestEvaluateQuoteEchoRejectsFixedSingleVTXOAmountChange verifies contract
// outputs can opt out of the single-output implicit-change rule. A fixed
// replacement vHTLC must either be quoted at its exact amount or the round must
// be rejected before any forfeit is signed.
func TestEvaluateQuoteEchoRejectsFixedSingleVTXOAmountChange(t *testing.T) {
	t.Parallel()

	intents := buildSingleVTXOIntents(t)
	intents.VTXOs[0].FixedAmount = true

	quote := quoteFromSingleVTXOWithFee(t, intents, 2_500)

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "fixed single vtxo amount change must reject")
	require.Contains(t, rej.Reason, "non-change amount")
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "single-leave underpayment must reject")
	require.True(
		t,
		containsAny(
			rej.Reason, []string{
				"realised operator fee",
				"disagrees with realised fee",
			},
		),
		"unexpected rejection reason: %q",
		rej.Reason,
	)
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
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	_, ok := decision.(*QuoteAccepted)
	require.True(t, ok, "honest single-leave fee deduction must "+
		"accept")
}

// TestEvaluateQuoteEchoAcceptsSingleVTXOResidualAboveTarget verifies
// the boarding-style implicit-change shape where the intent target is
// a conservative lower bound, but the actual seal-time residual is
// higher because the realised operator fee is lower than the wallet's
// estimate.
func TestEvaluateQuoteEchoAcceptsSingleVTXOResidualAboveTarget(t *testing.T) {
	t.Parallel()

	intents := buildSingleVTXOIntents(t)
	intents.VTXOs[0].Amount = 95_000

	quote := quoteFromIntents(t, intents, 1_000)
	quote.VTXOQuotes[0].AmountSat = 99_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	_, ok := decision.(*QuoteAccepted)
	require.True(
		t, ok, "implicit-change residual above target must accept",
	)
}

// TestEvaluateQuoteUsesDetachedContextForForfeitLookup verifies that
// seal-time quote evaluation can still read local forfeit amounts if
// the caller context that delivered the quote has already been canceled.
func TestEvaluateQuoteUsesDetachedContextForForfeitLookup(t *testing.T) {
	t.Parallel()

	opPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	op := opPriv.PubKey()

	req := mkReq(t, op, 0x60, true)
	req.req.Amount = 100_000
	req.req.IsChange = false

	outpoint := wire.OutPoint{Index: 9}
	intents := Intents{
		Forfeits: []types.ForfeitRequest{{
			VTXOOutpoint: &outpoint,
		}},
		VTXOs: []types.VTXORequest{
			req.req,
		},
	}

	quote := quoteFromSingleVTXOWithFee(t, intents, 1_000)

	store := &MockVTXOStore{}
	store.On(
		"GetVTXO",
		mock.MatchedBy(func(ctx context.Context) bool {
			return ctx.Err() == nil
		}),
		outpoint,
	).Return(&ClientVTXO{Amount: 100_000}, nil).Once()

	env := quoteReceivedTestEnv(10_000)
	env.VTXOStore = store

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	decision := evaluateQuote(ctx, env, RoundID{}, intents, quote)
	_, ok := decision.(*QuoteAccepted)
	require.True(t, ok, "canceled caller context must not fail quote")
	store.AssertExpectations(t)
}

// TestEvaluateQuoteEchoRejectsSingleVTXOOverpayment guards against a
// hostile server increasing the lone output above available input value.
// The realised-fee check catches this as value creation.
func TestEvaluateQuoteEchoRejectsSingleVTXOOverpayment(t *testing.T) {
	t.Parallel()

	intents := buildSingleVTXOIntents(t)
	quote := quoteFromIntents(t, intents, 1_000)

	// Operator inflates the lone slot above (Amount − fee).
	quote.VTXOQuotes[0].AmountSat = int64(intents.VTXOs[0].Amount) +
		10_000

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(t, ok, "single-output overpayment must reject")
	require.Contains(t, rej.Reason, "realised fee is negative")
}

// TestEvaluateQuoteEchoRejectsSingleVTXOMissingFeeDeduction guards
// the boundary case where the server claims a non-zero operator fee
// but echoes the lone slot at the full intent target (i.e. shifts
// the fee somewhere else, like an off-tree mint). The realised-fee
// check rejects because the actual outputs imply a zero operator fee.
func TestEvaluateQuoteEchoRejectsSingleVTXOMissingFeeDeduction(t *testing.T) {
	t.Parallel()

	intents := buildSingleVTXOIntents(t)
	quote := quoteFromIntents(t, intents, 1_000)
	// Intentionally do not subtract the fee: echo == Amount.

	env := quoteReceivedTestEnv(10_000)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rej, ok := decision.(*QuoteRejected)
	require.True(
		t, ok, "single-output missing fee deduction must reject",
	)
	require.Contains(t, rej.Reason, "disagrees with realised fee")
}
