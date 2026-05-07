package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/stretchr/testify/require"
)

// TestIncomingMetadataFromRPCOperatorKey verifies incoming metadata parsing
// preserves the per-VTXO operator key returned by the indexer.
func TestIncomingMetadataFromRPCOperatorKey(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	commitmentTxID := chainhash.Hash{1, 2, 3}
	meta, err := incomingMetadataFromRPC(&arkrpc.VTXO{
		RoundId:        "round-keyed",
		CommitmentTxid: commitmentTxID[:],
		OperatorPubkey: operatorKey.PubKey().SerializeCompressed(),
		AncestryPaths: []*arkrpc.AncestryPath{{
			CommitmentTxid: commitmentTxID[:],
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, meta.OperatorKey)
	require.True(t, meta.OperatorKey.IsEqual(operatorKey.PubKey()))
}

// TestIncomingMetadataFromRPCLegacyOperatorKey verifies older indexer
// responses that omit operator_pubkey still decode for compatibility.
func TestIncomingMetadataFromRPCLegacyOperatorKey(t *testing.T) {
	t.Parallel()

	commitmentTxID := chainhash.Hash{4, 5, 6}
	meta, err := incomingMetadataFromRPC(&arkrpc.VTXO{
		RoundId:        "round-legacy",
		CommitmentTxid: commitmentTxID[:],
		AncestryPaths: []*arkrpc.AncestryPath{{
			CommitmentTxid: commitmentTxID[:],
		}},
	})
	require.NoError(t, err)
	require.Nil(t, meta.OperatorKey)
}

// TestIncomingMetadataFromRPCRejectsInvalidOperatorKey verifies malformed
// operator key bytes fail before incoming VTXO materialization.
func TestIncomingMetadataFromRPCRejectsInvalidOperatorKey(t *testing.T) {
	t.Parallel()

	commitmentTxID := chainhash.Hash{7, 8, 9}
	_, err := incomingMetadataFromRPC(&arkrpc.VTXO{
		RoundId:        "round-invalid-key",
		CommitmentTxid: commitmentTxID[:],
		OperatorPubkey: []byte{0x02},
		AncestryPaths: []*arkrpc.AncestryPath{{
			CommitmentTxid: commitmentTxID[:],
		}},
	})
	require.ErrorContains(t, err, "parse indexer vtxo operator pubkey")
}
