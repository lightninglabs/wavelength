package arktx

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
)

const (
	// TxVersion is the canonical transaction version used for v0 Ark
	// transfers. We use v3 to support package relay.
	TxVersion = 3
)

// IsAnchorOutput returns true if the output is the v0 Ark anchor output (P2A,
// value 0).
func IsAnchorOutput(out *wire.TxOut) bool {
	if out == nil {
		return false
	}

	if out.Value != 0 {
		return false
	}

	return bytes.Equal(out.PkScript, arkscript.AnchorPkScript)
}

// IsP2AAnchorScript returns true if pkScript is the keyless P2A anchor
// script, regardless of the spending output's value. Unlike IsAnchorOutput,
// this does not require the zero-value ephemeral-dust form: it also matches a
// "funded" anchor whose value has been lifted above the P2A dust threshold
// (see arkscript.WithAnchorValue) so that its parent transaction can pay a
// non-zero miner fee and confirm on its own. Use this when locating a CPFP
// anchor to spend on a parent that is already independently valid, where the
// anchor is a spare fee-bump handle rather than the parent's only fee source.
func IsP2AAnchorScript(pkScript []byte) bool {
	return bytes.Equal(pkScript, arkscript.AnchorPkScript)
}

// IsFundedAnchorOutput returns true if out is a P2A anchor output carrying a
// non-zero value (the "funded" anchor form). A funded anchor lets its parent
// pay its own fee and confirm standalone, with the anchor reserved as an
// optional CPFP handle for fee-bumping only when the parent is stuck. This is
// the complement of IsAnchorOutput, which matches only the zero-value
// ephemeral form used by TRUC parents that pay zero fee themselves.
func IsFundedAnchorOutput(out *wire.TxOut) bool {
	if out == nil {
		return false
	}

	if out.Value == 0 {
		return false
	}

	return bytes.Equal(out.PkScript, arkscript.AnchorPkScript)
}

// ValidateCanonicalTx validates canonical ordering rules for an Ark tx
// (as a raw transaction), including that it contains exactly one anchor output
// and that the anchor output is the last output.
//
// This is a structural validator only. It does not validate signatures, script
// satisfaction, or VTXO ownership. Those checks belong in higher-level
// validators that have access to policy and VTXO set state.
func ValidateCanonicalTx(tx *wire.MsgTx) error {
	if tx == nil {
		return fmt.Errorf("ark tx must be provided")
	}

	if len(tx.TxOut) == 0 {
		return fmt.Errorf("ark tx has no outputs")
	}

	anchorCount := 0
	for _, out := range tx.TxOut {
		if IsAnchorOutput(out) {
			anchorCount++
		}
	}

	if anchorCount != 1 {
		return fmt.Errorf("ark tx must have exactly one anchor "+
			"output, got %d", anchorCount)
	}

	last := tx.TxOut[len(tx.TxOut)-1]
	if !IsAnchorOutput(last) {
		return fmt.Errorf("ark tx must have anchor as last output")
	}

	err := validateCanonicalOutputs(tx)
	if err != nil {
		return err
	}

	return validateCanonicalInputs(tx)
}

// ValidateCanonicalPSBT validates canonical ordering for an Ark tx PSBT.
func ValidateCanonicalPSBT(pkt *psbt.Packet) error {
	if pkt == nil || pkt.UnsignedTx == nil {
		return fmt.Errorf("ark psbt must include unsigned tx")
	}

	return ValidateCanonicalTx(pkt.UnsignedTx)
}

// CanonicalizeOrdering sorts the transaction inputs and outputs in-place
// according to the v0 rules.
//
// CanonicalizeOrdering does not insert or remove outputs. It assumes the
// caller has already constructed an Ark tx that includes exactly one anchor
// output of value 0. If the anchor output is missing or invalid, this returns
// an error rather than guessing what to do.
func CanonicalizeOrdering(tx *wire.MsgTx) error {
	if tx == nil {
		return fmt.Errorf("ark tx must be provided")
	}

	if len(tx.TxOut) == 0 {
		return fmt.Errorf("ark tx has no outputs")
	}

	anchorIndex := -1
	for idx, out := range tx.TxOut {
		if IsAnchorOutput(out) {
			if anchorIndex != -1 {
				return fmt.Errorf("multiple anchor outputs")
			}

			anchorIndex = idx
		}
	}

	if anchorIndex == -1 {
		return fmt.Errorf("missing anchor output")
	}

	anchorOut := tx.TxOut[anchorIndex]

	recipientOuts := make([]*wire.TxOut, 0, len(tx.TxOut)-1)
	for idx, out := range tx.TxOut {
		if idx == anchorIndex {
			continue
		}

		recipientOuts = append(recipientOuts, out)
	}

	sort.SliceStable(recipientOuts, func(i, j int) bool {
		a := recipientOuts[i]
		b := recipientOuts[j]

		// We order recipient outputs using BIP69 output ordering
		// (amount, then pkScript bytes).
		if a.Value != b.Value {
			return a.Value < b.Value
		}

		return bytes.Compare(a.PkScript, b.PkScript) < 0
	})

	recipientOuts = append(recipientOuts, anchorOut)
	tx.TxOut = recipientOuts

	sort.SliceStable(tx.TxIn, func(i, j int) bool {
		a := tx.TxIn[i].PreviousOutPoint
		b := tx.TxIn[j].PreviousOutPoint

		cmp := bytes.Compare(a.Hash[:], b.Hash[:])
		if cmp != 0 {
			return cmp < 0
		}

		return a.Index < b.Index
	})

	return nil
}

// validateCanonicalInputs validates BIP69 ordering of Ark tx inputs.
func validateCanonicalInputs(tx *wire.MsgTx) error {
	for i := 1; i < len(tx.TxIn); i++ {
		prev := tx.TxIn[i-1].PreviousOutPoint
		cur := tx.TxIn[i].PreviousOutPoint

		cmp := bytes.Compare(prev.Hash[:], cur.Hash[:])
		switch {
		case cmp < 0:
			continue

		case cmp > 0:
			return fmt.Errorf("ark tx inputs are not canonical " +
				"(outpoint hash order)")
		}

		if prev.Index > cur.Index {
			return fmt.Errorf("ark tx inputs are not canonical " +
				"(outpoint index order)")
		}
	}

	return nil
}

// validateCanonicalOutputs validates recipient output ordering rules and anchor
// placement for v0 Ark transfers.
func validateCanonicalOutputs(tx *wire.MsgTx) error {
	if len(tx.TxOut) == 0 {
		return fmt.Errorf("ark tx has no outputs")
	}

	recipientOuts := tx.TxOut[:len(tx.TxOut)-1]

	for i := 1; i < len(recipientOuts); i++ {
		prev := recipientOuts[i-1]
		cur := recipientOuts[i]

		if IsAnchorOutput(prev) || IsAnchorOutput(cur) {
			return fmt.Errorf("anchor output must be last")
		}

		if prev.Value < cur.Value {
			continue
		}

		if prev.Value > cur.Value {
			return fmt.Errorf("ark tx outputs are not canonical " +
				"(value order)")
		}

		if bytes.Compare(prev.PkScript, cur.PkScript) > 0 {
			return fmt.Errorf("ark tx outputs are not canonical " +
				"(pkScript order)")
		}
	}

	return nil
}
