package unroller

import (
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

// LevelTxids groups transactions by tree level.
type LevelTxids struct {
	Level int
	Txids []chainhash.Hash
	Nodes []*tree.Node
}

// ConfirmationInfo tracks when a transaction confirmed.
type ConfirmationInfo struct {
	Height    int32
	BlockHash chainhash.Hash
}

// extractLevelOrder converts tree path to level-ordered TXIDs and nodes.
func extractLevelOrder(treePath *tree.Tree) []LevelTxids {
	if treePath == nil || treePath.Root == nil {
		return nil
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

		// Get this node's txid.
		txid, err := node.TXID()
		if err != nil {
			continue
		}

		levelMap[level] = append(levelMap[level], txid)
		nodeMap[level] = append(nodeMap[level], node)

		if level > maxLevel {
			maxLevel = level
		}

		// Enqueue children for next level.
		for _, child := range node.Children {
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

	return result
}
