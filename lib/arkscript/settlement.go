package arkscript

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
)

// SettlementPair captures the unilateral proof/auth path and the paired
// operator-backed forfeit path for one participant branch of a custom policy.
type SettlementPair struct {
	// ParticipantKey is the participant key this settlement
	// pair belongs to.
	ParticipantKey *btcec.PublicKey

	// AuthLeafIndex is the canonical leaf index of the
	// unilateral auth path.
	AuthLeafIndex int

	// AuthPath is the unilateral proof/auth spend path for the participant.
	AuthPath *SpendPath

	// ForfeitLeafIndex is the canonical leaf index of the
	// operator-backed path.
	ForfeitLeafIndex int

	// ForfeitPath is the spend path used by the later round forfeit tx.
	ForfeitPath *SpendPath
}

// SettlementPairsForParticipant derives the settlement pairs available to the
// given participant within the semantic policy.
func (p *PolicyTemplate) SettlementPairsForParticipant(
	participant, operator *btcec.PublicKey) ([]SettlementPair, error) {

	if p == nil {
		return nil, fmt.Errorf("policy template must be provided")
	}

	if participant == nil {
		return nil, fmt.Errorf("participant key must be provided")
	}

	if operator == nil {
		return nil, fmt.Errorf("operator key must be provided")
	}

	compiled, err := p.Compile()
	if err != nil {
		return nil, err
	}

	// Build a mapping from template leaf index to canonical leaf
	// index. Compile() sorts leaves canonically, so template order
	// may differ from the compiled tree order.
	templateToCanonical := make(map[int]int, len(p.Leaves))
	for i := range p.Leaves {
		script, err := p.Leaves[i].Script()
		if err != nil {
			return nil, fmt.Errorf("compile template leaf %d: %w",
				i, err)
		}

		for j, cl := range compiled.Leaves {
			if bytes.Equal(script, cl.Leaf.Script) {
				templateToCanonical[i] = j

				break
			}
		}
	}

	type candidate struct {
		templateIndex  int
		canonicalIndex int
		node           Node
		key            []byte
	}

	var (
		authCandidates    []candidate
		forfeitCandidates []candidate
	)

	for i := range p.Leaves {
		node := p.Leaves[i].Node
		if node == nil {
			return nil, fmt.Errorf("policy leaf %d has nil node", i)
		}

		canonIdx, ok := templateToCanonical[i]
		if !ok {
			return nil, fmt.Errorf("template leaf %d not found in "+
				"compiled tree", i)
		}

		norm, err := normalizeSettlementNode(node, operator)
		if err != nil {
			return nil, fmt.Errorf("normalize policy leaf %d: %w",
				i, err)
		}

		normKey, err := EncodeNode(norm)
		if err != nil {
			return nil, fmt.Errorf("encode normalized leaf %d: %w",
				i, err)
		}

		switch {
		case ContainsKey(node, participant) &&
			ContainsKey(node, operator):

			forfeitCandidates = append(
				forfeitCandidates, candidate{
					templateIndex:  i,
					canonicalIndex: canonIdx,
					node:           node,
					key:            normKey,
				},
			)

		case ContainsKey(node, participant):
			authCandidates = append(
				authCandidates, candidate{
					templateIndex:  i,
					canonicalIndex: canonIdx,
					node:           node,
					key:            normKey,
				},
			)
		}
	}

	pairs := make([]SettlementPair, 0, len(authCandidates))
	for _, auth := range authCandidates {
		var matched *candidate
		for idx := range forfeitCandidates {
			if bytes.Equal(auth.key, forfeitCandidates[idx].key) {
				matched = &forfeitCandidates[idx]

				break
			}
		}

		if matched == nil {
			continue
		}

		authPath, err := spendPathForLeaf(
			compiled, auth.canonicalIndex, auth.node, nil,
		)
		if err != nil {
			return nil, fmt.Errorf("auth spend path: %w", err)
		}

		forfeitPath, err := spendPathForLeaf(
			compiled, matched.canonicalIndex, matched.node, nil,
		)
		if err != nil {
			return nil, fmt.Errorf("forfeit spend path: %w", err)
		}

		pairs = append(pairs, SettlementPair{
			ParticipantKey:   participant,
			AuthLeafIndex:    auth.canonicalIndex,
			AuthPath:         authPath,
			ForfeitLeafIndex: matched.canonicalIndex,
			ForfeitPath:      forfeitPath,
		})
	}

	if len(pairs) == 0 {
		return nil, fmt.Errorf("no settlement pairs for participant")
	}

	return pairs, nil
}

// ExtractAbsoluteLockTime returns the absolute locktime required by the AST
// node, or zero when no recognized CLTV predicate is present.
func ExtractAbsoluteLockTime(node Node) uint32 {
	switch n := node.(type) {
	case *CSV:
		return ExtractAbsoluteLockTime(n.Inner)

	case *Condition:
		lock, ok := parseAbsoluteLockTimePredicate(n.Predicate)
		if ok {
			return lock
		}

		return ExtractAbsoluteLockTime(n.Inner)

	default:
		return 0
	}
}

// spendPathForLeaf derives a spend path for a compiled leaf and applies
// tx-context requirements inferred from the AST node.
func spendPathForLeaf(policy *CompiledPolicy, leafIndex int, node Node,
	conditions [][]byte) (*SpendPath, error) {

	info, err := policy.SpendInfo(leafIndex)
	if err != nil {
		return nil, err
	}

	return &SpendPath{
		SpendInfo:        info,
		RequiredSequence: DeriveSequence(node),
		RequiredLockTime: ExtractAbsoluteLockTime(node),
		Conditions:       cloneWitnessItems(conditions),
	}, nil
}

// normalizeSettlementNode strips Ark exit-only structure and operator keys so
// the unilateral and operator-backed leaves of one business branch compare
// equal.
func normalizeSettlementNode(node Node,
	operator *btcec.PublicKey) (Node, error) {

	switch n := node.(type) {
	case *CSV:
		return normalizeSettlementNode(n.Inner, operator)

	case *Condition:
		inner, err := normalizeSettlementNode(n.Inner, operator)
		if err != nil {
			return nil, err
		}

		return &Condition{
			Predicate: bytes.Clone(n.Predicate),
			Inner:     inner,
		}, nil

	case *Multisig:
		keys := make([]*btcec.PublicKey, 0, len(n.Keys))
		for _, key := range n.Keys {
			if key == nil {
				return nil, fmt.Errorf("multisig key is nil")
			}

			if operator != nil && sameXOnlyKey(key, operator) {
				continue
			}

			keys = append(keys, key)
		}

		if len(keys) == 0 {
			return nil, fmt.Errorf("normalized multisig has no " +
				"keys")
		}

		return &Multisig{Keys: keys}, nil

	default:
		return nil, fmt.Errorf("unsupported settlement node %T", node)
	}
}

// parseAbsoluteLockTimePredicate extracts the absolute locktime from the
// canonical CLTV predicate prefix when present.
func parseAbsoluteLockTimePredicate(script []byte) (uint32, bool) {
	tokenizer := txscript.MakeScriptTokenizer(0, script)
	if !tokenizer.Next() {
		return 0, false
	}

	lock, ok := parseScriptNumToken(&tokenizer)
	if !ok {
		return 0, false
	}

	if !tokenizer.Next() ||
		tokenizer.Opcode() != txscript.OP_CHECKLOCKTIMEVERIFY {
		return 0, false
	}

	if !tokenizer.Next() || tokenizer.Opcode() != txscript.OP_DROP {
		return 0, false
	}

	if tokenizer.Next() || tokenizer.Err() != nil {
		return 0, false
	}

	if lock < 0 || lock > int64(^uint32(0)) {
		return 0, false
	}

	return uint32(lock), true
}

// cltvScriptNumLen is the max byte length of a script number operand used
// with OP_CHECKLOCKTIMEVERIFY, per BIP-65. Absolute locktimes can exceed
// the 4-byte range (e.g. past Nov 2038), so CLTV permits one extra byte
// above the default 4-byte ceiling used by most opcodes.
const cltvScriptNumLen = 5

// parseScriptNumToken decodes the current tokenizer token as a minimally
// encoded script number. Small-integer opcodes (OP_0, OP_1..OP_16,
// OP_1NEGATE) are handled inline; everything else is delegated to
// txscript.MakeScriptNum so the consensus minimal-encoding rules stay
// anchored to the upstream interpreter.
func parseScriptNumToken(tokenizer *txscript.ScriptTokenizer) (int64, bool) {
	if tokenizer == nil {
		return 0, false
	}

	opcode := tokenizer.Opcode()
	switch {
	case opcode == txscript.OP_0:
		return 0, true

	case opcode >= txscript.OP_1 && opcode <= txscript.OP_16:
		return int64(opcode - (txscript.OP_1 - 1)), true

	case opcode == txscript.OP_1NEGATE:
		return -1, true
	}

	data := tokenizer.Data()
	if len(data) == 0 {
		return 0, false
	}

	num, err := txscript.MakeScriptNum(data, true, cltvScriptNumLen)
	if err != nil {
		return 0, false
	}

	return int64(num), true
}

// sameXOnlyKey returns true when both public keys serialize to the same x-only
// key.
func sameXOnlyKey(a, b *btcec.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}

	return bytes.Equal(
		schnorr.SerializePubKey(a), schnorr.SerializePubKey(b),
	)
}

// cloneWitnessItems deep-copies a witness-item list.
func cloneWitnessItems(items [][]byte) [][]byte {
	if len(items) == 0 {
		return nil
	}

	cloned := make([][]byte, 0, len(items))
	for _, item := range items {
		cloned = append(cloned, bytes.Clone(item))
	}

	return cloned
}
