package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// defaultMaxUnrollDepth bounds local resolver traversal depth as a
	// defense-in-depth guard against malformed ancestry graphs.
	defaultMaxUnrollDepth = 64
)

var (
	// ErrResolveUnrollMaxDepthExceeded indicates resolver traversal
	// exceeded the configured depth bound.
	ErrResolveUnrollMaxDepthExceeded = errors.New("resolve unroll max " +
		"depth exceeded")
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
func (s *OORArtifactPersistenceStore) ResolveUnrollPackages(ctx context.Context,
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
		processedDepth := make(map[chainhash.Hash]int)
		resolvedByInput := make(map[string]*OORPackageBundle)
		missingByInput := make(map[string]struct{})
		missing := make(map[string]wire.OutPoint)

		for len(sessionQueue) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}

			currentID := sessionQueue[0]
			sessionQueue = sessionQueue[1:]

			node := nodes[currentID]
			if depth, ok := processedDepth[currentID]; ok &&
				node.depth <= depth {

				continue
			}
			processedDepth[currentID] = node.depth

			checkpointInputs := checkpointInputOutpoints(node.pkg)

			for i := range checkpointInputs {
				input := checkpointInputs[i]
				key := outpointKey(input)
				if _, ok := missingByInput[key]; ok {
					missing[key] = input
					continue
				}

				inputPkg, ok := resolvedByInput[key]
				if !ok {
					pkg, miss, err := resolveInputPackage(
						ctx, q, input, loadCreated,
					)
					if err != nil {
						return err
					}
					if miss {
						missingByInput[key] = struct{}{}
						missing[key] = input

						continue
					}

					inputPkg = pkg
					resolvedByInput[key] = inputPkg
				}

				nextDepth := node.depth + 1
				if nextDepth > s.maxUnrollDepth {
					return resolveUnrollDepthErr(
						input, nextDepth,
						s.maxUnrollDepth,
					)
				}

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

// resolveInputPackage looks up the package that creates a given checkpoint
// input outpoint. It first asks for a package bound to that exact outpoint,
// and on miss falls back to a session-id lookup using the outpoint hash:
// foreign-owned ancestor packages are persisted by session id only because
// the local wallet has no per-output binding for them.
//
// Returns:
//   - (pkg, false, nil) when a binding was resolved.
//   - (nil, true,  nil) when no local binding exists, signalling the caller
//     to record the input as unresolved.
//   - (nil, false, err) on any unexpected DB error.
func resolveInputPackage(ctx context.Context, q OORArtifactStore,
	input wire.OutPoint,
	loadCreated func(context.Context, OORArtifactStore, wire.OutPoint) (
		*OORPackageBundle, error)) (*OORPackageBundle, bool, error) {

	pkg, err := loadCreated(ctx, q, input)
	if err == nil {
		return pkg, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	// Fall back to a session-id lookup for foreign-owned ancestors that
	// the local wallet only has session-keyed visibility into. Ancestor
	// packages persisted under a session id are operator/indexer-supplied
	// artifacts without a local outpoint binding, so we must re-verify the
	// txid binding and the referenced output at read time before treating
	// the package as the parent of an unroll-chain input. Trusting only
	// the stored session id would let a poisoned row claim ancestry for
	// any checkpoint input whose previous hash matches its session id.
	pkg, err = loadPackageBundleBySessionID(ctx, q, input.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}

	if !packageProducesAncestorOutput(pkg, input.Hash, input.Index) {
		return nil, true, nil
	}

	return pkg, false, nil
}

// packageProducesAncestorOutput reports whether the foreign-ancestor package
// actually produces the referenced checkpoint-input outpoint. It rejects:
//
//   - packages whose stored Ark transaction does not actually hash to the
//     requested session id (txid binding mismatch from a tampered or
//     mismatched row),
//   - output indices outside the package's TxOut range,
//   - references that land on the Ark anchor output, which is never a
//     spendable VTXO and so cannot be a real ancestor checkpoint input.
func packageProducesAncestorOutput(pkg *OORPackageBundle,
	expectedTxid chainhash.Hash, index uint32) bool {

	if pkg == nil || pkg.ArkPSBT == nil || pkg.ArkPSBT.UnsignedTx == nil {
		return false
	}

	tx := pkg.ArkPSBT.UnsignedTx
	if tx.TxHash() != expectedTxid {
		return false
	}

	if index >= uint32(len(tx.TxOut)) {
		return false
	}

	return !arktx.IsAnchorOutput(tx.TxOut[index])
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

func resolveUnrollDepthErr(outpoint wire.OutPoint, depth, maxDepth int) error {
	return fmt.Errorf("%w: outpoint=%v depth=%d max=%d",
		ErrResolveUnrollMaxDepthExceeded, outpoint, depth, maxDepth)
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

	pkg, err := materializePackageBundle(ctx, q, sqlc.OorPackage{
		SessionID:            row.SessionID,
		Direction:            row.Direction,
		ArkPsbt:              row.ArkPsbt,
		TaprootAssetTransfer: row.TaprootAssetTransfer,
		CreatedAt:            row.PackageCreatedAt,
		UpdatedAt:            row.PackageUpdatedAt,
	})
	if err != nil {
		return nil, err
	}

	matched, err := bindingFromOutpointJoinRow(row)
	if err != nil {
		return nil, err
	}

	pkg.MatchedOutpointBinding = fn.Some(*matched)

	return pkg, nil
}

// loadPackageBundleBySessionID resolves a full package bundle by OOR session
// id without requiring a local VTXO binding for one of its outputs.
func loadPackageBundleBySessionID(ctx context.Context, q OORArtifactStore,
	sessionID chainhash.Hash) (*OORPackageBundle, error) {

	row, err := q.GetOORPackage(ctx, sessionID[:])
	if err != nil {
		return nil, err
	}

	return materializePackageBundle(ctx, q, row)
}

// loadPackageBundleByCreatedOutputOutpoint resolves a full package bundle
// from one created-output binding outpoint.
func loadPackageBundleByCreatedOutputOutpoint(ctx context.Context,
	q OORArtifactStore, outpoint wire.OutPoint) (*OORPackageBundle, error) {

	row, err := q.GetOORPackageByOutpointAndKind(ctx,
		sqlc.GetOORPackageByOutpointAndKindParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
			LinkKind:      oorPackageLinkKindCreatedOutputCode,
		},
	)
	if err != nil {
		return nil, err
	}

	pkg, err := materializePackageBundle(ctx, q, sqlc.OorPackage{
		SessionID:            row.SessionID,
		Direction:            row.Direction,
		ArkPsbt:              row.ArkPsbt,
		TaprootAssetTransfer: row.TaprootAssetTransfer,
		CreatedAt:            row.PackageCreatedAt,
		UpdatedAt:            row.PackageUpdatedAt,
	})
	if err != nil {
		return nil, err
	}

	matched, err := bindingFromOutpointAndKindJoinRow(row)
	if err != nil {
		return nil, err
	}

	pkg.MatchedOutpointBinding = fn.Some(*matched)

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
