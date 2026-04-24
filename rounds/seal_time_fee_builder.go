// Seal-time fee builder for the #270 operator-decides-at-seal-time
// protocol. The pure function computeSealTimeQuotes takes the
// post-intent registration set and the live market inputs (fee rate,
// treasury utilization, chain height) and returns one Quote per
// client. Each Quote either carries binding per-output amounts and a
// final operator_fee (QuoteReasonOK), or a RejectReason explaining
// why the client was dropped for the pass (e.g. residual would go
// below dust, no valid change designation, etc.).
//
// The builder is the sole authority on fee math in the seal-time
// flow: it replaces the submit-time `validateOperatorFee` path. It
// is intentionally decoupled from the FSM (takes plain data,
// returns plain data) so it can be golden-fixture tested without
// wiring a full actor.
package rounds

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// QuoteReason classifies the outcome of computeSealTimeQuotes for a
// single client. Mirrors roundpb.QuoteReason so the Quote struct can
// be translated directly into the wire message without an enum
// re-derivation step.
type QuoteReason uint8

const (
	// QuoteReasonOK is the success case; the quote carries binding
	// amounts and an operator_fee the client must accept.
	QuoteReasonOK QuoteReason = 0

	// QuoteReasonInsufficientResidual means the change output
	// residual would go negative or below dust after deducting the
	// operator fee from Σin − Σ(fixed targets).
	QuoteReasonInsufficientResidual QuoteReason = 1

	// QuoteReasonInvalidChangeDesignation means the intent did not
	// carry exactly one IsChange marker (zero or two-plus), and the
	// total output count was not 1 (which would make the change
	// designation implicit).
	QuoteReasonInvalidChangeDesignation QuoteReason = 2
)

// String returns a human-readable representation of QuoteReason.
func (r QuoteReason) String() string {
	switch r {
	case QuoteReasonOK:
		return "ok"
	case QuoteReasonInsufficientResidual:
		return "insufficient_residual"
	case QuoteReasonInvalidChangeDesignation:
		return "invalid_change_designation"
	default:
		return "unknown"
	}
}

// FeeBreakdown is the server-side mirror of roundpb.FeeBreakdown.
// Carries the itemized fee decomposition so the client can validate
// the operator's bookkeeping and log the numbers for observability.
type FeeBreakdown struct {
	// ChainFeeSat is the on-chain share of the fee (per-input share
	// of the commitment-tx broadcast cost).
	ChainFeeSat int64

	// LiquidityFeeSat is the time-value-of-money component (non-zero
	// for forfeit inputs, zero for fresh boardings).
	LiquidityFeeSat int64

	// CongestionFeeSat is the treasury-utilization-driven spread.
	CongestionFeeSat int64

	// FeeRateSatKw is the on-chain fee rate the server saw at seal
	// time, in sat/kW.
	FeeRateSatKw int64

	// BatchSize is the number of clients the server sized the
	// on-chain share against for this pass.
	BatchSize uint32
}

// Quote is the per-client result of the seal-time fee builder. It
// carries the binding per-output amounts, the total operator_fee the
// client must accept, the quote_id that scopes every downstream
// response to this pass, and a RejectReason of non-OK when the
// client's intent could not be admitted.
type Quote struct {
	// ClientID identifies the client this quote is addressed to.
	ClientID ClientID

	// QuoteID is the 32-byte BLAKE3 hash of
	// (round_id || seal_pass || client_id). Bound to a specific
	// pass/client pair so stale responses after a reseal can be
	// dropped at the FSM boundary.
	QuoteID [32]byte

	// SealPass is the zero-indexed reseal pass number this quote
	// belongs to. Echoed into the wire message so clients can log
	// the pass they are responding to.
	SealPass uint32

	// VTXOAmounts is the positional binding amount (sats) for each
	// VTXORequest in the client's intent, indexed by the same order
	// as reg.IntentVTXOReqs. Honors target_amount_sat for non-change
	// outputs and carries the change residual for the designated
	// change output. Position-indexed (not key-indexed) so the proto
	// wire encoding is deterministic and the client's positional
	// validation path lines up with the server's fan-out.
	VTXOAmounts []btcutil.Amount

	// LeaveAmounts is the positional binding amount (sats) for each
	// LeaveRequest in the client's intent, indexed the same way as
	// reg.IntentLeaveReqs. Same semantics as VTXOAmounts.
	LeaveAmounts []btcutil.Amount

	// OperatorFee is the total operator fee (sats) summed across
	// this client's boarding and forfeit inputs.
	OperatorFee btcutil.Amount

	// Breakdown itemizes OperatorFee. Populated on QuoteReasonOK.
	Breakdown FeeBreakdown

	// RejectReason is QuoteReasonOK when the quote is binding, or a
	// non-OK classifier when the client's intent was dropped at
	// seal time. Reject quotes do not populate VTXOAmounts /
	// LeaveAmounts / OperatorFee.
	RejectReason QuoteReason
}

// isOK reports whether the quote admits the client.
func (q *Quote) isOK() bool {
	return q.RejectReason == QuoteReasonOK
}

// computeQuoteID derives a 32-byte quote identifier from
// (round_id || seal_pass || client_id). sha256 is used for the
// derivation: stable across restarts (deterministic from inputs) and
// already a project dependency, so we avoid pulling in a second hash
// library just for this purpose.
func computeQuoteID(roundID RoundID, sealPass uint32,
	clientID ClientID) [32]byte {

	var passBuf [4]byte
	binary.BigEndian.PutUint32(passBuf[:], sealPass)

	h := sha256.New()
	h.Write(roundID[:])
	h.Write(passBuf[:])
	h.Write([]byte(clientID))

	var out [32]byte
	copy(out[:], h.Sum(nil))

	return out
}

// computeSealTimeQuotes computes one Quote per registered client.
// Pure function: takes plain inputs, returns plain outputs. The
// caller is responsible for threading live fee-rate + utilization +
// chain-height + seal-pass from the round's Environment.
//
// Per-client algorithm:
//  1. Validate the intent carries exactly one IsChange marker (across
//     VTXORequests + LeaveRequests), OR the total output count is
//     exactly 1 (implicit change). Otherwise emit
//     QuoteReasonInvalidChangeDesignation.
//  2. Compute per-boarding fee via FeeCalculator.ComputeBoardingFee
//     and per-forfeit fee via ComputeForfeitFee using
//     (BatchExpiry − currentHeight) as the remaining-blocks input.
//     Sum them into OperatorFee.
//  3. Compute residual = Σin − Σ(fixed target amounts) −
//     OperatorFee. If residual < dust OR residual < 0 emit
//     QuoteReasonInsufficientResidual.
//  4. Populate VTXOAmounts / LeaveAmounts: non-change entries echo
//     the intent's target; the single change entry takes residual.
//  5. Derive QuoteID = BLAKE3(round_id || seal_pass || client_id).
//
// dustLimit is the environment's configured dust threshold
// (terms.DustLimit). batchSize is the number of clients participating
// in this seal pass — used as the divisor for per-input on-chain
// share.
func computeSealTimeQuotes(
	roundID RoundID,
	regs map[ClientID]*ClientRegistration,
	sealPass uint32,
	currentHeight uint32,
	feeRate chainfee.SatPerKWeight,
	utilization float64,
	dustLimit btcutil.Amount,
	calc *fees.Calculator,
) (map[ClientID]*Quote, error) {

	if calc == nil {
		return nil, fmt.Errorf("seal-time quote builder requires " +
			"a non-nil fee calculator")
	}

	batchSize := len(regs)
	if batchSize < 1 {
		batchSize = 1
	}

	out := make(map[ClientID]*Quote, len(regs))
	for cid, reg := range regs {
		quote := quoteForClient(
			roundID, sealPass, reg, batchSize,
			currentHeight, feeRate, utilization,
			dustLimit, calc,
		)
		quote.ClientID = cid
		quote.QuoteID = computeQuoteID(roundID, sealPass, cid)
		quote.SealPass = sealPass
		out[cid] = quote
	}

	return out, nil
}

// quoteForClient applies the computeSealTimeQuotes algorithm to a
// single registration. Broken out so unit tests can drive one client
// at a time without constructing a full map.
func quoteForClient(roundID RoundID, sealPass uint32,
	reg *ClientRegistration, batchSize int, currentHeight uint32,
	feeRate chainfee.SatPerKWeight, utilization float64,
	dustLimit btcutil.Amount, calc *fees.Calculator) *Quote {

	_ = roundID
	_ = sealPass

	// Validate change-output designation: exactly one IsChange
	// marker across VTXORequests + LeaveRequests, OR total output
	// count == 1 (implicit change). Anything else is a dead client
	// for this pass.
	changeDesignation, ok := resolveChangeDesignation(reg)
	if !ok {
		return &Quote{
			RejectReason: QuoteReasonInvalidChangeDesignation,
		}
	}

	// Sum per-input operator fee components. Boarding inputs carry
	// no liquidity cost (user brought on-chain BTC). Forfeit inputs
	// carry full liquidity + on-chain + margin + congestion.
	var (
		totalFee         int64
		totalChain       int64
		totalLiquidity   int64
		totalCongestion  int64
		totalInputValue  btcutil.Amount
		totalFixedOutput btcutil.Amount
	)

	for _, boarding := range reg.BoardingInputs {
		totalInputValue += boarding.Value
		bd := calc.ComputeBoardingFee(
			int64(boarding.Value), batchSize, feeRate,
		)
		totalFee += bd.TotalFeeSat
		totalChain += bd.OnChainShareSat
	}

	for _, forfeit := range reg.ForfeitInputs {
		if forfeit == nil || forfeit.VTXO == nil {
			continue
		}

		amt := forfeit.VTXO.Descriptor.Amount
		totalInputValue += amt

		// Clamp remaining-blocks at zero when the VTXO has already
		// passed its batch expiry; Calculator applies its own
		// δ_min floor on top so lazy refreshes still pay.
		var remaining uint32
		if forfeit.VTXO.BatchExpiry > currentHeight {
			remaining = forfeit.VTXO.BatchExpiry - currentHeight
		}

		bd := calc.ComputeForfeitFee(
			int64(amt), batchSize, remaining,
			feeRate, utilization,
		)
		totalFee += bd.TotalFeeSat
		totalChain += bd.OnChainShareSat
		totalLiquidity += bd.LiquidityFeeSat
		// Congestion is not a standalone breakdown field on fees
		// today; derive an approximation from Total - (OnChain +
		// Margin + Liquidity) so the proto breakdown stays
		// informative without requiring a schema change in fees/.
		congestion := bd.TotalFeeSat -
			(bd.OnChainShareSat + bd.MarginSat +
				bd.LiquidityFeeSat)
		if congestion < 0 {
			congestion = 0
		}
		totalCongestion += congestion
	}

	// Sum fixed (non-change) target amounts across both output
	// channels. The change entry is zeroed below and refilled with
	// the residual.
	for i, vr := range reg.IntentVTXOReqs {
		if changeDesignation.isVTXO && changeDesignation.idx == i {
			continue
		}
		totalFixedOutput += vr.Amount
	}

	for i, lr := range reg.IntentLeaveReqs {
		if !changeDesignation.isVTXO && changeDesignation.idx == i {
			continue
		}
		if lr != nil && lr.Output != nil {
			totalFixedOutput += btcutil.Amount(lr.Output.Value)
		}
	}

	// Residual for the change output = Σin − Σ(fixed) − fee. Must
	// be non-negative and above dust to admit the client.
	residualSat := int64(totalInputValue) -
		int64(totalFixedOutput) - totalFee
	if residualSat < 0 || btcutil.Amount(residualSat) < dustLimit {
		return &Quote{
			RejectReason: QuoteReasonInsufficientResidual,
		}
	}

	// Build the per-output positional amount slices, echoing intent
	// targets for fixed entries and stamping the residual on the
	// change entry. The slices align with reg.IntentVTXOReqs and
	// reg.IntentLeaveReqs respectively so the proto wire encoding
	// and the client-side positional validation are deterministic.
	vtxoAmounts := make(
		[]btcutil.Amount, len(reg.IntentVTXOReqs),
	)
	for i, vr := range reg.IntentVTXOReqs {
		amt := vr.Amount
		if changeDesignation.isVTXO && changeDesignation.idx == i {
			amt = btcutil.Amount(residualSat)
		}
		vtxoAmounts[i] = amt
	}

	leaveAmounts := make(
		[]btcutil.Amount, len(reg.IntentLeaveReqs),
	)
	for i, lr := range reg.IntentLeaveReqs {
		if lr == nil || lr.Output == nil {
			continue
		}
		amt := btcutil.Amount(lr.Output.Value)
		if !changeDesignation.isVTXO &&
			changeDesignation.idx == i {

			amt = btcutil.Amount(residualSat)
		}
		leaveAmounts[i] = amt
	}

	return &Quote{
		VTXOAmounts:  vtxoAmounts,
		LeaveAmounts: leaveAmounts,
		OperatorFee:  btcutil.Amount(totalFee),
		Breakdown: FeeBreakdown{
			ChainFeeSat:      totalChain,
			LiquidityFeeSat:  totalLiquidity,
			CongestionFeeSat: totalCongestion,
			FeeRateSatKw:     int64(feeRate),
			BatchSize:        uint32(batchSize),
		},
		RejectReason: QuoteReasonOK,
	}
}

// changeDesignation pinpoints the one output in the client's intent
// that receives the residual (Σin − Σ(fixed) − fee). isVTXO toggles
// between the two channels; idx is the positional index within that
// channel.
type changeDesignation struct {
	isVTXO bool
	idx    int
}

// resolveChangeDesignation applies the #270 intent-change-output
// rule:
//   - Exactly one IsChange=true marker across VTXORequests +
//     LeaveRequests, OR
//   - Total output count == 1 (implicit change on the single entry).
//
// Returns (designation, true) on a valid intent, or (zero, false)
// when the intent is malformed.
func resolveChangeDesignation(
	reg *ClientRegistration) (changeDesignation, bool) {

	numVTXO := len(reg.IntentVTXOReqs)
	numLeave := len(reg.IntentLeaveReqs)
	totalOutputs := numVTXO + numLeave

	// Implicit change: the single output is the change by default.
	if totalOutputs == 1 {
		if numVTXO == 1 {
			return changeDesignation{isVTXO: true, idx: 0}, true
		}
		return changeDesignation{isVTXO: false, idx: 0}, true
	}

	// Explicit change: scan both channels for exactly one marker.
	var (
		found bool
		sel   changeDesignation
	)

	for i, vr := range reg.IntentVTXOReqs {
		if vr == nil || !vr.IsChange {
			continue
		}
		if found {
			return changeDesignation{}, false
		}
		sel = changeDesignation{isVTXO: true, idx: i}
		found = true
	}

	for i, lr := range reg.IntentLeaveReqs {
		if lr == nil || !lr.IsChange {
			continue
		}
		if found {
			return changeDesignation{}, false
		}
		sel = changeDesignation{isVTXO: false, idx: i}
		found = true
	}

	if !found {
		return changeDesignation{}, false
	}

	return sel, true
}

// signingKeyVertex extracts the 33-byte serialized signing pubkey
// from a VTXORequest and returns it as the SigningKeyHex map key
// used throughout the package. Mirrors the helper in validation.go.
func signingKeyVertex(vr *types.VTXORequest) SigningKeyHex {
	if vr == nil || vr.SigningKey.PubKey == nil {
		return SigningKeyHex{}
	}

	var key SigningKeyHex
	copy(key[:], vr.SigningKey.PubKey.SerializeCompressed())

	return key
}
