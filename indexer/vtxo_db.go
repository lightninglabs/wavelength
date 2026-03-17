package indexer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/arkrpc"
	clientdb "github.com/lightninglabs/darepo-client/db"
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

type vtxoLineage struct {
	roundID string

	commitmentTxID chainhash.Hash

	batchExpiry int32

	relativeExpiry uint32

	treeDepth int

	chainDepth int

	createdHeight int32

	treePath *tree.Tree

	treePathTLV []byte
}

// LineageResolver resolves authoritative VTXO lineage metadata for
// a given VTXO row. Implementations handle both round-backed and
// virtual (OOR-created) VTXOs.
type LineageResolver interface {
	// Resolve returns the authoritative lineage metadata for the
	// provided VTXO row, including the commitment round, tree
	// path, chain depth, and creation height.
	Resolve(ctx context.Context,
		row VTXORow) (*vtxoLineage, error)
}

// Compile-time check that *lineageResolver satisfies LineageResolver.
var _ LineageResolver = (*lineageResolver)(nil)

type lineageResolver struct {
	store Store

	roundRowByID map[rounds.RoundID]*RoundRow

	treeByKey map[subtreeTreeKey]*tree.Tree

	lineageByOutpoint map[string]*vtxoLineage

	sessionByID map[string]*OORSession

	checkpointsBySessionID map[string][]OORCheckpoint
}

// newLineageResolver creates a per-request resolver for authoritative VTXO
// lineage metadata.
func newLineageResolver(store Store,
	roundRowByID map[rounds.RoundID]*RoundRow) *lineageResolver {

	if roundRowByID == nil {
		roundRowByID = make(map[rounds.RoundID]*RoundRow)
	}

	return &lineageResolver{
		store:             store,
		roundRowByID:      roundRowByID,
		treeByKey:         make(map[subtreeTreeKey]*tree.Tree),
		lineageByOutpoint: make(map[string]*vtxoLineage),
		sessionByID:       make(map[string]*OORSession),
		checkpointsBySessionID: make(
			map[string][]OORCheckpoint,
		),
	}
}

// resolve returns the authoritative lineage metadata for the provided VTXO
// row.
func (r *lineageResolver) Resolve(ctx context.Context,
	row VTXORow) (*vtxoLineage, error) {

	key := row.Outpoint.String()
	if cached, ok := r.lineageByOutpoint[key]; ok {
		return cached, nil
	}

	var (
		lineage *vtxoLineage
		err     error
	)

	if row.RoundID != nil && row.BatchOutputIndex != nil {
		lineage, err = r.resolveRoundBacked(
			ctx, *row.RoundID, *row.BatchOutputIndex,
			[]wire.OutPoint{row.Outpoint},
		)
	} else {
		lineage, err = r.resolveVirtual(ctx, row)
	}
	if err != nil {
		return nil, err
	}

	r.lineageByOutpoint[key] = lineage

	return lineage, nil
}

// resolveRoundBacked resolves lineage for VTXOs directly backed by a
// round-created tree leaf.
func (r *lineageResolver) resolveRoundBacked(ctx context.Context,
	roundID rounds.RoundID, batchOutputIndex int32,
	targetOutpoints []wire.OutPoint) (*vtxoLineage, error) {

	roundRow, err := r.resolveRoundRow(ctx, roundID)
	if err != nil {
		return nil, err
	}

	key := subtreeTreeKey{
		roundIDHex: hex.EncodeToString(roundID[:]),
		batchIdx:   int(batchOutputIndex),
	}

	fullTree, ok := r.treeByKey[key]
	if !ok {
		fullTree, err = r.store.LoadVTXOTree(
			ctx, roundID, int(batchOutputIndex),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"load vtxo tree: %w", err,
			)
		}

		r.treeByKey[key] = fullTree
	}

	extracted, err := extractTreeForOutpoints(
		fullTree, targetOutpoints,
	)
	if err != nil {
		return nil, fmt.Errorf("extract tree path: %w", err)
	}

	treePathTLV, err := clientdb.SerializeTree(extracted)
	if err != nil {
		return nil, fmt.Errorf(
			"serialize tree path: %w", err,
		)
	}

	lineage := &vtxoLineage{
		roundID:        roundID.String(),
		commitmentTxID: roundRow.CommitmentTxid,
		relativeExpiry: uint32(roundRow.CsvDelay),
		treeDepth:      extracted.Depth(),
		chainDepth:     0,
		createdHeight:  0,
		treePath:       extracted,
		treePathTLV:    treePathTLV,
	}

	if roundRow.ConfirmationHeight != nil {
		lineage.createdHeight = *roundRow.ConfirmationHeight
		lineage.batchExpiry = *roundRow.ConfirmationHeight +
			roundRow.CsvDelay
	}

	return lineage, nil
}

// resolveVirtual resolves lineage for OOR-created virtual VTXOs by inheriting
// the commitment lineage from the checkpoint inputs that back the session.
func (r *lineageResolver) resolveVirtual(ctx context.Context,
	row VTXORow) (*vtxoLineage, error) {

	sessionID := append([]byte(nil), row.Outpoint.Hash[:]...)
	session, err := r.resolveSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("resolve OOR session: %w", err)
	}

	checkpoints, err := r.resolveSessionCheckpoints(
		ctx, sessionID, session,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"resolve OOR checkpoints: %w", err,
		)
	}

	parentOutpoints, err := sessionParentOutpoints(
		session, checkpoints,
	)
	if err != nil {
		return nil, err
	}
	if len(parentOutpoints) == 0 {
		return nil, fmt.Errorf("missing session parent inputs")
	}

	parentRows := make([]VTXORow, 0, len(parentOutpoints))
	parentLineages := make([]*vtxoLineage, 0, len(parentOutpoints))
	for _, parentOutpoint := range parentOutpoints {
		parentRow, err := r.store.GetVTXO(ctx, parentOutpoint)
		if err != nil {
			return nil, fmt.Errorf(
				"get parent vtxo %v: %w",
				parentOutpoint, err,
			)
		}

		parentLineage, err := r.Resolve(ctx, parentRow)
		if err != nil {
			return nil, fmt.Errorf(
				"resolve parent lineage %v: %w",
				parentOutpoint, err,
			)
		}

		parentRows = append(parentRows, parentRow)
		parentLineages = append(parentLineages, parentLineage)
	}

	return r.combineVirtualLineage(
		ctx, row.Outpoint, parentRows, parentOutpoints,
		parentLineages,
	)
}

// combineVirtualLineage folds the parent lineage set into the authoritative
// lineage for a virtual/OOR-created VTXO.
func (r *lineageResolver) combineVirtualLineage(ctx context.Context,
	outpoint wire.OutPoint, parentRows []VTXORow,
	parentOutpoints []wire.OutPoint,
	parentLineages []*vtxoLineage) (*vtxoLineage, error) {

	if len(parentLineages) == 0 {
		return nil, fmt.Errorf("missing parent lineage")
	}

	lineage := cloneLineage(parentLineages[0])

	maxChainDepth := lineage.chainDepth
	for i := 1; i < len(parentLineages); i++ {
		next := parentLineages[i]

		if next.roundID != lineage.roundID {
			return nil, fmt.Errorf("OOR VTXO %v spans "+
				"multiple rounds", outpoint)
		}

		if next.commitmentTxID != lineage.commitmentTxID {
			return nil, fmt.Errorf("OOR VTXO %v spans "+
				"multiple commitments", outpoint)
		}

		if next.batchExpiry != lineage.batchExpiry {
			return nil, fmt.Errorf("OOR VTXO %v spans "+
				"multiple batch expiries", outpoint)
		}

		if next.createdHeight != lineage.createdHeight {
			return nil, fmt.Errorf("OOR VTXO %v spans "+
				"multiple created heights", outpoint)
		}

		if next.relativeExpiry != lineage.relativeExpiry {
			return nil, fmt.Errorf("OOR VTXO %v spans "+
				"multiple CSV delays", outpoint)
		}

		if next.chainDepth > maxChainDepth {
			maxChainDepth = next.chainDepth
		}
	}

	if len(parentRows) > 1 {
		combined, err := r.tryResolveCombinedRoundPath(
			ctx, parentRows, parentOutpoints,
		)
		if err != nil {
			return nil, err
		}

		if combined != nil {
			lineage.treePath = combined.treePath
			lineage.treePathTLV = append(
				[]byte(nil), combined.treePathTLV...,
			)
			lineage.treeDepth = combined.treeDepth
		} else {
			for i := 1; i < len(parentLineages); i++ {
				if !bytes.Equal(
					parentLineages[i].treePathTLV,
					lineage.treePathTLV,
				) {

					return nil, fmt.Errorf("OOR VTXO %v "+
						"requires multiple commitment "+
						"paths", outpoint)
				}
			}
		}
	}

	if lineage.treePath == nil || len(lineage.treePathTLV) == 0 {
		return nil, fmt.Errorf("missing inherited tree path")
	}

	lineage.chainDepth = maxChainDepth + 1

	return lineage, nil
}

// tryResolveCombinedRoundPath extracts a combined commitment path when all
// parents are direct leaves in the same round-backed tree.
func (r *lineageResolver) tryResolveCombinedRoundPath(ctx context.Context,
	parentRows []VTXORow,
	parentOutpoints []wire.OutPoint) (*vtxoLineage, error) {

	if len(parentRows) != len(parentOutpoints) || len(parentRows) == 0 {
		return nil, nil
	}

	firstRow := parentRows[0]
	if firstRow.RoundID == nil || firstRow.BatchOutputIndex == nil {
		return nil, nil
	}

	for i := 1; i < len(parentRows); i++ {
		row := parentRows[i]
		if row.RoundID == nil || row.BatchOutputIndex == nil {
			return nil, nil
		}

		if *row.RoundID != *firstRow.RoundID {
			return nil, nil
		}

		if *row.BatchOutputIndex != *firstRow.BatchOutputIndex {
			return nil, nil
		}
	}

	return r.resolveRoundBacked(
		ctx, *firstRow.RoundID, *firstRow.BatchOutputIndex,
		parentOutpoints,
	)
}

// resolveRoundRow returns cached round metadata or fetches it on demand.
func (r *lineageResolver) resolveRoundRow(ctx context.Context,
	roundID rounds.RoundID) (*RoundRow, error) {

	if roundRow, ok := r.roundRowByID[roundID]; ok {
		return roundRow, nil
	}

	roundRow, err := r.store.GetRound(ctx, roundID)
	if err != nil {
		return nil, fmt.Errorf("get round: %w", err)
	}

	r.roundRowByID[roundID] = &roundRow

	return &roundRow, nil
}

// resolveSession returns cached OOR session state or fetches it on demand.
func (r *lineageResolver) resolveSession(ctx context.Context,
	sessionID []byte) (*OORSession, error) {

	key := hex.EncodeToString(sessionID)
	if session, ok := r.sessionByID[key]; ok {
		return session, nil
	}

	session, err := r.store.GetOORSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	r.sessionByID[key] = &session

	return &session, nil
}

// resolveSessionCheckpoints returns cached checkpoint PSBTs or fetches them
// on demand.
func (r *lineageResolver) resolveSessionCheckpoints(ctx context.Context,
	sessionID []byte, session *OORSession) ([]OORCheckpoint, error) {

	key := hex.EncodeToString(sessionID)
	if checkpoints, ok := r.checkpointsBySessionID[key]; ok {
		return checkpoints, nil
	}

	checkpoints, err := r.store.ListOORCheckpoints(
		ctx, int32(session.ID),
	)
	if err != nil {
		return nil, err
	}

	r.checkpointsBySessionID[key] = checkpoints

	return checkpoints, nil
}

// cloneLineage creates a defensive copy of immutable lineage metadata.
func cloneLineage(src *vtxoLineage) *vtxoLineage {
	if src == nil {
		return nil
	}

	return &vtxoLineage{
		roundID:        src.roundID,
		commitmentTxID: src.commitmentTxID,
		batchExpiry:    src.batchExpiry,
		relativeExpiry: src.relativeExpiry,
		treeDepth:      src.treeDepth,
		chainDepth:     src.chainDepth,
		createdHeight:  src.createdHeight,
		treePath:       src.treePath,
		treePathTLV: append(
			[]byte(nil), src.treePathTLV...,
		),
	}
}

// sessionParentOutpoints extracts the current VTXO parents that back an OOR
// session. Finalized checkpoints are authoritative; Ark PSBT inputs are a
// fallback for older rows that predate checkpoint persistence.
func sessionParentOutpoints(session *OORSession,
	checkpoints []OORCheckpoint) ([]wire.OutPoint, error) {

	if len(checkpoints) > 0 {
		outpoints := make([]wire.OutPoint, 0, len(checkpoints))
		seen := make(map[string]struct{}, len(checkpoints))

		for _, checkpoint := range checkpoints {
			if checkpoint.Psbt == nil ||
				checkpoint.Psbt.UnsignedTx == nil {

				return nil, fmt.Errorf("missing checkpoint tx")
			}

			if len(checkpoint.Psbt.UnsignedTx.TxIn) == 0 {
				return nil, fmt.Errorf("checkpoint has no " +
					"inputs")
			}

			outpoint := checkpoint.Psbt.UnsignedTx.TxIn[0].
				PreviousOutPoint
			key := outpoint.String()
			if _, ok := seen[key]; ok {
				continue
			}

			seen[key] = struct{}{}
			outpoints = append(outpoints, outpoint)
		}

		return outpoints, nil
	}

	if session == nil || len(session.ArkPsbt) == 0 {
		return nil, fmt.Errorf("missing OOR session package")
	}

	ark, err := psbt.NewFromRawBytes(
		bytes.NewReader(session.ArkPsbt), false,
	)
	if err != nil {
		return nil, fmt.Errorf("parse ark psbt: %w", err)
	}

	if ark.UnsignedTx == nil {
		return nil, fmt.Errorf("missing ark unsigned tx")
	}

	outpoints := make([]wire.OutPoint, 0, len(ark.UnsignedTx.TxIn))
	seen := make(map[string]struct{}, len(ark.UnsignedTx.TxIn))
	for _, txIn := range ark.UnsignedTx.TxIn {
		outpoint := txIn.PreviousOutPoint
		key := outpoint.String()
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		outpoints = append(outpoints, outpoint)
	}

	return outpoints, nil
}

// applyLineageMetadata copies authoritative lineage fields onto the RPC view.
func applyLineageMetadata(out *arkrpc.VTXO, lineage *vtxoLineage) error {
	if out == nil || lineage == nil {
		return nil
	}

	if lineage.roundID != "" {
		out.RoundId = lineage.roundID
	}

	if lineage.commitmentTxID != (chainhash.Hash{}) {
		out.CommitmentTxid = append(
			[]byte(nil), lineage.commitmentTxID[:]...,
		)
	}

	out.BatchExpiryHeight = lineage.batchExpiry
	out.RelativeExpiry = lineage.relativeExpiry
	out.TreeDepth = uint32(lineage.treeDepth)
	out.ChainDepth = uint32(lineage.chainDepth)
	out.CreatedHeight = lineage.createdHeight
	if lineage.treePath != nil {
		tp, err := arkrpc.TreePathFromTree(lineage.treePath)
		if err != nil {
			return fmt.Errorf("convert tree path: %w", err)
		}
		out.TreePath = tp
	}

	return nil
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
