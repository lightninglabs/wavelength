package roundpb

import (
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// OutpointToProto converts a wire.OutPoint to a proto Outpoint.
func OutpointToProto(op wire.OutPoint) *Outpoint {
	hash := op.Hash[:]

	return &Outpoint{
		TxHash:      hash,
		OutputIndex: op.Index,
	}
}

// OutpointFromProto converts a proto Outpoint to a wire.OutPoint.
func OutpointFromProto(op *Outpoint) (wire.OutPoint, error) {
	if op == nil {
		return wire.OutPoint{}, fmt.Errorf("nil outpoint")
	}

	if len(op.TxHash) != chainhash.HashSize {
		return wire.OutPoint{}, fmt.Errorf("invalid tx hash length: %d",
			len(op.TxHash))
	}

	var hash chainhash.Hash
	copy(hash[:], op.TxHash)

	return wire.OutPoint{
		Hash:  hash,
		Index: op.OutputIndex,
	}, nil
}

// OutpointsToProto converts a slice of wire.OutPoint to proto Outpoints.
func OutpointsToProto(ops []wire.OutPoint) []*Outpoint {
	if ops == nil {
		return nil
	}

	result := make([]*Outpoint, len(ops))
	for i, op := range ops {
		result[i] = OutpointToProto(op)
	}

	return result
}

// OutpointsFromProto converts proto Outpoints to a slice of wire.OutPoint.
func OutpointsFromProto(ops []*Outpoint) ([]wire.OutPoint, error) {
	if ops == nil {
		return nil, nil
	}

	result := make([]wire.OutPoint, len(ops))
	for i, op := range ops {
		var err error
		result[i], err = OutpointFromProto(op)
		if err != nil {
			return nil, fmt.Errorf("outpoint[%d]: %w", i, err)
		}
	}

	return result, nil
}

// TxOutToProto converts a wire.TxOut to a proto TxOut.
func TxOutToProto(out *wire.TxOut) *TxOut {
	if out == nil {
		return nil
	}

	return &TxOut{
		Value:    out.Value,
		PkScript: out.PkScript,
	}
}

// TxOutFromProto converts a proto TxOut to a wire.TxOut. It rejects
// negative output values which would corrupt fee calculations.
func TxOutFromProto(out *TxOut) (*wire.TxOut, error) {
	if out == nil {
		return nil, nil
	}

	if out.Value < 0 {
		return nil, fmt.Errorf("negative output value: %d", out.Value)
	}

	return &wire.TxOut{
		Value:    out.Value,
		PkScript: out.PkScript,
	}, nil
}

// PSBTToBytes serializes a PSBT packet to bytes.
func PSBTToBytes(p *psbt.Packet) ([]byte, error) {
	if p == nil {
		return nil, nil
	}

	var buf []byte
	w := &bytesWriter{buf: &buf}
	if err := p.Serialize(w); err != nil {
		return nil, fmt.Errorf("serialize PSBT: %w", err)
	}

	return *w.buf, nil
}

// PSBTFromBytes deserializes a PSBT packet from bytes.
func PSBTFromBytes(b []byte) (*psbt.Packet, error) {
	if b == nil {
		return nil, nil
	}

	p, err := psbt.NewFromRawBytes(
		&bytesReader{data: b}, false,
	)
	if err != nil {
		return nil, fmt.Errorf("deserialize PSBT: %w", err)
	}

	return p, nil
}

// SchnorrSigToBytes converts a schnorr.Signature to 64 bytes.
func SchnorrSigToBytes(sig *schnorr.Signature) []byte {
	if sig == nil {
		return nil
	}

	return sig.Serialize()
}

// SchnorrSigFromBytes converts 64 bytes to a schnorr.Signature.
func SchnorrSigFromBytes(b []byte) (*schnorr.Signature, error) {
	if b == nil {
		return nil, nil
	}

	sig, err := schnorr.ParseSignature(b)
	if err != nil {
		return nil, fmt.Errorf("parse schnorr sig: %w", err)
	}

	return sig, nil
}

// TxIDToHex converts a tree.TxID (chainhash.Hash) to a hex string key for
// proto maps.
func TxIDToHex(id tree.TxID) string {
	return hex.EncodeToString(id[:])
}

// TxIDFromHex converts a hex string back to a tree.TxID.
func TxIDFromHex(s string) (tree.TxID, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return tree.TxID{}, fmt.Errorf("decode tx id hex: %w", err)
	}

	if len(b) != chainhash.HashSize {
		return tree.TxID{}, fmt.Errorf("invalid tx id length: %d",
			len(b))
	}

	var id tree.TxID
	copy(id[:], b)

	return id, nil
}

// OutpointToMapKey serializes a wire.OutPoint to a deterministic string
// key for use in proto maps. Uses the standard "hash:index" format.
func OutpointToMapKey(op wire.OutPoint) string {
	return op.String()
}

// OutpointFromMapKey deserializes a string key back to a
// wire.OutPoint. The key is expected in the standard "hash:index"
// format produced by wire.OutPoint.String(), where the hash is
// byte-reversed hex.
func OutpointFromMapKey(key string) (wire.OutPoint, error) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return wire.OutPoint{}, fmt.Errorf("invalid outpoint key: %q",
			key)
	}

	hash, err := chainhash.NewHashFromStr(parts[0])
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf("invalid hash in outpoint "+
			"key %q: %w", key, err)
	}

	idx, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf("invalid index in outpoint "+
			"key %q: %w", key, err)
	}

	return wire.OutPoint{
		Hash:  *hash,
		Index: uint32(idx),
	}, nil
}

// TreeToProto converts a tree.Tree to a proto VTXOTree by flattening the
// recursive node structure into a pre-order indexed slice.
func TreeToProto(t *tree.Tree) (*VTXOTree, error) {
	if t == nil {
		return nil, nil
	}

	// Flatten nodes in pre-order.
	var nodes []*TreeNode
	nodeIndex := make(map[*tree.Node]uint32)
	if err := flattenNode(
		t.Root, &nodes, nodeIndex,
	); err != nil {
		return nil, err
	}

	return &VTXOTree{
		Nodes:              nodes,
		BatchOutpoint:      OutpointToProto(t.BatchOutpoint),
		BatchOutput:        TxOutToProto(t.BatchOutput),
		SweepTapscriptRoot: t.SweepTapscriptRoot,
	}, nil
}

// flattenNode recursively flattens a tree node into the nodes slice.
func flattenNode(n *tree.Node, nodes *[]*TreeNode,
	index map[*tree.Node]uint32) error {

	if n == nil {
		return nil
	}

	myIdx := uint32(len(*nodes))
	index[n] = myIdx

	// Convert outputs.
	outputs := make([]*TxOut, len(n.Outputs))
	for i, out := range n.Outputs {
		outputs[i] = TxOutToProto(out)
	}

	// Convert co-signers.
	coSigners := make([][]byte, len(n.CoSigners))
	for i, pk := range n.CoSigners {
		coSigners[i] = pk.SerializeCompressed()
	}

	protoNode := &TreeNode{
		Input:     OutpointToProto(n.Input),
		Outputs:   outputs,
		CoSigners: coSigners,
		Children:  make(map[uint32]uint32),
		Amount:    int64(n.Amount),
		Signature: SchnorrSigToBytes(n.Signature),
	}

	*nodes = append(*nodes, protoNode)

	// Recurse into children in deterministic order so the
	// flattened output is stable across runs.
	childIndices := make([]uint32, 0, len(n.Children))
	for outIdx := range n.Children {
		childIndices = append(childIndices, outIdx)
	}
	sort.Slice(childIndices, func(i, j int) bool {
		return childIndices[i] < childIndices[j]
	})

	for _, outIdx := range childIndices {
		child := n.Children[outIdx]
		if err := flattenNode(
			child, nodes, index,
		); err != nil {
			return err
		}

		protoNode.Children[outIdx] = index[child]
	}

	return nil
}

// DefaultMaxTreeNodes is the upper bound on the number of nodes
// allowed in a VTXOTree received from the server. A round with 1000
// participants at depth 10 produces ~2000 nodes; 50,000 is generous.
const DefaultMaxTreeNodes = 50_000

// TreeFromProtoOption is a functional option for TreeFromProto that
// allows callers to override default validation parameters.
type TreeFromProtoOption func(*treeFromProtoConfig)

// treeFromProtoConfig holds configurable validation parameters for
// tree deserialization.
type treeFromProtoConfig struct {
	maxNodes int
}

// defaultTreeFromProtoConfig returns the default configuration.
func defaultTreeFromProtoConfig() treeFromProtoConfig {
	return treeFromProtoConfig{
		maxNodes: DefaultMaxTreeNodes,
	}
}

// WithMaxTreeNodes sets the maximum number of nodes allowed in a
// deserialized VTXOTree. A value of 0 disables the limit.
func WithMaxTreeNodes(maxNodes int) TreeFromProtoOption {
	return func(cfg *treeFromProtoConfig) {
		cfg.maxNodes = maxNodes
	}
}

// TreeFromProto converts a proto VTXOTree back to a tree.Tree by
// reconstructing the recursive node structure.
//
// NOTE: The returned tree nodes will have a nil FinalKey. Callers that
// need the aggregated taproot key must run Materialize on the tree to
// recompute FinalKey from CoSigners and the sweep tapscript root.
func TreeFromProto(pt *VTXOTree,
	opts ...TreeFromProtoOption) (*tree.Tree, error) {

	if pt == nil {
		return nil, nil
	}

	cfg := defaultTreeFromProtoConfig()
	for _, o := range opts {
		o(&cfg)
	}

	if len(pt.Nodes) == 0 {
		return nil, fmt.Errorf("empty tree nodes")
	}

	if cfg.maxNodes > 0 && len(pt.Nodes) > cfg.maxNodes {
		return nil, fmt.Errorf("tree has %d nodes, exceeds maximum %d",
			len(pt.Nodes), cfg.maxNodes)
	}

	// Convert all proto nodes to Go nodes.
	goNodes := make([]*tree.Node, len(pt.Nodes))
	for i, pn := range pt.Nodes {
		node, err := treeNodeFromProto(pn)
		if err != nil {
			return nil, fmt.Errorf("node[%d]: %w", i, err)
		}
		goNodes[i] = node
	}

	// Wire up children references. We enforce two structural
	// invariants:
	//
	// 1. Pre-order invariant: childIdx > i. Since flattenNode
	//    serializes in pre-order, children always have higher
	//    indices than parents. This prevents cycles (self-refs,
	//    mutual refs, back-edges) and diamond DAGs (shared
	//    children) in a single check.
	//
	// 2. Output index bounds: outIdx must be within the parent
	//    node's output count. Without this, downstream code that
	//    accesses Outputs[outIdx] would panic.
	for i, pn := range pt.Nodes {
		for outIdx, childIdx := range pn.Children {
			if childIdx <= uint32(i) {
				return nil, fmt.Errorf("node[%d] child index "+
					"%d must be > parent index (cycle or "+
					"back-reference)", i, childIdx)
			}

			if int(childIdx) >= len(goNodes) {
				return nil, fmt.Errorf("node[%d] child index "+
					"%d out of range", i, childIdx)
			}

			if int(outIdx) >= len(goNodes[i].Outputs) {
				return nil, fmt.Errorf("node[%d] child output "+
					"index %d out of range (node has %d "+
					"outputs)", i, outIdx,
					len(goNodes[i].Outputs))
			}

			goNodes[i].Children[outIdx] = goNodes[childIdx]
		}
	}

	// Compute FinalKey for each node now that we have the
	// sweep tapscript root. The constructors (NewLeafNode,
	// NewBranchNode) normally do this, but proto deserialization
	// bypasses them. Without FinalKey, signature verification
	// in VerifySigned will fail. Nodes without cosigners (e.g.
	// certain connector nodes) are skipped.
	for _, node := range goNodes {
		if len(node.CoSigners) == 0 {
			continue
		}

		// Copy cosigners before computing the final key
		// because MuSig2 key aggregation sorts the slice
		// in-place, which would reorder the node's
		// CoSigners field.
		csCopy := make(
			[]*btcec.PublicKey, len(node.CoSigners),
		)
		copy(csCopy, node.CoSigners)

		fk, fkErr := tree.ComputeFinalKey(
			csCopy, pt.SweepTapscriptRoot,
		)
		if fkErr != nil {
			return nil, fmt.Errorf("compute final key: %w", fkErr)
		}

		node.FinalKey = fk
	}

	batchOP, err := OutpointFromProto(pt.BatchOutpoint)
	if err != nil {
		return nil, fmt.Errorf("batch outpoint: %w", err)
	}

	batchOut, batchOutErr := TxOutFromProto(pt.BatchOutput)
	if batchOutErr != nil {
		return nil, fmt.Errorf("batch output: %w", batchOutErr)
	}

	return &tree.Tree{
		Root:               goNodes[0],
		BatchOutpoint:      batchOP,
		BatchOutput:        batchOut,
		SweepTapscriptRoot: pt.SweepTapscriptRoot,
	}, nil
}

// treeNodeFromProto converts a single proto TreeNode to a tree.Node.
func treeNodeFromProto(pn *TreeNode) (*tree.Node, error) {
	input, err := OutpointFromProto(pn.Input)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}

	// Convert outputs.
	outputs := make([]*wire.TxOut, len(pn.Outputs))
	for i, out := range pn.Outputs {
		txOut, txOutErr := TxOutFromProto(out)
		if txOutErr != nil {
			return nil, fmt.Errorf("output[%d]: %w", i, txOutErr)
		}
		outputs[i] = txOut
	}

	// Convert co-signers.
	coSigners := make(
		[]*btcec.PublicKey, len(pn.CoSigners),
	)
	for i, pkBytes := range pn.CoSigners {
		pk, err := btcec.ParsePubKey(pkBytes)
		if err != nil {
			return nil, fmt.Errorf("co_signer[%d]: %w", i, err)
		}
		coSigners[i] = pk
	}

	var sig *schnorr.Signature
	if len(pn.Signature) > 0 {
		sig, err = SchnorrSigFromBytes(pn.Signature)
		if err != nil {
			return nil, fmt.Errorf("signature: %w", err)
		}
	}

	return &tree.Node{
		Input:     input,
		Outputs:   outputs,
		CoSigners: coSigners,
		Children:  make(map[uint32]*tree.Node),
		Amount:    btcutil.Amount(pn.Amount),
		Signature: sig,
	}, nil
}

// BoardingInputSigToProto converts a domain BoardingInputSignature to its
// proto representation. It rejects input indices outside the int32 range
// to prevent silent truncation in the proto field.
func BoardingInputSigToProto(sig *types.BoardingInputSignature) (
	*BoardingInputSignature, error) {

	if sig.InputIndex < 0 || sig.InputIndex > math.MaxInt32 {
		return nil, fmt.Errorf("input index %d out of int32 range",
			sig.InputIndex)
	}

	return &BoardingInputSignature{
		InputIndex: int32(sig.InputIndex),
		Outpoint:   OutpointToProto(sig.Outpoint),
		ClientSignature: SchnorrSigToBytes(
			sig.ClientSignature,
		),
	}, nil
}

// MsgTxToBytes serializes a wire.MsgTx to bytes.
func MsgTxToBytes(tx *wire.MsgTx) ([]byte, error) {
	if tx == nil {
		return nil, nil
	}

	var buf []byte
	w := &bytesWriter{buf: &buf}
	if err := tx.Serialize(w); err != nil {
		return nil, fmt.Errorf("serialize tx: %w", err)
	}

	return *w.buf, nil
}

// MsgTxFromBytes deserializes a wire.MsgTx from bytes.
func MsgTxFromBytes(b []byte) (*wire.MsgTx, error) {
	if b == nil {
		return nil, nil
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	if err := tx.Deserialize(
		&bytesReader{data: b},
	); err != nil {
		return nil, fmt.Errorf("deserialize tx: %w", err)
	}

	return tx, nil
}

// bytesWriter implements io.Writer for PSBT serialization.
type bytesWriter struct {
	buf *[]byte
}

// Write appends p to the internal buffer.
func (w *bytesWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)

	return len(p), nil
}

// bytesReader implements io.Reader for PSBT deserialization.
type bytesReader struct {
	data []byte
	pos  int
}

// Read reads up to len(p) bytes from the internal buffer.
func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}

	n := copy(p, r.data[r.pos:])
	r.pos += n

	return n, nil
}
