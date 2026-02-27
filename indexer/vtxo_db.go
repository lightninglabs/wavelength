package indexer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/rounds"
)

type subtreeTreeKey struct {
	roundIDHex string
	batchIdx   int
}

type subtreeVirtualLeaf struct {
	row  VTXORow
	leaf *arkrpc.VTXO
}

type subtreeDBInputs struct {
	leaves []*arkrpc.VTXO

	leafTxids map[string]struct{}

	targetOutpointsByTree map[subtreeTreeKey][]wire.OutPoint

	roundIDByHex map[string]rounds.RoundID

	virtualLeaves []*subtreeVirtualLeaf
}

// rpcVTXOFromDB converts an indexer VTXORow and optional RoundRow
// into the proto VTXO representation used in RPC responses.
func rpcVTXOFromDB(v VTXORow, roundRow *RoundRow) (*arkrpc.VTXO, error) {
	var batchIndex uint32
	if v.BatchOutputIndex != nil {
		batchIndex = uint32(*v.BatchOutputIndex)
	}

	out := &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: v.Outpoint.Hash[:],
			Vout: v.Outpoint.Index,
		},
		ValueSat:         uint64(v.Amount),
		PkScript:         append([]byte(nil), v.PkScript...),
		Status:           VTXOStatusFromStore(v.Status),
		BatchOutputIndex: batchIndex,
	}

	if v.RoundID != nil {
		out.RoundId = v.RoundID.String()
	}

	if roundRow != nil {
		// CommitmentTxid is a chainhash.Hash; zero-value means
		// no commitment yet.
		zeroHash := [32]byte{}
		if roundRow.CommitmentTxid != zeroHash {
			out.CommitmentTxid = roundRow.CommitmentTxid[:]
		}

		out.RelativeExpiry = uint32(roundRow.CsvDelay)
	}

	return out, nil
}

// loadSubtreeInputs queries VTXOs by script and groups them into tree
// targets and virtual leaves for downstream subtree extraction.
//
// Round metadata is batch-fetched in a single query rather than
// per-round to avoid N+1 round lookups.
func loadSubtreeInputs(ctx context.Context, q Store,
	allowedScripts map[string]struct{},
	allowedScriptBytes [][]byte) (*subtreeDBInputs, error) {

	rows, err := q.ListVTXOsByPkScripts(ctx, allowedScriptBytes)
	if err != nil {
		return nil, fmt.Errorf("list vtxos by script: %w", err)
	}

	// First pass: collect unique round IDs referenced by the
	// filtered VTXO rows so we can batch-fetch them.
	uniqueRoundIDs := make(map[rounds.RoundID]struct{})
	for _, row := range rows {
		scriptHex := hex.EncodeToString(row.PkScript)
		if _, ok := allowedScripts[scriptHex]; !ok {
			continue
		}

		if row.RoundID != nil {
			uniqueRoundIDs[*row.RoundID] = struct{}{}
		}
	}

	// Batch-fetch all referenced rounds in a single query.
	roundRowByHex := make(map[string]*RoundRow, len(uniqueRoundIDs))
	if len(uniqueRoundIDs) > 0 {
		roundIDSlice := make(
			[]rounds.RoundID, 0, len(uniqueRoundIDs),
		)
		for id := range uniqueRoundIDs {
			roundIDSlice = append(roundIDSlice, id)
		}

		roundRows, err := q.ListRoundsByIDs(ctx, roundIDSlice)
		if err != nil {
			return nil, fmt.Errorf(
				"batch fetch rounds: %w", err,
			)
		}

		for i := range roundRows {
			rr := &roundRows[i]
			hexKey := hex.EncodeToString(rr.RoundID[:])
			roundRowByHex[hexKey] = rr
		}
	}

	inputs := &subtreeDBInputs{
		leaves:                nil,
		leafTxids:             make(map[string]struct{}),
		targetOutpointsByTree: make(map[subtreeTreeKey][]wire.OutPoint),
		roundIDByHex:          make(map[string]rounds.RoundID),
		virtualLeaves:         nil,
	}

	// Second pass: build subtree inputs using pre-fetched round
	// data.
	for _, row := range rows {
		scriptHex := hex.EncodeToString(row.PkScript)
		if _, ok := allowedScripts[scriptHex]; !ok {
			continue
		}

		hasRound := row.RoundID != nil
		hasTreeLink := hasRound && row.BatchOutputIndex != nil

		var (
			roundHex string
			roundRow *RoundRow
		)

		if hasRound {
			roundHex = hex.EncodeToString(row.RoundID[:])
			inputs.roundIDByHex[roundHex] = *row.RoundID
			roundRow = roundRowByHex[roundHex]
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
			inputs.virtualLeaves = append(
				inputs.virtualLeaves,
				&subtreeVirtualLeaf{
					row:  row,
					leaf: vtxo,
				},
			)

			continue
		}

		key := subtreeTreeKey{
			roundIDHex: roundHex,
			batchIdx:   int(*row.BatchOutputIndex),
		}

		inputs.targetOutpointsByTree[key] = append(
			inputs.targetOutpointsByTree[key],
			row.Outpoint,
		)
	}

	return inputs, nil
}

// extractTreeForOutpoints extracts the minimal subtree containing the
// paths to the given target outpoints from the full VTXO tree.
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
			return nil, fmt.Errorf(
				"outpoint not found in tree",
			)
		}

		targetLeafIndices = append(targetLeafIndices, idx)
	}

	extracted, err := fullTree.ExtractPathForIndices(
		targetLeafIndices...,
	)
	if err != nil {
		return nil, fmt.Errorf("extract subtree: %w", err)
	}

	return extracted, nil
}

// serializeNodeSignedTx extracts and serializes the signed (or
// unsigned) transaction from a tree node.
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

// collectLeafProofTXs serializes every leaf transaction in the
// extracted subtree, keyed by txid hex.
func collectLeafProofTXs(
	extracted *tree.Tree) (map[string][]byte, error) {

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

// enrichVirtualLeafProofs populates OOR proof data (Ark PSBT,
// checkpoint PSBTs, leaf tx) for virtual VTXO leaves that are linked
// to finalized OOR sessions.
func enrichVirtualLeafProofs(ctx context.Context, q OORReader,
	virtualLeaves []*subtreeVirtualLeaf) error {

	for _, virtualLeaf := range virtualLeaves {
		if virtualLeaf == nil || virtualLeaf.leaf == nil {
			continue
		}
		leaf := virtualLeaf.leaf

		// For OOR VTXOs, the outpoint txid is the Ark txid, which is
		// also the OOR session identifier (see oor/interfaces.go
		// SessionID and oor/actor.go sessionID :=
		// SessionID(validated.ArkTxid)).
		_, err := q.GetOORRecipientEventBySessionOutput(
			ctx,
			append([]byte(nil), virtualLeaf.row.PkScript...),
			virtualLeaf.row.Outpoint.Hash[:],
			int32(virtualLeaf.row.Outpoint.Index),
		)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}

		sessionID := append(
			[]byte(nil),
			virtualLeaf.row.Outpoint.Hash[:]...,
		)
		sessionRow, err := q.GetOORSession(ctx, sessionID)
		if err != nil {
			return err
		}

		leaf.OorArkPsbt = append(
			[]byte(nil), sessionRow.ArkPsbt...,
		)

		if len(sessionRow.ArkPsbt) > 0 {
			leafTx := extractSerializedPSBTTX(
				sessionRow.ArkPsbt,
			)
			if len(leafTx) > 0 {
				leaf.LeafTx = leafTx
			}
		}

		checkpoints, err := q.ListOORCheckpoints(
			ctx, int32(sessionRow.ID),
		)
		if err != nil {
			return err
		}

		for _, cp := range checkpoints {
			cpBytes, err := serializePSBT(cp.Psbt)
			if err != nil {
				return fmt.Errorf(
					"serialize checkpoint: %w", err,
				)
			}

			leaf.OorFinalCheckpointPsbts = append(
				leaf.OorFinalCheckpointPsbts, cpBytes,
			)
		}
	}

	return nil
}

// extractSerializedPSBTTX extracts and serializes the final TX from a
// PSBT.
//
// The indexer treats this proof material as optional and returns nil
// when the PSBT cannot be decoded or extracted.
func extractSerializedPSBTTX(packet []byte) []byte {
	pkt, err := psbt.NewFromRawBytes(
		bytes.NewReader(packet), false,
	)
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

// serializePSBT serializes a parsed PSBT packet to raw bytes.
func serializePSBT(pkt *psbt.Packet) ([]byte, error) {
	if pkt == nil {
		return nil, fmt.Errorf("nil psbt packet")
	}

	var buf bytes.Buffer
	if err := pkt.Serialize(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// recordSubtreeRPCView populates the RPC node and edge maps from
// the extracted subtree for the GetSubtreeByScripts response.
func recordSubtreeRPCView(extracted *tree.Tree,
	includeInternal bool,
	leafTxids map[string]struct{},
	nodesByTxid map[string]*arkrpc.TreeNode,
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
					NumOutputs: uint32(
						len(n.Outputs),
					),
					RawTx: rawTX,
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

			edgeKey := fmt.Sprintf(
				"%s:%d:%s",
				txid.String(), outIdx,
				childTxid.String(),
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
