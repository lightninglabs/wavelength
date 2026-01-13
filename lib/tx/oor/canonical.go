package oor

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
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

	return bytes.Equal(out.PkScript, scripts.AnchorPkScript)
}

// ValidateCanonicalArkTx validates the canonical ordering rules for an Ark tx
// (as a raw transaction).
//
// The v0 rule set is based on BIP-0069-style sorting:
//   - inputs are ordered by previous outpoint (txid, then vout); and
//   - non-anchor outputs are ordered lexicographically by raw pkScript bytes,
//     with output value used only as a tie-breaker.
//
// Ark txs additionally include exactly one anchor output (P2A, value 0), and
// the anchor output must be the final output.
//
// This is a structural validator only. It does not validate signatures, script
// satisfaction, or VTXO ownership. Those checks belong in higher-level
// validators that have access to policy and VTXO set state.
func ValidateCanonicalArkTx(tx *wire.MsgTx) error {
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
		return fmt.Errorf("ark tx must have anchor as last " +
			"output")
	}

	err := validateCanonicalArkOutputs(tx)
	if err != nil {
		return err
	}

	return validateCanonicalArkInputs(tx)
}

// ValidateCanonicalArkPSBT validates canonical ordering for an Ark tx PSBT.
func ValidateCanonicalArkPSBT(pkt *psbt.Packet) error {
	if pkt == nil || pkt.UnsignedTx == nil {
		return fmt.Errorf("ark psbt must include unsigned tx")
	}

	return ValidateCanonicalArkTx(pkt.UnsignedTx)
}

// validateCanonicalArkInputs validates BIP-0069-style ordering of Ark tx
// inputs.
func validateCanonicalArkInputs(tx *wire.MsgTx) error {
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

// validateCanonicalArkOutputs validates recipient output ordering rules and
// anchor placement for v0 OOR transfers.
//
// Recipient outputs exclude the anchor output. Their ordering is compatible
// with BIP-0069 output ordering, using raw pkScript bytes as the primary sort
// key.
func validateCanonicalArkOutputs(tx *wire.MsgTx) error {
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

		cmp := bytes.Compare(prev.PkScript, cur.PkScript)
		switch {
		case cmp < 0:
			continue

		case cmp > 0:
			return fmt.Errorf("ark tx outputs are not canonical " +
				"(pkScript order)")
		}

		if prev.Value > cur.Value {
			return fmt.Errorf("ark tx outputs are not canonical " +
				"(value tie-break)")
		}
	}

	return nil
}

// CanonicalizeArkTxOrdering sorts the transaction inputs and outputs in-place
// according to the v0 rules.
//
// CanonicalizeArkTxOrdering does not insert or remove outputs. It assumes the
// caller has already constructed an Ark tx that includes exactly one anchor
// output of value 0. If the anchor output is missing or invalid, this returns
// an error rather than guessing what to do.
func CanonicalizeArkTxOrdering(tx *wire.MsgTx) error {
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

		cmp := bytes.Compare(a.PkScript, b.PkScript)
		if cmp != 0 {
			return cmp < 0
		}

		return a.Value < b.Value
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
