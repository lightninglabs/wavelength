package unroller

import (
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/round"
)

// UnrollStatus represents the current phase of unroll.
type UnrollStatus int

const (
	// UnrollStatusPending means waiting for preconditions.
	UnrollStatusPending UnrollStatus = iota

	// UnrollStatusBroadcasting means actively broadcasting tree levels.
	UnrollStatusBroadcasting

	// UnrollStatusAwaitingCSV means leaf confirmed, waiting CSV delay.
	UnrollStatusAwaitingCSV

	// UnrollStatusComplete means unroll finished successfully (VTXO is
	// on-chain and CSV satisfied). A separate sweeper actor will handle
	// spending the VTXO output.
	UnrollStatusComplete

	// UnrollStatusFailed means unroll failed permanently.
	UnrollStatusFailed
)

// String returns human-readable status.
func (s UnrollStatus) String() string {
	switch s {
	case UnrollStatusPending:
		return "pending"

	case UnrollStatusBroadcasting:
		return "broadcasting"

	case UnrollStatusAwaitingCSV:
		return "awaiting_csv"

	case UnrollStatusComplete:
		return "complete"

	case UnrollStatusFailed:
		return "failed"

	default:
		return "unknown"
	}
}

// UnrollState tracks the progress of unrolling a single VTXO tree.
type UnrollState struct {
	// VTXOOutpoint identifies the VTXO being unrolled.
	VTXOOutpoint wire.OutPoint

	// VTXO is the full descriptor with tree path. We use round.ClientVTXO
	// directly since it contains all fields needed for unrolling (TreePath
	// and Expiry) and matches what the database layer returns.
	VTXO *round.ClientVTXO

	// LevelOrder contains TXIDs organized by tree level.
	LevelOrder []LevelTxids

	// CurrentLevel is the level currently being broadcast (0-indexed).
	CurrentLevel int

	// BroadcastTxids tracks which transactions have been broadcast.
	BroadcastTxids map[chainhash.Hash]bool

	// ConfirmedTxids tracks which transactions have confirmed.
	ConfirmedTxids map[chainhash.Hash]ConfirmationInfo

	// Status is the current unroll phase.
	Status UnrollStatus

	// LeafConfirmHeight is when the final leaf tx confirmed (for CSV).
	LeafConfirmHeight int32

	// Error tracks any failure reason.
	Error error

	// RetryCount tracks broadcast retry attempts.
	RetryCount int
}

// LevelTxids groups transactions by tree level for ordered broadcasting.
// Each level corresponds to a depth in the VTXO tree, where level 0 is
// the root transaction that spends the batch outpoint.
type LevelTxids struct {
	// Level is the tree depth (0 = root, increasing toward leaves).
	Level int

	// Txids contains the transaction hashes at this level, ordered
	// by child index for deterministic replay.
	Txids []chainhash.Hash

	// Nodes contains the corresponding tree nodes, aligned 1:1 with
	// Txids. Each node holds the unsigned transaction and witness
	// data needed to construct the signed transaction via ToSignedTx.
	Nodes []*tree.Node
}

// ConfirmationInfo records when and where a transaction was confirmed
// on-chain.
type ConfirmationInfo struct {
	// Height is the block height at which the transaction confirmed.
	Height int32

	// BlockHash is the hash of the block containing the transaction.
	BlockHash chainhash.Hash
}

// extractLevelOrder performs a BFS (breadth-first) traversal of the VTXO
// tree and returns transactions grouped by level. Level 0 is the root
// transaction that spends the batch outpoint, and the final level contains
// the leaf VTXO outputs. Children are visited in sorted key order to ensure
// deterministic traversal.
func extractLevelOrder(treePath *tree.Tree) ([]LevelTxids, error) {
	if treePath == nil || treePath.Root == nil {
		return nil, fmt.Errorf("tree path or root is nil")
	}

	// Use BFS to traverse the tree level by level.
	type nodeWithLevel struct {
		node  *tree.Node
		level int
	}

	queue := []*nodeWithLevel{{node: treePath.Root, level: 0}}

	levelMap := make(map[int][]chainhash.Hash)
	nodeMap := make(map[int][]*tree.Node)
	maxLevel := 0

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		node, level := item.node, item.level

		// Compute this node's txid. A failure here indicates
		// corrupt or incomplete tree data.
		txid, err := node.TXID()
		if err != nil {
			return nil, fmt.Errorf("compute txid at "+
				"level %d: %w", level, err)
		}

		levelMap[level] = append(levelMap[level], txid)
		nodeMap[level] = append(nodeMap[level], node)

		if level > maxLevel {
			maxLevel = level
		}

		// Sort child indices for deterministic traversal order,
		// since Go map iteration is non-deterministic.
		childKeys := make([]uint32, 0, len(node.Children))
		for idx := range node.Children {
			childKeys = append(childKeys, idx)
		}
		slices.Sort(childKeys)

		// Enqueue children for next level.
		for _, idx := range childKeys {
			child := node.Children[idx]
			if child != nil {
				queue = append(
					queue, &nodeWithLevel{
						node:  child,
						level: level + 1,
					},
				)
			}
		}
	}

	// Convert to ordered slice.
	result := make([]LevelTxids, maxLevel+1)
	for level := 0; level <= maxLevel; level++ {
		result[level] = LevelTxids{
			Level: level,
			Txids: levelMap[level],
			Nodes: nodeMap[level],
		}
	}

	return result, nil
}
