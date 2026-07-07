package arkscript

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	// leafTemplateVersion is the current binary encoding
	// version for semantic leaf templates.
	leafTemplateVersion = uint8(1)

	// policyTemplateVersion is the current binary encoding
	// version for semantic policy templates.
	policyTemplateVersion = uint8(1)

	// MaxPolicyTemplateBytes caps the raw size of a policy template
	// blob accepted for decoding. Standard Ark policies are well under
	// 1 KiB; the headroom tolerates future custom policies while still
	// preventing decode-bomb amplification from untrusted sources.
	MaxPolicyTemplateBytes = 64 * 1024

	// MaxLeafTemplateBytes caps the raw size of a single leaf template
	// blob. Sized to hold the largest legitimate vHTLC-style leaf with
	// comfortable headroom.
	MaxLeafTemplateBytes = 16 * 1024

	// MaxPolicyLeaves caps the number of leaves in a single policy
	// template. Standard Ark VTXO policies have 2 leaves; vHTLC has
	// 6. The cap is intentionally generous for future policy shapes
	// while still bounding the per-policy work.
	MaxPolicyLeaves = 32

	// MaxPolicyDepth caps the AST recursion depth during decode. Ark
	// policies nest at most a couple of levels (e.g. CSV->Multisig,
	// Condition->Multisig). Anything deeper is either a bug or an
	// attempt to cause stack/CPU blowup.
	MaxPolicyDepth = 16

	// MaxPolicyNodes caps the total node count decoded from a single
	// policy or leaf blob. Prevents an attacker from amplifying a
	// moderate-size blob into a huge in-memory AST.
	MaxPolicyNodes = 256

	// MaxMultisigKeys caps the number of keys inside a single Multisig
	// node. Ark multisig nodes typically have 1-3 keys.
	MaxMultisigKeys = 64
)

// decodeBudget bounds work performed while decoding a policy or node
// blob. It is threaded through the recursive decode helpers so every
// nested node charges against the same depth and node-count caps.
type decodeBudget struct {
	// maxDepth is the allowed recursion depth.
	maxDepth int

	// maxNodes is the allowed total node count across the whole
	// decode call.
	maxNodes int

	// depth is the current recursion depth.
	depth int

	// nodes is the running node count.
	nodes int
}

// defaultDecodeBudget returns a budget initialised with the package
// default limits.
func defaultDecodeBudget() *decodeBudget {
	return &decodeBudget{
		maxDepth: MaxPolicyDepth,
		maxNodes: MaxPolicyNodes,
	}
}

// enter charges one level of recursion and one node against the
// budget. Callers must pair this with exit.
func (b *decodeBudget) enter() error {
	b.depth++
	b.nodes++

	if b.depth > b.maxDepth {
		return fmt.Errorf("policy decode depth %d exceeds maximum %d",
			b.depth, b.maxDepth)
	}

	if b.nodes > b.maxNodes {
		return fmt.Errorf("policy decode node count %d exceeds "+
			"maximum %d", b.nodes, b.maxNodes)
	}

	return nil
}

// exit releases one level of recursion.
func (b *decodeBudget) exit() {
	b.depth--
}

// nodeKind identifies the semantic AST node type during binary encoding.
type nodeKind uint8

const (
	// nodeKindMultisig encodes an N-of-N multisig node.
	nodeKindMultisig nodeKind = iota + 1

	// nodeKindCSV encodes a CSV-gated child node.
	nodeKindCSV

	// nodeKindCondition encodes a generic predicate-gated child node.
	nodeKindCondition
)

// LeafTemplate is the semantic representation of one policy leaf.
// Currently this always compiles to a base-leaf-version (0xc0) tapscript.
// If leaf-versioned policies are needed in the future, this type would
// need a LeafVersion field.
type LeafTemplate struct {
	// Node is the semantic AST that compiles into the tapscript leaf.
	Node Node
}

// Script compiles the semantic leaf into canonical tapscript bytes.
func (l LeafTemplate) Script() ([]byte, error) {
	if l.Node == nil {
		return nil, fmt.Errorf("leaf template node must be provided")
	}

	return l.Node.Script()
}

// Encode serializes the semantic leaf into a stable binary encoding.
func (l LeafTemplate) Encode() ([]byte, error) {
	nodeBytes, err := EncodeNode(l.Node)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := buf.WriteByte(leafTemplateVersion); err != nil {
		return nil, err
	}
	if err := writeVarBytes(&buf, nodeBytes); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// ParticipantKeys returns the unique public keys referenced by the leaf.
func (l LeafTemplate) ParticipantKeys() []*btcec.PublicKey {
	return collectNodeKeys(l.Node)
}

// DecodeLeafTemplate deserializes a binary leaf template using the default
// decode budget. It rejects blobs larger than MaxLeafTemplateBytes and
// caps AST recursion via MaxPolicyDepth/MaxPolicyNodes.
func DecodeLeafTemplate(raw []byte) (*LeafTemplate, error) {
	if len(raw) > MaxLeafTemplateBytes {
		return nil, fmt.Errorf("leaf template size %d exceeds "+
			"maximum %d", len(raw), MaxLeafTemplateBytes)
	}

	return decodeLeafTemplateWithBudget(raw, defaultDecodeBudget())
}

// decodeLeafTemplateWithBudget deserializes a leaf template while charging
// every nested node against the supplied budget. The budget is shared across
// all leaves decoded from the same policy template so a decode-bomb cannot
// amplify across leaf boundaries.
func decodeLeafTemplateWithBudget(raw []byte,
	budget *decodeBudget) (*LeafTemplate, error) {

	if len(raw) == 0 {
		return nil, fmt.Errorf("leaf template encoding is empty")
	}

	r := bytes.NewReader(raw)

	version, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if version != leafTemplateVersion {
		return nil, fmt.Errorf("unknown leaf template version %d",
			version)
	}

	nodeBytes, err := readVarBytes(r, "leaf template node")
	if err != nil {
		return nil, err
	}

	node, err := decodeNodeWithBudget(nodeBytes, budget)
	if err != nil {
		return nil, err
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("unexpected %d trailing bytes in leaf "+
			"template", r.Len())
	}

	return &LeafTemplate{
		Node: node,
	}, nil
}

// PolicyTemplate is the semantic representation of an Ark tapscript policy.
type PolicyTemplate struct {
	// Leaves are the semantic tapscript leaves in this policy.
	Leaves []LeafTemplate
}

// Compile builds the canonical compiled policy from the semantic template.
func (p *PolicyTemplate) Compile() (*CompiledPolicy, error) {
	if p == nil {
		return nil, fmt.Errorf("policy template must be provided")
	}
	if len(p.Leaves) == 0 {
		return nil, fmt.Errorf("policy template must contain leaves")
	}

	compiledLeaves := make([]PolicyLeaf, 0, len(p.Leaves))
	for i := range p.Leaves {
		script, err := p.Leaves[i].Script()
		if err != nil {
			return nil, fmt.Errorf("compile policy leaf %d: %w", i,
				err)
		}

		compiledLeaves = append(compiledLeaves, PolicyLeaf{
			Leaf: txscript.NewBaseTapLeaf(script),
		})
	}

	return BuildTree(compiledLeaves, &ARKNUMSKey)
}

// ValidateArkPolicy enforces Ark policy invariants on the semantic template.
func (p *PolicyTemplate) ValidateArkPolicy(
	opts PolicyValidationOpts,
) error {

	if p == nil {
		return fmt.Errorf("policy template must be provided")
	}
	if len(p.Leaves) == 0 {
		return fmt.Errorf("policy template must contain leaves")
	}

	nodes := make([]Node, 0, len(p.Leaves))
	for i := range p.Leaves {
		if _, err := p.Leaves[i].Script(); err != nil {
			return fmt.Errorf("compile policy leaf %d: %w", i, err)
		}
		nodes = append(nodes, p.Leaves[i].Node)
	}

	return ValidatePolicy(nodes, opts)
}

// Encode serializes the semantic policy template into a binary encoding.
// The encoding preserves the author's leaf order (not canonical order).
// Two templates with the same leaves in different order will produce
// different encoded bytes but identical compiled output keys.
func (p *PolicyTemplate) Encode() ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("policy template must be provided")
	}
	if len(p.Leaves) == 0 {
		return nil, fmt.Errorf("policy template must contain leaves")
	}

	var buf bytes.Buffer
	if err := buf.WriteByte(policyTemplateVersion); err != nil {
		return nil, err
	}

	if err := wire.WriteVarInt(&buf, 0, uint64(len(p.Leaves))); err != nil {
		return nil, err
	}

	for i := range p.Leaves {
		leafBytes, err := p.Leaves[i].Encode()
		if err != nil {
			return nil, fmt.Errorf("encode leaf %d: %w", i, err)
		}

		if err := writeVarBytes(&buf, leafBytes); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// ParticipantKeys returns the unique public keys referenced by the policy.
// The returned slice is sorted by x-only key bytes for deterministic output.
func (p *PolicyTemplate) ParticipantKeys() []*btcec.PublicKey {
	if p == nil {
		return nil
	}

	unique := make(map[string]*btcec.PublicKey)
	for i := range p.Leaves {
		for _, key := range p.Leaves[i].ParticipantKeys() {
			xOnly := string(schnorr.SerializePubKey(key))
			unique[xOnly] = key
		}
	}

	keys := make([]*btcec.PublicKey, 0, len(unique))
	for _, key := range unique {
		keys = append(keys, key)
	}

	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(
			schnorr.SerializePubKey(keys[i]),
			schnorr.SerializePubKey(keys[j]),
		) < 0
	})

	return keys
}

// DecodePolicyTemplate deserializes a binary policy template using the
// default decode budget. It rejects blobs larger than MaxPolicyTemplateBytes,
// policies with more than MaxPolicyLeaves leaves, and ASTs that exceed
// MaxPolicyDepth recursion or MaxPolicyNodes total nodes. The budget is
// shared across all leaves so a crafted blob cannot amplify across leaves.
func DecodePolicyTemplate(raw []byte) (*PolicyTemplate, error) {
	if len(raw) > MaxPolicyTemplateBytes {
		return nil, fmt.Errorf("policy template size %d exceeds "+
			"maximum %d", len(raw), MaxPolicyTemplateBytes)
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("policy template encoding is empty")
	}

	r := bytes.NewReader(raw)

	version, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if version != policyTemplateVersion {
		return nil, fmt.Errorf("unknown policy template version %d",
			version)
	}

	template := &PolicyTemplate{}

	leafCount, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}
	if leafCount == 0 {
		return nil, fmt.Errorf("policy template must contain leaves")
	}
	if leafCount > MaxPolicyLeaves {
		return nil, fmt.Errorf("policy template leaf count %d exceeds "+
			"maximum %d", leafCount, MaxPolicyLeaves)
	}

	// A single budget is shared across every leaf decode so an adversary
	// cannot spend MaxPolicyNodes per leaf — the caps apply to the whole
	// policy blob, not per-leaf.
	budget := defaultDecodeBudget()

	template.Leaves = make([]LeafTemplate, 0, leafCount)
	for i := uint64(0); i < leafCount; i++ {
		leafBytes, err := readVarBytes(r, "policy template leaf")
		if err != nil {
			return nil, err
		}

		if len(leafBytes) > MaxLeafTemplateBytes {
			return nil, fmt.Errorf("leaf %d size %d exceeds "+
				"maximum %d", i, len(leafBytes),
				MaxLeafTemplateBytes)
		}

		leaf, err := decodeLeafTemplateWithBudget(leafBytes, budget)
		if err != nil {
			return nil, fmt.Errorf("decode leaf %d: %w", i, err)
		}

		template.Leaves = append(template.Leaves, *leaf)
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("unexpected %d trailing bytes in "+
			"policy template", r.Len())
	}

	return template, nil
}

// EncodeNode serializes one semantic AST node into a stable binary encoding.
func EncodeNode(node Node) ([]byte, error) {
	if node == nil {
		return nil, fmt.Errorf("node must be provided")
	}

	var buf bytes.Buffer

	switch n := node.(type) {
	case *Multisig:
		if err := buf.WriteByte(byte(nodeKindMultisig)); err != nil {
			return nil, err
		}
		if err := wire.WriteVarInt(
			&buf, 0, uint64(len(n.Keys)),
		); err != nil {
			return nil, err
		}
		for i := range n.Keys {
			if n.Keys[i] == nil {
				return nil, fmt.Errorf("multisig key %d is nil",
					i)
			}
			if _, err := buf.Write(
				schnorr.SerializePubKey(n.Keys[i]),
			); err != nil {
				return nil, err
			}
		}

	case *CSV:
		if err := encodeUnaryNode(
			&buf, nodeKindCSV, n.Lock, n.Inner,
		); err != nil {
			return nil, err
		}

	case *Condition:
		if n.Inner == nil {
			return nil, fmt.Errorf("condition inner node must be " +
				"provided")
		}

		childBytes, err := EncodeNode(n.Inner)
		if err != nil {
			return nil, err
		}

		if err := buf.WriteByte(byte(nodeKindCondition)); err != nil {
			return nil, err
		}
		if err := writeVarBytes(&buf, n.Predicate); err != nil {
			return nil, err
		}
		if err := writeVarBytes(&buf, childBytes); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unsupported node type %T", node)
	}

	return buf.Bytes(), nil
}

// DecodeNode deserializes one semantic AST node from binary encoding using
// the default decode budget. Each nested node charges against
// MaxPolicyDepth and MaxPolicyNodes.
func DecodeNode(raw []byte) (Node, error) {
	return decodeNodeWithBudget(raw, defaultDecodeBudget())
}

// decodeNodeWithBudget deserializes one AST node while charging the supplied
// budget for each level of recursion and each node added to the tree.
func decodeNodeWithBudget(raw []byte, budget *decodeBudget) (Node, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("node encoding is empty")
	}

	if err := budget.enter(); err != nil {
		return nil, err
	}
	defer budget.exit()

	r := bytes.NewReader(raw)

	kindByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}

	node, err := decodeNodePayload(r, nodeKind(kindByte), budget)
	if err != nil {
		return nil, err
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("unexpected %d trailing bytes in node",
			r.Len())
	}

	return node, nil
}

// encodeUnaryNode serializes one lock-gated unary node.
func encodeUnaryNode(buf *bytes.Buffer, kind nodeKind, lock uint32,
	child Node) error {

	if child == nil {
		return fmt.Errorf("child node must be provided")
	}

	childBytes, err := EncodeNode(child)
	if err != nil {
		return err
	}

	if err := buf.WriteByte(byte(kind)); err != nil {
		return err
	}
	if err := wire.WriteVarInt(buf, 0, uint64(lock)); err != nil {
		return err
	}
	if err := writeVarBytes(buf, childBytes); err != nil {
		return err
	}

	return nil
}

// decodeNodePayload decodes the payload for one node kind, charging any
// recursive child decode against the supplied budget.
func decodeNodePayload(r *bytes.Reader, kind nodeKind,
	budget *decodeBudget) (Node, error) {

	switch kind {
	case nodeKindMultisig:
		keyCount, err := wire.ReadVarInt(r, 0)
		if err != nil {
			return nil, err
		}
		if keyCount == 0 {
			return nil, fmt.Errorf("multisig must contain keys")
		}

		// Sanity check: Ark multisig nodes have a small number
		// of keys (typically 1-3).
		if keyCount > MaxMultisigKeys {
			return nil, fmt.Errorf("multisig key count %d exceeds "+
				"maximum %d", keyCount, MaxMultisigKeys)
		}

		keys := make([]*btcec.PublicKey, 0, keyCount)
		for i := uint64(0); i < keyCount; i++ {
			keyBytes := make([]byte, schnorr.PubKeyBytesLen)
			if _, err := io.ReadFull(r, keyBytes); err != nil {
				return nil, err
			}

			key, err := schnorr.ParsePubKey(keyBytes)
			if err != nil {
				return nil, err
			}

			keys = append(keys, key)
		}

		return &Multisig{
			Keys: keys,
		}, nil

	case nodeKindCSV:
		return decodeLockedNode(r, budget)

	case nodeKindCondition:
		predicate, err := readVarBytes(r, "condition predicate")
		if err != nil {
			return nil, err
		}

		if len(predicate) == 0 {
			return nil, fmt.Errorf("condition predicate must not " +
				"be empty")
		}

		childBytes, err := readVarBytes(r, "condition child")
		if err != nil {
			return nil, err
		}

		child, err := decodeNodeWithBudget(childBytes, budget)
		if err != nil {
			return nil, err
		}

		return &Condition{
			Predicate: predicate,
			Inner:     child,
		}, nil

	default:
		return nil, fmt.Errorf("unknown node kind %d", kind)
	}
}

// decodeLockedNode decodes one CSV-gated unary node, charging the recursive
// child decode against the supplied budget.
func decodeLockedNode(r *bytes.Reader, budget *decodeBudget) (Node, error) {
	lock, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	if lock > math.MaxUint32 {
		return nil, fmt.Errorf("csv lock value %d exceeds uint32 max",
			lock)
	}

	childBytes, err := readVarBytes(r, "locked child")
	if err != nil {
		return nil, err
	}

	child, err := decodeNodeWithBudget(childBytes, budget)
	if err != nil {
		return nil, err
	}

	return &CSV{
		Lock:  uint32(lock),
		Inner: child,
	}, nil
}

// collectNodeKeys returns the unique public keys referenced by one node.
func collectNodeKeys(node Node) []*btcec.PublicKey {
	unique := make(map[string]*btcec.PublicKey)

	var walk func(Node)
	walk = func(n Node) {
		switch t := n.(type) {
		case *Multisig:
			for i := range t.Keys {
				if t.Keys[i] == nil {
					continue
				}

				xOnly := schnorr.SerializePubKey(
					t.Keys[i],
				)
				unique[string(xOnly)] = t.Keys[i]
			}

		case *CSV:
			walk(t.Inner)

		case *Condition:
			walk(t.Inner)
		}
	}

	walk(node)

	keys := make([]*btcec.PublicKey, 0, len(unique))
	for _, key := range unique {
		keys = append(keys, key)
	}

	return keys
}

// writeVarBytes writes one length-prefixed byte slice.
func writeVarBytes(w io.Writer, value []byte) error {
	if err := wire.WriteVarInt(w, 0, uint64(len(value))); err != nil {
		return err
	}

	if len(value) == 0 {
		return nil
	}

	_, err := w.Write(value)

	return err
}

// readVarBytes reads one length-prefixed byte slice. The inner
// cap matches the outer MaxPolicyTemplateBytes cap so a single
// nested field can never claim more bytes than the enclosing
// blob carries. The previous 1 << 20 (1 MiB) value was
// cooperatively redundant -- the outer DecodePolicyTemplate cap
// (64 KiB) made the larger inner cap unreachable in practice --
// but keeping the two numbers identical makes the invariant
// explicit and prevents a future loosening of either side from
// silently widening the other.
func readVarBytes(r *bytes.Reader, field string) ([]byte, error) {
	value, err := wire.ReadVarBytes(
		r, 0, MaxPolicyTemplateBytes, field,
	)
	if err != nil {
		return nil, err
	}

	return value, nil
}
