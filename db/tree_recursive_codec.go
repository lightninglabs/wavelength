package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/rounds"
)

// TreeReader is the read-only query surface required by
// DeserializeTreeRecursive. Accepting an interface rather than the concrete
// *sqlc.Queries allows callers in other packages to supply their own
// narrower store interfaces without a hard dependency on sqlc.Queries.
type TreeReader interface {
	// GetVTXOTreeNodes returns all tree nodes for a given round and
	// batch output index.
	GetVTXOTreeNodes(ctx context.Context,
		arg sqlc.GetVTXOTreeNodesParams,
	) ([]sqlc.GetVTXOTreeNodesRow, error)

	// GetVTXOTreeNodeOutputs returns all outputs for every node in a
	// given tree.
	GetVTXOTreeNodeOutputs(ctx context.Context,
		arg sqlc.GetVTXOTreeNodeOutputsParams,
	) ([]sqlc.GetVTXOTreeNodeOutputsRow, error)

	// GetVTXOTreeCosigners returns all cosigner keys for every node
	// in a given tree.
	GetVTXOTreeCosigners(ctx context.Context,
		arg sqlc.GetVTXOTreeCosignersParams,
	) ([]sqlc.GetVTXOTreeCosignersRow, error)
}

// Compile-time check that *sqlc.Queries satisfies the TreeReader interface.
var _ TreeReader = (*sqlc.Queries)(nil)

// SerializeTreeRecursive stores a tree in normalized recursive format.
// This allows SQL queries to traverse the tree structure and answer
// questions like "find all leaves for a given cosigner" or "get the path
// to specific VTXOs".
func SerializeTreeRecursive(ctx context.Context, q *sqlc.Queries,
	roundID rounds.RoundID, batchOutputIndex int,
	vtxoTree *tree.Tree) error {

	if vtxoTree == nil || vtxoTree.Root == nil {
		return fmt.Errorf("tree or root is nil")
	}

	// Traverse the tree and insert each node.
	return serializeNodeRecursive(
		ctx, q, roundID, batchOutputIndex, vtxoTree.Root, "0",
		nil, 0, 0, vtxoTree.SweepTapscriptRoot,
	)
}

// serializeNodeRecursive recursively serializes a node and its children.
func serializeNodeRecursive(ctx context.Context, q *sqlc.Queries,
	roundID rounds.RoundID, batchOutputIndex int, node *tree.Node,
	nodeID string, parentNodeID *string, parentOutputIndex int, depth int,
	sweepTapscriptRoot []byte) error {

	if node == nil {
		return nil
	}

	// Determine if this is a leaf node.
	isLeaf := 0
	if len(node.Children) == 0 {
		isLeaf = 1
	}

	// Optional signature and final key.
	var signature []byte
	if node.Signature != nil {
		signature = node.Signature.Serialize()
	}

	var finalKey []byte
	if node.FinalKey != nil {
		finalKey = node.FinalKey.SerializeCompressed()
	}

	// Insert the node.
	var parentOutputIndexSQL sql.NullInt32
	if parentNodeID != nil {
		parentOutputIndexSQL = sql.NullInt32{
			Int32: int32(parentOutputIndex),
			Valid: true,
		}
	}

	var parentNodeIDSQL sql.NullString
	if parentNodeID != nil {
		parentNodeIDSQL = sql.NullString{
			String: *parentNodeID,
			Valid:  true,
		}
	}

	var signatureSQL []byte
	if signature != nil {
		signatureSQL = signature
	}

	var finalKeySQL []byte
	if finalKey != nil {
		finalKeySQL = finalKey
	}

	err := q.InsertVTXOTreeNode(ctx, sqlc.InsertVTXOTreeNodeParams{
		RoundID:           roundID[:],
		BatchOutputIndex:  int32(batchOutputIndex),
		NodeID:            nodeID,
		ParentNodeID:      parentNodeIDSQL,
		ParentOutputIndex: parentOutputIndexSQL,
		Depth:             int32(depth),
		IsLeaf:            int32(isLeaf),
		InputHash:         node.Input.Hash[:],
		InputIndex:        int32(node.Input.Index),
		Amount:            int64(node.Amount),
		Signature:         signatureSQL,
		FinalKey:          finalKeySQL,
	})
	if err != nil {
		return fmt.Errorf("insert node %s: %w", nodeID, err)
	}

	// Insert outputs for this node.
	for i, output := range node.Outputs {
		err = q.InsertVTXOTreeNodeOutput(ctx,
			sqlc.InsertVTXOTreeNodeOutputParams{
				RoundID:          roundID[:],
				BatchOutputIndex: int32(batchOutputIndex),
				NodeID:           nodeID,
				OutputIndex:      int32(i),
				Value:            output.Value,
				PkScript:         output.PkScript,
			})
		if err != nil {
			return fmt.Errorf("insert output %d for node %s: %w",
				i, nodeID, err)
		}
	}

	// Insert cosigners with index for ordering.
	for i, key := range node.CoSigners {
		keyBytes := key.SerializeCompressed()
		err = q.InsertVTXOTreeCosigner(ctx,
			sqlc.InsertVTXOTreeCosignerParams{
				RoundID:          roundID[:],
				BatchOutputIndex: int32(batchOutputIndex),
				NodeID:           nodeID,
				CosignerKey:      keyBytes,
				KeyIndex:         int32(i),
			})
		if err != nil {
			return fmt.Errorf(
				"insert cosigner %d for node %s: %w",
				i, nodeID, err,
			)
		}
	}

	// Recursively serialize children.
	// Sort child indices for deterministic ordering.
	childIndices := make([]uint32, 0, len(node.Children))
	for idx := range node.Children {
		childIndices = append(childIndices, idx)
	}
	sort.Slice(childIndices, func(i, j int) bool {
		return childIndices[i] < childIndices[j]
	})

	for _, childIdx := range childIndices {
		child := node.Children[childIdx]
		childNodeID := fmt.Sprintf("%s.%d", nodeID, childIdx)
		err = serializeNodeRecursive(
			ctx, q, roundID, batchOutputIndex, child,
			childNodeID, &nodeID, int(childIdx), depth+1,
			sweepTapscriptRoot,
		)
		if err != nil {
			return fmt.Errorf(
				"serialize child %d: %w", childIdx, err,
			)
		}
	}

	return nil
}

// DeserializeTreeRecursive reconstructs a tree from normalized recursive
// format.
func DeserializeTreeRecursive(ctx context.Context, q TreeReader,
	roundID rounds.RoundID, batchOutputIndex int,
	batchOutpoint wire.OutPoint, batchOutput *wire.TxOut,
	sweepTapscriptRoot []byte) (*tree.Tree, error) {

	// Load all nodes.
	nodes, err := q.GetVTXOTreeNodes(ctx, sqlc.GetVTXOTreeNodesParams{
		RoundID:          roundID[:],
		BatchOutputIndex: int32(batchOutputIndex),
	})
	if err != nil {
		return nil, fmt.Errorf("get nodes: %w", err)
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes found")
	}

	// Load all outputs.
	outputRows, err := q.GetVTXOTreeNodeOutputs(ctx,
		sqlc.GetVTXOTreeNodeOutputsParams{
			RoundID:          roundID[:],
			BatchOutputIndex: int32(batchOutputIndex),
		})
	if err != nil {
		return nil, fmt.Errorf("get outputs: %w", err)
	}

	// Group outputs by node_id.
	outputsByNode := make(map[string][]*wire.TxOut)
	for _, row := range outputRows {
		output := &wire.TxOut{
			Value:    row.Value,
			PkScript: row.PkScript,
		}
		outputsByNode[row.NodeID] = append(
			outputsByNode[row.NodeID], output,
		)
	}

	// Load all cosigners.
	cosignerRows, err := q.GetVTXOTreeCosigners(ctx,
		sqlc.GetVTXOTreeCosignersParams{
			RoundID:          roundID[:],
			BatchOutputIndex: int32(batchOutputIndex),
		})
	if err != nil {
		return nil, fmt.Errorf("get cosigners: %w", err)
	}

	// Group cosigners by node_id.
	cosignersByNode := make(map[string][]*btcec.PublicKey)
	for _, row := range cosignerRows {
		key, err := btcec.ParsePubKey(row.CosignerKey)
		if err != nil {
			return nil, fmt.Errorf("parse cosigner key: %w", err)
		}
		cosignersByNode[row.NodeID] = append(
			cosignersByNode[row.NodeID], key,
		)
	}

	// Build the node map.
	nodeMap := make(map[string]*tree.Node)
	for _, row := range nodes {
		// Reconstruct input outpoint.
		var inputHash [32]byte
		copy(inputHash[:], row.InputHash)
		input := wire.OutPoint{
			Hash:  inputHash,
			Index: uint32(row.InputIndex),
		}

		// Get outputs for this node.
		outputs := outputsByNode[row.NodeID]

		// Get cosigners for this node.
		cosigners := cosignersByNode[row.NodeID]

		// Reconstruct signature if present.
		var signature *schnorr.Signature
		if len(row.Signature) > 0 {
			sig, err := schnorr.ParseSignature(row.Signature)
			if err != nil {
				return nil, fmt.Errorf("parse signature: %w",
					err)
			}
			signature = sig
		}

		// Reconstruct final key if present.
		var finalKey *btcec.PublicKey
		if len(row.FinalKey) > 0 {
			key, err := btcec.ParsePubKey(row.FinalKey)
			if err != nil {
				return nil, fmt.Errorf("parse final key: %w",
					err)
			}
			finalKey = key
		}

		nodeMap[row.NodeID] = &tree.Node{
			Input:     input,
			Outputs:   outputs,
			CoSigners: cosigners,
			Children:  make(map[uint32]*tree.Node),
			Amount:    btcutil.Amount(row.Amount),
			Signature: signature,
			FinalKey:  finalKey,
		}
	}

	// Build the tree structure by connecting parents to children.
	var root *tree.Node
	for _, row := range nodes {
		node := nodeMap[row.NodeID]

		if !row.ParentNodeID.Valid {
			// This is the root node.
			root = node
			continue
		}

		// Add this node as a child of its parent.
		parentNode := nodeMap[row.ParentNodeID.String]
		if parentNode == nil {
			return nil, fmt.Errorf(
				"parent node %s not found for node %s",
				row.ParentNodeID.String, row.NodeID,
			)
		}

		childIdx := uint32(row.ParentOutputIndex.Int32)
		parentNode.Children[childIdx] = node
	}

	if root == nil {
		return nil, fmt.Errorf("root node not found")
	}

	return &tree.Tree{
		Root:               root,
		BatchOutpoint:      batchOutpoint,
		BatchOutput:        batchOutput,
		SweepTapscriptRoot: sweepTapscriptRoot,
	}, nil
}
