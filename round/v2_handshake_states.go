package round

import (
	"context"
	"errors"

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
