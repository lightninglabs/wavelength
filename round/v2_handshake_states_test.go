package round

import (
	"context"
	"testing"

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
