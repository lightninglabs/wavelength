package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
)

const incomingMetadataCorrelationPrefix = "oor-incoming-metadata:"

// IncomingMetadataMatch carries authoritative metadata for one materialized
// incoming Ark output.
type IncomingMetadataMatch struct {
	// OutputIndex identifies the Ark output this metadata belongs to.
	OutputIndex uint32

	// Metadata carries the authoritative lineage and expiry data.
	Metadata IncomingVTXOMetadata
}

// IncomingMetadataResolvedEvent delivers the authoritative incoming metadata
// query result back into the receive FSM.
type IncomingMetadataResolvedEvent struct {
	// Matches contains metadata keyed by Ark output index for the current
	// incoming transfer session.
	Matches []IncomingMetadataMatch
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *IncomingMetadataResolvedEvent) eventSealed() {}

// IncomingMetadataCorrelationID returns the stable unary correlation ID used
// for durable incoming metadata queries for the given session.
func IncomingMetadataCorrelationID(sessionID SessionID) string {
	return incomingMetadataCorrelationPrefix +
		chainhash.Hash(sessionID).String()
}

// ParseIncomingMetadataCorrelationID decodes a durable incoming metadata query
// correlation ID back into the OOR session ID.
func ParseIncomingMetadataCorrelationID(correlationID string) (
	SessionID, error) {

	if len(correlationID) <= len(incomingMetadataCorrelationPrefix) ||
		correlationID[:len(incomingMetadataCorrelationPrefix)] !=
			incomingMetadataCorrelationPrefix {

		return SessionID{}, fmt.Errorf("unexpected incoming metadata "+
			"correlation id: %q", correlationID)
	}

	hash, err := chainhash.NewHashFromStr(
		correlationID[len(incomingMetadataCorrelationPrefix):],
	)
	if err != nil {
		return SessionID{}, fmt.Errorf("parse incoming metadata "+
			"session id: %w", err)
	}

	return SessionID(*hash), nil
}

// IncomingMetadataMatchesFromResponse filters an indexer
// ListVTXOsByScriptsResponse down to the entries created by the given Ark
// session and converts them into the local incoming metadata shape.
func IncomingMetadataMatchesFromResponse(
	sessionID SessionID,
	resp *arkrpc.ListVTXOsByScriptsResponse,
) ([]IncomingMetadataMatch, error) {

	if resp == nil {
		return nil, fmt.Errorf(
			"incoming metadata response must be provided",
		)
	}

	matches := make([]IncomingMetadataMatch, 0, len(resp.GetVtxos()))
	for _, candidate := range resp.GetVtxos() {
		if candidate == nil {
			continue
		}

		outpoint := candidate.GetOutpoint()
		if outpoint == nil {
			return nil, fmt.Errorf("indexer vtxo missing outpoint")
		}

		if !matchesIncomingVTXO(sessionID, outpoint.GetTxid()) {
			continue
		}

		metadata, err := incomingMetadataFromRPC(candidate)
		if err != nil {
			return nil, err
		}

		matches = append(matches, IncomingMetadataMatch{
			OutputIndex: outpoint.GetVout(),
			Metadata:    metadata,
		})
	}

	return matches, nil
}

// matchesIncomingVTXO reports whether the candidate txid belongs to the
// current Ark session.
func matchesIncomingVTXO(sessionID SessionID, txid []byte) bool {
	return len(txid) == chainhash.HashSize &&
		string(txid) == string(sessionID[:])
}

// incomingMetadataFromRPC maps the authoritative indexer VTXO view into the
// metadata shape required by the incoming OOR materialization path.
func incomingMetadataFromRPC(candidate *arkrpc.VTXO) (
	IncomingVTXOMetadata, error) {

	if candidate == nil {
		return IncomingVTXOMetadata{}, fmt.Errorf("indexer vtxo " +
			"must be provided")
	}

	if candidate.GetRoundId() == "" {
		return IncomingVTXOMetadata{}, fmt.Errorf("indexer vtxo " +
			"missing round id")
	}

	if len(candidate.GetCommitmentTxid()) != chainhash.HashSize {
		return IncomingVTXOMetadata{}, fmt.Errorf("indexer vtxo " +
			"missing commitment txid")
	}

	treePath, err := arkrpc.TreePathToTree(
		candidate.GetTreePath(),
	)
	if err != nil {
		return IncomingVTXOMetadata{}, fmt.Errorf("convert "+
			"tree path: %w", err)
	}

	var commitmentTxID chainhash.Hash
	copy(commitmentTxID[:], candidate.GetCommitmentTxid())

	return IncomingVTXOMetadata{
		RoundID:        candidate.GetRoundId(),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    candidate.GetBatchExpiryHeight(),
		TreeDepth:      int(candidate.GetTreeDepth()),
		ChainDepth:     int(candidate.GetChainDepth()),
		CreatedHeight:  candidate.GetCreatedHeight(),
		TreePath:       treePath,
	}, nil
}
