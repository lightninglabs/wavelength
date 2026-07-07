package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// LineageMaterial is the normalized internal representation of all local
// lineage fragments that contribute to one unroll target.
type LineageMaterial struct {
	// TargetOutpoint identifies the VTXO being unrolled.
	TargetOutpoint wire.OutPoint

	// CSVDelay is the relative CSV lock on the target's timeout spend path.
	CSVDelay uint32

	// TreePaths holds zero or more rooted tree fragments that contribute
	// ancestry to the target.
	TreePaths []*tree.Tree

	// ExtraNodes holds zero or more finalized non-tree transactions
	// that bridge or extend lineage beyond the tree fragments.
	ExtraNodes []*recovery.Node
}

// Validate checks that the material is internally consistent enough for proof
// assembly.
func (m *LineageMaterial) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: lineage material is nil",
			ErrUnrollProofUnavailable)
	}

	if m.TargetOutpoint == (wire.OutPoint{}) {
		return fmt.Errorf("%w: lineage material missing target",
			ErrUnrollProofUnavailable)
	}

	if len(m.TreePaths) == 0 && len(m.ExtraNodes) == 0 {
		return fmt.Errorf("%w: lineage material has no tree paths and "+
			"no extra nodes", ErrUnrollProofUnavailable)
	}

	for i, tp := range m.TreePaths {
		if tp == nil || tp.Root == nil {
			return fmt.Errorf("%w: tree path %d missing root",
				ErrUnrollProofUnavailable, i)
		}
	}

	seen := make(map[chainhash.Hash]struct{})
	for i, node := range m.ExtraNodes {
		if node == nil {
			return fmt.Errorf("%w: extra node %d is nil",
				ErrUnrollProofInvalid, i)
		}

		txid, err := node.TXID()
		if err != nil {
			return fmt.Errorf("%w: extra node %d: %w",
				ErrUnrollProofInvalid, i, err)
		}

		if _, dup := seen[txid]; dup {
			return fmt.Errorf("%w: duplicate extra node %s",
				ErrUnrollProofInvalid, txid)
		}

		seen[txid] = struct{}{}
	}

	return nil
}

// LineageResolver gathers normalized local lineage material for one unroll
// target.
type LineageResolver interface {
	// ResolveLineage returns the normalized lineage material required to
	// assemble a recovery proof for the given target.
	ResolveLineage(ctx context.Context,
		target wire.OutPoint) (*LineageMaterial, error)
}
