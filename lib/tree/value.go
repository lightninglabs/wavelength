package tree

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// ValidateValueConservation verifies the monetary and funding relationships
// for every reachable transaction in the tree. BatchOutput is the root
// transaction's spent output; callers at a trust boundary must first bind it
// to the authoritative commitment transaction output.
//
// Tree transactions use v3 ephemeral-anchor relay, so each node must preserve
// the exact value of the parent output it spends and pay no fee itself. Every
// output is included in the total, including outputs whose child was pruned
// from an extracted client path. Each reachable node's input must also name
// the parent output whose value funds it.
func (t *Tree) ValidateValueConservation() error {
	if t == nil {
		return fmt.Errorf("tree is nil")
	}
	if t.Root == nil {
		return fmt.Errorf("tree root is nil")
	}
	if t.BatchOutput == nil {
		return fmt.Errorf("batch output is nil")
	}

	if err := validateSatoshiValue(
		"batch output", t.BatchOutput.Value,
	); err != nil {
		return err
	}

	if err := validateNodeValueConservation(
		t.Root, t.BatchOutpoint, t.BatchOutput.Value,
		make(map[*Node]bool),
	); err != nil {
		return fmt.Errorf("root: %w", err)
	}

	return nil
}

// validateNodeValueConservation verifies a node's output values against the
// value of the parent output it spends, then recursively verifies each
// reachable child against its parent output. visitState is false for nodes on
// the active recursion stack and true for nodes already verified, allowing the
// traversal to reject cycles and nodes referenced by multiple parents.
func validateNodeValueConservation(node *Node, expectedInput wire.OutPoint,
	inputValue int64, visitState map[*Node]bool) error {

	if node == nil {
		return fmt.Errorf("node is nil")
	}
	if verified, ok := visitState[node]; ok {
		if !verified {
			return fmt.Errorf("tree contains a cycle")
		}

		return fmt.Errorf("node is reachable from multiple parents")
	}
	visitState[node] = false

	if node.Input != expectedInput {
		return fmt.Errorf("input %s does not match funding outpoint %s",
			node.Input, expectedInput)
	}

	if len(node.Outputs) == 0 {
		return fmt.Errorf("transaction has no outputs")
	}

	var totalOutputValue int64
	for i, output := range node.Outputs {
		if output == nil {
			return fmt.Errorf("output %d is nil", i)
		}
		if err := validateSatoshiValue(
			fmt.Sprintf("output %d", i), output.Value,
		); err != nil {
			return err
		}

		if totalOutputValue > int64(btcutil.MaxSatoshi)-output.Value {
			return fmt.Errorf("total output value exceeds "+
				"maximum %d", int64(btcutil.MaxSatoshi))
		}
		totalOutputValue += output.Value
	}

	if totalOutputValue > inputValue {
		return fmt.Errorf("output value %d exceeds input value %d",
			totalOutputValue, inputValue)
	}
	if totalOutputValue < inputValue {
		return fmt.Errorf("output value %d does not equal input "+
			"value %d", totalOutputValue, inputValue)
	}

	txid, err := node.TXID()
	if err != nil {
		return fmt.Errorf("derive transaction ID: %w", err)
	}

	for _, outputIndex := range sortedChildIndices(node.Children) {
		if uint64(outputIndex) >= uint64(len(node.Outputs)) {
			return fmt.Errorf("child references non-existent "+
				"output index %d", outputIndex)
		}

		child := node.Children[outputIndex]
		if child == nil {
			return fmt.Errorf("child at output index %d is nil",
				outputIndex)
		}

		expectedChildInput := wire.OutPoint{
			Hash:  txid,
			Index: outputIndex,
		}
		if err := validateNodeValueConservation(
			child, expectedChildInput,
			node.Outputs[outputIndex].Value, visitState,
		); err != nil {
			return fmt.Errorf("child at output index %d: %w",
				outputIndex, err)
		}
	}

	visitState[node] = true

	return nil
}

// validateSatoshiValue verifies that a transaction value is in Bitcoin's
// consensus monetary range.
func validateSatoshiValue(name string, value int64) error {
	if value < 0 {
		return fmt.Errorf("%s has negative value %d", name, value)
	}
	if value > int64(btcutil.MaxSatoshi) {
		return fmt.Errorf("%s value %d exceeds maximum %d", name, value,
			int64(btcutil.MaxSatoshi))
	}

	return nil
}
