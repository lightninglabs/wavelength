package round

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
)

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

// maintenanceIntents returns a one-for-one cohort with a small fixed output
// and a larger fee-bearing output. The latter must not subsidize automatic
// maintenance merely because the small output retains its full value.
func maintenanceIntents(t *testing.T) Intents {
	t.Helper()

	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	var intents Intents
	for i, amount := range []btcutil.Amount{1_200, 2_738} {
		req := mkReq(t, operator.PubKey(), byte(i+1), true).req
		req.Amount = amount
		req.IsChange = i == 1
		req.Origin = types.VTXOOriginAutoRefresh
		outpoint := wire.OutPoint{Index: uint32(i)}
		intents.Forfeits = append(
			intents.Forfeits, types.ForfeitRequest{
				VTXOOutpoint: &outpoint,
				Amount:       amount,
			},
		)
		intents.VTXOs = append(intents.VTXOs, req)
	}

	return intents
}

// TestAutomaticRefreshRequiresZeroFee checks both default and legacy budget
// settings, mixed-origin intents, and explicitly requested paid refreshes.
func TestAutomaticRefreshRequiresZeroFee(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		fee          int64
		manual       bool
		mixed        bool
		legacyBudget bool
		accept       bool
	}{
		{
			name:   "free cohort",
			accept: true,
		},
		{
			name: "one sat is not free",
			fee:  1,
		},
		{
			name: "small output hides cohort charge",
			fee:  261,
		},
		{name: "legacy allowance cannot authorize fees", fee: 261,
			legacyBudget: true},
		{name: "manual output cannot absorb automatic fee", fee: 261,
			mixed: true},
		{
			name:   "free mixed refresh is allowed",
			mixed:  true,
			accept: true,
		},
		{name: "manual paid refresh", fee: 261, manual: true,
			accept: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			intents := maintenanceIntents(t)
			for i := range intents.VTXOs {
				if test.manual || (test.mixed && i == 1) {
					intents.VTXOs[i].Origin =
						types.VTXOOriginRoundRefresh
				}
			}
			quote := quoteFromIntents(t, intents, test.fee)
			quote.VTXOQuotes[1].AmountSat -= test.fee
			env := quoteReceivedTestEnv(1_000_000)
			env.OperatorTerms = &types.OperatorTerms{
				MinVTXOAmount: 1_000,
			}
			if test.legacyBudget {
				env.AutoRefreshFeeFloor = 1_000
				env.AutoRefreshFeeRatePPM = 1_000_000
			}

			decision := evaluateQuote(
				t.Context(), env, RoundID{}, intents, quote,
			)
			if test.accept {
				require.IsType(t, &QuoteAccepted{}, decision)

				return
			}
			rejected, ok := decision.(*QuoteRejected)
			require.True(t, ok)
			require.Contains(
				t, rejected.Reason,
				"automatic refresh requires zero fee",
			)
			if test.mixed {
				require.Contains(
					t, rejected.Reason,
					"mixed manual/automatic round",
				)
				require.Contains(
					t, rejected.Reason,
					"retry the manual request separately",
				)
			}
		})
	}
}

// TestAutomaticRefreshPaidResealRollsBack proves that a free first quote
// cannot authorize a later charge and that rejection releases the inputs.
func TestAutomaticRefreshPaidResealRollsBack(t *testing.T) {
	t.Parallel()

	intents := maintenanceIntents(t)
	first := quoteFromIntents(t, intents, 0)
	first.SealPass = 1
	state := &QuoteReceivedState{Intents: intents, Quote: first}
	reseal := quoteFromIntents(t, intents, 261)
	reseal.SealPass = 2
	reseal.VTXOQuotes[1].AmountSat -= 261
	env := quoteReceivedTestEnv(1_000_000)
	transition, err := state.ProcessEvent(
		t.Context(), &JoinRoundQuoteReceived{
			Quote: reseal,
		},
		env,
	)
	require.NoError(t, err)
	events := transition.NewEvents.UnwrapOr(ClientEmittedEvent{})
	require.Len(t, events.InternalEvent, 1)
	decision := events.InternalEvent[0]
	rejected, ok := decision.(*QuoteRejected)
	require.True(t, ok)
	require.Contains(
		t, rejected.Reason, "automatic refresh requires zero fee",
	)

	transition, err = transition.NextState.ProcessEvent(
		t.Context(), decision, env,
	)
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, transition.NextState)
	outbox := transition.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	require.IsType(t, &JoinRoundRejectOutbox{}, outbox[1])
	require.Len(t, outbox, 2)
	release, ok := outbox[0].(*ReleaseForfeitReservation)
	require.True(t, ok)
	require.ElementsMatch(t, []wire.OutPoint{
		*intents.Forfeits[0].VTXOOutpoint,
		*intents.Forfeits[1].VTXOOutpoint,
	}, release.Outpoints)

	// Exercise delivery, not just outbox shape: a failed server send must
	// not prevent the manager from receiving both local releases.
	h := newActorTestHarness(t)
	sendErr := errors.New("server mailbox unavailable")
	h.actor.cfg.ServerConn = &failedQuoteRejectRef{
		mockServerConnRef: h.serverConn,
		err:               sendErr,
	}
	err = h.actor.processOutbox(h.ctx, outbox)
	require.ErrorIs(t, err, sendErr)
	messages := h.vtxoManager.getMessages()
	require.Len(t, messages, 1)
	request, ok := messages[0].(*actormsg.ReleaseForfeitRequest)
	require.True(t, ok)
	require.ElementsMatch(t, release.Outpoints, request.Outpoints)
}

// failedQuoteRejectRef simulates a server mailbox refusing the reject message.
type failedQuoteRejectRef struct {
	*mockServerConnRef
	err error
}

// Tell fails the remote send while the embedded ref supplies its identity.
func (r *failedQuoteRejectRef) Tell(context.Context,
	serverconn.ServerConnMsg) error {

	return r.err
}
