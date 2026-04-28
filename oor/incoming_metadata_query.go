package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/vtxo"
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

	// TODO(oor-receive): The maxIncomingMetadataMatches limit guards
	// against unbounded allocations from a malicious or misconfigured
	// indexer response. Raise this constant via a tracked issue if the
	// protocol ever allows more outputs per incoming transfer.
	const maxIncomingMetadataMatches = 128

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

		if len(matches) > maxIncomingMetadataMatches {
			return nil, fmt.Errorf(
				"incoming metadata match count "+
					"exceeds limit %d",
				maxIncomingMetadataMatches,
			)
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

	ancestry, err := ancestryFromRPC(candidate.GetAncestryPaths())
	if err != nil {
		return IncomingVTXOMetadata{}, fmt.Errorf("convert "+
			"ancestry paths: %w", err)
	}

	var commitmentTxID chainhash.Hash
	copy(commitmentTxID[:], candidate.GetCommitmentTxid())

	return IncomingVTXOMetadata{
		RoundID:        candidate.GetRoundId(),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    candidate.GetBatchExpiryHeight(),
		ChainDepth:     int(candidate.GetChainDepth()),
		CreatedHeight:  candidate.GetCreatedHeight(),
		Ancestry:       ancestry,
	}, nil
}

// maxAncestryPaths bounds the per-VTXO ancestry slice the indexer is
// allowed to return. Real cross-round multi-input OOR VTXOs see at most
// a handful of contributing commitments; the cap exists so a misbehaving
// or compromised indexer cannot force unbounded allocation here before
// the per-entry validation runs.
const maxAncestryPaths = 64

// ancestryFromRPC converts a slice of arkrpc.AncestryPath into the typed
// vtxo.Ancestry shape used by the descriptor and incoming metadata
// pipelines. Returns an error when the slice is empty (a VTXO without
// ancestry would persist as unexitable, so version-skew producers that
// still send the retired tree_path/tree_depth scalars must fail closed
// here rather than silently materialize a stranded descriptor) or when
// the slice exceeds maxAncestryPaths.
func ancestryFromRPC(paths []*arkrpc.AncestryPath) ([]vtxo.Ancestry, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("indexer vtxo missing ancestry paths")
	}

	if len(paths) > maxAncestryPaths {
		return nil, fmt.Errorf(
			"indexer vtxo ancestry exceeds cap: got %d, max %d",
			len(paths), maxAncestryPaths,
		)
	}

	out := make([]vtxo.Ancestry, 0, len(paths))
	for i, p := range paths {
		if p == nil {
			continue
		}

		treePath, err := arkrpc.AncestryPathToTree(p)
		if err != nil {
			return nil, fmt.Errorf("path[%d] tree: %w", i, err)
		}

		commitmentTxID, err := arkrpc.AncestryCommitmentTxID(p)
		if err != nil {
			return nil, fmt.Errorf(
				"path[%d] commitment: %w", i, err,
			)
		}

		out = append(out, vtxo.Ancestry{
			TreePath:       treePath,
			CommitmentTxID: commitmentTxID,
			InputIndices: append(
				[]uint32(nil), p.GetInputIndices()...,
			),
			TreeDepth: p.GetTreeDepth(),
		})
	}

	return out, nil
}
