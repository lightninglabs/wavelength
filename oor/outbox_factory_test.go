package oor

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestNewOutboxHandlerForwardsExpiryAuthenticator proves the shared handler
// factory retains the chain-authentication dependency used by incoming OOR
// materialization.
func TestNewOutboxHandlerForwardsExpiryAuthenticator(t *testing.T) {
	t.Parallel()

	const authenticatedExpiry = int32(144)
	var calls int
	handler := NewOutboxHandler(OutboxHandlerConfig{
		AuthenticateIncomingExpiry: func(_ context.Context,
			_ []vtxo.Ancestry) (int32, error) {

			calls++

			return authenticatedExpiry, nil
		},
	})

	events, err := handler.Handle(
		t.Context(), SessionID{}, &AuthenticateIncomingMetadataRequest{
			Matches: []IncomingMetadataMatch{{}},
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Len(t, events, 1)

	resolved, ok := events[0].(*IncomingMetadataResolvedEvent)
	require.True(t, ok)
	require.True(t, resolved.ExpiryAuthenticated)
	require.Equal(
		t, authenticatedExpiry,
		resolved.Matches[0].Metadata.BatchExpiry,
	)
}
