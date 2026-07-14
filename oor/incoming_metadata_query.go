package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/vtxo"
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

// IsIncomingMetadataCorrelationID returns true when correlationID belongs to a
// durable incoming metadata query.
func IsIncomingMetadataCorrelationID(correlationID string) bool {
	return len(correlationID) > len(incomingMetadataCorrelationPrefix) &&
		correlationID[:len(incomingMetadataCorrelationPrefix)] ==
			incomingMetadataCorrelationPrefix
}

// ParseIncomingMetadataCorrelationID decodes a durable incoming metadata query
// correlation ID back into the OOR session ID.
func ParseIncomingMetadataCorrelationID(correlationID string) (SessionID,
	error) {

	if !IsIncomingMetadataCorrelationID(correlationID) {
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
func IncomingMetadataMatchesFromResponse(sessionID SessionID,
	resp *arkrpc.ListVTXOsByScriptsResponse) ([]IncomingMetadataMatch,
	error) {

	return IncomingMetadataMatchesFromResponseWithLimits(
		sessionID, resp, ReceiveLimits{},
	)
}

// IncomingMetadataMatchesFromResponseWithLimits filters an indexer
// ListVTXOsByScriptsResponse down to entries created by the given Ark session
// using the supplied defense-in-depth limits. Zero limit fields use package
// defaults.
func IncomingMetadataMatchesFromResponseWithLimits(sessionID SessionID,
	resp *arkrpc.ListVTXOsByScriptsResponse,
	limits ReceiveLimits) ([]IncomingMetadataMatch, error) {

	if resp == nil {
		return nil, fmt.Errorf("incoming metadata response must be " +
			"provided")
	}

	limits = normalizeReceiveLimits(limits)

	candidates := vtxo.FlattenListVTXOsByScriptsResponse(resp)
	matches := make([]IncomingMetadataMatch, 0, len(candidates))
	for _, candidate := range candidates {
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

		if uint64(len(matches)) >
			uint64(limits.MaxVTXOMatches) {
			return nil, fmt.Errorf("max metadata matches "+
				"exceeded: incoming metadata match count "+
				"exceeds limit %d", limits.MaxVTXOMatches)
		}
	}

	return matches, nil
}

// matchesIncomingVTXO reports whether the candidate txid belongs to the
// current Ark session.
func matchesIncomingVTXO(sessionID SessionID, txid []byte) bool {
	return bytes.Equal(txid, sessionID[:])
}

// incomingMetadataFromRPC maps the authoritative indexer VTXO view into the
// metadata shape required by the incoming OOR materialization path.
func incomingMetadataFromRPC(candidate *arkrpc.VTXO) (IncomingVTXOMetadata,
	error) {

	if candidate == nil {
		return IncomingVTXOMetadata{}, fmt.Errorf("indexer vtxo must " +
			"be provided")
	}

	if candidate.GetRoundId() == "" {
		return IncomingVTXOMetadata{}, fmt.Errorf("indexer vtxo " +
			"missing round id")
	}

	if len(candidate.GetCommitmentTxid()) != chainhash.HashSize {
		return IncomingVTXOMetadata{}, fmt.Errorf("indexer vtxo " +
			"missing commitment txid")
	}

	ancestry, err := vtxo.AncestryFromRPC(candidate.GetAncestryPaths())
	if err != nil {
		return IncomingVTXOMetadata{}, fmt.Errorf("convert ancestry "+
			"paths: %w", err)
	}

	operatorKey, err := incomingOperatorKeyFromRPC(
		candidate.GetOperatorPubkey(),
	)
	if err != nil {
		return IncomingVTXOMetadata{}, err
	}

	var commitmentTxID chainhash.Hash
	copy(commitmentTxID[:], candidate.GetCommitmentTxid())

	return IncomingVTXOMetadata{
		RoundID:        candidate.GetRoundId(),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    candidate.GetBatchExpiryHeight(),
		ChainDepth:     int(candidate.GetChainDepth()),
		CreatedHeight:  candidate.GetCreatedHeight(),
		OperatorKey:    operatorKey,
		Ancestry:       ancestry,
	}, nil
}

// incomingOperatorKeyFromRPC parses the optional operator key carried by an
// indexer VTXO response. An empty value is allowed for compatibility with
// older indexers that predate per-VTXO operator-key metadata.
func incomingOperatorKeyFromRPC(raw []byte) (*btcec.PublicKey, error) {
	return decodeOptionalPubKey(raw, "indexer vtxo operator pubkey")
}
