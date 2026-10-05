package waved

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
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

// TestIncomingAncestryOnlyFetcherRetriesPages proves shed requests reuse a
// page key while advancing the cursor creates a distinct logical request.
func TestIncomingAncestryOnlyFetcherRetriesPages(t *testing.T) {
	t.Parallel()
	idx, rpc, recipient, _ := newTestIncomingMetadataIndexer(t)
	candidate, _ := expiryfixture.Round(
		t, 10000, recipient.PkScript, 50, 321, 51,
	)
	rpc.responses = []*arkrpc.ListVTXOsByScriptsResponse{
		{
			Vtxos: []*arkrpc.VTXO{{
				Outpoint: &arkrpc.OutPoint{
					Txid: testTxIDBytes(2),
				},
			}},
			NextCursor: []byte{
				1,
			},
		},
		{
			Vtxos: []*arkrpc.VTXO{
				candidate,
			},
		},
	}
	rpc.shedFirst = 2
	fetcher, err := incomingAncestryOnlyFetcher(
		idx, (&recoveryKeyBackend{}).ProofSigner,
		fastCommitmentRepairPage,
	)
	require.NoError(t, err)
	var outpoint wire.OutPoint
	copy(outpoint.Hash[:], candidate.Outpoint.Txid)
	outpoint.Index = candidate.Outpoint.Vout
	extras, err := fetcher(
		t.Context(), outpoint, recipient.PkScript,
		testKeyDescriptor(t, 1),
	)
	require.NoError(t, err)
	require.Equal(t, int32(321), extras.Ancestry[0].CommitmentHeight)
	keys := rpc.idempotencyKeys()
	require.Len(t, keys, 4)
	require.NotEmpty(t, keys[0])
	require.Equal(t, keys[0], keys[1])
	require.Equal(t, keys[1], keys[2])
	require.NotEqual(t, keys[2], keys[3])
	require.Empty(t, rpc.sent[0].Cursor)
	require.Equal(t, []byte{1}, rpc.sent[1].Cursor)
}
