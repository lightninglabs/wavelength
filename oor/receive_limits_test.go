package oor

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/stretchr/testify/require"
)

// TestIncomingTransferEventFromResponseUsesConfiguredCheckpointLimit verifies
// incoming transfer response adaptation enforces the configured checkpoint cap.
func TestIncomingTransferEventFromResponseUsesConfiguredCheckpointLimit(
	t *testing.T) {

	t.Parallel()

	resp, sessionID, _, recipientEventID := buildIncomingResolveResponse(t)
	recipientEvt := resp.Events[0]
	require.Len(t, recipientEvt.CheckpointPsbts, 1)

	recipientEvt.CheckpointPsbts = append(
		recipientEvt.CheckpointPsbts,
		append(
			[]byte(nil), recipientEvt.CheckpointPsbts[0]...,
		),
	)

	_, err := IncomingTransferEventFromResponseWithLimits(
		sessionID, recipientEventID, resp, ReceiveLimits{
			MaxCheckpoints: 1,
		},
	)
	require.ErrorContains(t, err, "checkpoint count 2 exceeds limit 1")
}

// TestIncomingMetadataMatchesFromResponseUsesConfiguredMatchLimit verifies
// incoming metadata response adaptation enforces the configured match cap.
func TestIncomingMetadataMatchesFromResponseUsesConfiguredMatchLimit(
	t *testing.T) {

	t.Parallel()

	sessionID := SessionID(chainhash.Hash{1, 2, 3})
	commitmentTxID := chainhash.Hash{4, 5, 6}

	resp := &arkrpc.ListVTXOsByScriptsResponse{
		Vtxos: []*arkrpc.VTXO{
			testIncomingMetadataVTXO(sessionID, commitmentTxID, 0),
			testIncomingMetadataVTXO(sessionID, commitmentTxID, 1),
		},
	}

	_, err := IncomingMetadataMatchesFromResponseWithLimits(
		sessionID, resp, ReceiveLimits{
			MaxVTXOMatches: 1,
		},
	)
	require.ErrorContains(
		t, err, "incoming metadata match count exceeds limit 1",
	)
}

// TestDecodeLengthPrefixedBlobListUsesConfiguredCountLimit verifies the shared
// mailbox blob-list decoder enforces the configured item-count cap.
func TestDecodeLengthPrefixedBlobListUsesConfiguredCountLimit(t *testing.T) {
	t.Parallel()

	raw, err := encodeLengthPrefixedBlobList([][]byte{{0x01}, {0x02}})
	require.NoError(t, err)

	_, err = decodeLengthPrefixedBlobListWithLimits(
		raw, ReceiveLimits{
			MaxMailboxItems: 1,
		},
	)
	require.ErrorContains(t, err, "blob list count 2 exceeds limit 1")
}

// testIncomingMetadataVTXO builds a minimal RPC VTXO that can be converted
// into an IncomingMetadataMatch by the response adapter.
func testIncomingMetadataVTXO(sessionID SessionID,
	commitmentTxID chainhash.Hash, outputIndex uint32) *arkrpc.VTXO {

	return &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: sessionID[:],
			Vout: outputIndex,
		},
		RoundId:        "round-configured-limit",
		CommitmentTxid: commitmentTxID[:],
		AncestryPaths: []*arkrpc.AncestryPath{{
			CommitmentTxid: commitmentTxID[:],
		}},
	}
}
