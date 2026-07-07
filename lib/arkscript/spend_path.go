package arkscript

import (
	"bytes"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
)

const (
	// spendPathVersion is the current binary encoding version for spend
	// paths.
	spendPathVersion = uint8(1)
)

// SpendPath bundles everything needed to spend a custom VTXO leaf
// through OOR. It includes witness data (script + control block),
// tx-context (sequence/locktime), and any condition witnesses.
type SpendPath struct {
	// SpendInfo is the compiled leaf script + control block.
	*SpendInfo

	// RequiredSequence is the BIP-68 sequence value required for
	// this leaf (e.g., CSV delay). 0xffffffff means no constraint.
	RequiredSequence uint32

	// RequiredLockTime is the nLockTime value required for this
	// leaf (e.g., CLTV locktime). 0 means no constraint.
	RequiredLockTime uint32

	// Conditions holds extra witness elements needed by the spend
	// script beyond signatures (e.g., preimage for hashlock).
	Conditions [][]byte
}

// Validate checks that the spend path contains the required script-path data.
func (s *SpendPath) Validate() error {
	switch {
	case s == nil:
		return fmt.Errorf("spend path must be provided")

	case s.SpendInfo == nil:
		return fmt.Errorf("spend info must be provided")

	case len(s.WitnessScript) == 0:
		return fmt.Errorf("witness script must be provided")

	case len(s.ControlBlock) == 0:
		return fmt.Errorf("control block must be provided")
	}

	return nil
}

// VerifyBindsToPkScript checks that the spend path's witness script and
// control block commit to a taproot output whose script is exactly the
// supplied pkScript. This is the binding check that prevents a caller
// from supplying a malformed control block for an unrelated taproot
// output and obtaining signatures against a script they did not prove
// ownership of.
//
// The check runs:
//
//  1. Parse the control block.
//  2. Verify the internal key is the Ark NUMS point (no key-path spend).
//  3. Compute the tap root hash from the witness script using the
//     control block's inclusion proof.
//  4. Tweak the NUMS internal key by the root hash to get the taproot
//     output key.
//  5. Build a P2TR script from the output key and compare to pkScript.
func (s *SpendPath) VerifyBindsToPkScript(pkScript []byte) error {
	if err := s.Validate(); err != nil {
		return err
	}

	if len(pkScript) == 0 {
		return fmt.Errorf("pk script must be provided")
	}

	ctrlBlock, err := txscript.ParseControlBlock(s.ControlBlock)
	if err != nil {
		return fmt.Errorf("parse control block: %w", err)
	}

	// Every Ark taproot output commits to the NUMS internal key so no
	// key-path spend is possible. A control block whose internal key
	// deviates from NUMS is either malformed or a different output key
	// entirely, and the downstream binding check would fail anyway —
	// rejecting early yields a clearer error.
	internalX := schnorr.SerializePubKey(ctrlBlock.InternalKey)
	numsX := schnorr.SerializePubKey(&ARKNUMSKey)
	if !bytes.Equal(internalX, numsX) {
		return fmt.Errorf("control block internal key is not the Ark " +
			"NUMS point")
	}

	rootHash := ctrlBlock.RootHash(s.WitnessScript)

	outputKey := txscript.ComputeTaprootOutputKey(&ARKNUMSKey, rootHash)

	expectedPkScript, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return fmt.Errorf("derive p2tr from output key: %w", err)
	}

	if !bytes.Equal(expectedPkScript, pkScript) {
		return fmt.Errorf("control block does not commit to declared "+
			"pkScript: got %x want %x", expectedPkScript, pkScript)
	}

	return nil
}

// Encode serializes the spend path into a stable binary encoding.
func (s *SpendPath) Encode() ([]byte, error) {
	err := s.Validate()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := buf.WriteByte(spendPathVersion); err != nil {
		return nil, err
	}

	condCount := uint64(len(s.Conditions))
	if err := wire.WriteVarInt(&buf, 0, condCount); err != nil {
		return nil, err
	}

	for i := range s.Conditions {
		err := writeVarBytes(&buf, s.Conditions[i])
		if err != nil {
			return nil, fmt.Errorf("encode condition %d: %w", i,
				err)
		}
	}

	err = writeVarBytes(&buf, s.WitnessScript)
	if err != nil {
		return nil, fmt.Errorf("encode witness script: %w", err)
	}

	err = writeVarBytes(&buf, s.ControlBlock)
	if err != nil {
		return nil, fmt.Errorf("encode control block: %w", err)
	}

	reqSeq := uint64(s.RequiredSequence)
	if err := wire.WriteVarInt(&buf, 0, reqSeq); err != nil {
		return nil, err
	}

	reqLT := uint64(s.RequiredLockTime)
	if err := wire.WriteVarInt(&buf, 0, reqLT); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DecodeSpendPath deserializes a binary spend path encoding.
func DecodeSpendPath(raw []byte) (*SpendPath, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("spend path encoding is empty")
	}

	r := bytes.NewReader(raw)

	version, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if version != spendPathVersion {
		return nil, fmt.Errorf("unknown spend path version %d", version)
	}

	conditionCount, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	// Sanity check: Ark spend paths have at most a handful of
	// condition witness items.
	const maxConditions = 64
	if conditionCount > maxConditions {
		return nil, fmt.Errorf("condition count %d exceeds maximum %d",
			conditionCount, maxConditions)
	}

	conditions := make([][]byte, 0, conditionCount)
	for i := uint64(0); i < conditionCount; i++ {
		cond, err := readVarBytes(r, "spend path condition")
		if err != nil {
			return nil, err
		}

		conditions = append(conditions, cond)
	}

	witnessScript, err := readVarBytes(r, "spend path witness script")
	if err != nil {
		return nil, err
	}

	controlBlock, err := readVarBytes(r, "spend path control block")
	if err != nil {
		return nil, err
	}

	requiredSequence, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	if requiredSequence > math.MaxUint32 {
		return nil, fmt.Errorf("required sequence %d exceeds "+
			"uint32 max", requiredSequence)
	}

	requiredLockTime, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	if requiredLockTime > math.MaxUint32 {
		return nil, fmt.Errorf("required locktime %d exceeds "+
			"uint32 max", requiredLockTime)
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("unexpected %d trailing bytes in "+
			"spend path", r.Len())
	}

	path := &SpendPath{
		SpendInfo: &SpendInfo{
			WitnessScript: witnessScript,
			ControlBlock:  controlBlock,
		},
		RequiredSequence: uint32(requiredSequence),
		RequiredLockTime: uint32(requiredLockTime),
		Conditions:       conditions,
	}

	if err := path.Validate(); err != nil {
		return nil, err
	}

	return path, nil
}

// Witness assembles a full script-path witness using the provided signatures,
// then appends any condition items, the witness script, and the control block.
func (s *SpendPath) Witness(sigItems ...[]byte) (wire.TxWitness, error) {
	err := s.Validate()
	if err != nil {
		return nil, err
	}

	witness := make(
		wire.TxWitness, 0,
		len(sigItems)+len(s.Conditions)+2,
	)
	witness = append(witness, sigItems...)
	witness = append(witness, s.Conditions...)
	witness = append(witness, s.WitnessScript, s.ControlBlock)

	return witness, nil
}

// SingleSigWitness assembles a script-path witness for a single-signature leaf.
func (s *SpendPath) SingleSigWitness(sig input.Signature,
	sigHash txscript.SigHashType) (wire.TxWitness, error) {

	if sig == nil {
		return nil, fmt.Errorf("signature must be provided")
	}

	return s.Witness(MaybeAppendSighash(sig, sigHash))
}

// AttachTapLeafScript adds the spend path's script and control block to a PSBT
// input so script-path signatures can be validated against the correct leaf.
func (s *SpendPath) AttachTapLeafScript(in *psbt.PInput) error {
	err := s.Validate()
	if err != nil {
		return err
	}

	if in == nil {
		return fmt.Errorf("psbt input must be provided")
	}

	in.TaprootLeafScript = append(in.TaprootLeafScript,
		&psbt.TaprootTapLeafScript{
			Script:       s.WitnessScript,
			ControlBlock: s.ControlBlock,
			LeafVersion:  txscript.BaseLeafVersion,
		},
	)

	return nil
}
