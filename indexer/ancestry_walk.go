package indexer

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// AncestryPreVisitor runs in pre-order with the current session id and
// recursion depth, and returns the parent session ids the driver should
// recurse into. Returning a duplicate id is harmless: the driver
// short-circuits at the seen-set.
type AncestryPreVisitor func(ctx context.Context, sessionID []byte,
	depth int) (parentIDs [][]byte, err error)

// AncestryPostVisitor runs in post-order, after every parent of the
// current session has been recursively walked. Pass nil when the caller
// has no post-order side effects.
type AncestryPostVisitor func(ctx context.Context, sessionID []byte,
	depth int) error

// walkOORSessionAncestryDriver is the shared recursion driver for OOR
// ancestry graph walks. The two consumers — the cap-arithmetic path
// (`lineage_vbytes.go::walkOORAncestry`) and the recipient-events path
// (`service.go::loadOORAncestorPackages`) — share this driver so the
// depth bound, cycle protection, and visit ordering stay in lockstep.
//
// pre runs once per distinct session in pre-order, returning the
// parent ids to descend into; post runs once per distinct session in
// post-order, after every parent has been recursively walked. Either
// callback may be nil; the depth argument lets a post visitor skip the
// root if it is only interested in ancestors (e.g. emit-in-dependency-
// order patterns).
//
// Cycle protection: a session id is recorded as seen the moment it is
// first scheduled for visit; an A→B→A back-edge in the persisted store
// (DB corruption, future bug in the parent extractor) short-circuits at
// the second A rather than infinite-recursing.
//
// Depth protection: when depth > DefaultMaxLineageDepth the walker
// returns a typed error with the offending session id, mirroring the
// existing recipient-events path's hard-error contract. Both walkers
// share this bound so a chain that the recipient path rejects cannot
// silently pass the cap path.
//
// The seen-set is keyed by the 32-byte session id (interpreted as a
// chainhash.Hash); session ids are derived from the producing OOR Ark
// txid so distinct sessions necessarily have distinct keys.
func walkOORSessionAncestryDriver(ctx context.Context, startSessionID []byte,
	pre AncestryPreVisitor, post AncestryPostVisitor) error {

	seen := make(map[chainhash.Hash]struct{})

	var startKey chainhash.Hash
	copy(startKey[:], startSessionID)
	seen[startKey] = struct{}{}

	var walk func(curID []byte, depth int) error
	walk = func(curID []byte, depth int) error {
		if depth > DefaultMaxLineageDepth {
			return fmt.Errorf("oor ancestry walk exceeded max "+
				"depth %d at session %x",
				DefaultMaxLineageDepth, curID)
		}

		var parentIDs [][]byte
		if pre != nil {
			ids, err := pre(ctx, curID, depth)
			if err != nil {
				return err
			}
			parentIDs = ids
		}

		for _, parentID := range parentIDs {
			var pKey chainhash.Hash
			copy(pKey[:], parentID)
			if _, ok := seen[pKey]; ok {
				continue
			}
			seen[pKey] = struct{}{}

			if err := walk(parentID, depth+1); err != nil {
				return err
			}
		}

		if post != nil {
			if err := post(ctx, curID, depth); err != nil {
				return err
			}
		}

		return nil
	}

	return walk(startSessionID, 0)
}
