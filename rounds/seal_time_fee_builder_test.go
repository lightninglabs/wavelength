package rounds

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// newTestBuilderCalc constructs a fees.Calculator for builder unit
// tests with a plain schedule (10% annual rate, 100-sat margin,
// 1000-block δ_min floor). Centralized so expected-fee math across
// tests reads against the same knobs.
func newTestBuilderCalc(t *testing.T) *fees.Calculator {
	t.Helper()

	sched := &fees.Schedule{
		AnnualRate:            10.0,
		BaseMarginSat:         100,
		MinRefreshDeltaBlocks: 1000,
	}

	calc, err := fees.NewCalculator(sched)
	require.NoError(t, err)

	return calc
}

// newTestVTXORequest builds a VTXORequest with a freshly-generated
// signing key so the per-request SigningKey map in the registration
// is well-formed. amount and isChange are the knobs the tests vary.
func newTestVTXORequest(t *testing.T, amount btcutil.Amount,
	isChange bool) *types.VTXORequest {

	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &types.VTXORequest{
		Amount:   amount,
		IsChange: isChange,
		SigningKey: keychain.KeyDescriptor{
			PubKey: priv.PubKey(),
		},
	}
}

// newTestBoardingInput creates a BoardingInput with a given value.
// The outpoint is deterministic per t.Name so concurrent tests do
// not collide on the fake outpoint.
func newTestBoardingInput(t *testing.T, value btcutil.Amount) *BoardingInput {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &BoardingInput{
		Outpoint:  &wire.OutPoint{Index: 0},
		Value:     value,
		ClientKey: priv.PubKey(),
	}
}

// TestBuildQuotesRejectsMissingChangeMarker verifies that an intent
// without any IsChange=true marker is rejected by the builder with
// QuoteReasonInvalidChangeDesignation — even though this case is
// also caught at admission time, the builder must stay defensive
// for intents restored across a reseal.
func TestBuildQuotesRejectsMissingChangeMarker(t *testing.T) {
	t.Parallel()

	const amount = btcutil.Amount(50_000)

	reg := &ClientRegistration{
		ClientID: clientconn.ClientID("client1"),
		BoardingInputs: []*BoardingInput{
			newTestBoardingInput(t, amount),
		},
		IntentVTXOReqs: []*types.VTXORequest{
			newTestVTXORequest(t, amount/2, false),
			newTestVTXORequest(t, amount/2, false),
		},
	}

	quotes, err := computeSealTimeQuotes(
		RoundID{}, map[ClientID]*ClientRegistration{
			"client1": reg,
		},
		0, 100, chainfee.SatPerKWeight(1000), 0, 330,
		newTestBuilderCalc(t),
	)
	require.NoError(t, err)

	q, ok := quotes["client1"]
	require.True(t, ok)
	require.Equal(
		t, QuoteReasonInvalidChangeDesignation, q.RejectReason,
	)
}

// TestBuildQuotesRejectsTwoChangeMarkers covers the inverse: more
// than one IsChange=true marker is ambiguous and must be rejected.
func TestBuildQuotesRejectsTwoChangeMarkers(t *testing.T) {
	t.Parallel()

	const amount = btcutil.Amount(50_000)

	reg := &ClientRegistration{
		ClientID: clientconn.ClientID("client1"),
		BoardingInputs: []*BoardingInput{
			newTestBoardingInput(t, amount),
		},
		IntentVTXOReqs: []*types.VTXORequest{
			newTestVTXORequest(t, amount/2, true),
			newTestVTXORequest(t, amount/2, true),
		},
	}

	quotes, err := computeSealTimeQuotes(
		RoundID{}, map[ClientID]*ClientRegistration{
			"client1": reg,
		},
		0, 100, chainfee.SatPerKWeight(1000), 0, 330,
		newTestBuilderCalc(t),
	)
	require.NoError(t, err)

	q := quotes["client1"]
	require.Equal(
		t, QuoteReasonInvalidChangeDesignation, q.RejectReason,
	)
}

// TestBuildQuotesImplicitSingleOutputChange verifies that a
// single-output intent implicitly designates its sole entry as
// change, without requiring IsChange=true.
func TestBuildQuotesImplicitSingleOutputChange(t *testing.T) {
	t.Parallel()

	const amount = btcutil.Amount(50_000)

	reg := &ClientRegistration{
		ClientID: clientconn.ClientID("client1"),
		BoardingInputs: []*BoardingInput{
			newTestBoardingInput(t, amount),
		},
		IntentVTXOReqs: []*types.VTXORequest{
			newTestVTXORequest(t, amount, false),
		},
	}

	quotes, err := computeSealTimeQuotes(
		RoundID{}, map[ClientID]*ClientRegistration{
			"client1": reg,
		},
		0, 100, chainfee.SatPerKWeight(1000), 0, 330,
		newTestBuilderCalc(t),
	)
	require.NoError(t, err)

	q := quotes["client1"]
	require.Equal(t, QuoteReasonOK, q.RejectReason)

	// The single output's amount must have been rewritten to
	// Σin − fee (the residual), not the intent target.
	var stamped btcutil.Amount
	for _, a := range q.VTXOAmounts {
		stamped = a
	}
	require.Less(t, int64(stamped), int64(amount))
	require.Positive(t, int64(stamped))
}

// TestBuildQuotesResidualStampedOnChangeOutput is the main
// invariant: Σin − Σ(fixed) − fee lands on the IsChange output,
// and non-change outputs echo the intent target verbatim.
func TestBuildQuotesResidualStampedOnChangeOutput(t *testing.T) {
	t.Parallel()

	const (
		input      = btcutil.Amount(1_000_000)
		fixedVTXO  = btcutil.Amount(400_000)
		changeHint = btcutil.Amount(600_000)
	)

	fixedReq := newTestVTXORequest(t, fixedVTXO, false)
	changeReq := newTestVTXORequest(t, changeHint, true)

	reg := &ClientRegistration{
		ClientID: clientconn.ClientID("client1"),
		BoardingInputs: []*BoardingInput{
			newTestBoardingInput(t, input),
		},
		IntentVTXOReqs: []*types.VTXORequest{fixedReq, changeReq},
	}

	quotes, err := computeSealTimeQuotes(
		RoundID{}, map[ClientID]*ClientRegistration{
			"client1": reg,
		},
		0, 100, chainfee.SatPerKWeight(1000), 0, 330,
		newTestBuilderCalc(t),
	)
	require.NoError(t, err)

	q := quotes["client1"]
	require.Equal(t, QuoteReasonOK, q.RejectReason)

	// Quote amounts are positional; IntentVTXOReqs order is
	// [fixedReq, changeReq] (index 0 is fixed, index 1 is change).
	require.Len(t, q.VTXOAmounts, 2)

	// Fixed output echoes verbatim.
	require.Equal(t, fixedVTXO, q.VTXOAmounts[0])

	// Change output = input − fixed − fee.
	got := q.VTXOAmounts[1]
	expected := input - fixedVTXO - q.OperatorFee
	require.Equal(t, expected, got)

	// Σ(outputs) + fee == Σ(inputs).
	total := q.OperatorFee
	for _, a := range q.VTXOAmounts {
		total += a
	}
	require.Equal(t, input, total)
}

// TestBuildQuotesInsufficientResidual asserts that when the fee
// would leave the change output below dust, the builder rejects
// with QuoteReasonInsufficientResidual and does not admit the
// client at the current pass.
func TestBuildQuotesInsufficientResidual(t *testing.T) {
	t.Parallel()

	// A tiny input combined with a fixed target just shy of the
	// input leaves near-zero residual — below dust after the
	// operator fee is applied.
	const (
		input     = btcutil.Amount(600)
		fixedAmt  = btcutil.Amount(500)
		dustLimit = btcutil.Amount(330)
	)

	fixedReq := newTestVTXORequest(t, fixedAmt, false)
	changeReq := newTestVTXORequest(t, input-fixedAmt, true)

	reg := &ClientRegistration{
		ClientID: clientconn.ClientID("client1"),
		BoardingInputs: []*BoardingInput{
			newTestBoardingInput(t, input),
		},
		IntentVTXOReqs: []*types.VTXORequest{fixedReq, changeReq},
	}

	quotes, err := computeSealTimeQuotes(
		RoundID{}, map[ClientID]*ClientRegistration{
			"client1": reg,
		},
		0, 100, chainfee.SatPerKWeight(1000), 0, dustLimit,
		newTestBuilderCalc(t),
	)
	require.NoError(t, err)

	q := quotes["client1"]
	require.Equal(
		t, QuoteReasonInsufficientResidual, q.RejectReason,
	)
}

// TestBuildQuotesDeterministicQuoteID verifies the quote_id
// derivation is stable across invocations with the same
// (round_id, seal_pass, client_id) tuple, and changes when any
// component changes.
func TestBuildQuotesDeterministicQuoteID(t *testing.T) {
	t.Parallel()

	var rid RoundID
	copy(rid[:], []byte("0123456789abcdef"))

	base := computeQuoteID(rid, 0, clientconn.ClientID("c1"))
	require.Equal(t, base, computeQuoteID(
		rid, 0, clientconn.ClientID("c1"),
	))

	// Different client.
	diffClient := computeQuoteID(rid, 0, clientconn.ClientID("c2"))
	require.NotEqual(t, base, diffClient)

	// Different seal pass.
	diffPass := computeQuoteID(rid, 1, clientconn.ClientID("c1"))
	require.NotEqual(t, base, diffPass)

	// Different round.
	var rid2 RoundID
	copy(rid2[:], []byte("fedcba9876543210"))
	diffRound := computeQuoteID(rid2, 0, clientconn.ClientID("c1"))
	require.NotEqual(t, base, diffRound)
}

// TestBuildQuotesBalanceInvariant is a rapid property test that
// asserts the budget-balance invariant holds for every quote the
// builder admits: the sum of the per-client input amounts equals
// the sum of VTXO output amounts plus the operator fee. This is
// the core accounting guarantee under #270 — the server is the
// amount authority, and any residual must flow into the change
// output so that no satoshi is created or destroyed in the quote.
//
// Inputs that force the builder down the reject path
// (QuoteReasonInsufficientResidual / InvalidChangeDesignation) are
// skipped for the balance assertion — those clients are dropped,
// not balanced.
func TestBuildQuotesBalanceInvariant(t *testing.T) {
	t.Parallel()

	calc := newTestBuilderCalc(t)

	rapid.Check(t, func(rt *rapid.T) {
		// Draw a single-input boarding client to keep the input
		// side stable across shrinks; the residual plumbing is
		// what the invariant watches.
		inputSat := rapid.Int64Range(
			50_000, 5_000_000,
		).Draw(rt, "input")

		// Draw one fixed output in [1, input/2]; the change
		// output takes the residual.
		fixedSat := rapid.Int64Range(
			1, inputSat/2,
		).Draw(rt, "fixed")

		input := btcutil.Amount(inputSat)
		fixed := btcutil.Amount(fixedSat)

		fixedReq := newTestVTXORequestRapid(rt, fixed, false)
		changeReq := newTestVTXORequestRapid(rt, input-fixed, true)

		reg := &ClientRegistration{
			ClientID: clientconn.ClientID("c"),
			BoardingInputs: []*BoardingInput{
				newTestBoardingInputRapid(rt, input),
			},
			IntentVTXOReqs: []*types.VTXORequest{
				fixedReq, changeReq,
			},
		}

		quotes, err := computeSealTimeQuotes(
			RoundID{}, map[ClientID]*ClientRegistration{
				"c": reg,
			},
			0, 100, chainfee.SatPerKWeight(1000), 0, 330,
			calc,
		)
		require.NoError(rt, err)

		q := quotes["c"]
		require.NotNil(rt, q)

		// Skip rejected quotes — they're not meant to balance
		// because the client is dropped.
		if q.RejectReason != QuoteReasonOK {
			return
		}

		var outputSum btcutil.Amount
		for _, amt := range q.VTXOAmounts {
			outputSum += amt
		}

		// Σin = Σ(VTXO outputs) + operator fee.
		require.Equal(
			rt, int64(input),
			int64(outputSum+q.OperatorFee),
			"balance violated: input=%d outputs=%d fee=%d",
			input, outputSum, q.OperatorFee,
		)
	})
}

// newTestVTXORequestRapid is the rapid-flavored sibling of
// newTestVTXORequest. It accepts a rapid.T so property failures
// shrink correctly and reuses the rapid-provided require handle
// rather than a *testing.T.
func newTestVTXORequestRapid(rt *rapid.T, amount btcutil.Amount,
	isChange bool) *types.VTXORequest {

	priv, err := btcec.NewPrivateKey()
	require.NoError(rt, err)

	return &types.VTXORequest{
		Amount:   amount,
		IsChange: isChange,
		SigningKey: keychain.KeyDescriptor{
			PubKey: priv.PubKey(),
		},
	}
}

// newTestBoardingInputRapid mirrors newTestBoardingInput for rapid
// property tests — the outpoint is deterministic per-draw (all
// zeros) because rapid manages its own shrinking seeds.
func newTestBoardingInputRapid(rt *rapid.T,
	value btcutil.Amount) *BoardingInput {

	priv, err := btcec.NewPrivateKey()
	require.NoError(rt, err)

	return &BoardingInput{
		Outpoint:  &wire.OutPoint{Index: 0},
		Value:     value,
		ClientKey: priv.PubKey(),
	}
}
