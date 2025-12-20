package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
)

// This file contains compatibility wrappers around the `lib/tx/arktx` package.
//
// Keeping these symbols in `lib/tx/oor` makes it possible for callers to use
// a single import path while the codebase migrates to finer-grained
// primitives.

// IsAnchorOutput returns true if the output is the v0 Ark anchor output (P2A,
// value 0).
func IsAnchorOutput(out *wire.TxOut) bool {
	return arktx.IsAnchorOutput(out)
}

// ValidateCanonicalArkTx validates canonical ordering rules for an Ark tx.
//
// The v0 rule set matches BIP-0069 ordering, plus Ark's anchor output rules.
// See `lib/tx/arktx.ValidateCanonicalTx` for the full rule set.
func ValidateCanonicalArkTx(tx *wire.MsgTx) error {
	return arktx.ValidateCanonicalTx(tx)
}

// ValidateCanonicalArkPSBT validates canonical ordering for an Ark tx PSBT.
func ValidateCanonicalArkPSBT(pkt *psbt.Packet) error {
	return arktx.ValidateCanonicalPSBT(pkt)
}

// CanonicalizeArkTxOrdering sorts the transaction inputs and outputs in-place
// according to the v0 rules.
func CanonicalizeArkTxOrdering(tx *wire.MsgTx) error {
	return arktx.CanonicalizeOrdering(tx)
}
