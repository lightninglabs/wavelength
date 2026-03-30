package arkscript

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
)

// VTXOPolicy represents a compiled VTXO taproot policy with canonical
// structure and validated invariants.
type VTXOPolicy struct {
	// Template is the semantic policy template for this VTXO.
	Template *PolicyTemplate

	// CompiledPolicy is the underlying compiled taproot tree.
	*CompiledPolicy

	// OwnerKey is the key that owns this VTXO.
	OwnerKey *btcec.PublicKey

	// OperatorKey is the operator/cosigner key.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the CSV delay for the exit path (in blocks).
	ExitDelay uint32

	// collabLeafIndex is the canonical leaf index of the cooperative path.
	collabLeafIndex int

	// exitLeafIndex is the canonical leaf index of the unilateral
	// exit path.
	exitLeafIndex int
}

// CollabSpendInfo returns the spend information for the collaborative path.
func (v *VTXOPolicy) CollabSpendInfo() (*SpendInfo, error) {
	return v.SpendInfoWithContext(v.collabLeafIndex)
}

// ExitSpendInfo returns the spend information for the exit/timeout path.
func (v *VTXOPolicy) ExitSpendInfo() (*SpendInfo, error) {
	return v.SpendInfoWithContext(v.exitLeafIndex)
}

// SpendInfoWithContext returns the spend information for the leaf at the given
// index with tx-context requirements derived from the leaf's AST.
func (v *VTXOPolicy) SpendInfoWithContext(leafIndex int) (*SpendInfo, error) {
	info, err := v.CompiledPolicy.SpendInfo(leafIndex)
	if err != nil {
		return nil, err
	}

	// Derive tx-context based on the canonical VTXO path kind.
	switch leafIndex {
	case v.collabLeafIndex:
		// Collaborative path has no timelock requirements.
		info.RequiredSequence = 0xffffffff
		info.RequiredLockTime = 0

	case v.exitLeafIndex:
		// Exit path requires CSV delay.
		info.RequiredSequence = v.ExitDelay
		info.RequiredLockTime = 0

	default:
		// Custom leaves default to no requirements.
		info.RequiredSequence = 0xffffffff
		info.RequiredLockTime = 0
	}

	return info, nil
}

// NewVTXOPolicy creates and validates a standard VTXO policy from the given
// parameters. The semantic template consists of:
// - Collab: 2-of-2 multisig requiring both owner and operator
// - Exit: CSV-gated 1-of-1 multisig for owner only
//
// After canonical sorting, the actual leaf indices may differ from the
// template order. Use CollabSpendInfo/ExitSpendInfo to access the
// correct paths regardless of canonical position.
//
// This produces byte-identical output to lib/scripts.VTXOTapScript().
func NewVTXOPolicy(ownerKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) (*VTXOPolicy, error) {

	template, err := StandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, err
	}

	collabNode, ok := template.Leaves[0].Node.(*Multisig)
	if !ok {
		return nil, fmt.Errorf("vtxo: collab leaf is not multisig")
	}

	exitNode, ok := template.Leaves[1].Node.(*CSV)
	if !ok {
		return nil, fmt.Errorf("vtxo: exit leaf is not csv")
	}

	// Build the tree using the NUMS key as internal key.
	policy, err := template.Compile()
	if err != nil {
		return nil, fmt.Errorf("vtxo: failed to build tree: %w", err)
	}

	collabScript, err := collabNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vtxo: compile collab leaf: %w", err)
	}

	exitScript, err := exitNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vtxo: compile exit leaf: %w", err)
	}

	scriptToIndex := make(map[string]int, len(policy.Leaves))
	for i, leaf := range policy.Leaves {
		scriptToIndex[string(leaf.Leaf.Script)] = i
	}

	collabIdx := scriptToIndex[string(collabScript)]
	exitIdx := scriptToIndex[string(exitScript)]

	// Set roles explicitly after canonical sorting so the zero-value
	// LeafRoleCollab default doesn't misclassify the exit leaf.
	policy.Leaves[collabIdx].Role = LeafRoleCollab
	policy.Leaves[exitIdx].Role = LeafRoleExit

	return &VTXOPolicy{
		Template:        template,
		CompiledPolicy:  policy,
		OwnerKey:        ownerKey,
		OperatorKey:     operatorKey,
		ExitDelay:       exitDelay,
		collabLeafIndex: collabIdx,
		exitLeafIndex:   exitIdx,
	}, nil
}

// DeriveSequence returns the required nSequence value for spending the given
// AST node. This implements the tx-context derivation rules from the RFC:
// - If the node contains CSV(lock, ...), return the lock value.
// - Else return 0xffffffff.
func DeriveSequence(node Node) uint32 {
	csvLock := extractCSVDelay(node)
	if csvLock > 0 {
		return csvLock
	}

	// If the node contains CLTV, nSequence must be non-final to allow
	// OP_CHECKLOCKTIMEVERIFY evaluation.
	if ExtractAbsoluteLockTime(node) > 0 {
		return 0xfffffffe
	}

	return 0xffffffff
}

// DeriveLockTime returns the required nLockTime value for spending the given
// AST node. This implements the tx-context derivation rules:
//   - If the node contains a CLTV condition, return the locktime value.
//   - Otherwise return 0.
func DeriveLockTime(node Node) uint32 {
	return ExtractAbsoluteLockTime(node)
}

// extractCSVDelay recursively extracts the outermost CSV lock value from an
// AST node.
func extractCSVDelay(node Node) uint32 {
	switch n := node.(type) {
	case *Multisig:
		return 0

	case *CSV:
		innerCSV := extractCSVDelay(n.Inner)
		if innerCSV > 0 {
			// Use the outermost CSV lock.
			return n.Lock
		}

		return n.Lock

	case *Condition:
		return extractCSVDelay(n.Inner)

	default:
		return 0
	}
}

// VTXOValidationError represents a VTXO policy validation failure.
type VTXOValidationError struct {
	Code    string
	Message string
}

// Error implements the error interface.
func (e *VTXOValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Standard validation error codes.
var (
	// ErrMissingCollab indicates no collab leaf was found.
	ErrMissingCollab = &VTXOValidationError{
		Code:    "MISSING_COLLAB",
		Message: "VTXO policy must contain a collab leaf",
	}

	// ErrMissingExit indicates no exit leaf was found.
	ErrMissingExit = &VTXOValidationError{
		Code:    "MISSING_EXIT",
		Message: "VTXO policy must contain an exit leaf",
	}

	// ErrExitNotCSVGated indicates the exit leaf is not CSV-gated.
	ErrExitNotCSVGated = &VTXOValidationError{
		Code:    "EXIT_NOT_CSV_GATED",
		Message: "exit leaf must be CSV-gated",
	}
)
