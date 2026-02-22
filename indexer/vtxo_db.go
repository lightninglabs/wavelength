package indexer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/rounds"
)

type subtreeTreeKey struct {
	roundIDHex string
	batchIdx   int
}

type subtreeVirtualLeaf struct {
	row  sqlc.Vtxo
	leaf *arkrpc.VTXO
}

type subtreeDBInputs struct {
	leaves []*arkrpc.VTXO

	leafTxids map[string]struct{}

	targetOutpointsByTree map[subtreeTreeKey][]wire.OutPoint

	roundIDByHex map[string]rounds.RoundID

	virtualLeaves []*subtreeVirtualLeaf
}

func roundIDFromBytes(b []byte) (rounds.RoundID, error) {
	if len(b) != 16 {
		return rounds.RoundID{}, fmt.Errorf(
			"unexpected round_id length: %d", len(b),
		)
	}

	var id rounds.RoundID
	copy(id[:], b)

	return id, nil
}

func wireOutPointFromDBRow(outHash []byte, outIndex int32) (wire.OutPoint,
	error) {

	if len(outHash) != 32 {
		return wire.OutPoint{}, fmt.Errorf(
			"unexpected outpoint hash length: %d", len(outHash),
		)
	}

	var op wire.OutPoint
	copy(op.Hash[:], outHash)
	op.Index = uint32(outIndex)

	return op, nil
}

func rpcVTXOFromDB(v sqlc.Vtxo, roundRow *sqlc.Round) (*arkrpc.VTXO, error) {
	op, err := wireOutPointFromDBRow(v.OutpointHash, v.OutpointIndex)
	if err != nil {
		return nil, err
	}

	var batchIndex uint32
	if v.BatchOutputIndex.Valid {
		batchIndex = uint32(v.BatchOutputIndex.Int32)
	}

	out := &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: op.Hash[:],
			Vout: op.Index,
		},
		ValueSat:         uint64(v.Amount),
		PkScript:         append([]byte(nil), v.PkScript...),
		Status:           VTXOStatusFromStore(v.Status),
		BatchOutputIndex: batchIndex,
	}

	switch len(v.RoundID) {
	case 0:
		// Virtual/OOR VTXOs have no direct round linkage.

	case 16:
		roundID, err := roundIDFromBytes(v.RoundID)
		if err != nil {
			return nil, err
		}

		out.RoundId = roundID.String()

	default:
		return nil, fmt.Errorf(
			"unexpected round_id length: %d", len(v.RoundID),
		)
	}

	if roundRow != nil {
		if roundRow.CommitmentTxid != "" {
			txidHex := roundRow.CommitmentTxid
			txidBytes, err := hex.DecodeString(txidHex)
			if err != nil {
				return nil, fmt.Errorf(
					"decode commitment_txid: %w", err,
				)
			}
			out.CommitmentTxid = txidBytes
		}

		out.RelativeExpiry = uint32(roundRow.CsvDelay)
	}

	return out, nil
}

func loadSubtreeInputs(ctx context.Context, q *sqlc.Queries,
	allowedScripts map[string]struct{}, allowedScriptBytes [][]byte) (
	*subtreeDBInputs, error) {

	rows, err := q.ListVTXOsByPkScripts(ctx, allowedScriptBytes)
	if err != nil {
		return nil, fmt.Errorf("list vtxos by script: %w", err)
	}

	inputs := &subtreeDBInputs{
		leaves:                nil,
		leafTxids:             make(map[string]struct{}),
		targetOutpointsByTree: make(map[subtreeTreeKey][]wire.OutPoint),
		roundIDByHex:          make(map[string]rounds.RoundID),
		virtualLeaves:         nil,
	}

	roundRowByHex := make(map[string]*sqlc.Round)

	for _, row := range rows {
		scriptHex := hex.EncodeToString(row.PkScript)
		if _, ok := allowedScripts[scriptHex]; !ok {
			continue
		}

		roundHex := hex.EncodeToString(row.RoundID)
		hasRound := len(row.RoundID) > 0
		hasTreeLink := hasRound && row.BatchOutputIndex.Valid

		var roundRow *sqlc.Round
		if hasRound {
			rid, err := roundIDFromBytes(row.RoundID)
			if err != nil {
				return nil, err
			}

			inputs.roundIDByHex[roundHex] = rid

			rr, ok := roundRowByHex[roundHex]
			if !ok {
				rowCopy, err := q.GetRound(ctx, row.RoundID)
				if err != nil {
					return nil, fmt.Errorf(
						"get round: %w", err,
					)
				}

				rr = &rowCopy
				roundRowByHex[roundHex] = rr
			}

			roundRow = rr
		}

		vtxo, err := rpcVTXOFromDB(row, roundRow)
		if err != nil {
			return nil, err
		}

		inputs.leaves = append(inputs.leaves, vtxo)
		if vtxo.Outpoint != nil {
			txidHex := hex.EncodeToString(vtxo.Outpoint.Txid)
			inputs.leafTxids[txidHex] = struct{}{}
		}

		if !hasTreeLink {
			inputs.virtualLeaves = append(inputs.virtualLeaves,
				&subtreeVirtualLeaf{
					row:  row,
					leaf: vtxo,
				},
			)

			continue
		}

		wireOP, err := wireOutPointFromDBRow(
			row.OutpointHash, row.OutpointIndex,
		)
		if err != nil {
			return nil, err
		}

		key := subtreeTreeKey{
			roundIDHex: roundHex,
			batchIdx:   int(row.BatchOutputIndex.Int32),
		}

		inputs.targetOutpointsByTree[key] = append(
			inputs.targetOutpointsByTree[key], wireOP,
		)
	}

	return inputs, nil
}

func loadRoundVTXOTree(ctx context.Context, q *sqlc.Queries,
	roundID rounds.RoundID, batchOutputIndex int) (*tree.Tree, error) {

	roundRow, err := q.GetRound(ctx, roundID[:])
	if err != nil {
		return nil, fmt.Errorf("get round: %w", err)
	}

	if len(roundRow.FinalTx) == 0 {
		return nil, fmt.Errorf("missing final tx")
	}

	finalTx := &wire.MsgTx{}
	if err := finalTx.Deserialize(
		bytes.NewReader(roundRow.FinalTx),
	); err != nil {
		return nil, fmt.Errorf("deserialize final tx: %w", err)
	}

	sweepKey, err := btcec.ParsePubKey(roundRow.SweepKey)
	if err != nil {
		return nil, fmt.Errorf("parse sweep key: %w", err)
	}

	sweepTapLeaf, err := scripts.UnilateralCSVTimeoutTapLeaf(
		sweepKey, uint32(roundRow.CsvDelay),
	)
	if err != nil {
		return nil, fmt.Errorf("compute sweep tapscript: %w", err)
	}
	sweepTapRoot := sweepTapLeaf.TapHash()

	commitmentTxid := finalTx.TxHash()
	batchOutpoint := wire.OutPoint{
		Hash:  commitmentTxid,
		Index: uint32(batchOutputIndex),
	}

	if batchOutputIndex < 0 || batchOutputIndex >= len(finalTx.TxOut) {
		return nil, fmt.Errorf("batch output index out of range")
	}
	batchOutput := finalTx.TxOut[batchOutputIndex]

	vtxoTree, err := db.DeserializeTreeRecursive(
		ctx, q, roundID, batchOutputIndex,
		batchOutpoint, batchOutput, sweepTapRoot[:],
	)
	if err != nil {
		return nil, fmt.Errorf("deserialize tree: %w", err)
	}

	return vtxoTree, nil
}

func extractTreeForOutpoints(fullTree *tree.Tree,
	targetOutpoints []wire.OutPoint) (*tree.Tree, error) {

	if fullTree == nil || fullTree.Root == nil {
		return nil, fmt.Errorf("missing full tree")
	}
	if len(targetOutpoints) == 0 {
		return nil, fmt.Errorf("missing target outpoints")
	}

	leafIndexByOutpoint := make(map[string]int)
	var leafNodes []*tree.Node
	for leaf := range fullTree.Root.LeavesIter() {
		leafNodes = append(leafNodes, leaf)
	}

	for i, leaf := range leafNodes {
		op, err := leaf.GetNonAnchorOutpoint()
		if err != nil {
			return nil, fmt.Errorf("leaf outpoint: %w", err)
		}
		leafIndexByOutpoint[op.String()] = i
	}

	var targetLeafIndices []int
	for _, op := range targetOutpoints {
		idx, ok := leafIndexByOutpoint[op.String()]
		if !ok {
			return nil, fmt.Errorf("outpoint not found in tree")
		}

		targetLeafIndices = append(targetLeafIndices, idx)
	}

	extracted, err := fullTree.ExtractPathForIndices(targetLeafIndices...)
	if err != nil {
		return nil, fmt.Errorf("extract subtree: %w", err)
	}

	return extracted, nil
}

func serializeNodeSignedTx(node *tree.Node) ([]byte, error) {
	tx, err := node.ToSignedTx()
	if err != nil {
		tx, err = node.ToTx()
		if err != nil {
			return nil, err
		}
	}

	var signedTxBuf bytes.Buffer
	if err := tx.Serialize(&signedTxBuf); err != nil {
		return nil, err
	}

	return signedTxBuf.Bytes(), nil
}

func collectLeafProofTXs(extracted *tree.Tree) (map[string][]byte, error) {
	if extracted == nil || extracted.Root == nil {
		return nil, fmt.Errorf("missing extracted tree")
	}

	leafTXByTxid := make(map[string][]byte)
	for leaf := range extracted.Root.LeavesIter() {
		txid, err := leaf.TXID()
		if err != nil {
			return nil, err
		}

		signedTX, err := serializeNodeSignedTx(leaf)
		if err != nil {
			return nil, err
		}

		leafTXByTxid[txid.String()] = signedTX
	}

	return leafTXByTxid, nil
}

func enrichVirtualLeafProofs(ctx context.Context, q *sqlc.Queries,
	virtualLeaves []*subtreeVirtualLeaf) error {

	for _, virtualLeaf := range virtualLeaves {
		if virtualLeaf == nil || virtualLeaf.leaf == nil {
			continue
		}
		leaf := virtualLeaf.leaf

		lookupParams := sqlc.GetOORRecipientEventBySessionOutputParams{
			RecipientPkScript: append([]byte(nil),
				virtualLeaf.row.PkScript...),
			SessionID: append([]byte(nil),
				virtualLeaf.row.OutpointHash...),
			OutputIndex: virtualLeaf.row.OutpointIndex,
		}

		_, err := q.GetOORRecipientEventBySessionOutput(
			ctx, lookupParams,
		)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}

		sessionID := append(
			[]byte(nil), virtualLeaf.row.OutpointHash...,
		)
		sessionRow, err := q.GetOORSession(ctx, sessionID)
		if err != nil {
			return err
		}

		leaf.OorArkPsbt = append([]byte(nil),
			sessionRow.ArkPsbt...)

		if len(sessionRow.ArkPsbt) > 0 {
			leafTx := extractSerializedPSBTTX(
				sessionRow.ArkPsbt,
			)
			if len(leafTx) > 0 {
				leaf.LeafTx = leafTx
			}
		}

		checkpointRows, err := q.ListOORCheckpoints(
			ctx, int32(sessionRow.ID),
		)
		if err != nil {
			return err
		}

		for _, checkpointRow := range checkpointRows {
			leaf.OorFinalCheckpointPsbts = append(
				leaf.OorFinalCheckpointPsbts,
				append(
					[]byte(nil),
					checkpointRow.CheckpointPsbt...,
				),
			)
		}
	}

	return nil
}

// extractSerializedPSBTTX extracts and serializes the final TX from a PSBT.
//
// The indexer treats this proof material as optional and returns nil when the
// PSBT cannot be decoded or extracted.
func extractSerializedPSBTTX(packet []byte) []byte {
	pkt, err := psbt.NewFromRawBytes(bytes.NewReader(packet), false)
	if err != nil {
		return nil
	}

	tx, err := psbt.Extract(pkt)
	if err != nil {
		return nil
	}

	var txBuf bytes.Buffer
	if err := tx.Serialize(&txBuf); err != nil {
		return nil
	}

	return append([]byte(nil), txBuf.Bytes()...)
}

func recordSubtreeRPCView(extracted *tree.Tree, includeInternal bool,
	leafTxids map[string]struct{}, nodesByTxid map[string]*arkrpc.TreeNode,
	edgesByKey map[string]*arkrpc.TreeEdge) error {

	if extracted == nil || extracted.Root == nil {
		return fmt.Errorf("missing extracted tree")
	}

	var walk func(n *tree.Node) error
	walk = func(n *tree.Node) error {
		if n == nil {
			return nil
		}

		txid, err := n.TXID()
		if err != nil {
			return err
		}
		txidHex := txid.String()

		if includeInternal && !n.IsLeaf() {
			if _, isLeaf := leafTxids[txidHex]; !isLeaf {
				rawTX, err := serializeNodeSignedTx(n)
				if err != nil {
					return err
				}

				nodesByTxid[txidHex] = &arkrpc.TreeNode{
					Txid: txid[:],
					Input: &arkrpc.OutPoint{
						Txid: n.Input.Hash[:],
						Vout: n.Input.Index,
					},
					NumOutputs: uint32(len(n.Outputs)),
					RawTx:      rawTX,
				}
			}
		}

		for outIdx, child := range n.Children {
			childTxid, err := child.TXID()
			if err != nil {
				return err
			}

			edge := &arkrpc.TreeEdge{
				ParentTxid:        txid[:],
				ParentOutputIndex: outIdx,
				ChildTxid:         childTxid[:],
			}

			edgeKey := fmt.Sprintf("%s:%d:%s",
				txid.String(), outIdx, childTxid.String(),
			)
			edgesByKey[edgeKey] = edge

			if err := walk(child); err != nil {
				return err
			}
		}

		return nil
	}

	return walk(extracted.Root)
}
