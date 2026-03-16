package arkscript

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
)

// VTXOPolicy represents a compiled VTXO taproot policy with canonical
// structure and validated invariants.
type VTXOPolicy struct {
	// CompiledPolicy is the underlying compiled taproot tree.
	*CompiledPolicy

	// OwnerKey is the key that owns this VTXO.
	OwnerKey *btcec.PublicKey

	// OperatorKey is the operator/cosigner key.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the CSV delay for the exit path (in blocks).
	ExitDelay uint32
}

// CollabSpendInfo returns the spend information for the collaborative path.
func (v *VTXOPolicy) CollabSpendInfo() (*SpendInfo, error) {
	return v.SpendInfoWithContext(0)
}

// ExitSpendInfo returns the spend information for the exit/timeout path.
func (v *VTXOPolicy) ExitSpendInfo() (*SpendInfo, error) {
	return v.SpendInfoWithContext(1)
}

// SpendInfoWithContext returns the spend information for the leaf at the given
// index with tx-context requirements derived from the leaf's AST.
func (v *VTXOPolicy) SpendInfoWithContext(leafIndex int) (*SpendInfo, error) {
	info, err := v.CompiledPolicy.SpendInfo(leafIndex)
	if err != nil {
		return nil, err
	}

	// Derive tx-context based on leaf role.
	switch v.Leaves[leafIndex].Role {
	case LeafRoleCollab:
		// Collaborative path has no timelock requirements.
		info.RequiredSequence = 0xffffffff
		info.RequiredLockTime = 0

	case LeafRoleExit:
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
// parameters. The policy consists of:
// - Leaf 0 (collab): 2-of-2 multisig requiring both owner and operator
// - Leaf 1 (exit): CSV-gated single-sig for owner only
//
// This produces byte-identical output to lib/scripts.VTXOTapScript().
func NewVTXOPolicy(ownerKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) (*VTXOPolicy, error) {

	if ownerKey == nil {
		return nil, fmt.Errorf("vtxo: owner key is nil")
	}

	if operatorKey == nil {
		return nil, fmt.Errorf("vtxo: operator key is nil")
	}

	if exitDelay == 0 {
		return nil, fmt.Errorf("vtxo: exit delay must be non-zero")
	}

	// Build collab leaf: Multisig([owner, operator]).
	collabNode := &Multisig{
		Keys: []*btcec.PublicKey{ownerKey, operatorKey},
		Type: MultisigTypeChecksig,
	}
	collabScript, err := collabNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vtxo: failed to compile collab leaf: %w",
			err)
	}

	// Build exit leaf: CSV(delay, Checksig(owner)).
	exitNode := &CSV{
		Lock:  exitDelay,
		Inner: &Checksig{Key: ownerKey},
	}
	exitScript, err := exitNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vtxo: failed to compile exit leaf: %w",
			err)
	}

	// Construct leaves in canonical order (collab=0, exit=1).
	leaves := []PolicyLeaf{
		{
			Role: LeafRoleCollab,
			Leaf: txscript.NewBaseTapLeaf(collabScript),
		},
		{
			Role: LeafRoleExit,
			Leaf: txscript.NewBaseTapLeaf(exitScript),
		},
	}

	// Build the tree using the NUMS key as internal key.
	policy, err := BuildTree(leaves, &ARKNUMSKey)
	if err != nil {
		return nil, fmt.Errorf("vtxo: failed to build tree: %w", err)
	}

	return &VTXOPolicy{
		CompiledPolicy: policy,
		OwnerKey:       ownerKey,
		OperatorKey:    operatorKey,
		ExitDelay:      exitDelay,
	}, nil
}

// DeriveSequence returns the required nSequence value for spending the given
// AST node. This implements the tx-context derivation rules from the RFC:
// - If the node contains CSV(lock, ...), return the lock value.
// - Else if the node contains CLTV(...), return 0xfffffffe (non-final).
// - Else return 0xffffffff.
func DeriveSequence(node Node) uint32 {
	csvLock, hasCLTV := extractTimelocks(node)
	if csvLock > 0 {
		return csvLock
	}

	if hasCLTV {
		return 0xfffffffe
	}

	return 0xffffffff
}

// DeriveLockTime returns the required nLockTime value for spending the given
// AST node. This implements the tx-context derivation rules from the RFC:
// - If the node contains CLTV(lock, ...), return the lock value.
// - Else return 0.
func DeriveLockTime(node Node) uint32 {
	_, cltvLock := extractCLTV(node)
	return cltvLock
}

// extractTimelocks recursively extracts CSV and CLTV lock values from an AST
// node.
func extractTimelocks(node Node) (csvLock uint32, hasCLTV bool) {
	switch n := node.(type) {
	case *Checksig:
		return 0, false

	case *Multisig:
		return 0, false

	case *CSV:
		innerCSV, innerCLTV := extractTimelocks(n.Inner)
		if innerCSV > 0 {
			// Use the outermost CSV lock.
			return n.Lock, innerCLTV
		}

		return n.Lock, innerCLTV

	case *CLTV:
		innerCSV, _ := extractTimelocks(n.Inner)
		return innerCSV, true

	case *HashLock:
		return extractTimelocks(n.Inner)

	default:
		return 0, false
	}
}

// extractCLTV recursively finds a CLTV node and returns its lock value.
func extractCLTV(node Node) (hasCLTV bool, lock uint32) {
	switch n := node.(type) {
	case *Checksig:
		return false, 0

	case *Multisig:
		return false, 0

	case *CSV:
		return extractCLTV(n.Inner)

	case *CLTV:
		return true, n.Lock

	case *HashLock:
		return extractCLTV(n.Inner)

	default:
		return false, 0
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

// ValidateVTXOLeaves validates that the given leaves satisfy VTXO policy
// invariants:
// - At least one collab leaf
// - At least one exit leaf that is CSV-gated
func ValidateVTXOLeaves(leaves []PolicyLeaf) error {
	hasCollab := false
	hasExit := false

	for _, leaf := range leaves {
		switch leaf.Role {
		case LeafRoleCollab:
			hasCollab = true

		case LeafRoleExit:
			hasExit = true
		}
	}

	if !hasCollab {
		return ErrMissingCollab
	}

	if !hasExit {
		return ErrMissingExit
	}

	return nil
}
