// Package closure provides a flexible system for building VTXO (Virtual
// Transaction Output) tapscripts from composable closure primitives.
//
// A TapscriptsVtxoScript holds an arbitrary collection of Closures, each
// defining a spend condition as a tapscript leaf. The system supports:
//
//   - CSVSigClosure: Exit paths with relative timelocks (single key)
//   - CSVMultisigClosure: Exit paths with relative timelocks (multiple keys)
//   - MultisigClosure: Collaborative paths requiring multiple signatures
//   - CLTVMultisigClosure: Paths with absolute timelocks (CLTV)
//   - ConditionMultisigClosure: Paths with custom script conditions
//   - ConditionCSVMultisigClosure: Conditional paths with relative timelocks
//
// The key requirement is that every valid VTXO must have at least one exit
// closure (CSVSigClosure or CSVMultisigClosure) to ensure the owner can
// unilaterally recover funds after the timeout expires.
//
// Closures are categorized by type:
//   - ExitClosures(): Returns CSVSigClosure and CSVMultisigClosure instances
//   - ForfeitClosures(): Returns MultisigClosure, CLTVMultisigClosure,
//     and ConditionMultisigClosure instances (collaborative paths)
//
// NOTE: The exit vs collaborative distinction is semantic. Exit closures
// contain only owner key(s). Collaborative closures include the signer key.

package closure

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
)

var ErrNoExitLeaf = fmt.Errorf("no exit leaf")

// TaprootMerkleProof contains the proof data needed to spend a taproot leaf.
type TaprootMerkleProof struct {
	ControlBlock []byte
	Script       []byte
}

// NewDefaultVtxoScript returns the common VTXO script: A + S | A after T with:
// - A: the owner of the VTXO.
// - S: the pubkey of the signer who provided the liquidity for the VTXO.
// - T: exit delay that must be waited by alice to spend the VTXO once unrolled
//
// onchain.
func NewDefaultVtxoScript(
	owner, signer *btcec.PublicKey, exitDelay RelativeLocktime,
) *TapscriptsVtxoScript {
	return &TapscriptsVtxoScript{
		[]Closure{
			// Exit: owner can spend after CSV delay expires.
			&CSVSigClosure{
				PubKey:   owner,
				Locktime: exitDelay,
			},
			// Collab: owner + signer can spend immediately.
			&MultisigClosure{
				PubKeys: []*btcec.PublicKey{owner, signer},
			},
		},
	}
}

// ParseVtxoScript attempts to parse a list of hex-encoded scripts into a
// VtxoScript.
func ParseVtxoScript(scripts []string) (*TapscriptsVtxoScript, error) {
	if len(scripts) == 0 {
		return nil, fmt.Errorf("empty tapscripts array")
	}

	vtxoScript := &TapscriptsVtxoScript{}
	if err := vtxoScript.Decode(scripts); err != nil {
		return nil, fmt.Errorf("invalid vtxo scripts: %s", scripts)
	}

	return vtxoScript, nil
}

// TapscriptsVtxoScript holds a list of closures that make up a VTXO tapscript.
type TapscriptsVtxoScript struct {
	Closures []Closure
}

// Encode returns the hex-encoded scripts for all closures.
func (v *TapscriptsVtxoScript) Encode() ([]string, error) {
	encoded := make([]string, 0)
	for _, closure := range v.Closures {
		script, err := closure.Script()
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, hex.EncodeToString(script))
	}
	return encoded, nil
}

// Decode parses hex-encoded scripts into closures.
func (v *TapscriptsVtxoScript) Decode(scripts []string) error {
	if len(scripts) == 0 {
		return fmt.Errorf("empty scripts array")
	}

	v.Closures = make([]Closure, 0, len(scripts))
	for _, script := range scripts {
		scriptBytes, err := hex.DecodeString(script)
		if err != nil {
			return err
		}

		closure, err := DecodeClosure(scriptBytes)
		if err != nil {
			return err
		}
		v.Closures = append(v.Closures, closure)
	}

	if len(v.Closures) == 0 {
		return fmt.Errorf("no valid closures found in scripts")
	}

	return nil
}

// Validate checks that the VTXO script is valid for the given signer and
// minimum locktime.
func (v *TapscriptsVtxoScript) Validate(
	signer *btcec.PublicKey, minLocktime RelativeLocktime, blockTypeAllowed bool,
) error {
	xOnlySigner := schnorr.SerializePubKey(signer)
	for _, forfeit := range v.ForfeitClosures() {
		keys := make([]*btcec.PublicKey, 0)
		switch c := forfeit.(type) {
		case *MultisigClosure:
			keys = c.PubKeys
		case *CLTVMultisigClosure:
			if !blockTypeAllowed && !c.Locktime.IsSeconds() {
				return fmt.Errorf("invalid forfeit closure, CLTV block type not allowed")
			}
			keys = c.PubKeys
		case *ConditionMultisigClosure:
			keys = c.PubKeys
		}

		if len(keys) == 0 {
			return fmt.Errorf(
				"invalid forfeit closure, expected MultisigClosure, CLTVMultisigClosure or ConditionMultisigClosure",
			)
		}

		// must contain signer pubkey
		found := false
		for _, pubkey := range keys {
			if bytes.Equal(schnorr.SerializePubKey(pubkey), xOnlySigner) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("invalid forfeit closure, signer pubkey not found")
		}
	}

	for _, closure := range v.ExitClosures() {
		var locktimeType RelativeLocktimeType

		switch c := closure.(type) {
		case *CSVSigClosure:
			locktimeType = c.Locktime.Type
		case *CSVMultisigClosure:
			locktimeType = c.Locktime.Type
		}

		if !blockTypeAllowed && locktimeType == LocktimeTypeBlock {
			return fmt.Errorf("invalid exit closure, CSV block type not allowed")
		}
	}

	smallestExit, err := v.SmallestExitDelay()
	if err != nil {
		if err == ErrNoExitLeaf {
			return nil
		}
		return err
	}

	if smallestExit.LessThan(minLocktime) {
		return fmt.Errorf("exit delay is too short")
	}

	return nil
}

// SmallestExitDelay returns the smallest exit delay among all CSV closures.
// This checks both CSVSigClosure and CSVMultisigClosure types.
func (v *TapscriptsVtxoScript) SmallestExitDelay() (*RelativeLocktime, error) {
	var smallest *RelativeLocktime

	for _, closure := range v.Closures {
		var locktime *RelativeLocktime

		switch c := closure.(type) {
		case *CSVSigClosure:
			locktime = &c.Locktime
		case *CSVMultisigClosure:
			locktime = &c.Locktime
		default:
			continue
		}

		if smallest == nil || locktime.LessThan(*smallest) {
			smallest = locktime
		}
	}

	if smallest == nil {
		return nil, ErrNoExitLeaf
	}

	return smallest, nil
}

// ForfeitClosures returns the closures that can be used for forfeit (immediate
// spend with signer).
func (v *TapscriptsVtxoScript) ForfeitClosures() []Closure {
	forfeits := make([]Closure, 0)
	for _, closure := range v.Closures {
		switch closure.(type) {
		case *MultisigClosure, *CLTVMultisigClosure, *ConditionMultisigClosure:
			forfeits = append(forfeits, closure)
		}
	}
	return forfeits
}

// ExitClosures returns the closures that can be used for exit (delayed spend
// by owner only). This includes both CSVSigClosure (single-sig) and
// CSVMultisigClosure (multi-sig) types.
func (v *TapscriptsVtxoScript) ExitClosures() []Closure {
	exits := make([]Closure, 0)
	for _, closure := range v.Closures {
		switch closure.(type) {
		case *CSVSigClosure, *CSVMultisigClosure:
			exits = append(exits, closure)
		}
	}
	return exits
}

// TapTree returns the taproot output key and indexed script tree for this VTXO
// script.
func (v *TapscriptsVtxoScript) TapTree() (*btcec.PublicKey, *TaprootTree, error) {
	leaves := make([]txscript.TapLeaf, len(v.Closures))
	for i, closure := range v.Closures {
		script, err := closure.Script()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get script for closure %d: %w", i, err)
		}
		leaves[i] = txscript.NewBaseTapLeaf(script)
	}

	tapTree := txscript.AssembleTaprootScriptTree(leaves...)
	root := tapTree.RootNode.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(
		UnspendableKey(),
		root[:],
	)

	return taprootKey, &TaprootTree{tapTree}, nil
}

// TaprootTree is a wrapper around txscript.IndexedTapScriptTree to provide
// additional helper methods.
type TaprootTree struct {
	*txscript.IndexedTapScriptTree
}

// GetRoot returns the root hash of the taproot tree.
func (b *TaprootTree) GetRoot() chainhash.Hash {
	return b.RootNode.TapHash()
}

// GetTaprootMerkleProof returns the merkle proof for the given leaf hash.
func (b *TaprootTree) GetTaprootMerkleProof(
	leafhash chainhash.Hash,
) (*TaprootMerkleProof, error) {
	index, ok := b.LeafProofIndex[leafhash]
	if !ok {
		return nil, fmt.Errorf("leaf %s not found in tree", leafhash.String())
	}
	proof := b.LeafMerkleProofs[index]

	controlBlock := proof.ToControlBlock(UnspendableKey())
	controlBlockBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return &TaprootMerkleProof{
		ControlBlock: controlBlockBytes,
		Script:       proof.Script,
	}, nil
}

// GetLeaves returns all leaf hashes in the tree.
func (b *TaprootTree) GetLeaves() []chainhash.Hash {
	leafHashes := make([]chainhash.Hash, 0)
	for hash := range b.LeafProofIndex {
		leafHashes = append(leafHashes, hash)
	}
	return leafHashes
}

// GetSpendInfo returns the spend information for a closure at the given index.
func (v *TapscriptsVtxoScript) GetSpendInfo(
	closureIndex int,
) (*TaprootMerkleProof, error) {
	if closureIndex < 0 || closureIndex >= len(v.Closures) {
		return nil, fmt.Errorf("closure index %d out of bounds", closureIndex)
	}

	_, tree, err := v.TapTree()
	if err != nil {
		return nil, err
	}

	script, err := v.Closures[closureIndex].Script()
	if err != nil {
		return nil, err
	}

	leaf := txscript.NewBaseTapLeaf(script)
	leafHash := leaf.TapHash()

	return tree.GetTaprootMerkleProof(leafHash)
}
