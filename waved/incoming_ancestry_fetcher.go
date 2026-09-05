package waved

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

// incomingAncestryFetcherWithTimeout bounds the complete indexer-and-chain
// lookup. A stalled indexer must release the durable actor turn so the pending
// event can be postponed and retried.
func incomingAncestryFetcherWithTimeout(fetcher vtxo.IncomingAncestryFetcher,
	timeout time.Duration) vtxo.IncomingAncestryFetcher {

	return func(ctx context.Context, outpoint wire.OutPoint,
		pkScript []byte, clientKey keychain.KeyDescriptor) (
		vtxo.IncomingVTXOExtras, error) {

		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		return fetcher(ctx, outpoint, pkScript, clientKey)
	}
}

// incomingAncestryFetcher returns a vtxo.IncomingAncestryFetcher that resolves
// the round commit tree fragments needed for the unilateral exit unroll CPFP
// child. The fetcher composes the daemon's shared indexer client with a
// per-script signer so the proof-of-control on each ListVTXOsByScripts query
// is signed by the owner key for the specific receive script being queried.
//
// Returns an error when prerequisites are not yet wired; callers gate the
// IncomingVTXOHandler construction on a non-nil fetcher so the handler never
// runs with a broken dependency in production.
//
// The fetched ancestry travels through the same vtxo.AncestryFromRPC validator
// the OOR receive path uses. Structural or authentication errors keep the
// durable incoming event pending for retry; no VTXO is accepted without the
// expiry evidence needed to enforce its safety margins.
func incomingAncestryFetcher(idx *indexer.Client,
	signerFactory OORReceiveScriptSignerFactory,
	authenticateExpiry oor.IncomingExpiryAuthenticator) (
	vtxo.IncomingAncestryFetcher, error) {

	if authenticateExpiry == nil {
		return nil, fmt.Errorf("expiry authenticator not initialized")
	}

	return newIncomingAncestryFetcher(
		idx, signerFactory, authenticateExpiry,
	)
}

// incomingAncestryOnlyFetcher builds the fetch-only variant used by the
// legacy commitment-height repair. New VTXO acceptance must use
// incomingAncestryFetcher so expiry authentication cannot be omitted.
func incomingAncestryOnlyFetcher(idx *indexer.Client,
	signerFactory OORReceiveScriptSignerFactory) (
	vtxo.IncomingAncestryFetcher, error) {

	return newIncomingAncestryFetcher(idx, signerFactory, nil)
}

func newIncomingAncestryFetcher(idx *indexer.Client,
	signerFactory OORReceiveScriptSignerFactory,
	authenticateExpiry oor.IncomingExpiryAuthenticator) (
	vtxo.IncomingAncestryFetcher, error) {

	if idx == nil {
		return nil, fmt.Errorf("indexer client not initialized")
	}
	if signerFactory == nil {
		return nil, fmt.Errorf("signer factory not initialized")
	}

	return func(ctx context.Context, outpoint wire.OutPoint,
		pkScript []byte, clientKey keychain.KeyDescriptor) (
		vtxo.IncomingVTXOExtras, error) {

		scopedIndexer := idx.WithSigner(signerFactory(clientKey))
		query := func(ctx context.Context, script []byte, cursor []byte,
			limit uint32) (*arkrpc.ListVTXOsByScriptsResponse,
			error) {

			// One scope per pkScript; the indexer signs each
			// ScriptScope with the supplied signer so the
			// proof-of-control attaches to the owner key for this
			// specific receive script.
			scope := indexer.TaprootScriptScope{
				PkScript: script,
			}

			return scopedIndexer.ListVTXOsByScriptsTaproot(
				ctx, []indexer.TaprootScriptScope{scope},
				cursor, limit, nil, /* statusFilter: any */
			)
		}

		extras, err := vtxo.ResolveIncomingAncestry(
			ctx, query, outpoint, pkScript,
			vtxo.DefaultIncomingAncestryIndexPageSize,
			uint64(oor.DefaultMaxVTXOMatches),
		)
		if err != nil {
			return vtxo.IncomingVTXOExtras{}, err
		}

		if authenticateExpiry != nil {
			extras.BatchExpiry, err = authenticateExpiry(
				ctx, extras.Ancestry,
			)
			if err != nil {
				return vtxo.IncomingVTXOExtras{}, fmt.Errorf(
					"authenticate batch "+
						"expiry: %w", err)
			}
		}

		return extras, nil
	}, nil
}
