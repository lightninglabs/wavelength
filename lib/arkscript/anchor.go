package arkscript

import (
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

var (
	// AnchorPkScript is a standard P2A script. This is a keyless output.
	// This corresponds to address: bc1pfeessrawgf.
	// The script path for this output requires no witness elements,
	// meaning that it can be spent by anyone who provides the script and
	// control block.
	AnchorPkScript = []byte{
		txscript.OP_TRUE,
		txscript.OP_DATA_2,
		// The P2A identifier.
		0x4e, 0x73,
	}
)

// AnchorOption customizes the P2A output returned by AnchorOutput. The
// default (no options) produces a zero-value ephemeral anchor suitable for
// BIP-431 TRUC parents that pay zero fee themselves and rely on a CPFP
// descendant to fund the package (per the BIP-433 ephemeral-dust rule).
// Callers whose parent tx pays its own miner fee must use WithAnchorValue
// to lift the anchor above the P2A dust threshold; otherwise relay
// rejects the parent with "dust, tx with dust output must be 0-fee".
type AnchorOption func(*wire.TxOut)

// WithAnchorValue overrides the default zero-value anchor with a caller-
// supplied sat amount. Per BIP-433 the P2A dust threshold is 240 sats, so
// any value strictly greater than 240 takes the output out of the
// ephemeral-dust regime and lets the parent pay a non-zero fee.
func WithAnchorValue(sats int64) AnchorOption {
	return func(out *wire.TxOut) {
		out.Value = sats
	}
}

// AnchorOutput returns a P2A output. Without options the value is zero,
// matching the BIP-433 ephemeral-anchor pattern used by canonical Ark
// tree / forfeit / checkpoint parents that pay 0 fee themselves and are
// confirmed via a CPFP child. Callers whose parent pays a non-zero fee
// must pass WithAnchorValue to lift the anchor above dust (the P2A dust
// threshold is 240 sats by default), so bitcoind does not reject the
// parent with "dust, tx with dust output must be 0-fee". The returned
// PkScript is a defensive copy so callers cannot mutate the package-level
// global.
func AnchorOutput(opts ...AnchorOption) *wire.TxOut {
	script := make([]byte, len(AnchorPkScript))
	copy(script, AnchorPkScript)

	out := &wire.TxOut{
		PkScript: script,
		Value:    0,
	}
	for _, opt := range opts {
		opt(out)
	}

	return out
}
