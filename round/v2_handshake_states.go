package round

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// ErrV2HandlerNotImplemented is returned by the v2 client FSM
// handlers while the protocol lands in stages. Each follow-up
// commit replaces a returns-err handler with the real transition
// logic; until then a v2 event arriving on a code path that has not
// been wired surfaces the error rather than silently falling through
// to the v1 RegistrationSentState path.
var ErrV2HandlerNotImplemented = errors.New(
	"v2 handshake: client handler not yet implemented on this branch",
)

// ErrV2QuoteFeeExceedsCap is returned by the quote-policy checker
// when the server-authored operator fee is above the client's
// configured MaxOperatorFee. The quote is rejected and the FSM
// transitions toward a JoinRoundReject emission.
var ErrV2QuoteFeeExceedsCap = errors.New(
	"v2 handshake: authored operator fee exceeds local MaxOperatorFee " +
		"cap",
)

// ErrV2QuoteExpired is returned when the server-authored quote's
// quote_expires_at_unix timestamp has already passed. Rejecting
// the quote protects the client from signing a round the operator
// has already treated as an implicit reject.
var ErrV2QuoteExpired = errors.New(
	"v2 handshake: authored quote window has already expired",
)

// ErrV2QuoteProtocolMismatch is returned when the server's
// authored quote carries a lower protocol version than the client
// asked for and the downgrade is not acceptable. Under the
// minimum-viable v2 policy the client refuses any downgrade.
var ErrV2QuoteProtocolMismatch = errors.New(
	"v2 handshake: server-authored quote protocol version lower than " +
		"client-requested",
)

// V2MinProtocolVersion is the lowest v2 protocol version the
// client understands. The client-authored intent sets this on the
// outbound JoinRoundIntent; the server's returned quote must
// match or the client rejects the handshake.
const V2MinProtocolVersion uint32 = 2

// quotePolicyNow is the wall-clock source for quote-expiry
// checks. Injectable for tests via SetQuotePolicyNow; defaults
// to time.Now.
var quotePolicyNow = time.Now

// SetQuotePolicyNow overrides the wall-clock used by
// ValidateQuotePolicy for deterministic testing. Returns a
// restore function the caller defers to reset the global.
//
// Tests that exercise the quote-expiry branch inject a fixed
// clock so the assertion is not subject to the real time of day.
// Production callers never touch this helper.
func SetQuotePolicyNow(fn func() time.Time) func() {
	prev := quotePolicyNow
	quotePolicyNow = fn

	return func() {
		quotePolicyNow = prev
	}
}

// ValidateQuotePolicy applies the client's local accept/reject
// policy to a server-authored JoinRoundQuote. Under issue #270
// the client has no direct control over VTXO output amounts --
// the server is the sole fee author -- so the policy surface is
// narrow: enforce the fee cap, reject stale quotes, and reject
// downgraded protocol versions. Each rejection returns a typed
// sentinel error so the caller can map reasons onto the
// JoinRoundReject.reason field.
//
// Contract: returns nil when the quote is acceptable; otherwise
// returns one of ErrV2QuoteFeeExceedsCap / ErrV2QuoteExpired /
// ErrV2QuoteProtocolMismatch wrapping the offending field so the
// caller can log the decision with structured detail.
func ValidateQuotePolicy(quote *types.JoinRoundQuote,
	maxOperatorFee btcutil.Amount) error {

	if quote == nil {
		return fmt.Errorf(
			"v2 handshake: cannot validate a nil quote",
		)
	}

	if quote.ProtocolVersion < V2MinProtocolVersion {
		return fmt.Errorf(
			"%w: got version %d, minimum %d",
			ErrV2QuoteProtocolMismatch,
			quote.ProtocolVersion, V2MinProtocolVersion,
		)
	}

	// Fee cap: the server-authored operator fee must not exceed
	// the client's configured upper bound. Replaces the legacy v1
	// pre-flight check (transitions.go:383) which compared an
	// implicit fee against MinOperatorFee / MaxOperatorFee; under
	// v2 the client only has the max-cap side of the check since
	// the operator-authored fee cannot be below the operator's
	// own expectation.
	if quote.OperatorFeeSat > maxOperatorFee {
		return fmt.Errorf(
			"%w: authored %d sats, cap %d sats",
			ErrV2QuoteFeeExceedsCap,
			quote.OperatorFeeSat, maxOperatorFee,
		)
	}

	if quote.QuoteExpiresAtUnix > 0 {
		expiry := time.Unix(quote.QuoteExpiresAtUnix, 0)
		if !quotePolicyNow().Before(expiry) {
			return fmt.Errorf(
				"%w: expired at %s",
				ErrV2QuoteExpired, expiry.Format(
					time.RFC3339,
				),
			)
		}
	}

	return nil
}

// IntentSentState is the v2 client analogue of RegistrationSentState.
// The client has sent a JoinRoundIntent (Phase 1) and is waiting for
// the server to seal registration and dispatch a JoinRoundQuote
// (Phase 2). No VTXO amounts are committed at this stage; the server
// authors them at seal-time and returns them in the quote.
//
// Why a separate state from RegistrationSentState: under v1 the
// client's next inbound is ClientSuccessResp + ClientBatchInfo (the
// commitment PSBT that already embeds amounts the client committed
// to). Under v2 the next inbound is JoinRoundQuote, which carries
// server-authored amounts the client must validate against its local
// MaxOperatorFee policy before signing. Keeping the two states
// distinct lets the v1 and v2 dispatch tables stay non-overlapping.
type IntentSentState struct {
	// Intent is the JoinRoundIntent the client sent. Held so the
	// QuoteReceivedState handler can correlate the inbound quote
	// against the request and reject mismatches.
	Intent *types.JoinRoundIntent
}

// String returns the state's human-readable name.
func (s *IntentSentState) String() string {
	return "IntentSent"
}

// IsTerminal returns false; IntentSent transitions to QuoteReceived
// (or ClientFailed) on quote arrival.
func (s *IntentSentState) IsTerminal() bool {
	return false
}

// clientStateSealed marks IntentSentState as implementing the sealed
// ClientState interface.
func (s *IntentSentState) clientStateSealed() {}

// QuoteReceivedState is the decision point under the v2 handshake:
// the client has received a JoinRoundQuote with server-authored VTXO
// amounts and an itemized fee. The transition handler validates the
// quote against the local policy:
//
//   - operator_fee_sat <= MaxOperatorFee (configurable cap)
//   - quote_expires_at_unix in the future
//   - vtxo_outputs match the original VTXOIntents on signing_key +
//     policy_template
//
// If valid the handler emits an outbound JoinRoundCommit (or
// proceeds straight to NoncesSent for an implicit commit) and
// transitions onward through the existing v1 batch path with the
// server-authored amounts. If invalid the handler emits a
// JoinRoundReject and transitions to ClientFailed.
type QuoteReceivedState struct {
	// Intent is the matching JoinRoundIntent the client sent.
	Intent *types.JoinRoundIntent

	// Quote is the server-authored quote awaiting decision.
	Quote *types.JoinRoundQuote
}

// String returns the state's human-readable name.
func (s *QuoteReceivedState) String() string {
	return "QuoteReceived"
}

// IsTerminal returns false.
func (s *QuoteReceivedState) IsTerminal() bool {
	return false
}

// clientStateSealed marks QuoteReceivedState as implementing the
// sealed ClientState interface.
func (s *QuoteReceivedState) clientStateSealed() {}

// --------------------------------------------------------------------------
// ProcessEvent stubs.
//
// Same rationale as the server side: stubs exist so the v2 states
// satisfy the protofsm.State interface, and a v2 event hitting an
// unwired code path returns ErrV2HandlerNotImplemented rather than
// silently falling through. The real transition logic
// (intent-builder, quote validator, accept/reject) lands in
// follow-up commits on this branch.
// --------------------------------------------------------------------------

// ProcessEvent for IntentSentState. Future implementation routes
// QuoteReceivedEvent into a transition to QuoteReceivedState; until
// then the handler returns ErrV2HandlerNotImplemented.
func (s *IntentSentState) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (
	*ClientStateTransition, error) {

	return nil, ErrV2HandlerNotImplemented
}

// ProcessEvent for QuoteReceivedState. Future implementation
// validates the quote against MaxOperatorFee + expiry and either
// transitions to NoncesSentState (accept) or emits JoinRoundReject
// + ClientFailedState (decline).
func (s *QuoteReceivedState) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (
	*ClientStateTransition, error) {

	return nil, ErrV2HandlerNotImplemented
}

// Compile-time assertions that the v2 states satisfy the sealed
// ClientState interface.
var (
	_ ClientState = (*IntentSentState)(nil)
	_ ClientState = (*QuoteReceivedState)(nil)
)
