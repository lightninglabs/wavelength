package arkscript

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
)

// StandardVTXOParams captures the semantic parameters of the standard Ark
// VTXO/boarding policy shape.
type StandardVTXOParams struct {
	// OwnerKey is the participant key on the collab and exit paths.
	OwnerKey *btcec.PublicKey

	// OperatorKey is the operator key on the collab path.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the CSV delay on the unilateral exit path.
	ExitDelay uint32
}

// PkScript compiles the semantic policy into its canonical P2TR output
// script.
func (p *PolicyTemplate) PkScript() ([]byte, error) {
	compiled, err := p.Compile()
	if err != nil {
		return nil, err
	}

	return txscript.PayToTaprootScript(compiled.OutputKey())
}

// StandardVTXOTemplate builds the semantic policy template for the standard
// Ark VTXO/boarding output shape.
func StandardVTXOTemplate(ownerKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) (*PolicyTemplate, error) {

	if ownerKey == nil {
		return nil, fmt.Errorf("vtxo: owner key is nil")
	}

	if operatorKey == nil {
		return nil, fmt.Errorf("vtxo: operator key is nil")
	}

	if exitDelay == 0 {
		return nil, fmt.Errorf("vtxo: exit delay must be non-zero")
	}

	return &PolicyTemplate{
		Leaves: []LeafTemplate{{
			Node: &Multisig{
				Keys: []*btcec.PublicKey{ownerKey, operatorKey},
			},
		}, {
			Node: &CSV{
				Lock: exitDelay,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{ownerKey},
				},
			},
		}},
	}, nil
}

// EncodeStandardVTXOTemplate serializes the standard Ark VTXO policy.
func EncodeStandardVTXOTemplate(ownerKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) ([]byte, error) {

	template, err := StandardVTXOTemplate(ownerKey, operatorKey, exitDelay)
	if err != nil {
		return nil, err
	}

	return template.Encode()
}

// DecodeStandardVTXOParams validates that the semantic policy is a standard
// Ark VTXO policy and extracts its owner/operator/exit-delay tuple.
func DecodeStandardVTXOParams(
	template *PolicyTemplate) (*StandardVTXOParams, error) {

	if template == nil {
		return nil, fmt.Errorf("policy template must be provided")
	}

	if len(template.Leaves) != 2 {
		return nil, fmt.Errorf(
			"standard vtxo policy must contain 2 leaves",
		)
	}

	var (
		collab *Multisig
		exit   *CSV
	)

	for i := range template.Leaves {
		switch node := template.Leaves[i].Node.(type) {
		case *Multisig:
			if collab != nil {
				return nil, fmt.Errorf(
					"multiple collab leaves found",
				)
			}

			collab = node

		case *CSV:
			if exit != nil {
				return nil, fmt.Errorf(
					"multiple exit leaves found",
				)
			}

			exit = node

		default:
			return nil, fmt.Errorf(
				"leaf %d is not standard vtxo", i,
			)
		}
	}

	if collab == nil || exit == nil {
		return nil, fmt.Errorf(
			"standard vtxo policy missing collab or exit",
		)
	}

	if len(collab.Keys) != 2 {
		return nil, fmt.Errorf("collab leaf must contain 2 keys")
	}

	if exit.Inner == nil {
		return nil, fmt.Errorf("exit leaf must contain inner multisig")
	}

	exitMultisig, ok := exit.Inner.(*Multisig)
	if !ok {
		return nil, fmt.Errorf("exit leaf inner node must be multisig")
	}

	if len(exitMultisig.Keys) != 1 {
		return nil, fmt.Errorf("exit leaf must contain 1 owner key")
	}

	ownerKey := exitMultisig.Keys[0]
	if ownerKey == nil {
		return nil, fmt.Errorf("exit leaf owner key is nil")
	}

	var operatorKey *btcec.PublicKey
	for i := range collab.Keys {
		key := collab.Keys[i]
		if key == nil {
			return nil, fmt.Errorf("collab key %d is nil", i)
		}

		if sameXOnlyKey(key, ownerKey) {
			continue
		}

		if operatorKey != nil {
			return nil, fmt.Errorf("collab leaf contains extra key")
		}

		operatorKey = key
	}

	if operatorKey == nil {
		return nil, fmt.Errorf("collab leaf missing operator key")
	}

	if !containsKeyBytes(collab, schnorr.SerializePubKey(ownerKey)) {
		return nil, fmt.Errorf("collab leaf missing owner key")
	}

	return &StandardVTXOParams{
		OwnerKey:    ownerKey,
		OperatorKey: operatorKey,
		ExitDelay:   exit.Lock,
	}, nil
}

// IsStandardVTXOTemplate returns true when the policy matches the standard
// Ark VTXO shape.
func IsStandardVTXOTemplate(template *PolicyTemplate) bool {
	_, err := DecodeStandardVTXOParams(template)
	return err == nil
}

// MatchesPkScript compiles the policy and checks whether it matches the given
// output script.
func (p *PolicyTemplate) MatchesPkScript(pkScript []byte) bool {
	compiledPkScript, err := p.PkScript()
	if err != nil {
		return false
	}

	return bytes.Equal(compiledPkScript, pkScript)
}
