package arkscript

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
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

	// collabNode is the collab leaf AST (Multisig{owner, operator}).
	collabNode Node

	// exitNode is the exit leaf AST (CSV{delay, Multisig{owner}}).
	exitNode Node
}

// CollabSpendInfo returns the spend information for the collaborative
// path. Tx-context is derived from the collab AST node.
func (v *VTXOPolicy) CollabSpendInfo() (*SpendInfo, error) {
	return v.CompiledPolicy.SpendInfoForNode(v.collabNode)
}

// ExitSpendInfo returns the spend information for the exit/timeout
// path. Tx-context is derived from the exit AST node.
func (v *VTXOPolicy) ExitSpendInfo() (*SpendInfo, error) {
	return v.CompiledPolicy.SpendInfoForNode(v.exitNode)
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
func NewVTXOPolicy(ownerKey, operatorKey *btcec.PublicKey, exitDelay uint32) (
	*VTXOPolicy, error) {

	template, err := StandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, err
	}

	policy, err := template.Compile()
	if err != nil {
		return nil, fmt.Errorf("vtxo: failed to build tree: %w", err)
	}

	return &VTXOPolicy{
		Template:       template,
		CompiledPolicy: policy,
		OwnerKey:       ownerKey,
		OperatorKey:    operatorKey,
		ExitDelay:      exitDelay,
		collabNode:     template.Leaves[0].Node,
		exitNode:       template.Leaves[1].Node,
	}, nil
}

// DeriveSequence returns the required nSequence value for spending the given
// AST node. This implements the tx-context derivation rules from the RFC:
//   - If the node contains CSV(lock, ...), return the lock value.
//   - If the node contains CLTV, return the "non-final" sentinel
//     (wire.MaxTxInSequenceNum - 1) so OP_CHECKLOCKTIMEVERIFY can evaluate.
//   - Otherwise return the fully-final wire.MaxTxInSequenceNum.
func DeriveSequence(node Node) uint32 {
	csvLock := extractCSVDelay(node)
	if csvLock > 0 {
		return csvLock
	}

	// If the node contains CLTV, nSequence must be non-final to allow
	// OP_CHECKLOCKTIMEVERIFY evaluation. Subtracting one from the
	// fully-final value gives the canonical "CLTV-enabled" sequence.
	if ExtractAbsoluteLockTime(node) > 0 {
		return wire.MaxTxInSequenceNum - 1
	}

	return wire.MaxTxInSequenceNum
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
