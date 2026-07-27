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

		extras, err := vtxo.ResolveIncomingAncestry(
			ctx, query, outpoint, pkScript,
			vtxo.DefaultIncomingAncestryIndexPageSize,
			uint64(oor.DefaultMaxVTXOMatches),
		)
		if err != nil {
			return vtxo.IncomingVTXOExtras{}, err
		}

		// ResolveIncomingAncestry has authenticated each tree's batch
		// output against its real commitment tx
		// (EvidenceFromAncestryPaths). Now cryptographically bind the
		// received VTXO to that authenticated lineage (F-H1): prove it
		// is a genuine, operator-signed leaf of the commitment we will
		// watch, not a decoy the indexer named. A failure surfaces as a
		// fetch failure, and the handler then persists the VTXO without
		// ancestry (fail closed -- no reorg watch armed on an unverified
		// commitment; exit material restored by a later backfill).
		if err := vtxo.VerifyReceivedVTXOBinding(
			extras.Ancestry, pkScript,
		); err != nil {
			return vtxo.IncomingVTXOExtras{}, fmt.Errorf(
				"bind received vtxo to commitment: %w", err)
		}

		return extras, nil
	}, nil
}
