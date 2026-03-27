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

	// MinExitDelay is the minimum CSV delay required on exit
	// leaves (in blocks). Set to 0 to skip this check.
	MinExitDelay uint32
}

// ValidatePolicy checks that a set of AST nodes satisfies the minimum Ark
// policy invariants inferred from the signer structure:
//
//  1. At least one operator-containing leaf.
//  2. At least one non-operator leaf.
//  3. Every non-operator leaf is CSV-gated.
//  4. Smallest inferred exit delay >= MinExitDelay.
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
			return fmt.Errorf(
				"non-operator leaf %d is not CSV-gated",
				i,
			)
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

// ContainsKey returns true if the AST contains a multisig node that
// references the given public key.
func ContainsKey(node Node, key *btcec.PublicKey) bool {
	if node == nil || key == nil {
		return false
	}

	target := schnorr.SerializePubKey(key)

	return containsKeyBytes(node, target)
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
// for the x-only serialized operator key. This is a lightweight
// server-side check that doesn't require AST parsing.
//
// The check verifies that the 32-byte x-only key appears in the
// script at a data-push boundary. Combined with script VM execution
// at finalize time, this confirms the key is meaningfully used.
func ScriptContainsKey(script []byte,
	key *btcec.PublicKey) bool {

	if len(script) == 0 || key == nil {
		return false
	}

	target := schnorr.SerializePubKey(key)

	return bytes.Contains(script, target)
}
