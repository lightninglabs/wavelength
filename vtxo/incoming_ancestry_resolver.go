package vtxo

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/internal/indexerlimits"
)

const (
	// DefaultIncomingAncestryIndexPageSize is the per-page cap for
	// incoming ancestry lookups.
	DefaultIncomingAncestryIndexPageSize uint32 = 128

	// DefaultIncomingAncestryMaxScan bounds how many indexer VTXOs an
	// incoming ancestry lookup may inspect before failing closed.
	DefaultIncomingAncestryMaxScan uint64 = 128
)

// IncomingAncestryQuery pages through indexer VTXOs for one taproot
// pkScript. The caller supplies the proof-of-control and transport
// concerns; this package owns the matching, cursor, cap, and ancestry
// validation policy.
type IncomingAncestryQuery func(ctx context.Context, pkScript []byte,
	cursor []byte, limit uint32) (*arkrpc.ListVTXOsByScriptsResponse, error)

// ResolveIncomingAncestry scans indexer VTXOs for outpoint and maps the
// matching entry into the descriptor extras required for unilateral exit.
func ResolveIncomingAncestry(ctx context.Context, query IncomingAncestryQuery,
	outpoint wire.OutPoint, pkScript []byte, pageSize uint32,
	maxScanned uint64) (IncomingVTXOExtras, error) {

	if query == nil {
		return IncomingVTXOExtras{}, fmt.Errorf("incoming ancestry " +
			"query must be provided")
	}
	if pageSize == 0 {
		pageSize = DefaultIncomingAncestryIndexPageSize
	}
	if maxScanned == 0 {
		maxScanned = DefaultIncomingAncestryMaxScan
	}
	if maxScanned < uint64(pageSize) {
		pageSize = uint32(maxScanned)
	}

	script := slices.Clone(pkScript)

	var (
		cursor  []byte
		scanned uint64
	)
	for {
		resp, err := query(ctx, script, cursor, pageSize)
		if err != nil {
			return IncomingVTXOExtras{}, fmt.Errorf("indexer list "+
				"vtxos by scripts: %w", err)
		}

		page := FlattenListVTXOsByScriptsResponse(resp)
		for _, candidate := range page {
			scanned++
			if scanned > maxScanned {
				return IncomingVTXOExtras{}, fmt.Errorf(
					"ancestry index scan exceeded limit "+
						"%d entries for outpoint %s",
					maxScanned, outpoint)
			}

			if !matchesIncomingAncestryOutpoint(
				candidate, outpoint,
			) {

				continue
			}

			ancestry, err := AncestryFromRPC(
				candidate.GetAncestryPaths(),
			)
			if err != nil {
				return IncomingVTXOExtras{}, fmt.Errorf(
					"convert ancestry paths: %w", err)
			}

			return IncomingVTXOExtras{
				Ancestry:      ancestry,
				CreatedHeight: candidate.GetCreatedHeight(),
			}, nil
		}

		nextCursor := resp.GetNextCursor()
		if len(page) == 0 || len(nextCursor) == 0 ||
			bytes.Equal(nextCursor, cursor) {

			break
		}

		if scanned >= maxScanned {
			return IncomingVTXOExtras{}, fmt.Errorf("ancestry "+
				"index scan exceeded limit %d entries for "+
				"outpoint %s", maxScanned, outpoint)
		}

		if err := indexerlimits.ValidateVTXOsByScriptsCursor(
			nextCursor,
		); err != nil {
			return IncomingVTXOExtras{}, fmt.Errorf("indexer next "+
				"cursor: %w", err)
		}

		cursor = nextCursor
	}

	return IncomingVTXOExtras{}, fmt.Errorf("indexer response missing "+
		"vtxo for outpoint %s after paginated scan", outpoint)
}

// matchesIncomingAncestryOutpoint returns true when candidate carries
// the target outpoint.
func matchesIncomingAncestryOutpoint(candidate *arkrpc.VTXO,
	target wire.OutPoint) bool {

	if candidate == nil {
		return false
	}

	op := candidate.GetOutpoint()
	if op == nil {
		return false
	}

	if op.GetVout() != target.Index {
		return false
	}

	if len(op.GetTxid()) != chainhash.HashSize {
		return false
	}

	var hash chainhash.Hash
	copy(hash[:], op.GetTxid())

	return hash == target.Hash
}
