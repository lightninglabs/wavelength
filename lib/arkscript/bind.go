package arkscript

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// BindOperatorKey returns a copy of the policy template with every
// operator-key placeholder (OperatorKeyPlaceholder) replaced by operatorKey.
// The server calls this at round admission to stamp its current operator key
// into a client-submitted template that deliberately left the operator key
// unbound, so the client never has to commit to (or even know) the operator
// key when building its signed intent.
//
// It errors if the template contains no placeholder: every Ark VTXO output
// must commit to the operator on at least one leaf, so a template with no
// placeholder is either malformed or an attempt to dodge operator binding.
func (p *PolicyTemplate) BindOperatorKey(operatorKey *btcec.PublicKey) (
	*PolicyTemplate, error) {

	if operatorKey == nil {
		return nil, fmt.Errorf("bind operator key: operator key is nil")
	}

	bound := make([]LeafTemplate, len(p.Leaves))
	total := 0
	for i, leaf := range p.Leaves {
		node, n := bindNodeOperatorKey(leaf.Node, operatorKey)
		bound[i] = LeafTemplate{Node: node}
		total += n
	}

	if total == 0 {
		return nil, fmt.Errorf("bind operator key: template has no " +
			"operator key placeholder")
	}

	return &PolicyTemplate{Leaves: bound}, nil
}

// bindNodeOperatorKey recursively rewrites an AST node, replacing every
// operator-key placeholder with operatorKey. It returns the rewritten node
// and the number of substitutions made so the caller can reject templates
// that never reference the operator. The operator key only ever appears in
// Multisig key lists (including those nested under CSV/Condition gates);
// opaque Condition predicates are left untouched.
func bindNodeOperatorKey(node Node, operatorKey *btcec.PublicKey) (Node, int) {
	switch n := node.(type) {
	case *Multisig:
		keys := make([]*btcec.PublicKey, len(n.Keys))
		count := 0
		for i, k := range n.Keys {
			if k != nil && isOperatorPlaceholder(k) {
				keys[i] = operatorKey
				count++

				continue
			}
			keys[i] = k
		}

		return &Multisig{Keys: keys}, count

	case *CSV:
		inner, count := bindNodeOperatorKey(n.Inner, operatorKey)

		return &CSV{Lock: n.Lock, Inner: inner}, count

	case *Condition:
		inner, count := bindNodeOperatorKey(n.Inner, operatorKey)

		return &Condition{
			Predicate: n.Predicate,
			Inner:     inner,
		}, count

	default:
		return node, 0
	}
}

// isOperatorPlaceholder reports whether key is the operator-key placeholder
// sentinel. The comparison is x-only because tapscript keys are serialized
// x-only, so a parity difference must not cause a missed substitution.
func isOperatorPlaceholder(key *btcec.PublicKey) bool {
	return bytes.Equal(
		schnorr.SerializePubKey(key),
		schnorr.SerializePubKey(&OperatorKeyPlaceholder),
	)
}
