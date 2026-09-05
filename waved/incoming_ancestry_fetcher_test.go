package waved

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestIncomingAncestryFetcherWithTimeout proves a stalled indexer lookup
// returns control to the durable incoming actor for postponement.
func TestIncomingAncestryFetcherWithTimeout(t *testing.T) {
	t.Parallel()

	fetcher := incomingAncestryFetcherWithTimeout(
		func(ctx context.Context, _ wire.OutPoint, _ []byte,
			_ keychain.KeyDescriptor) (vtxo.IncomingVTXOExtras,
			error) {

			<-ctx.Done()

			return vtxo.IncomingVTXOExtras{}, ctx.Err()
		}, 10*time.Millisecond,
	)

	_, err := fetcher(
		t.Context(), wire.OutPoint{}, nil, keychain.KeyDescriptor{},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
