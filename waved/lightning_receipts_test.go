package waved

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLightningReceiptBoundsPrecedeLookup rejects an invalid full request
// before invoking even the first otherwise-valid source lookup.
func TestLightningReceiptBoundsPrecedeLookup(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	server := &Server{walletServicesReady: ready}
	called := false
	server.lightningReceiptLookup = func(context.Context,
		LightningReceiptReference, int64) (*LightningReceipt, error) {

		called = true

		return nil, nil
	}
	ref := LightningReceiptReference{
		Source: LightningReceiptCredit,
		ID:     "op",
	}
	for _, request := range []LightningReceiptRequest{
		{},
		{
			References: []LightningReceiptReference{
				ref,
			},
			CreatedAfterUnix: -1,
		},
		{
			References: make([]LightningReceiptReference, 65),
		},
		{
			References: []LightningReceiptReference{
				ref,
				{
					Source: "unknown",
					ID:     "x",
				},
			},
		},
		{References: []LightningReceiptReference{ref, {
			Source: LightningReceiptCredit, ID: strings.Repeat(
				"x", 129,
			),
		}}},
	} {
		_, err := server.ReadLightningReceipts(t.Context(), request)
		require.Error(t, err)
		require.False(t, called)
	}
	result, err := server.ReadLightningReceipts(
		t.Context(), LightningReceiptRequest{
			References: []LightningReceiptReference{ref},
		},
	)
	require.NoError(t, err)
	require.True(t, called)
	require.NotNil(t, result)
	require.Empty(t, result)
}
