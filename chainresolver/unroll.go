package chainresolver

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// buildTreeLevelBroadcasts constructs the outbox messages needed to broadcast
// all transactions at a given tree level and register confirmation watches
// for them.
//
// The tree is traversed using BFS and nodes at the target level are collected.
// Each node is converted to a signed transaction and both a broadcast and
// confirmation watch message are emitted.
func buildTreeLevelBroadcasts(treePath *tree.Tree, level int,
	resolverID wire.OutPoint) ([]ResolverOutMsg, error) {

	if treePath == nil || treePath.Root == nil {
		return nil, fmt.Errorf("tree path is nil")
	}

	// Collect nodes at the target level using BFS.
	nodes, err := collectNodesAtLevel(treePath.Root, level)
	if err != nil {
		return nil, fmt.Errorf(
			"collect nodes at level %d: %w", level, err,
		)
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes found at level %d", level)
	}

	var outbox []ResolverOutMsg

	for _, node := range nodes {
		signedTx, err := node.ToSignedTx()
		if err != nil {
			return nil, fmt.Errorf(
				"build signed tx at level %d: %w", level, err,
			)
		}

		txid := signedTx.TxHash()

		// Broadcast the transaction.
		outbox = append(outbox, &BroadcastTxOutMsg{
			Tx: signedTx,
			Label: fmt.Sprintf(
				"tree-level-%d-%s",
				level, resolverID.String(),
			),
		})

		// Register a confirmation watch. We use the first output's
		// pkScript for monitoring since all tree txs have at least
		// one output.
		if len(signedTx.TxOut) == 0 {
			return nil, fmt.Errorf("tx has no outputs")
		}

		callerID := fmt.Sprintf(
			"resolver.%s.tree.%s",
			resolverID.String(), txid.String(),
		)

		outbox = append(outbox, &RegisterConfWatchOutMsg{
			Txid:        txid,
			PkScript:    signedTx.TxOut[0].PkScript,
			TargetConfs: 1,
			CallerID:    callerID,
		})
	}

	return outbox, nil
}

// collectNodesAtLevel performs a BFS traversal of the tree and returns all
// nodes at the specified level (0 = root).
func collectNodesAtLevel(root *tree.Node, level int) ([]*tree.Node, error) {
	if root == nil {
		return nil, fmt.Errorf("root node is nil")
	}

	// BFS using a simple queue with level tracking.
	type nodeWithLevel struct {
		node  *tree.Node
		level int
	}

	queue := []nodeWithLevel{{node: root, level: 0}}
	var result []*tree.Node

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if current.level == level {
			result = append(result, current.node)

			continue
		}

		// Only enqueue children if we haven't reached the target
		// level yet.
		if current.level < level {
			for _, child := range current.node.Children {
				queue = append(queue, nodeWithLevel{
					node:  child,
					level: current.level + 1,
				})
			}
		}
	}

	return result, nil
}

// buildCheckpointBroadcasts constructs the outbox messages needed to
// broadcast the checkpoint transactions from an OOR package bundle.
// Checkpoint PSBTs are extracted and broadcast in order.
func buildCheckpointBroadcasts(pkg *db.OORPackageBundle,
	resolverID wire.OutPoint) ([]ResolverOutMsg, error) {

	if pkg == nil {
		return nil, fmt.Errorf("package bundle is nil")
	}

	if len(pkg.FinalCheckpointPSBTs) == 0 {
		return nil, fmt.Errorf("package has no checkpoint PSBTs")
	}

	var outbox []ResolverOutMsg

	for i, pkt := range pkg.FinalCheckpointPSBTs {
		tx, err := extractCheckpointTx(pkt)
		if err != nil {
			return nil, fmt.Errorf(
				"extract checkpoint tx %d: %w", i, err,
			)
		}

		txid := tx.TxHash()

		outbox = append(outbox, &BroadcastTxOutMsg{
			Tx: tx,
			Label: fmt.Sprintf(
				"checkpoint-%d-%s",
				i, resolverID.String(),
			),
		})

		// Register a confirmation watch for the checkpoint.
		if len(tx.TxOut) == 0 {
			return nil, fmt.Errorf(
				"checkpoint tx %d has no outputs", i,
			)
		}

		callerID := fmt.Sprintf(
			"resolver.%s.checkpoint.%s",
			resolverID.String(), txid.String(),
		)

		outbox = append(outbox, &RegisterConfWatchOutMsg{
			Txid:        txid,
			PkScript:    tx.TxOut[0].PkScript,
			TargetConfs: 1,
			CallerID:    callerID,
		})
	}

	return outbox, nil
}

// extractCheckpointTx extracts a finalized transaction from a checkpoint
// PSBT. The PSBT must be fully signed and finalizable.
func extractCheckpointTx(pkt *psbt.Packet) (*wire.MsgTx, error) {
	if pkt == nil {
		return nil, fmt.Errorf("checkpoint PSBT is nil")
	}

	// Attempt to extract the finalized transaction from the PSBT.
	tx, err := psbt.Extract(pkt)
	if err != nil {
		return nil, fmt.Errorf("extract PSBT: %w", err)
	}

	return tx, nil
}

// computeLeafOutpoint finds the non-anchor outpoint of the leaf node in the
// tree path. This is the final on-chain outpoint where the VTXO value can
// be claimed.
func computeLeafOutpoint(treePath *tree.Tree) (wire.OutPoint, error) {
	if treePath == nil || treePath.Root == nil {
		return wire.OutPoint{}, fmt.Errorf("tree path is nil")
	}

	// Find the leaf node. For a VTXO's extracted path, there should be
	// exactly one leaf.
	leaves := treePath.Root.GetLeafNodes()
	if len(leaves) == 0 {
		return wire.OutPoint{}, fmt.Errorf("no leaf nodes in tree")
	}

	// Use the first leaf's non-anchor outpoint.
	outpoint, err := leaves[0].GetNonAnchorOutpoint()
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf(
			"get leaf outpoint: %w", err,
		)
	}

	return *outpoint, nil
}
