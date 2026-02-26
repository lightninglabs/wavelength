package assets

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/taproot-assets/proof"
)

// -----------------------------------------------------------------------------
// PSBT and Transaction Clone Utilities
// -----------------------------------------------------------------------------

// clonePsbt produces a deep copy of the given PSBT packet by round-tripping
// through serialization. This is necessary when we need to work with a copy
// that won't affect the original.
func clonePsbt(packet *psbt.Packet) (*psbt.Packet, error) {
	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, err
	}

	return psbt.NewFromRawBytes(bytes.NewReader(buf.Bytes()), false)
}

// cloneTxOut creates a deep copy of a wire.TxOut.
func cloneTxOut(txOut *wire.TxOut) *wire.TxOut {
	if txOut == nil {
		return nil
	}

	return &wire.TxOut{
		Value:    txOut.Value,
		PkScript: append([]byte(nil), txOut.PkScript...),
	}
}

// cloneTxOuts creates a deep copy of a slice of wire.TxOut pointers.
func cloneTxOuts(txOuts []*wire.TxOut) []*wire.TxOut {
	if len(txOuts) == 0 {
		return nil
	}

	cloned := make([]*wire.TxOut, len(txOuts))
	for i, out := range txOuts {
		cloned[i] = cloneTxOut(out)
	}

	return cloned
}

// cloneTaprootLeafScripts creates a deep copy of a slice of taproot leaf
// scripts.
func cloneTaprootLeafScripts(
	scripts []*psbt.TaprootTapLeafScript) []*psbt.TaprootTapLeafScript {

	if len(scripts) == 0 {
		return nil
	}

	clones := make([]*psbt.TaprootTapLeafScript, len(scripts))
	for i, leaf := range scripts {
		if leaf == nil {
			continue
		}

		clones[i] = &psbt.TaprootTapLeafScript{
			LeafVersion:  leaf.LeafVersion,
			ControlBlock: append([]byte(nil), leaf.ControlBlock...),
			Script:       append([]byte(nil), leaf.Script...),
		}
	}

	return clones
}

// cloneTaprootBip32 creates a deep copy of taproot BIP32 derivation paths.
func cloneTaprootBip32(
	src []*psbt.TaprootBip32Derivation) []*psbt.TaprootBip32Derivation {

	if len(src) == 0 {
		return nil
	}

	clones := make([]*psbt.TaprootBip32Derivation, len(src))
	for i, derivation := range src {
		if derivation == nil {
			continue
		}

		clone := &psbt.TaprootBip32Derivation{
			XOnlyPubKey: append(
				[]byte(nil), derivation.XOnlyPubKey...,
			),
			MasterKeyFingerprint: derivation.MasterKeyFingerprint,
			Bip32Path: append(
				[]uint32(nil), derivation.Bip32Path...,
			),
		}

		if len(derivation.LeafHashes) > 0 {
			clone.LeafHashes = make(
				[][]byte, len(derivation.LeafHashes),
			)
			for idx, hash := range derivation.LeafHashes {
				clone.LeafHashes[idx] = append(
					[]byte(nil), hash...,
				)
			}
		}

		clones[i] = clone
	}

	return clones
}

// cloneBip32 creates a deep copy of BIP32 derivation paths.
func cloneBip32(src []*psbt.Bip32Derivation) []*psbt.Bip32Derivation {
	if len(src) == 0 {
		return nil
	}

	clones := make([]*psbt.Bip32Derivation, len(src))
	for i, derivation := range src {
		if derivation == nil {
			continue
		}

		clones[i] = &psbt.Bip32Derivation{
			PubKey: append(
				[]byte(nil), derivation.PubKey...,
			),
			MasterKeyFingerprint: derivation.MasterKeyFingerprint,
			Bip32Path: append(
				[]uint32(nil), derivation.Bip32Path...,
			),
		}
	}

	return clones
}

// cloneBtcInputPlan creates a deep copy of a BtcInputPlan.
func cloneBtcInputPlan(plan BtcInputPlan) BtcInputPlan {
	cloned := BtcInputPlan{
		Description: plan.Description,
		Outpoint:    plan.Outpoint,
		Sequence:    plan.Sequence,
	}

	if plan.WitnessUtxo != nil {
		cloned.WitnessUtxo = cloneTxOut(plan.WitnessUtxo)
	}

	return cloned
}

// cloneBtcOutputPlan creates a deep copy of a BtcOutputPlan.
func cloneBtcOutputPlan(plan BtcOutputPlan) BtcOutputPlan {
	return BtcOutputPlan{
		Description: plan.Description,
		ValueSat:    plan.ValueSat,
		PkScript:    append([]byte(nil), plan.PkScript...),
		OutputIndex: plan.OutputIndex,
	}
}

// copyWitness creates a deep copy of a TxWitness.
func copyWitness(witness wire.TxWitness) wire.TxWitness {
	cpy := make(wire.TxWitness, len(witness))
	for j := range witness {
		cpy[j] = append([]byte(nil), witness[j]...)
	}

	return cpy
}

// -----------------------------------------------------------------------------
// Witness and PSBT Manipulation Helpers
// -----------------------------------------------------------------------------

// applyWitness sets the final witness for a PSBT input at the given index.
func applyWitness(packet *psbt.Packet, idx int, witness wire.TxWitness) error {
	var buf bytes.Buffer
	if err := psbt.WriteTxWitness(&buf, witness); err != nil {
		return err
	}

	input := &packet.Inputs[idx]
	input.FinalScriptWitness = buf.Bytes()
	input.TaprootLeafScript = nil
	input.TaprootKeySpendSig = nil
	input.TaprootScriptSpendSig = nil

	packet.UnsignedTx.TxIn[idx].Witness = witness

	return nil
}

// -----------------------------------------------------------------------------
// Transaction Input Helpers
// -----------------------------------------------------------------------------

const (
	// probeSequence is the sequence value we attach to ephemeral "fee
	// probe" inputs when estimating the weight impact of yet-to-be-added
	// anchors.
	probeSequence = wire.MaxTxInSequenceNum

	// trucSequence is the sequence mandated by BIP 431 for TRUC
	// (nVersion=3) spends. Every input in the final package, wallet-funded
	// or not, must use this value.
	trucSequence = wire.MaxTxInSequenceNum - 2
)

// addInput attaches a generic input with the caller-supplied sequence value.
// Helpers like addProbeInput and addTRUCInput wrap this to capture the sequence
// semantics used in the rest of the builder.
func addInput(tx *wire.MsgTx, prev wire.OutPoint, sequence uint32) {
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: prev,
		Sequence:         sequence,
	})
}

// addProbeInput appends an input that mirrors how we temporarily account for
// anchor weight before we actually attach the inputs to the real PSBT.
func addProbeInput(tx *wire.MsgTx, prev wire.OutPoint) {
	addInput(tx, prev, probeSequence)
}

// addTRUCInput appends an input that conforms to the TRUC policy (sequence
// `0xfffffffd`) so the final transaction satisfies package relay rules.
func addTRUCInput(tx *wire.MsgTx, prev wire.OutPoint) {
	addInput(tx, prev, trucSequence)
}

// -----------------------------------------------------------------------------
// Validation Helpers
// -----------------------------------------------------------------------------

// validateAnchorKey ensures the provided anchor key specification is valid.
func validateAnchorKey(spec AnchorKeySpec) error {
	switch spec.Mode {
	case AnchorKeyModeMuSig2:
		if spec.MuSig2 == nil {
			return errors.New("musig2 specification missing")
		}

		if len(spec.MuSig2.Participants) == 0 {
			return errors.New("musig2 participants empty")
		}

		for i, p := range spec.MuSig2.Participants {
			if len(p.PubKey) != btcec.PubKeyBytesLenCompressed {
				return fmt.Errorf("musig2 participant %d "+
					"pubkey must be %d bytes", i,
					btcec.PubKeyBytesLenCompressed)
			}
		}

	case AnchorKeyModeStatic:
		if len(spec.Key) != schnorr.PubKeyBytesLen {
			return fmt.Errorf("static anchor key must be %d bytes",
				schnorr.PubKeyBytesLen)
		}

	case AnchorKeyModeTapdManaged:
		// TapdManaged keys can have either 32 or 33-byte keys
		// (x-only or compressed). The Key field holds the internal
		// key from the proof which is typically 33 bytes (compressed).
		if len(spec.Key) != schnorr.PubKeyBytesLen &&
			len(spec.Key) != btcec.PubKeyBytesLenCompressed {

			return fmt.Errorf("tapd-managed anchor key must be "+
				"%d or %d bytes", schnorr.PubKeyBytesLen,
				btcec.PubKeyBytesLenCompressed)
		}

	default:
		return fmt.Errorf("unknown anchor key mode %d", spec.Mode)
	}

	return nil
}

// -----------------------------------------------------------------------------
// Internal Types
// -----------------------------------------------------------------------------

// scriptSpendPlan holds pre-computed data for spending via a specific tapscript
// closure. This is populated during Compile() to avoid recomputing merkle
// proofs each time a script path spend is prepared.
type scriptSpendPlan struct {
	closure        ScriptClosure
	tapLeaf        txscript.TapLeaf
	controlBlock   []byte
	outputKey      *btcec.PublicKey
	assetRoot      [32]byte
	inclusionProof []byte
	scriptRoot     [32]byte
	taprootRoot    [32]byte
	internalKey    *btcec.PublicKey
}

// -----------------------------------------------------------------------------
// Taproot Merkle Tree Helpers
// -----------------------------------------------------------------------------

// tapBranchHashBytes computes the tap branch hash of two sibling nodes,
// sorting them lexicographically as required by BIP-341.
func tapBranchHashBytes(left, right []byte) [32]byte {
	if bytes.Compare(left, right) > 0 {
		left, right = right, left
	}

	hash := chainhash.TaggedHash(chainhash.TagTapBranch, left, right)

	return *hash
}

// buildScriptSpendPlans assembles the taproot inclusion proof for each closure.
// We do this once during Compile() so later calls can simply look up the cached
// control block, tapleaf and tweak material instead of reconstructing the
// merkle tree from scratch.
//
// The prf parameter is used to determine the output key parity from the actual
// on-chain output. This is necessary because the original output's tapscript
// tree structure may differ from the tree we're constructing here.
func buildScriptSpendPlans(
	internalKey *btcec.PublicKey, assetRoot [32]byte,
	closures []ScriptClosure, prf *proof.Proof,
) (map[string]*scriptSpendPlan, error) {

	if internalKey == nil {
		return nil, errors.New("internal key required for script " +
			"spend plan")
	}

	if len(closures) == 0 {
		return nil, nil
	}

	leaves := make([]txscript.TapLeaf, 0, len(closures))
	for _, closure := range closures {
		tapLeaf, err := closure.TapLeaf()
		if err != nil {
			return nil, fmt.Errorf("derive tap leaf for closure "+
				"%q: %w", closure.ID, err)
		}

		leaves = append(leaves, tapLeaf)
	}

	scriptTree := txscript.AssembleTaprootScriptTree(leaves...)
	scriptRootHash := scriptTree.RootNode.TapHash()
	var scriptRoot [32]byte
	copy(scriptRoot[:], scriptRootHash[:])

	// Note: The proof's TapSiblingPreimage in CommitmentProof is the asset
	// script sibling (e.g., OP_TRUE), not the anchor script sibling (e.g.,
	// CSV closure). We cannot validate anchor closures against this field
	// as they exist at different layers of the taproot structure.
	//
	// The anchor script tree structure is:
	//   OutputKey = tweak(InternalKey,
	//                     hash(assetCommitment || anchorScriptRoot))
	//
	// The asset script tree structure (CommitmentProof.TapSiblingPreimage):
	//   AssetScriptKey = tweak(NUMS, hash(OP_TRUE || assetTweak))
	//
	// These are independent and should not be compared.

	plans := make(map[string]*scriptSpendPlan, len(closures))
	for idx, closure := range closures {
		merkleProof := scriptTree.LeafMerkleProofs[idx]

		inclusionProof := append(
			[]byte(nil), merkleProof.InclusionProof...,
		)
		inclusionProof = append(inclusionProof, assetRoot[:]...)

		rootHash := tapBranchHashBytes(scriptRootHash[:], assetRoot[:])
		outputKey := txscript.ComputeTaprootOutputKey(
			internalKey, rootHash[:],
		)

		controlBlock := &txscript.ControlBlock{
			InternalKey:    internalKey,
			LeafVersion:    merkleProof.TapLeaf.LeafVersion,
			InclusionProof: inclusionProof,
		}
		if outputKey.SerializeCompressed()[0] ==
			secp256k1.PubKeyFormatCompressedOdd {

			controlBlock.OutputKeyYIsOdd = true
		}

		controlBlockBytes, err := controlBlock.ToBytes()
		if err != nil {
			return nil, fmt.Errorf("serialize control block for "+
				"closure %q: %w", closure.ID, err)
		}

		var rootCopy [32]byte
		copy(rootCopy[:], rootHash[:])
		var scriptRootCopy [32]byte
		copy(scriptRootCopy[:], scriptRoot[:])

		plans[closure.ID] = &scriptSpendPlan{
			closure:        closure,
			tapLeaf:        merkleProof.TapLeaf,
			controlBlock:   controlBlockBytes,
			outputKey:      outputKey,
			assetRoot:      assetRoot,
			inclusionProof: inclusionProof,
			scriptRoot:     scriptRootCopy,
			taprootRoot:    rootCopy,
			internalKey:    internalKey,
		}
	}

	return plans, nil
}
