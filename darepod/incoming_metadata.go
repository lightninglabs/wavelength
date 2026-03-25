package darepod

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/oor"
)

const incomingMetadataIndexPageSize = 128

// ResolveIncomingMetadataFromIndexer queries the authoritative indexer
// inventory for the just-created OOR output and maps the result into the
// incoming materialization metadata required by the local VTXO store.
func ResolveIncomingMetadataFromIndexer(ctx context.Context,
	idx *indexer.Client, sessionID oor.SessionID,
	recipient oor.ArkRecipientOutput) (oor.IncomingVTXOMetadata, error) {

	if idx == nil {
		return oor.IncomingVTXOMetadata{}, fmt.Errorf("indexer client " + //nolint:ll
			"must be provided")
	}

	logger := build.LoggerFromContext(ctx)

	logger.DebugS(ctx, "Resolving incoming metadata from indexer",
		slog.String("session_id", chainhash.Hash(sessionID).String()),
		slog.Int("output_index", int(recipient.OutputIndex)),
		slog.String("pk_script", fmt.Sprintf("%x", recipient.PkScript)))

	var cursor uint64
	for {
		resp, err := idx.ListVTXOsByScriptsTaproot(
			ctx,
			[]indexer.TaprootScriptScope{{
				PkScript: append(
					[]byte(nil), recipient.PkScript...,
				),
			}},
			cursor, incomingMetadataIndexPageSize, nil,
		)
		if err != nil {
			return oor.IncomingVTXOMetadata{}, fmt.Errorf("list "+
				"VTXOs by script: %w", err)
		}

		for _, candidate := range resp.GetVtxos() {
			match, err := matchesIncomingVTXO(
				candidate, sessionID, recipient.OutputIndex,
			)
			if err != nil {
				return oor.IncomingVTXOMetadata{}, err
			}
			if !match {
				continue
			}

			logger.DebugS(ctx, "Matched incoming indexer VTXO",
				slog.String("session_id",
					chainhash.Hash(sessionID).String()),
				slog.Int("output_index",
					int(recipient.OutputIndex)),
				slog.String("round_id",
					candidate.GetRoundId()),
				slog.Int("tree_depth",
					int(candidate.GetTreeDepth())),
				slog.Int("chain_depth",
					int(candidate.GetChainDepth())))

			return incomingMetadataFromRPC(candidate)
		}

		nextCursor := resp.GetNextCursor()
		if len(resp.GetVtxos()) == 0 || nextCursor <= cursor {
			break
		}

		cursor = nextCursor
	}

	logger.DebugS(ctx, "Incoming indexer VTXO not found",
		slog.String("session_id", chainhash.Hash(sessionID).String()),
		slog.Int("output_index", int(recipient.OutputIndex)))

	return oor.IncomingVTXOMetadata{}, fmt.Errorf("incoming vtxo %s:%d "+
		"not found in indexer inventory",
		chainhash.Hash(sessionID), recipient.OutputIndex)
}

// matchesIncomingVTXO returns true when candidate identifies the target Ark
// output created by sessionID at outputIndex.
func matchesIncomingVTXO(candidate *arkrpc.VTXO, sessionID oor.SessionID,
	outputIndex uint32) (bool, error) {

	if candidate == nil {
		return false, nil
	}

	outpoint := candidate.GetOutpoint()
	if outpoint == nil {
		return false, fmt.Errorf("indexer VTXO missing outpoint")
	}

	return bytes.Equal(outpoint.GetTxid(), sessionID[:]) &&
		outpoint.GetVout() == outputIndex, nil
}

// incomingMetadataFromRPC maps the authoritative indexer VTXO view into the
// metadata shape required by the incoming OOR materialization path.
func incomingMetadataFromRPC(candidate *arkrpc.VTXO) (
	oor.IncomingVTXOMetadata, error) {

	if candidate == nil {
		return oor.IncomingVTXOMetadata{}, fmt.Errorf("indexer vtxo " +
			"must be provided")
	}

	if candidate.GetRoundId() == "" {
		return oor.IncomingVTXOMetadata{}, fmt.Errorf("indexer vtxo " +
			"missing round id")
	}

	if len(candidate.GetCommitmentTxid()) != chainhash.HashSize {
		return oor.IncomingVTXOMetadata{}, fmt.Errorf("indexer vtxo " +
			"missing commitment txid")
	}

	treePath, err := arkrpc.TreePathToTree(
		candidate.GetTreePath(),
	)
	if err != nil {
		return oor.IncomingVTXOMetadata{}, fmt.Errorf("convert "+
			"tree path: %w", err)
	}

	var commitmentTxID chainhash.Hash
	copy(commitmentTxID[:], candidate.GetCommitmentTxid())

	return oor.IncomingVTXOMetadata{
		RoundID:        candidate.GetRoundId(),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    candidate.GetBatchExpiryHeight(),
		TreeDepth:      int(candidate.GetTreeDepth()),
		ChainDepth:     int(candidate.GetChainDepth()),
		CreatedHeight:  candidate.GetCreatedHeight(),
		TreePath:       treePath,
	}, nil
}
