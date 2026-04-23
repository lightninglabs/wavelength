package round

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/stretchr/testify/require"
)

// TestV2HandshakeStateMetadata locks the basic state contract for
// the v2 client states (String / IsTerminal / clientStateSealed) so
// the protofsm.State interface assertion stays green as future
// commits flesh out the transition logic.
func TestV2HandshakeStateMetadata(t *testing.T) {
	t.Parallel()

	intent := &types.JoinRoundIntent{
		ProtocolVersion: 2,
	}
	quote := &types.JoinRoundQuote{
		ProtocolVersion: 2,
	}

	intentState := &IntentSentState{Intent: intent}
	require.Equal(t, "IntentSent", intentState.String())
	require.False(t, intentState.IsTerminal())

	quoteState := &QuoteReceivedState{Intent: intent, Quote: quote}
	require.Equal(t, "QuoteReceived", quoteState.String())
	require.False(t, quoteState.IsTerminal())
}

// TestV2HandshakeStubsReturnSentinel asserts the placeholder
// ProcessEvent handlers fail closed via ErrV2HandlerNotImplemented.
// Once a real handler lands, the test on that path will be
// rewritten or removed; until then this guarantees no v2 event is
// silently dropped by an unwired transition.
func TestV2HandshakeStubsReturnSentinel(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	intentState := &IntentSentState{}
	_, err := intentState.ProcessEvent(ctx, nil, nil)
	require.ErrorIs(t, err, ErrV2HandlerNotImplemented)

	quoteState := &QuoteReceivedState{}
	_, err = quoteState.ProcessEvent(ctx, nil, nil)
	require.ErrorIs(t, err, ErrV2HandlerNotImplemented)
}

// TestValidateQuotePolicyAccept covers the happy path: authored
// fee below cap, quote fresh, protocol version at the minimum.
// A quote that passes every check returns nil so the caller can
// proceed into signing.
func TestValidateQuotePolicyAccept(t *testing.T) {
	t.Parallel()

	restore := SetQuotePolicyNow(func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	defer restore()

	quote := &types.JoinRoundQuote{
		OperatorFeeSat:     btcutil.Amount(500),
		QuoteExpiresAtUnix: 1_700_000_010,
		ProtocolVersion:    V2MinProtocolVersion,
	}
	err := ValidateQuotePolicy(quote, btcutil.Amount(1000))
	require.NoError(t, err)
}

// TestValidateQuotePolicyFeeTooHigh covers the fee-cap rejection
// path. Replaces the v1 pre-flight MinOperatorFee check in
// transitions.go:383 -- under v2 the client only enforces the
// upper-bound cap, since under-quoting is structurally
// impossible once the server is the sole fee author.
func TestValidateQuotePolicyFeeTooHigh(t *testing.T) {
	t.Parallel()

	restore := SetQuotePolicyNow(func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	defer restore()

	quote := &types.JoinRoundQuote{
		OperatorFeeSat:     btcutil.Amount(5000),
		QuoteExpiresAtUnix: 1_700_000_010,
		ProtocolVersion:    V2MinProtocolVersion,
	}
	err := ValidateQuotePolicy(quote, btcutil.Amount(1000))
	require.ErrorIs(t, err, ErrV2QuoteFeeExceedsCap)
}

// TestValidateQuotePolicyExpired covers the stale-quote
// rejection path. A client that receives a quote past its
// expiry window must reject rather than sign: the server has
// already treated the quote as an implicit reject and may have
// re-sealed the round without this client.
func TestValidateQuotePolicyExpired(t *testing.T) {
	t.Parallel()

	restore := SetQuotePolicyNow(func() time.Time {
		return time.Unix(1_700_000_100, 0)
	})
	defer restore()

	quote := &types.JoinRoundQuote{
		OperatorFeeSat:     btcutil.Amount(500),
		QuoteExpiresAtUnix: 1_700_000_010,
		ProtocolVersion:    V2MinProtocolVersion,
	}
	err := ValidateQuotePolicy(quote, btcutil.Amount(1000))
	require.ErrorIs(t, err, ErrV2QuoteExpired)
}

// TestValidateQuotePolicyNoExpiry covers the informal contract
// that a zero QuoteExpiresAtUnix means "no expiry policy" --
// useful for tests and for servers that don't want to expire
// quotes. With zero, the expiry branch is skipped and the
// quote is accepted purely on the fee-cap + protocol-version
// checks.
func TestValidateQuotePolicyNoExpiry(t *testing.T) {
	t.Parallel()

	restore := SetQuotePolicyNow(func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	defer restore()

	quote := &types.JoinRoundQuote{
		OperatorFeeSat:     btcutil.Amount(500),
		QuoteExpiresAtUnix: 0,
		ProtocolVersion:    V2MinProtocolVersion,
	}
	err := ValidateQuotePolicy(quote, btcutil.Amount(1000))
	require.NoError(t, err)
}

// TestValidateQuotePolicyProtocolDowngrade covers the downgrade-
// reject path. A server that authors a quote with
// protocol_version below V2MinProtocolVersion (e.g. 0 or 1) is
// either misconfigured or attempting to downgrade the client;
// either way the client refuses.
func TestValidateQuotePolicyProtocolDowngrade(t *testing.T) {
	t.Parallel()

	quote := &types.JoinRoundQuote{
		OperatorFeeSat:     btcutil.Amount(500),
		QuoteExpiresAtUnix: 0,
		ProtocolVersion:    1,
	}
	err := ValidateQuotePolicy(quote, btcutil.Amount(1000))
	require.ErrorIs(t, err, ErrV2QuoteProtocolMismatch)
}

// TestValidateQuotePolicyNilQuote covers the structural guard
// that a nil quote fails fast with a descriptive error rather
// than panicking on dereference.
func TestValidateQuotePolicyNilQuote(t *testing.T) {
	t.Parallel()

	err := ValidateQuotePolicy(nil, btcutil.Amount(1000))
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot validate a nil quote")
}
