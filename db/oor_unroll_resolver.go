package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db/sqlc"
)

// OORUnrollPackages is a local package-resolution result for one outpoint.
type OORUnrollPackages struct {
	// TargetOutpoint is the local outpoint requested by the caller.
	TargetOutpoint wire.OutPoint

	// Packages contains the locally known package chain ordered from the
	// farthest known ancestor package to the target package.
	Packages []*OORPackageBundle

	// UnresolvedCheckpointInputs contains checkpoint input outpoints
	// that do not currently have a locally stored package mapping.
	UnresolvedCheckpointInputs []wire.OutPoint
}

type unrollPackageNode struct {
	pkg   *OORPackageBundle
	depth int
}

// ResolveUnrollPackages resolves locally known OOR package artifacts needed to
// unroll the target outpoint.
//
// The resolver follows checkpoint-input ancestry through locally persisted
// outpoint bindings. Any checkpoint input that cannot be resolved to a local
// package binding is returned in UnresolvedCheckpointInputs.
func (s *OORArtifactPersistenceStore) ResolveUnrollPackages(
	ctx context.Context,
	outpoint wire.OutPoint) (*OORUnrollPackages, error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	readTx := ReadTxOption()
	var result *OORUnrollPackages

	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		targetPkg, err := loadPackageBundleByOutpoint(ctx, q, outpoint)
		if err != nil {
			return err
		}

		nodes := make(map[chainhash.Hash]*unrollPackageNode)
		nodes[targetPkg.SessionID] = &unrollPackageNode{
			pkg:   targetPkg,
			depth: 0,
		}

		sessionQueue := []chainhash.Hash{targetPkg.SessionID}
		loadCreated := loadPackageBundleByCreatedOutputOutpoint
		enqueueSession := func(id chainhash.Hash) {
			sessionQueue = append(sessionQueue, id)
		}
		missing := make(map[string]wire.OutPoint)

		for len(sessionQueue) > 0 {
			currentID := sessionQueue[0]
			sessionQueue = sessionQueue[1:]

			node := nodes[currentID]
			checkpointInputs := checkpointInputOutpoints(node.pkg)

			for i := range checkpointInputs {
				input := checkpointInputs[i]
				inputPkg, err := loadCreated(ctx, q, input)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						key := outpointKey(input)
						missing[key] = input
						continue
					}

					return err
				}

				nextDepth := node.depth + 1
				existing, ok := nodes[inputPkg.SessionID]
				if ok {
					if nextDepth > existing.depth {
						existing.depth = nextDepth
						// Requeue when depth increases.
						id := inputPkg.SessionID
						enqueueSession(id)
					}

					continue
				}

				nodes[inputPkg.SessionID] = &unrollPackageNode{
					pkg:   inputPkg,
					depth: nextDepth,
				}
				id := inputPkg.SessionID
				enqueueSession(id)
			}
		}

		packages := make([]*unrollPackageNode, 0, len(nodes))
		for _, node := range nodes {
			packages = append(packages, node)
		}

		sort.SliceStable(packages, func(i, j int) bool {
			if packages[i].depth != packages[j].depth {
				return packages[i].depth > packages[j].depth
			}

			return bytes.Compare(
				packages[i].pkg.SessionID[:],
				packages[j].pkg.SessionID[:],
			) < 0
		})

		orderedPkgs := make([]*OORPackageBundle, 0, len(packages))
		for i := range packages {
			orderedPkgs = append(orderedPkgs, packages[i].pkg)
		}

		unresolved := make([]wire.OutPoint, 0, len(missing))
		for _, outpoint := range missing {
			unresolved = append(unresolved, outpoint)
		}

		sortOutpoints(unresolved)

		result = &OORUnrollPackages{
			TargetOutpoint:             outpoint,
			Packages:                   orderedPkgs,
			UnresolvedCheckpointInputs: unresolved,
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// checkpointInputOutpoints returns de-duplicated checkpoint input outpoints
// referenced by the package's finalized checkpoints.
func checkpointInputOutpoints(pkg *OORPackageBundle) []wire.OutPoint {
	if pkg == nil {
		return nil
	}

	seen := make(map[string]struct{})
	outpoints := make([]wire.OutPoint, 0)

	for i := range pkg.FinalCheckpointPSBTs {
		checkpoint := pkg.FinalCheckpointPSBTs[i]
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			continue
		}

		for j := range checkpoint.UnsignedTx.TxIn {
			txIn := checkpoint.UnsignedTx.TxIn[j]
			outpoint := txIn.PreviousOutPoint
			key := outpointKey(outpoint)
			if _, ok := seen[key]; ok {
				continue
			}

			seen[key] = struct{}{}
			outpoints = append(outpoints, outpoint)
		}
	}

	return outpoints
}

// loadPackageBundleByOutpoint resolves a full package bundle from one binding
// outpoint.
func loadPackageBundleByOutpoint(ctx context.Context, q OORArtifactStore,
	outpoint wire.OutPoint) (*OORPackageBundle, error) {

	row, err := q.GetOORPackageByOutpoint(ctx,
		sqlc.GetOORPackageByOutpointParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		},
	)
	if err != nil {
		return nil, err
	}

	pkg, err := loadPackageBundleBySession(ctx, q, row.SessionID)
	if err != nil {
		return nil, err
	}

	matched, err := bindingFromOutpointJoinRow(row)
	if err != nil {
		return nil, err
	}

	pkg.MatchedOutpointBinding = matched

	return pkg, nil
}

// loadPackageBundleByCreatedOutputOutpoint resolves a full package bundle
// from one created-output binding outpoint.
func loadPackageBundleByCreatedOutputOutpoint(ctx context.Context,
	q OORArtifactStore, outpoint wire.OutPoint) (*OORPackageBundle, error) {

	row, err := q.GetOORPackageByOutpointAndKind(ctx,
		sqlc.GetOORPackageByOutpointAndKindParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
			LinkKind:      OORPackageLinkKindCreatedOutput,
		},
	)
	if err != nil {
		return nil, err
	}

	pkg, err := loadPackageBundleBySession(ctx, q, row.SessionID)
	if err != nil {
		return nil, err
	}

	matched, err := bindingFromOutpointAndKindJoinRow(row)
	if err != nil {
		return nil, err
	}

	pkg.MatchedOutpointBinding = matched

	return pkg, nil
}

// sortOutpoints sorts outpoints deterministically by hash then index.
func sortOutpoints(outpoints []wire.OutPoint) {
	sort.SliceStable(outpoints, func(i, j int) bool {
		hashCmp := bytes.Compare(
			outpoints[i].Hash[:], outpoints[j].Hash[:],
		)
		if hashCmp != 0 {
			return hashCmp < 0
		}

		return outpoints[i].Index < outpoints[j].Index
	})
}

// outpointKey converts an outpoint to a stable deduplication key string.
func outpointKey(outpoint wire.OutPoint) string {
	return fmt.Sprintf("%x:%d", outpoint.Hash[:], outpoint.Index)
}
