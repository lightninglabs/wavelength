package waved

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

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
// the OOR receive path uses; structural errors (missing or over-cap paths)
// surface as fetch failures and the handler then persists without ancestry
// rather than dropping the VTXO entirely. This keeps cooperative spend paths
// usable on the receiver even if the indexer is temporarily unhealthy — only
// unilateral exit is degraded.
func incomingAncestryFetcher(idx *indexer.Client,
	signerFactory OORReceiveScriptSignerFactory) (
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

		return vtxo.ResolveIncomingAncestry(
			ctx, query, outpoint, pkScript,
			vtxo.DefaultIncomingAncestryIndexPageSize,
			uint64(oor.DefaultMaxVTXOMatches),
		)
	}, nil
}
