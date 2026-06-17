package arkscript

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// PolicyValidationOpts configures VTXO policy validation against
// Ark invariants.
type PolicyValidationOpts struct {
	// OperatorKey is the operator's public key. Every collab leaf
	// must contain this key for the operator to safely co-sign.
	OperatorKey *btcec.PublicKey

	// MinExitDelay is the minimum CSV delay required on exit leaves
	// (in blocks). When zero, the CSV-minimum check is skipped —
	// callers that admit standard VTXO policies should use
	// ValidateStandardVTXOPolicy instead, which requires MinExitDelay
	// to be set and refuses zero fail-closed.
	MinExitDelay uint32
}

// ValidatePolicy checks that a set of AST nodes satisfies the structural
// Ark policy invariants inferred from the signer structure:
//
//  1. At least one operator-containing leaf (collab).
//  2. At least one non-operator leaf (exit).
//  3. No leaf permits the operator to spend unilaterally (every Multisig
//     node that contains the operator key must contain at least one other
//     key, so the operator always needs a cooperating signer).
//  4. Every non-operator leaf is CSV-gated.
//  5. When opts.MinExitDelay > 0, the smallest inferred exit delay must be
//     at least opts.MinExitDelay. When opts.MinExitDelay == 0, this check
//     is skipped — use ValidateStandardVTXOPolicy to require a non-zero
//     minimum.
//
// ValidatePolicy is the correct admission check for custom policy shapes
// (e.g. vHTLC) whose unilateral delays are protocol-specific rather than
// tied to the operator's standard VTXO exit delay.
func ValidatePolicy(nodes []Node, opts PolicyValidationOpts) error {
	if opts.OperatorKey == nil {
		return fmt.Errorf("operator key must be provided")
	}

	var (
		hasCollab bool
		hasExit   bool

		exitNodes []Node

		smallestCSVLock uint32
		foundCSV        bool
	)

	// Invariant 3 is enforced across every leaf, not just collab leaves,
	// so a malformed exit leaf that nevertheless includes the operator
	// is still rejected.
	for i, node := range nodes {
		if err := rejectOperatorUnilateral(
			node, opts.OperatorKey,
		); err != nil {
			return fmt.Errorf("leaf %d permits operator "+
				"unilateral spend: %w", i, err)
		}
	}

	for _, node := range nodes {
		if ContainsKey(node, opts.OperatorKey) {
			hasCollab = true

			continue
		}

		hasExit = true
		exitNodes = append(exitNodes, node)
	}

	if !hasCollab {
		return ErrMissingCollab
	}

	if !hasExit {
		return ErrMissingExit
	}

	for i, node := range exitNodes {
		csvLock := ExtractCSVDelay(node)
		if csvLock == 0 {
			return fmt.Errorf("non-operator leaf %d is not "+
				"CSV-gated", i)
		}

		if !foundCSV || csvLock < smallestCSVLock {
			smallestCSVLock = csvLock
			foundCSV = true
		}
	}

	if opts.MinExitDelay > 0 && smallestCSVLock < opts.MinExitDelay {
		return fmt.Errorf("exit delay %d is below minimum %d",
			smallestCSVLock, opts.MinExitDelay)
	}

	return nil
}

// ValidateStandardVTXOPolicy is the admission check for a standard Ark
// VTXO recipient. It enforces every invariant of ValidatePolicy and
// additionally requires a non-zero MinExitDelay. A zero minimum is
// rejected fail-closed because it would otherwise silently accept a
// policy with a 1-block CSV, breaking the forfeit incentive.
func ValidateStandardVTXOPolicy(nodes []Node, operatorKey *btcec.PublicKey,
	minExitDelay uint32) error {

	if minExitDelay == 0 {
		return fmt.Errorf("MinExitDelay must be non-zero for " +
			"standard VTXO admission: a zero minimum would " +
			"accept a 1-block CSV and break forfeit incentives")
	}

	return ValidatePolicy(nodes, PolicyValidationOpts{
		OperatorKey:  operatorKey,
		MinExitDelay: minExitDelay,
	})
}

// rejectOperatorUnilateral walks the AST and returns an error if any Multisig
// node contains the operator key as its sole participant. Such a leaf would
// let the operator spend without any cooperating signer, bypassing the Ark
// protocol's custody assumptions.
func rejectOperatorUnilateral(node Node, operatorKey *btcec.PublicKey) error {
	target := schnorr.SerializePubKey(operatorKey)

	return walkRejectOperatorUnilateral(node, target)
}

// walkRejectOperatorUnilateral is the recursive helper for
// rejectOperatorUnilateral. It descends through CSV and Condition wrappers
// and inspects every Multisig node it finds.
func walkRejectOperatorUnilateral(node Node, target []byte) error {
	switch n := node.(type) {
	case *Multisig:
		if !multisigContainsKey(n, target) {
			return nil
		}

		// A length check alone is insufficient: Multisig{operator,
		// operator} has len(Keys) == 2 but is still unilaterally
		// spendable by the operator because every required signer is
		// the same key. Require at least one key in the set that is
		// not the operator (and not nil). Nil keys cannot participate
		// in a valid CHECKSIG and so do not count as cooperating
		// signers for this invariant.
		hasNonOperator := false
		for _, k := range n.Keys {
			if k == nil {
				continue
			}

			if !bytes.Equal(
				schnorr.SerializePubKey(k), target,
			) {

				hasNonOperator = true

				break
			}
		}

		if !hasNonOperator {
			return fmt.Errorf("multisig is operator-only")
		}

		// The multisig contains the operator AND at least one other
		// key, so unilateral spend is impossible here.
		return nil

	case *CSV:
		return walkRejectOperatorUnilateral(n.Inner, target)

	case *Condition:
		return walkRejectOperatorUnilateral(n.Inner, target)

	default:
		return nil
	}
}

// multisigContainsKey reports whether the multisig references the target
// x-only public key.
func multisigContainsKey(m *Multisig, target []byte) bool {
	for _, k := range m.Keys {
		if k == nil {
			continue
		}

		if bytes.Equal(schnorr.SerializePubKey(k), target) {
			return true
		}
	}

	return false
}

// ContainsKey walks the typed AST nodes (Multisig, CSV, Condition) and
// returns true if any Multisig node references the given public key.
// Opaque predicate bytes are intentionally not inspected.
func ContainsKey(node Node, key *btcec.PublicKey) bool {
	if node == nil || key == nil {
		return false
	}

	target := schnorr.SerializePubKey(key)

	return containsKeyBytes(node, target)
}

// SigningKeys returns the tapscript CHECKSIG public keys committed to by node
// in witness-stack order. Today Ark policy leaves are built from a single
// Multisig, optionally wrapped by CSV/Condition gates. Returning the concrete
// script order lets callers assemble or validate witnesses without parsing
// compiled script bytes.
func SigningKeys(node Node) ([]*btcec.PublicKey, error) {
	switch n := node.(type) {
	case nil:
		return nil, fmt.Errorf("node is nil")

	case *Multisig:
		keys := make([]*btcec.PublicKey, 0, len(n.Keys))
		for i, key := range n.Keys {
			if key == nil {
				return nil, fmt.Errorf("multisig key %d is nil",
					i)
			}

			keys = append(keys, key)
		}

		return keys, nil

	case *CSV:
		if n.Inner == nil {
			return nil, fmt.Errorf("csv inner node is nil")
		}

		return SigningKeys(n.Inner)

	case *Condition:
		if n.Inner == nil {
			return nil, fmt.Errorf("condition inner node is nil")
		}

		return SigningKeys(n.Inner)

	default:
		return nil, fmt.Errorf("unsupported signing node %T", node)
	}
}

// SigningKeysForSpendPath locates the semantic template leaf selected by
// spendPath and returns the leaf's required signing keys in witness-stack
// order.
func SigningKeysForSpendPath(template *PolicyTemplate,
	spendPath *SpendPath) ([]*btcec.PublicKey, error) {

	if template == nil {
		return nil, fmt.Errorf("policy template is required")
	}
	if err := spendPath.Validate(); err != nil {
		return nil, err
	}

	for i := range template.Leaves {
		leafScript, err := template.Leaves[i].Script()
		if err != nil {
			return nil, fmt.Errorf("compile template leaf %d: %w",
				i, err)
		}

		if !bytes.Equal(leafScript, spendPath.WitnessScript) {
			continue
		}

		return SigningKeys(template.Leaves[i].Node)
	}

	return nil, fmt.Errorf("spend path witness script not found in " +
		"policy template")
}

// containsKeyBytes recursively walks the AST looking for the target
// x-only public key.
func containsKeyBytes(node Node, target []byte) bool {
	switch n := node.(type) {
	case *Multisig:
		for _, k := range n.Keys {
			if k == nil {
				continue
			}

			if bytes.Equal(
				schnorr.SerializePubKey(k), target,
			) {
				return true
			}
		}

		return false

	case *CSV:
		return containsKeyBytes(n.Inner, target)

	case *Condition:
		return containsKeyBytes(n.Inner, target)

	default:
		return false
	}
}

// ExtractCSVDelay returns the CSV lock value if the AST contains a
// CSV node, or 0 if it does not. If multiple CSV nodes are nested,
// the outermost lock is returned.
func ExtractCSVDelay(node Node) uint32 {
	return extractCSVDelay(node)
}

// ScriptContainsKey performs a byte-level scan of a compiled script
// for the x-only serialized public key. This is a lightweight
// server-side heuristic that doesn't require AST parsing.
//
// The check verifies that the 32-byte x-only key appears somewhere
// in the raw script bytes. This can produce false positives if the
// key bytes happen to span an opcode boundary, but combined with
// full script VM execution at finalize time the result is safe:
// the heuristic gates admission, the VM proves correctness.
func ScriptContainsKey(script []byte, key *btcec.PublicKey) bool {
	if len(script) == 0 || key == nil {
		return false
	}

	target := schnorr.SerializePubKey(key)

	return bytes.Contains(script, target)
}
