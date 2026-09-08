package vtxo

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestIndexedAncestryBindsTarget proves a valid later-expiring batch for
// another VTXO cannot authenticate the received target, including a forged leaf
// body.
func TestIndexedAncestryBindsTarget(t *testing.T) {
	t.Parallel()
	original, commitment := expiryfixture.Round(
		t, 10000, []byte{0x51}, 50, 100, 1,
	)
	other, _ := expiryfixture.Round(t, 10000, []byte{0x51}, 200, 100, 2)
	ancestry, err := IndexedAncestryFromRPC(original)
	require.NoError(t, err)
	expiry, err := AuthenticateBatchExpiry(t.Context(), ancestry,
		func(context.Context, chainhash.Hash, []byte, uint32) (
			CommitmentConfirmation, error) {

			return CommitmentConfirmation{
				Tx:          commitment,
				BlockHeight: 100,
			}, nil
		})
	require.NoError(t, err)
	require.Equal(t, int32(150), expiry)

	for _, mutation := range []struct {
		name  string
		apply func(*arkrpc.VTXO)
	}{
		{
			"different batch",
			func(v *arkrpc.VTXO) {
				v.AncestryPaths = other.AncestryPaths
			},
		},
		{
			"different outpoint",
			func(v *arkrpc.VTXO) {
				v.Outpoint = other.Outpoint
			},
		},
		{
			"wrong index",
			func(v *arkrpc.VTXO) {
				v.Outpoint.Vout = 1
			},
		},
		{
			"wrong value",
			func(v *arkrpc.VTXO) {
				v.ValueSat++
			},
		},
		{
			"wrong script",
			func(v *arkrpc.VTXO) {
				v.PkScript = []byte{
					0x52,
				}
			},
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			bad, ok := proto.Clone(original).(*arkrpc.VTXO)
			require.True(t, ok)
			mutation.apply(bad)
			_, err := IndexedAncestryFromRPC(bad)
			require.ErrorIs(t, err, ErrInvalidBatchExpiryEvidence)
		})
	}
}

// TestIndexedAncestryMultiParentExpiry verifies the authenticated graph retains
// every contributing batch and selects the earliest actual confirmation+delay.
func TestIndexedAncestryMultiParentExpiry(t *testing.T) {
	t.Parallel()
	candidate, commits := expiryfixture.Merge(t, false)
	ancestry, err := IndexedAncestryFromRPC(candidate)
	require.NoError(t, err)
	require.Len(t, ancestry, 2)
	lookup := make(map[chainhash.Hash]CommitmentConfirmation)
	for i, tx := range commits {
		lookup[tx.TxHash()] = CommitmentConfirmation{
			Tx:          tx,
			BlockHeight: int32(100 + 20*i),
		}
	}
	expiry, err := AuthenticateBatchExpiry(t.Context(), ancestry,
		func(_ context.Context, txid chainhash.Hash, _ []byte,
			_ uint32) (CommitmentConfirmation, error) {

			return lookup[txid], nil
		})
	require.NoError(t, err)
	require.Equal(t, int32(140), expiry)

	// Removing the earlier-expiring parent's path cannot make the later
	// parent's otherwise valid evidence authorize the complete target.
	candidate.AncestryPaths = candidate.AncestryPaths[:1]
	_, err = IndexedAncestryFromRPC(candidate)
	require.ErrorIs(t, err, ErrInvalidBatchExpiryEvidence)
	require.ErrorContains(t, err, "no authenticated parent")
}

// TestIndexedAncestryRejectsForgedLeafBody checks that matching a reconstructed
// target txid is insufficient when its signed parent never authorized the leaf.
func TestIndexedAncestryRejectsForgedLeafBody(t *testing.T) {
	t.Parallel()
	candidate, _ := expiryfixture.Round(t, 10000, []byte{0x51}, 50, 100, 3)
	originalPath := candidate.AncestryPaths[0]
	path, err := arkrpc.AncestryPathToTree(originalPath)
	require.NoError(t, err)
	path.Root.Outputs[0].PkScript = []byte{0x52}
	forged, err := arkrpc.AncestryPathFromTree(
		path, path.BatchOutpoint.Hash, nil,
	)
	require.NoError(t, err)
	forged.CommitmentSweepKey = originalPath.CommitmentSweepKey
	forged.CommitmentSweepDelay = originalPath.CommitmentSweepDelay
	candidate.AncestryPaths = []*arkrpc.AncestryPath{forged}
	hash, err := path.Root.TXID()
	require.NoError(t, err)
	candidate.Outpoint.Txid = hash[:]
	candidate.PkScript = []byte{0x52}
	_, err = IndexedAncestryFromRPC(candidate)
	require.ErrorIs(t, err, ErrInvalidBatchExpiryEvidence)
	require.ErrorContains(t, err, "proof transaction")
}
