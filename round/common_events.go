package round

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// This file defines common event types that are shared between client and
// server state machines. These event types are embedded in the specific
// client and server event structures to avoid duplication and ensure
// consistency in the data passed between state transitions.

// CommitmentTxBuiltEvent represents the completion of commitment transaction
// construction. The server emits this event after building the transaction
// that commits all boarding UTXOs, while clients receive it to validate the
// transaction structure and their VTXT paths.
type CommitmentTxBuiltEvent struct {
	// RoundID identifies which round this commitment transaction belongs
	// to.
	RoundID string

	// Tx is the unsigned commitment transaction that locks all boarding
	// UTXOs and commits to the VTXT tree.
	Tx *wire.MsgTx

	// VTXTTree contains the client's extracted sub-tree from the virtual
	// transaction tree. The server sends only the minimal path containing
	// the transactions needed to reach this client's VTXO leaves, not the
	// full tree (which may contain hundreds of transactions for all
	// participants). This sub-tree is sufficient for the client to verify
	// their VTXOs and perform unilateral exit if needed.
	VTXTTree *tree.Tree
}

// NoncesAggregatedEvent represents the completion of MuSig2 nonce aggregation.
// The server computes aggregated nonces from all participants and sends them
// back to clients, who use them to generate partial signatures in the next
// phase of the MuSig2 signing protocol.
type NoncesAggregatedEvent struct {
	// RoundID identifies which round these aggregated nonces belong to.
	RoundID string

	// AggregatedNonces maps transaction IDs to their aggregated MuSig2
	// nonces. Each entry corresponds to a transaction in the VTXT that
	// requires signing.
	AggregatedNonces map[chainhash.Hash][]byte
}

// OperatorSignedEvent represents the completion of VTXT signature aggregation
// by the operator. After collecting and validating all partial signatures from
// participants, the operator produces complete Schnorr signatures for each
// transaction in the VTXT. Clients must validate these signatures before
// proceeding to sign the boarding input.
type OperatorSignedEvent struct {
	// RoundID identifies which round these signatures belong to.
	RoundID string

	// Signatures contains the complete aggregated Schnorr signatures for
	// each transaction in the VTXT. The order corresponds to the
	// transaction ordering in the VTXT tree.
	Signatures [][]byte

	// SignedVTXT optionally contains the fully signed virtual transaction
	// tree in serialized form. This may be omitted and reconstructed from
	// the signatures if needed.
	SignedVTXT []byte
}

// VTXOInfo wraps VTXO information with additional boarding-specific context
// such as the round in which it was created, the boarding UTXO that funded it,
// and timestamps for tracking purposes.
//
// TODO(boarding): Evaluate if we need a custom wrapper struct or if lib types
// are sufficient. If we need to add boarding-specific metadata (e.g., round
// ID, boarding UTXO reference, creation timestamp), create a BoardingVTXO
// wrapper struct that embeds the lib VTXO type.
type VTXOInfo struct {
	// TODO: Add boarding-specific fields here if needed. For now, we'll
	// use lib types directly.
}
