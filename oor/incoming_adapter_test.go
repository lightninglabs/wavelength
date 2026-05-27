package oor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsIncomingResolveCorrelationID verifies only durable incoming-transfer
// resolution query correlation IDs match the OOR resolve route prefix.
func TestIsIncomingResolveCorrelationID(t *testing.T) {
	t.Parallel()

	var sessionID SessionID
	sessionID[0] = 1

	require.True(
		t,
		IsIncomingResolveCorrelationID(
			IncomingResolveCorrelationID(sessionID, 7),
		),
	)
	require.False(t, IsIncomingResolveCorrelationID(""))
	require.False(
		t, IsIncomingResolveCorrelationID("00aa8bfb11f09881bbd2"),
	)
	require.False(
		t, IsIncomingResolveCorrelationID(
			incomingResolveCorrelationPrefix,
		),
	)
}
