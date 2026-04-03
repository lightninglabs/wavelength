package oor

// Package oor defines the transaction-level primitives for v0 out-of-round
// transfers.
//
// These primitives are shared by both client and server to avoid validation
// drift. The package intentionally focuses on deterministic construction and
// canonical encoding:
// - PSBT is used as the transport and storage envelope.
// - Output ordering follows BIP69 ordering (amount, then pkScript) for
//   recipient outputs.
// - Checkpoint txs use output 0 as the spendable output and place their
//   anchor output last.
// - The Ark anchor output is always last.
//
// The submit flow builds a package containing the Ark PSBT plus one or more
// checkpoint PSBTs. The finalize flow attaches TapTree metadata binding each
// checkpoint to the Ark transaction, then validates the finalized package.
