package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestClientStateToProtoCoversAllStates asserts every round FSM
// state has an explicit mapping in clientStateToProto so new
// states cannot silently fall through to ROUND_STATE_UNKNOWN.
// The #270 seal-time handshake added QuoteReceivedState; before
// this test the state mapped to UNKNOWN and regressed
// ListRounds / WatchRounds observability.
func TestClientStateToProtoCoversAllStates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		state    round.ClientState
		expected waverpc.RoundState
	}{
		{
			name:  "quote_received",
			state: &round.QuoteReceivedState{},
			expected: waverpc.
				RoundState_ROUND_STATE_QUOTE_RECEIVED,
		},
		{
			name:  "intent_sent",
			state: &round.IntentSentState{},
			expected: waverpc.
				RoundState_ROUND_STATE_REGISTRATION_SENT,
		},
		{
			name:  "round_joined",
			state: &round.RoundJoinedState{},
			expected: waverpc.
				RoundState_ROUND_STATE_JOINED,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := clientStateToProto(tc.state)
			require.Equal(t, tc.expected, got)
		})
	}
}
