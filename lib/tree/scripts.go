package tree

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/txscript/v2"
)

// ValidateChildScripts verifies that every retained child can spend the
// parent output that funds it. Bitcoin trees derive each child output from the
// common sweep tapscript root. Asset trees use the signing tweak recorded for
// the child's input instead.
func (t *Tree) ValidateChildScripts() error {
	if t == nil {
		return fmt.Errorf("tree is nil")
	}
	if t.Root == nil {
		return fmt.Errorf("tree root is nil")
	}

	if t.AssetContext != nil {
		if err := t.AssetContext.Validate(t.Root); err != nil {
			return fmt.Errorf("asset tree context: %w", err)
		}
	}

	return validateNodeChildScripts(t.Root, t.childSigningTweak)
}

// childSigningTweak returns the signing tweak for the given child node.
func (t *Tree) childSigningTweak(child *Node) []byte {
	if t.AssetContext != nil {
		return t.AssetContext.SigningTweak(child.Input)
	}

	return t.SweepTapscriptRoot
}

// validateNodeChildScripts verifies one node's retained child outputs before
// descending into each child subtree.
func validateNodeChildScripts(node *Node, tweak func(*Node) []byte) error {
	for _, outputIdx := range sortedChildIndices(node.Children) {
		if uint64(outputIdx) >= uint64(len(node.Outputs)) {
			return fmt.Errorf("child references non-existent "+
				"output index %d", outputIdx)
		}

		child := node.Children[outputIdx]
		if child == nil {
			return fmt.Errorf("child at output index %d is nil",
				outputIdx)
		}

		finalKey, err := ComputeFinalKey(
			child.CoSigners, tweak(child),
		)
		if err != nil {
			return fmt.Errorf("derive child key at output index "+
				"%d: %w", outputIdx, err)
		}
		expectedScript, err := txscript.PayToTaprootScript(finalKey)
		if err != nil {
			return fmt.Errorf("derive child script at output "+
				"index %d: %w", outputIdx, err)
		}

		parentOutput := node.Outputs[outputIdx]
		if parentOutput == nil {
			return fmt.Errorf("output %d is nil", outputIdx)
		}
		if !bytes.Equal(parentOutput.PkScript, expectedScript) {
			return fmt.Errorf("output %d script does not match "+
				"child cosigners and signing tweak", outputIdx)
		}

		if err := validateNodeChildScripts(child, tweak); err != nil {
			return fmt.Errorf("child at output index %d: %w",
				outputIdx, err)
		}
	}

	return nil
}
