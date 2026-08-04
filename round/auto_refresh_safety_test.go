package round

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// mixedAutoRefreshIntents marks one output as automatic maintenance and the
// other as manual refresh, preserving the deterministic input/output fixture.
func mixedAutoRefreshIntents(t *testing.T) Intents {
	t.Helper()

	intents, _ := buildEchoTestIntents(t)
	intents.VTXOs[0].Origin = types.VTXOOriginAutoRefresh
	intents.VTXOs[1].Origin = types.VTXOOriginRoundRefresh

	return intents
}

// TestEvaluateQuoteRejectsVTXOBelowOperatorMinimum verifies the server cannot
// pay a seal-time fee by shrinking the designated change output below the
// currently negotiated minimum.
func TestEvaluateQuoteRejectsVTXOBelowOperatorMinimum(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	quote := quoteFromIntents(t, intents, 35_001)
	quote.VTXOQuotes[1].AmountSat = 29_999

	env := quoteReceivedTestEnv(50_000)
	env.OperatorTerms = &types.OperatorTerms{
		MinVTXOAmount: 30_000,
	}

	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rejected, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rejected.Reason, "below operator minimum")
}

// TestQuoteReceivedResealRejectsVTXOBelowOperatorMinimum verifies every new
// seal pass re-runs the minimum-output check instead of trusting pass zero.
func TestQuoteReceivedResealRejectsVTXOBelowOperatorMinimum(t *testing.T) {
	t.Parallel()

	intents, _ := buildEchoTestIntents(t)
	first := quoteFromIntents(t, intents, 5_000)
	first.SealPass = 1
	state := &QuoteReceivedState{
		RoundID: RoundID{},
		Quote:   first,
		Intents: intents,
	}

	reseal := quoteFromIntents(t, intents, 35_001)
	reseal.SealPass = 2
	reseal.VTXOQuotes[1].AmountSat = 29_999

	env := quoteReceivedTestEnv(50_000)
	env.OperatorTerms = &types.OperatorTerms{
		MinVTXOAmount: 30_000,
	}

	transition, err := state.ProcessEvent(
		context.Background(), &JoinRoundQuoteReceived{
			RoundID: RoundID{},
			Quote:   reseal,
		}, env,
	)
	require.NoError(t, err)

	next, ok := transition.NextState.(*QuoteReceivedState)
	require.True(t, ok)
	require.Equal(t, uint32(2), next.Quote.SealPass)

	events := transition.NewEvents.UnwrapOr(ClientEmittedEvent{})
	require.Len(t, events.InternalEvent, 1)
	rejected, ok := events.InternalEvent[0].(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rejected.Reason, "below operator minimum")
}

// TestEvaluateQuoteAppliesAutomaticFloor verifies the fixed component of the
// maintenance budget is evaluated against the authoritative realised fee.
func TestEvaluateQuoteAppliesAutomaticFloor(t *testing.T) {
	t.Parallel()

	intents := mixedAutoRefreshIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	env := quoteReceivedTestEnv(10_000)
	env.AutoRefreshFeeFloor = btcutil.Amount(4_999)
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rejected, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rejected.Reason, "automatic refresh fee")

	env.AutoRefreshFeeFloor = btcutil.Amount(5_000)
	decision = evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	_, ok = decision.(*QuoteAccepted)
	require.True(t, ok)
}

// TestEvaluateQuoteAppliesAutomaticProportionalBudgetToMixedRound verifies a
// manual output cannot dilute the proportional denominator or bypass policy
// when it shares an assembling round with automatic maintenance.
func TestEvaluateQuoteAppliesAutomaticProportionalBudgetToMixedRound(
	t *testing.T) {

	t.Parallel()

	intents := mixedAutoRefreshIntents(t)
	quote := quoteFromIntents(t, intents, 5_000)

	// Only the 40,000-sat automatic target is the denominator. A 124,999
	// ppm allowance rounds down to 4,999 sat, so the entire 5,000-sat
	// realised mixed-round fee must reject.
	env := quoteReceivedTestEnv(10_000)
	env.AutoRefreshFeeRatePPM = 124_999
	decision := evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	rejected, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(t, rejected.Reason, "automatic refresh fee")

	env.AutoRefreshFeeRatePPM = 125_000
	decision = evaluateQuote(
		context.Background(), env, RoundID{}, intents, quote,
	)
	_, ok = decision.(*QuoteAccepted)
	require.True(t, ok)
}

// TestAutoRefreshFeeBudgetUsesOneCurve verifies the fixed allowance rescues a
// small VTXO from a percentage budget below the operation's fixed costs, while
// the proportional allowance takes over for larger cohorts.
func TestAutoRefreshFeeBudgetUsesOneCurve(t *testing.T) {
	t.Parallel()

	budget, enabled := autoRefreshFeeBudget(
		10_000, 300, 10_000, 1_000,
	)
	require.True(t, enabled)
	require.Equal(t, int64(300), budget)

	budget, enabled = autoRefreshFeeBudget(
		10_000, 300, 10_000, 100_000,
	)
	require.True(t, enabled)
	require.Equal(t, int64(1_000), budget)
}

// TestAutoRefreshFeeBudgetClampsToGlobalCap verifies the mandatory global cap
// remains the hard ceiling over both automatic budget components.
func TestAutoRefreshFeeBudgetClampsToGlobalCap(t *testing.T) {
	t.Parallel()

	budget, enabled := autoRefreshFeeBudget(
		500, 750, 100_000, 100_000,
	)
	require.True(t, enabled)
	require.Equal(t, int64(500), budget)
}

// TestAutoRefreshFeeBudgetDefaultsToGlobalCap verifies zero/zero preserves
// the existing global-only policy rather than rejecting every automatic
// refresh with a zero budget.
func TestAutoRefreshFeeBudgetDefaultsToGlobalCap(t *testing.T) {
	t.Parallel()

	budget, enabled := autoRefreshFeeBudget(10_000, 0, 0, 1_000)
	require.False(t, enabled)
	require.Equal(t, int64(10_000), budget)
}
