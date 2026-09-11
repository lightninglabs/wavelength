package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/stretchr/testify/require"
)

// TestIsIncomingMetadataCorrelationID verifies only durable incoming metadata
// query correlation IDs match the OOR metadata route prefix.
func TestIsIncomingMetadataCorrelationID(t *testing.T) {
	t.Parallel()

	var sessionID SessionID
	sessionID[0] = 1

	require.True(
		t,
		IsIncomingMetadataCorrelationID(
			IncomingMetadataCorrelationID(sessionID),
		),
	)
	require.False(t, IsIncomingMetadataCorrelationID(""))
	require.False(
		t, IsIncomingMetadataCorrelationID("00aa8bfb11f09881bbd2"),
	)
	require.False(
		t, IsIncomingMetadataCorrelationID(
			incomingMetadataCorrelationPrefix,
		),
	)
}

// TestIncomingMetadataFromRPCOperatorKey verifies incoming metadata parsing
// preserves the per-VTXO operator key returned by the indexer.
func TestIncomingMetadataFromRPCOperatorKey(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	candidate, _ := expiryfixture.Round(t, 1000, []byte{0x51}, 50, 100, 1)
	candidate.OperatorPubkey = operatorKey.PubKey().SerializeCompressed()
	meta, err := incomingMetadataFromRPC(candidate)
	require.NoError(t, err)
	require.NotNil(t, meta.OperatorKey)
	require.True(t, meta.OperatorKey.IsEqual(operatorKey.PubKey()))
}

// TestIncomingMetadataFromRPCLegacyOperatorKey verifies older indexer
// responses that omit operator_pubkey still decode for compatibility.
func TestIncomingMetadataFromRPCLegacyOperatorKey(t *testing.T) {
	t.Parallel()

	candidate, _ := expiryfixture.Round(t, 1000, []byte{0x51}, 50, 100, 2)
	meta, err := incomingMetadataFromRPC(candidate)
	require.NoError(t, err)
	require.Nil(t, meta.OperatorKey)
}

// TestIncomingMetadataFromRPCRejectsInvalidOperatorKey verifies malformed
// operator key bytes fail before incoming VTXO materialization.
func TestIncomingMetadataFromRPCRejectsInvalidOperatorKey(t *testing.T) {
	t.Parallel()

	candidate, _ := expiryfixture.Round(t, 1000, []byte{0x51}, 50, 100, 3)
	candidate.OperatorPubkey = []byte{0x02}
	_, err := incomingMetadataFromRPC(candidate)
	require.ErrorContains(t, err, "parse indexer vtxo operator pubkey")
}

// TestIncomingMetadataFromRPCRejectsZeroTreeDepth is the unit-level
// regression for wavelength#370 on the OOR-package boundary: a
// matching VTXO whose AncestryPath claims tree_depth = 0 must be
// rejected before persistence.
func TestIncomingMetadataFromRPCRejectsZeroTreeDepth(t *testing.T) {
	t.Parallel()

	commitmentTxID := chainhash.Hash{0xab}
	path := testValidAncestryPath(commitmentTxID)
	path.TreeDepth = 0

	_, err := incomingMetadataFromRPC(&arkrpc.VTXO{
		RoundId:        "round-zero-depth",
		CommitmentTxid: commitmentTxID[:],
		AncestryPaths:  []*arkrpc.AncestryPath{path},
	})
	require.ErrorContains(t, err, "tree_depth must be non-zero")
}
