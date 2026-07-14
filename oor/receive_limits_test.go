package oor

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	lib_tree "github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
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

// TestIncomingTransferEventFromResponseLimitsAncestorCheckpoints verifies the
// RPC adapter bounds checkpoint lists on supplied ancestor packages before
// they can enter the receive FSM.
func TestIncomingTransferEventFromResponseLimitsAncestorCheckpoints(
	t *testing.T) {

	t.Parallel()

	resp, sessionID, _, recipientEventID := buildIncomingResolveResponse(t)
	ancestorArk, ancestorCheckpoints, _, _, _, _ :=
		buildTestIncomingMaterialization(t)

	ancestorArkRaw, err := psbtutil.Serialize(ancestorArk)
	require.NoError(t, err)

	ancestorCheckpointRaw, err := psbtutil.Serialize(
		ancestorCheckpoints[0],
	)
	require.NoError(t, err)

	ancestorID := SessionID(ancestorArk.UnsignedTx.TxHash())
	resp.Events[0].AncestorPackages = []*arkrpc.OORSessionPackage{{
		SessionId: ancestorID[:],
		ArkPsbt:   ancestorArkRaw,
		CheckpointPsbts: [][]byte{
			ancestorCheckpointRaw,
			ancestorCheckpointRaw,
		},
	}}

	_, err = IncomingTransferEventFromResponseWithLimits(
		sessionID, recipientEventID, resp, ReceiveLimits{
			MaxCheckpoints: 1,
		},
	)
	require.ErrorContains(
		t, err, "ancestor package 0 checkpoint count 2 exceeds limit 1",
	)
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
			testIncomingMetadataVTXO(
				sessionID, commitmentTxID, 0,
			),
			testIncomingMetadataVTXO(
				sessionID, commitmentTxID, 1,
			),
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

// TestDecodeResolveIncomingTransferPayloadUsesConfiguredPkScriptLimit verifies
// mailbox resolve payload decoding enforces the configured recipient script
// byte cap.
func TestDecodeResolveIncomingTransferPayloadUsesConfiguredPkScriptLimit(
	t *testing.T) {

	t.Parallel()

	raw, err := encodeResolveIncomingTransferPayload(
		SessionID(
			chainhash.Hash{7, 8, 9},
		),
		[]byte{0x51, 0x20},
		3,
	)
	require.NoError(t, err)

	_, _, _, err = decodeResolveIncomingTransferPayloadWithLimits(
		raw, ReceiveLimits{
			MaxMailboxScriptBytes: 1,
		},
	)
	require.ErrorContains(
		t, err, "recipient pk_script length 2 exceeds limit 1",
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
		AncestryPaths: []*arkrpc.AncestryPath{
			testValidAncestryPath(commitmentTxID),
		},
	}
}

// testValidAncestryPath returns an AncestryPath whose reconstructed
// tree_depth matches the proto scalar. Receive-time validation
// (arkrpc.ValidateAncestryPathDepth, the wavelength#370 guard)
// rejects zero or mismatched depths, so test fixtures must keep these
// in sync.
func testValidAncestryPath(commitmentTxID chainhash.Hash) *arkrpc.AncestryPath {
	t := &lib_tree.Tree{
		Root: &lib_tree.Node{},
		BatchOutpoint: wire.OutPoint{
			Hash: commitmentTxID,
		},
	}

	p, err := arkrpc.AncestryPathFromTree(t, commitmentTxID, []uint32{0})
	if err != nil {
		panic("build test ancestry path: " + err.Error())
	}

	return p
}
