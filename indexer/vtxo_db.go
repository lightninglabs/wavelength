package indexer

import (
	"bytes"
	"context"
	"encoding/binary"
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

// ancestryFragment describes one rooted commitment-tree fragment that
// contributes ancestry to a VTXO. Round-direct VTXOs and same-commitment
// OOR VTXOs carry exactly one fragment; cross-commitment multi-input
// OOR VTXOs carry one fragment per distinct contributing
// commitment tx so the recipient can broadcast every required parent
// path on-chain when unrolling.
type ancestryFragment struct {
	// treePath is the extracted tree.Tree fragment from the batch root
	// to the served leaf. Populated for round-backed lineages; nil only
	// for fragments whose underlying tree could not be resolved (those
	// surface as a hard error in combineVirtualLineage).
	treePath *tree.Tree

	// treePathTLV is the TLV-serialized form of treePath, retained so
	// equality comparisons and persistence stay byte-identical without
	// re-serializing on every check.
	treePathTLV []byte

	// commitmentTxID is the commitment tx hash anchoring this fragment.
	commitmentTxID chainhash.Hash

	// inputIndices are the Ark tx input indices (within the OOR Ark tx
	// that produced the parent VTXO) that this fragment serves. Empty
	// for round-direct VTXOs.
	inputIndices []uint32

	// treeDepth is the depth of the served leaf within this fragment.
	treeDepth int
}

type vtxoLineage struct {
	roundID string

	// commitmentTxID is the commitment tx hash of the primary (most
	// restrictive) ancestry fragment. Distinct from per-fragment
	// commitmentTxID values when ancestryPaths spans multiple commitments.
	commitmentTxID chainhash.Hash

	batchExpiry int32

	relativeExpiry uint32

	chainDepth int

	createdHeight int32

	// ancestryPaths is the set of rooted tree fragments required for
	// unilateral exit. Round-direct and same-commitment OOR VTXOs hold
	// exactly one fragment; cross-commitment multi-input OOR VTXOs hold
	// one fragment per distinct contributing commitment tx.
	ancestryPaths []ancestryFragment
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

	// maxDepth bounds recursive descent through virtual VTXO
	// parent chains. Zero means use DefaultMaxLineageDepth.
	maxDepth int

	roundRowByID map[rounds.RoundID]*RoundRow

	treeByKey map[subtreeTreeKey]*tree.Tree

	lineageByOutpoint map[string]*vtxoLineage

	spentByTxidByOutpoint map[string][]byte

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
		store:                 store,
		roundRowByID:          roundRowByID,
		treeByKey:             make(map[subtreeTreeKey]*tree.Tree),
		lineageByOutpoint:     make(map[string]*vtxoLineage),
		spentByTxidByOutpoint: make(map[string][]byte),
		sessionByID:           make(map[string]*OORSession),
		checkpointsBySessionID: make(
			map[string][]OORCheckpoint,
		),
	}
}

// DefaultMaxLineageDepth is the default maximum recursive depth for
// virtual VTXO lineage resolution. Each OOR hop adds one level. This
// prevents stack exhaustion and excessive DB queries from
// pathologically deep or circular checkpoint chains. The value is
// aligned with the client-side defaultMaxUnrollDepth (64) plus
// headroom for in-flight sessions.
//
// TODO(roasbeef): Make configurable via Service-level config and
// align with the client's maxUnrollDepth at the protocol level.
const DefaultMaxLineageDepth = 100

// Resolve returns the authoritative lineage metadata for the provided
// VTXO row.
func (r *lineageResolver) Resolve(ctx context.Context,
	row VTXORow) (*vtxoLineage, error) {

	return r.resolveWithDepth(ctx, row, 0)
}

// resolveWithDepth is the internal resolver that tracks recursion
// depth through virtual VTXO parent chains.
func (r *lineageResolver) resolveWithDepth(ctx context.Context,
	row VTXORow, depth int) (*vtxoLineage, error) {

	limit := r.maxDepth
	if limit == 0 {
		limit = DefaultMaxLineageDepth
	}
	if depth > limit {
		return nil, fmt.Errorf("lineage resolution exceeded "+
			"max depth %d for %v", limit,
			row.Outpoint)
	}

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
		lineage, err = r.resolveVirtual(ctx, row, depth)
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
		chainDepth:     0,
		createdHeight:  0,
		ancestryPaths: []ancestryFragment{{
			treePath:       extracted,
			treePathTLV:    treePathTLV,
			commitmentTxID: roundRow.CommitmentTxid,
			treeDepth:      extracted.Depth(),
		}},
	}

	if roundRow.ConfirmationHeight != nil {
		lineage.createdHeight = *roundRow.ConfirmationHeight
		lineage.batchExpiry = *roundRow.ConfirmationHeight +
			roundRow.CsvDelay
	}

	return lineage, nil
}

// resolveVirtual resolves lineage for OOR-created virtual VTXOs by
// inheriting the commitment lineage from the checkpoint inputs that
// back the session. The depth parameter tracks recursive descent
// through parent chains to enforce maxLineageDepth.
func (r *lineageResolver) resolveVirtual(ctx context.Context,
	row VTXORow, depth int) (*vtxoLineage, error) {

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

		parentLineage, err := r.resolveWithDepth(
			ctx, parentRow, depth+1,
		)
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
// lineage for a virtual/OOR-created VTXO. Parents are grouped by
// commitment_txid; each group contributes one ancestryFragment so a
// cross-commitment multi-input OOR VTXO produces multiple fragments
// (one per distinct contributing commitment), preserving the recipient's
// ability to unilaterally exit every required parent path on-chain.
//
// Non-path metadata (roundID, commitment, expiries) is taken from the
// most restrictive parent across all groups, so the produced VTXO
// surfaces the worst-case expiry/sweep timing rather than masking it.
func (r *lineageResolver) combineVirtualLineage(ctx context.Context,
	outpoint wire.OutPoint, parentRows []VTXORow,
	parentOutpoints []wire.OutPoint,
	parentLineages []*vtxoLineage) (*vtxoLineage, error) {

	if len(parentLineages) == 0 {
		return nil, fmt.Errorf("missing parent lineage")
	}

	// Pick the most restrictive (earliest-expiring) parent across the
	// full set, regardless of commitment grouping. Track maxChainDepth
	// the same way; the produced VTXO is one OOR hop deeper than its
	// deepest parent.
	baseLineage := parentLineages[0]
	maxChainDepth := parentLineages[0].chainDepth
	for i := 1; i < len(parentLineages); i++ {
		next := parentLineages[i]

		if moreRestrictiveLineage(next, baseLineage) {
			baseLineage = next
		}

		if next.chainDepth > maxChainDepth {
			maxChainDepth = next.chainDepth
		}
	}

	// Group every inherited parent fragment by (commitment_txid,
	// batch-tree discriminator). Each group becomes one
	// ancestryFragment in the combined lineage.
	//
	// A parent can already carry multiple fragments when this OOR hop
	// spends a VTXO created by an earlier cross-round multi-input
	// transfer; in that case the same current Ark input index must be
	// attached to each inherited root. Map insertion order is
	// non-deterministic, so we also track the order fragments first
	// appeared to produce a stable fragment ordering across calls.
	//
	// Two parents that share a commitment but live in different batch
	// trees (e.g. distinct `batch_output_index` values within the same
	// round) belong in distinct groups so each batch tree contributes
	// its own AncestryPath. The discriminator is the parent row's
	// `BatchOutputIndex` for round-direct parents and the inherited
	// fragment's root tx id for multi-fragment OOR parents; either
	// uniquely identifies the batch tree within the commitment so
	// same-batch leaves still merge via `tryResolveCombinedRoundPath`
	// while same-commitment-different-batch parents stay split.
	groups := make(map[ancestryGroupKey]*ancestryGroupEntry)
	groupOrder := make([]ancestryGroupKey, 0, len(parentLineages))
	for i, parent := range parentLineages {
		var (
			row      VTXORow
			outpoint wire.OutPoint
		)
		if i < len(parentRows) {
			row = parentRows[i]
		}
		if i < len(parentOutpoints) {
			outpoint = parentOutpoints[i]
		}

		// fragments is the list of inherited roots this parent
		// contributes to the combined lineage. A round-direct or
		// same-commitment OOR parent has zero entries here, in
		// which case we synthesize a single fragment from the
		// parent's scalar commitment_txid so the per-fragment
		// grouping below treats it uniformly with multi-root
		// parents.
		fragments := parent.ancestryPaths
		if len(fragments) == 0 {
			fragments = []ancestryFragment{{
				commitmentTxID: parent.commitmentTxID,
			}}
		}

		for _, fragment := range fragments {
			// Fall back to the parent's scalar commitment_txid
			// when an inherited fragment has none of its own
			// (e.g. a degenerate fragment built from older data).
			// This keeps every fragment group keyed by a real
			// commitment hash so the resolver below can rebuild
			// the round-backed tree path.
			commitmentKey := fragment.commitmentTxID
			if commitmentKey == (chainhash.Hash{}) {
				commitmentKey = parent.commitmentTxID
			}

			key := ancestryGroupKey{
				commitment: commitmentKey,
				batchTree: ancestryBatchDiscriminator(
					commitmentKey, row, fragment,
				),
			}

			if _, ok := groups[key]; !ok {
				groups[key] = &ancestryGroupEntry{}
				groupOrder = append(groupOrder, key)
			}

			g := groups[key]
			g.rows = append(g.rows, row)
			g.outpoints = append(g.outpoints, outpoint)
			g.fragments = append(g.fragments, fragment)

			// Ark tx input index equals the parent index: the
			// OOR Ark tx pulls one checkpoint per input, in the
			// same order as parentLineages. When one parent
			// already inherits multiple roots (its ancestryPaths
			// has more than one fragment) the same input index
			// is intentionally attached to every commitment
			// group it contributes to, so each downstream tree
			// path knows which Ark input it serves.
			g.inputIndices = append(g.inputIndices, uint32(i))
		}
	}

	// Resolve each group into one ancestryFragment. Same-commitment
	// multi-leaf groups go through tryResolveCombinedRoundPath to merge
	// into one spanning subtree; single-parent groups inherit their
	// parent's first ancestry fragment directly.
	ancestry := make([]ancestryFragment, 0, len(groupOrder))
	for _, key := range groupOrder {
		g := groups[key]

		fragment, err := r.combineGroupAncestry(
			ctx, key.commitment, g,
		)
		if err != nil {
			return nil, err
		}

		ancestry = append(ancestry, fragment)
	}

	if len(ancestry) == 0 {
		return nil, fmt.Errorf("missing inherited ancestry path")
	}

	combined := cloneLineage(baseLineage)
	combined.ancestryPaths = ancestry
	combined.chainDepth = maxChainDepth + 1

	return combined, nil
}

// ancestryGroupEntry collects the parents that share one
// (commitment_txid, batch-tree) group during combineVirtualLineage.
// Used internally to feed the per-group ancestry fragment builder; not
// part of the public API.
type ancestryGroupEntry struct {
	rows         []VTXORow
	outpoints    []wire.OutPoint
	fragments    []ancestryFragment
	inputIndices []uint32
}

// ancestryGroupKey identifies a single batch tree within a commitment
// for the purposes of combineVirtualLineage's grouping pass. Two
// parents share a group only when they live in the same batch tree;
// otherwise each batch tree contributes its own AncestryPath so the
// recipient can publish each tree independently for unilateral exit.
type ancestryGroupKey struct {
	commitment chainhash.Hash

	// batchTree distinguishes batch trees within a commitment. Two
	// parents that share a (commitment, batchTree) collide on this
	// key and merge via tryResolveCombinedRoundPath; two parents that
	// share a commitment but live in different batch trees stay split
	// into separate AncestryPath entries.
	batchTree chainhash.Hash
}

// ancestryBatchDiscriminator computes the per-batch-tree component of
// the ancestryGroupKey for one parent fragment. The row's
// BatchOutputIndex is the authoritative batch-tree identifier when
// present (round-direct parents) and is preferred over the inherited
// root tx id; an OOR-created parent (BatchOutputIndex == nil) falls
// back to the inherited fragment's tree-root tx id, which is the only
// per-batch-tree identifier available for inherited fragments.
// Degenerate inherited fragments without a tree path fall back to the
// zero hash and the downstream H-4 precondition rejects them so they
// cannot leak through.
func ancestryBatchDiscriminator(commitment chainhash.Hash, row VTXORow,
	fragment ancestryFragment) chainhash.Hash {

	if row.BatchOutputIndex != nil {
		var buf [chainhash.HashSize + 4]byte
		copy(buf[:chainhash.HashSize], commitment[:])
		idx := uint32(*row.BatchOutputIndex)
		binary.BigEndian.PutUint32(
			buf[chainhash.HashSize:], idx,
		)

		return chainhash.HashH(buf[:])
	}

	if fragment.treePath != nil && fragment.treePath.Root != nil {
		if txid, err := fragment.treePath.Root.TXID(); err == nil {
			return txid
		}
	}

	return chainhash.Hash{}
}

// combineGroupAncestry builds one ancestryFragment for a same-commitment
// group of parent fragments. When the group has multiple round-direct
// parents the existing tryResolveCombinedRoundPath helper merges them into a
// single spanning subtree; otherwise the fragment is taken from the deepest
// inherited fragment in the group.
func (r *lineageResolver) combineGroupAncestry(ctx context.Context,
	commitmentTxID chainhash.Hash,
	g *ancestryGroupEntry) (ancestryFragment, error) {

	// Stable copy of input_indices so callers cannot mutate the
	// fragment via the original slice.
	indices := append([]uint32(nil), g.inputIndices...)

	// Multi-leaf round-backed group: try to merge into a single
	// spanning subtree within the same batch tree.
	if len(g.rows) > 1 {
		merged, err := r.tryResolveCombinedRoundPath(
			ctx, g.rows, g.outpoints,
		)
		if err != nil {
			return ancestryFragment{}, err
		}

		if merged != nil && len(merged.ancestryPaths) > 0 {
			fragment := merged.ancestryPaths[0]
			fragment.commitmentTxID = commitmentTxID
			fragment.inputIndices = indices
			fragment.treePathTLV = append(
				[]byte(nil), fragment.treePathTLV...,
			)

			return fragment, nil
		}

		// Same-commitment but distinct batch outputs (e.g. parents
		// rooted at different batch_output_index values within the
		// same round). Falls through to picking a representative
		// fragment below.
	}

	// Pick the deepest inherited ancestry as the representative for
	// this commitment group. A single current parent can contribute
	// multiple fragments here when it was itself produced by a
	// cross-round multi-input OOR hop.
	//
	// Skip candidates with no live tree path: a fragment carrying
	// only treePathTLV but no parsed tree.Tree would survive a
	// treeDepth-only comparator and then trip the nil-tree hard-error
	// in arkrpc.AncestryPathFromTree downstream, surfacing as a
	// confusing generic gRPC Internal rather than the typed lineage
	// error. Filtering at the picker keeps the failure attached to a
	// clear source.
	var (
		rep      ancestryFragment
		repFound bool
	)
	for _, candidate := range g.fragments {
		if candidate.treePath == nil {
			continue
		}
		if !repFound || candidate.treeDepth > rep.treeDepth {
			rep = candidate
			repFound = true
		}
	}

	if !repFound {
		return ancestryFragment{}, fmt.Errorf(
			"missing inherited tree path for commitment %s",
			commitmentTxID,
		)
	}

	rep.commitmentTxID = commitmentTxID
	rep.inputIndices = indices
	rep.treePathTLV = append([]byte(nil), rep.treePathTLV...)

	return rep, nil
}

// sameSingularLineage reports whether two parents can be represented by the
// same singular lineage metadata fields in the current VTXO RPC shape.
func sameSingularLineage(a, b *vtxoLineage) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.roundID == b.roundID &&
		a.commitmentTxID == b.commitmentTxID &&
		a.batchExpiry == b.batchExpiry &&
		a.createdHeight == b.createdHeight &&
		a.relativeExpiry == b.relativeExpiry
}

// moreRestrictiveLineage chooses the parent with the earliest known absolute
// expiry. Unknown zero expiries sort after known expiries.
func moreRestrictiveLineage(candidate, current *vtxoLineage) bool {
	if candidate == nil {
		return false
	}

	if current == nil {
		return true
	}

	if candidate.batchExpiry == 0 {
		return false
	}

	return current.batchExpiry == 0 ||
		candidate.batchExpiry < current.batchExpiry
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

// cloneLineage creates a defensive copy of lineage metadata. Scalar
// fields and per-fragment treePathTLV / inputIndices slices are
// deep-copied; tree.Tree pointers are intentionally shared because the
// resolver's `treeByKey` cache aliases the same pointer across every
// fragment that touches a given (round, batch_output_index) so
// repeated lookups within one resolver call avoid re-loading the full
// tree from disk.
//
// **Cache-aliasing invariant.** Because `tree.Tree` pointers are
// shared, callers must treat any cached *tree.Tree extracted from a
// fragment as immutable: mutating one fragment's tree would silently
// corrupt every other fragment that aliases it AND the resolver's
// cache, surfacing as inscrutable "ancestry path conversion failed"
// errors at the next consumer. This invariant is also documented at
// the type-level in `client/lib/tree.Tree`'s doc-comment so future
// callers do not need to chase a doc-comment chain to learn it.
// Fragments themselves are only replaced wholesale in
// combineVirtualLineage, never mutated in place.
func cloneLineage(src *vtxoLineage) *vtxoLineage {
	if src == nil {
		return nil
	}

	dst := &vtxoLineage{
		roundID:        src.roundID,
		commitmentTxID: src.commitmentTxID,
		batchExpiry:    src.batchExpiry,
		relativeExpiry: src.relativeExpiry,
		chainDepth:     src.chainDepth,
		createdHeight:  src.createdHeight,
	}

	if len(src.ancestryPaths) > 0 {
		dst.ancestryPaths = make(
			[]ancestryFragment, len(src.ancestryPaths),
		)
		for i, f := range src.ancestryPaths {
			tlvCopy := append([]byte(nil), f.treePathTLV...)
			dst.ancestryPaths[i] = ancestryFragment{
				treePath:       f.treePath,
				treePathTLV:    tlvCopy,
				commitmentTxID: f.commitmentTxID,
				inputIndices: append(
					[]uint32(nil), f.inputIndices...,
				),
				treeDepth: f.treeDepth,
			}
		}
	}

	return dst
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

			cpTx := checkpoint.Psbt.UnsignedTx
			for _, txIn := range cpTx.TxIn {
				outpoint := txIn.PreviousOutPoint
				key := outpoint.String()
				if _, ok := seen[key]; ok {
					continue
				}

				seen[key] = struct{}{}
				outpoints = append(outpoints, outpoint)
			}
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
	out.ChainDepth = uint32(lineage.chainDepth)
	out.CreatedHeight = lineage.createdHeight

	// Populate the wire-level ancestry_paths slice (one entry per
	// distinct contributing commitment tx). Round-direct and
	// same-commitment OOR VTXOs surface a length-1 slice; cross-
	// commitment multi-input OOR VTXOs surface one entry
	// per group.
	if len(lineage.ancestryPaths) > 0 {
		out.AncestryPaths = make(
			[]*arkrpc.AncestryPath, 0, len(lineage.ancestryPaths),
		)
		for _, fragment := range lineage.ancestryPaths {
			path, err := arkrpc.AncestryPathFromTree(
				fragment.treePath, fragment.commitmentTxID,
				fragment.inputIndices,
			)
			if err != nil {
				return fmt.Errorf(
					"convert ancestry path: %w", err,
				)
			}

			// Override the auto-derived tree_depth with the
			// resolver-tracked value so callers that observe
			// inherited depth (e.g. via virtual recursion) see
			// the same number the resolver computed.
			path.TreeDepth = uint32(fragment.treeDepth)
			out.AncestryPaths = append(out.AncestryPaths, path)
		}
	}

	return nil
}

// resolveSpentByTxid returns the Ark txid of the OOR session that spent the
// given outpoint, when one exists.
func (r *lineageResolver) resolveSpentByTxid(ctx context.Context,
	outpoint wire.OutPoint) ([]byte, error) {

	key := outpoint.String()
	if cached, ok := r.spentByTxidByOutpoint[key]; ok {
		return append([]byte(nil), cached...), nil
	}

	txid, err := r.store.GetOORSpendingSessionTxidByInput(
		ctx, outpoint,
	)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			r.spentByTxidByOutpoint[key] = nil

			return nil, nil
		}

		return nil, err
	}

	r.spentByTxidByOutpoint[key] = append([]byte(nil), txid...)

	return append([]byte(nil), txid...), nil
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
		out.OperatorPubkey = append(
			[]byte(nil), roundRow.OperatorPubKey...,
		)
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
