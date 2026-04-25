package rounds

import (
	"fmt"
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

// newOKBoardingReg constructs a boarding-only registration with a
// single fixed-target VTXO and an explicit change VTXO. Returns a
// well-formed reg that quotes OK at any reasonable batch size.
func newOKBoardingReg(t *testing.T, cid ClientID,
	input, fixed btcutil.Amount) *ClientRegistration {

	t.Helper()

	require.Greater(t, fixed, btcutil.Amount(0))
	require.Greater(t, input, fixed)

	return &ClientRegistration{
		ClientID: cid,
		BoardingInputs: []*BoardingInput{
			newTestBoardingInput(t, input),
		},
		IntentVTXOReqs: []*types.VTXORequest{
			newTestVTXORequest(t, fixed, false),
			newTestVTXORequest(t, input-fixed, true),
		},
	}
}

// TestBuildQuotesRecomputesAfterPrune is the regression test for the
// codex P1 finding: when computeSealTimeQuotes drops a client with
// QuoteReasonInvalidChangeDesignation, the survivors must have their
// fees recomputed against the smaller batch size. Otherwise the
// admitted clients keep on-chain shares divided by the inflated
// pre-prune count and undercharge.
func TestBuildQuotesRecomputesAfterPrune(t *testing.T) {
	t.Parallel()

	calc := newTestBuilderCalc(t)

	// Two well-formed clients plus one client with no IsChange
	// markers across multi-output intents — the third client fails
	// QuoteReasonInvalidChangeDesignation and is dropped.
	const (
		input = btcutil.Amount(1_000_000)
		fixed = btcutil.Amount(100_000)
	)
	regs := map[ClientID]*ClientRegistration{
		"a": newOKBoardingReg(t, "a", input, fixed),
		"b": newOKBoardingReg(t, "b", input, fixed),
		"c": {
			ClientID: "c",
			BoardingInputs: []*BoardingInput{
				newTestBoardingInput(t, input),
			},
			// No IsChange marker on either output → invalid
			// designation, dropped on first pass.
			IntentVTXOReqs: []*types.VTXORequest{
				newTestVTXORequest(t, fixed, false),
				newTestVTXORequest(t, input-fixed, false),
			},
		},
	}

	quotes, err := computeSealTimeQuotes(
		RoundID{}, regs,
		0, 100, chainfee.SatPerKWeight(1000), 0, 330, calc,
	)
	require.NoError(t, err)
	require.Len(t, quotes, 3)

	// Dropped client carries the invalid-designation reason.
	require.Equal(
		t, QuoteReasonInvalidChangeDesignation,
		quotes["c"].RejectReason,
	)

	// Surviving clients are OK and their breakdown's BatchSize
	// reflects the post-prune count of 2, not the original 3.
	for _, cid := range []ClientID{"a", "b"} {
		q := quotes[cid]
		require.Equalf(
			t, QuoteReasonOK, q.RejectReason,
			"client %s should have OK reason", cid,
		)
		require.Equalf(
			t, uint32(2), q.Breakdown.BatchSize,
			"client %s should have BatchSize=2 after prune, "+
				"got %d", cid, q.Breakdown.BatchSize,
		)
	}

	// Expected on-chain share at batchSize=2 must equal the actual
	// on-chain share charged on each survivor's quote — i.e. the
	// survivors are NOT undercharged at batchSize=3.
	bdAtTwo := calc.ComputeBoardingFee(
		int64(input), 2, chainfee.SatPerKWeight(1000),
	)
	require.Equal(
		t, bdAtTwo.OnChainShareSat,
		quotes["a"].Breakdown.ChainFeeSat,
	)
	require.Equal(
		t, bdAtTwo.OnChainShareSat,
		quotes["b"].Breakdown.ChainFeeSat,
	)

	// Sanity: the inflated batchSize=3 share would have been
	// strictly smaller. Without the recompute fix the test would
	// observe that smaller value and fail.
	bdAtThree := calc.ComputeBoardingFee(
		int64(input), 3, chainfee.SatPerKWeight(1000),
	)
	require.Less(
		t, bdAtThree.OnChainShareSat, bdAtTwo.OnChainShareSat,
	)
}

// TestBuildQuotesCascadingResidualFailure exercises the iterative
// convergence: a client that is OK at the initial batch size but
// fails QuoteReasonInsufficientResidual once a peer drops out and
// the per-client fee rises. The builder must pick that failure up
// on the second pass and not silently admit the client at a fee it
// can no longer afford.
func TestBuildQuotesCascadingResidualFailure(t *testing.T) {
	t.Parallel()

	calc := newTestBuilderCalc(t)

	// Pre-compute the per-client on-chain share at batchSize=3
	// vs batchSize=2 so we can pick a marginal client whose
	// residual is positive at N=3 but negative at N=2.
	const (
		input    = btcutil.Amount(20_000)
		feeRate  = chainfee.SatPerKWeight(10_000)
		dustSat  = btcutil.Amount(330)
		baseRoom = btcutil.Amount(500)
	)
	bdThree := calc.ComputeBoardingFee(int64(input), 3, feeRate)
	bdTwo := calc.ComputeBoardingFee(int64(input), 2, feeRate)
	require.Greater(
		t, bdTwo.OnChainShareSat, bdThree.OnChainShareSat,
		"sanity: batchSize=2 charges more on-chain share",
	)

	// fixedAtThreeMargin = input - fee(3) - small slack > dust
	// → OK at batchSize=3.
	// At batchSize=2 the same fixed target leaves residual <
	// fee(2) − fee(3) of slack → flips below dust → rejected.
	feeThree := btcutil.Amount(bdThree.TotalFeeSat)
	feeTwo := btcutil.Amount(bdTwo.TotalFeeSat)
	require.Greater(
		t, feeTwo, feeThree,
		"sanity: batchSize=2 charges more total fee",
	)

	feeBump := feeTwo - feeThree
	require.Greater(
		t, feeBump, btcutil.Amount(0),
	)

	// The marginal client's residual at batchSize=3 is
	// `baseRoom`; at batchSize=2 it is `baseRoom - feeBump`.
	// Pick fixed so this is positive at N=3 and below dust at N=2.
	require.Greater(t, feeBump+dustSat, baseRoom,
		"test fixture must collapse below dust on the smaller "+
			"batch")
	fixed := input - feeThree - baseRoom

	// Marginal client.
	marginal := newOKBoardingReg(t, "marginal", input, fixed)

	// One client that always passes (lots of slack).
	healthy := newOKBoardingReg(
		t, "healthy", btcutil.Amount(2_000_000),
		btcutil.Amount(100_000),
	)

	// One client that immediately fails invalid-designation,
	// causing the first prune that triggers the recompute pass.
	bad := &ClientRegistration{
		ClientID: "bad",
		BoardingInputs: []*BoardingInput{
			newTestBoardingInput(t, input),
		},
		IntentVTXOReqs: []*types.VTXORequest{
			newTestVTXORequest(t, fixed, false),
			newTestVTXORequest(t, input-fixed, false),
		},
	}

	regs := map[ClientID]*ClientRegistration{
		"healthy":  healthy,
		"marginal": marginal,
		"bad":      bad,
	}

	quotes, err := computeSealTimeQuotes(
		RoundID{}, regs,
		0, 100, feeRate, 0, dustSat, calc,
	)
	require.NoError(t, err)
	require.Len(t, quotes, 3)

	// bad: dropped on pass 1 with invalid designation.
	require.Equal(
		t, QuoteReasonInvalidChangeDesignation,
		quotes["bad"].RejectReason,
	)

	// marginal: passed pass 1 at batchSize=3, fails pass 2 at
	// batchSize=2 with insufficient residual.
	require.Equal(
		t, QuoteReasonInsufficientResidual,
		quotes["marginal"].RejectReason,
	)

	// healthy: passes both passes; final BatchSize reflects the
	// surviving lone client.
	require.Equal(t, QuoteReasonOK, quotes["healthy"].RejectReason)
	require.Equal(
		t, uint32(1), quotes["healthy"].Breakdown.BatchSize,
	)
}

// TestBuildQuotesBatchSizeSurvivorInvariant is a rapid property
// test for the codex P1 fix: regardless of how many of the input
// intents fail admission, every OK quote must carry a
// Breakdown.BatchSize equal to the count of OK quotes — i.e. the
// fee was sized against the survivor count, not the inflated
// pre-prune count.
func TestBuildQuotesBatchSizeSurvivorInvariant(t *testing.T) {
	t.Parallel()

	calc := newTestBuilderCalc(t)

	rapid.Check(t, func(rt *rapid.T) {
		// Draw 1..5 healthy clients with lots of fee headroom.
		nHealthy := rapid.IntRange(1, 5).Draw(rt, "n_healthy")

		// Draw 0..3 invalid-designation clients (dropped on the
		// first pass).
		nBad := rapid.IntRange(0, 3).Draw(rt, "n_bad")

		regs := make(map[ClientID]*ClientRegistration)
		for i := 0; i < nHealthy; i++ {
			cid := ClientID(fmt.Sprintf("ok-%d", i))
			regs[cid] = newOKBoardingRegRapid(
				rt, cid,
				btcutil.Amount(2_000_000),
				btcutil.Amount(100_000),
			)
		}
		for i := 0; i < nBad; i++ {
			cid := ClientID(fmt.Sprintf("bad-%d", i))
			input := btcutil.Amount(1_000_000)
			regs[cid] = &ClientRegistration{
				ClientID: cid,
				BoardingInputs: []*BoardingInput{
					newTestBoardingInputRapid(rt, input),
				},
				// No IsChange marker → dropped first pass.
				IntentVTXOReqs: []*types.VTXORequest{
					newTestVTXORequestRapid(
						rt, input/2, false,
					),
					newTestVTXORequestRapid(
						rt, input/2, false,
					),
				},
			}
		}

		quotes, err := computeSealTimeQuotes(
			RoundID{}, regs,
			0, 100, chainfee.SatPerKWeight(1000), 0, 330, calc,
		)
		require.NoError(rt, err)
		require.Len(rt, quotes, len(regs))

		// Count OK survivors and assert their BatchSize agrees.
		var okCount uint32
		for _, q := range quotes {
			if q.RejectReason == QuoteReasonOK {
				okCount++
			}
		}

		for cid, q := range quotes {
			if q.RejectReason != QuoteReasonOK {
				continue
			}
			require.Equalf(
				rt, okCount, q.Breakdown.BatchSize,
				"client %s OK quote BatchSize=%d but only "+
					"%d quotes are OK",
				cid, q.Breakdown.BatchSize, okCount,
			)
		}
	})
}

// newOKBoardingRegRapid is the rapid-flavored sibling of
// newOKBoardingReg; it accepts a rapid.T so property failures
// shrink correctly.
func newOKBoardingRegRapid(rt *rapid.T, cid ClientID,
	input, fixed btcutil.Amount) *ClientRegistration {

	return &ClientRegistration{
		ClientID: cid,
		BoardingInputs: []*BoardingInput{
			newTestBoardingInputRapid(rt, input),
		},
		IntentVTXOReqs: []*types.VTXORequest{
			newTestVTXORequestRapid(rt, fixed, false),
			newTestVTXORequestRapid(rt, input-fixed, true),
		},
	}
}
