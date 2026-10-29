package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// testExpiryStatusUnknown keeps expiry status assertions readable.
const testExpiryStatusUnknown = waverpc.
	VTXOExpiryStatus_VTXO_EXPIRY_STATUS_UNKNOWN

// TestIndexedPolicyExitDelayUsesStandardPolicy verifies only a standard
// policy supplies the exit delay after the RPC has bound the queried script.
func TestIndexedPolicyExitDelayUsesStandardPolicy(t *testing.T) {
	t.Parallel()

	owner := testKeyDescriptor(t, 12)
	operator := testKeyDescriptor(t, 13)
	raw, _, err := arkscript.EncodeStandardVTXOArtifacts(
		owner.PubKey, operator.PubKey, 144,
	)
	require.NoError(t, err)
	policy, err := arkscript.DecodePolicyTemplate(raw)
	require.NoError(t, err)
	delay := indexedPolicyExitDelay(policy)
	require.Equal(t, uint32(144), delay)
	require.Zero(t, indexedPolicyExitDelay(nil))
}

// TestExpiryInfoFromDescriptorUsesDefaultThresholds verifies that local VTXO
// descriptors are classified with the wallet's dynamic expiry thresholds.
func TestExpiryInfoFromDescriptorUsesDefaultThresholds(t *testing.T) {
	t.Parallel()

	desc := &vtxo.Descriptor{
		BatchExpiry:    1000,
		RelativeExpiry: 144,
		Ancestry: []vtxo.Ancestry{
			{
				TreeDepth: 1,
			},
			{
				TreeDepth: 3,
			},
		},
		ChainDepth: 2,
	}

	info := expiryInfoFromDescriptor(
		desc, 784, vtxo.DefaultExpiryConfig(),
	)

	require.Equal(
		t, waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH,
		info.GetStatus(),
	)
	require.Equal(t, int32(784), info.GetCurrentHeight())
	require.Equal(t, int32(1000), info.GetBatchExpiry())
	require.Equal(t, int32(216), info.GetBlocksRemaining())
	require.Equal(t, int32(246), info.GetRefreshThresholdBlocks())
	require.Equal(t, int32(174), info.GetCriticalThresholdBlocks())
	require.Equal(t, uint32(144), info.GetRelativeExpiry())
	require.Equal(t, uint32(3), info.GetMaxTreeDepth())
	require.Equal(t, uint32(2), info.GetChainDepth())
}

// TestExpiryInfoUsesFreeRefreshWindow verifies RPC expiry posture reports the
// same safe delayed boundary used by the live VTXO manager.
func TestExpiryInfoUsesFreeRefreshWindow(t *testing.T) {
	t.Parallel()

	cfg := vtxo.DefaultExpiryConfig()
	cfg.FreeRefreshWindow = func() uint32 {
		return 120
	}

	info := expiryInfoFromTiming(
		1_000, 24, 2, 0, 880, cfg,
	)
	require.Equal(t, int32(120), info.GetRefreshThresholdBlocks())
	require.Equal(
		t, waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH,
		info.GetStatus(),
	)
}

// TestExpiryInfoFromTimingClassifiesPostures verifies the boundary conditions
// returned to RPC callers.
func TestExpiryInfoFromTimingClassifiesPostures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		currentHeight int32
		wantStatus    waverpc.VTXOExpiryStatus
	}{{
		name:          "safe",
		currentHeight: 700,
		wantStatus: waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_SAFE,
	}, {
		name:          "needs refresh",
		currentHeight: 784,
		wantStatus: waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH,
	}, {
		name:          "critical",
		currentHeight: 850,
		wantStatus: waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_CRITICAL,
	}, {
		name:          "expired",
		currentHeight: 1000,
		wantStatus: waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_EXPIRED,
	}, {
		name:          "unknown without height",
		currentHeight: 0,
		wantStatus:    testExpiryStatusUnknown,
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			info := expiryInfoFromTiming(
				1000, 144, 3, 0, test.currentHeight,
				vtxo.DefaultExpiryConfig(),
			)

			require.Equal(t, test.wantStatus, info.GetStatus())
		})
	}
}

// TestAuthenticatedIndexedExpiryIgnoresIndexerTiming verifies that a signed
// VTXO's locally confirmed batch expiry controls the daemon response even
// when the indexer advertises a much later deadline.
func TestAuthenticatedIndexedExpiryIgnoresIndexerTiming(t *testing.T) {
	t.Parallel()

	indexed, commitment := expiryfixture.Round(
		t, 10_000, []byte{0x51}, 200, 100, 1,
	)
	indexed.BatchExpiryHeight = 1_000
	indexed.RelativeExpiry = 24
	authenticate := func(ctx context.Context, ancestry []vtxo.Ancestry) (
		vtxo.AuthenticatedBatchEvidence, error) {

		return vtxo.AuthenticateBatchEvidence(
			ctx, ancestry,
			func(_ context.Context, txid chainhash.Hash, _ []byte,
				_ uint32) (vtxo.CommitmentConfirmation, error) {

				require.Equal(t, commitment.TxHash(), txid)

				return vtxo.CommitmentConfirmation{
					Tx: commitment, BlockHeight: 100,
				}, nil
			},
		)
	}
	verified, err := authenticateIndexedExpiry(
		t.Context(), indexed, authenticate,
	)
	require.NoError(t, err)
	response, err := indexedVTXOToProto(
		indexed, verified, 150, vtxo.DefaultExpiryConfig(),
	)
	require.NoError(t, err)
	require.Equal(t, int32(300), response.GetBatchExpiry())
	require.Equal(t, int32(300), response.GetExpiryInfo().GetBatchExpiry())
	require.Equal(t, uint32(1), response.GetExpiryInfo().GetMaxTreeDepth())
	require.Zero(t, response.GetRelativeExpiry())
	require.Equal(
		t, testExpiryStatusUnknown,
		response.GetExpiryInfo().GetStatus(),
	)
	require.Zero(t, response.GetExpiryInfo().GetCriticalThresholdBlocks())

	// A script-bound exit delay classifies the same deadline as critical.
	// The indexer's shorter batch relative expiry cannot make it safe.
	verified.exitDelay = 144
	response, err = indexedVTXOToProto(
		indexed, verified, 150, vtxo.DefaultExpiryConfig(),
	)
	require.NoError(t, err)
	require.Equal(t, uint32(144), response.GetRelativeExpiry())
	require.Equal(
		t, waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_CRITICAL,
		response.GetExpiryInfo().GetStatus(),
	)

	indexed.AncestryPaths[0].TreeDepth = 0
	_, err = authenticateIndexedExpiry(
		t.Context(), indexed, authenticate,
	)
	require.Error(t, err)

	_, err = authenticateIndexedExpiry(t.Context(), indexed, nil)
	require.ErrorIs(t, err, vtxo.ErrInvalidBatchExpiryEvidence)

	indexed.AncestryPaths[0].TreeDepth = 1
	_, err = authenticateIndexedExpiry(t.Context(), indexed, nil)
	require.ErrorContains(t, err, "authenticator not initialized")
	chainErr := errors.New("chain unavailable")
	verified, err = authenticateIndexedExpiry(
		t.Context(), indexed,
		func(context.Context, []vtxo.Ancestry) (
			vtxo.AuthenticatedBatchEvidence, error) {

			return vtxo.AuthenticatedBatchEvidence{}, chainErr
		},
	)
	require.NoError(t, err)
	require.Zero(t, verified.batchExpiry)
	require.Equal(t, uint32(1), verified.treeDepth)

	_, err = authenticateIndexedExpiry(
		t.Context(), indexed,
		func(context.Context, []vtxo.Ancestry) (
			vtxo.AuthenticatedBatchEvidence, error) {

			return vtxo.AuthenticatedBatchEvidence{},
				vtxo.ErrInvalidBatchExpiryEvidence
		},
	)
	require.ErrorIs(t, err, vtxo.ErrInvalidBatchExpiryEvidence)
}

// TestAuthenticatedIndexedExpiryCountsOORPackages verifies that an indexer
// cannot shorten the exit buffer by understating the OOR chain depth.
func TestAuthenticatedIndexedExpiryCountsOORPackages(t *testing.T) {
	t.Parallel()

	indexed, commitments := expiryfixture.Merge(t, false)
	indexed.BatchExpiryHeight = 1_000
	indexed.ChainDepth = 0
	authenticate := func(ctx context.Context, ancestry []vtxo.Ancestry) (
		vtxo.AuthenticatedBatchEvidence, error) {

		return vtxo.AuthenticateBatchEvidence(
			ctx, ancestry,
			func(_ context.Context, txid chainhash.Hash, _ []byte,
				_ uint32) (vtxo.CommitmentConfirmation, error) {

				var confirmed vtxo.CommitmentConfirmation
				for i, tx := range commitments {
					if tx.TxHash() == txid {
						confirmed.Tx = tx
						confirmed.BlockHeight = int32(
							100 + 20*i,
						)

						return confirmed, nil
					}
				}

				var missing vtxo.CommitmentConfirmation

				return missing, errors.New("unknown commitment")
			},
		)
	}
	verified, err := authenticateIndexedExpiry(
		t.Context(), indexed, authenticate,
	)
	require.NoError(t, err)
	require.Equal(t, int32(140), verified.batchExpiry)
	require.Equal(t, uint32(1), verified.chainDepth)
}

// TestExpiryInfoFromTimingIncludesChainDepth verifies the public posture uses
// the same OOR-hop budget as the live actor instead of merely echoing depth.
func TestExpiryInfoFromTimingIncludesChainDepth(t *testing.T) {
	t.Parallel()

	withoutOOR := expiryInfoFromTiming(
		1_000, 144, 3, 0, 700, vtxo.DefaultExpiryConfig(),
	)
	withOOR := expiryInfoFromTiming(
		1_000, 144, 3, 2, 700, vtxo.DefaultExpiryConfig(),
	)

	require.Equal(t, int32(162),
		withoutOOR.GetCriticalThresholdBlocks())
	require.Equal(t, int32(174),
		withOOR.GetCriticalThresholdBlocks())
	require.Greater(
		t, withOOR.GetRefreshThresholdBlocks(),
		withoutOOR.GetRefreshThresholdBlocks(),
	)
}

// TestIndexedExpiryStatusFilterDefaultsToLive verifies pkScript expiry lookups
// prefer the active generation when callers do not request historical statuses.
func TestIndexedExpiryStatusFilterDefaultsToLive(t *testing.T) {
	t.Parallel()

	statusFilter, err := indexedExpiryStatusFilter(nil)
	require.NoError(t, err)
	require.Equal(
		t,
		[]arkrpc.VTXOStatus{
			arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
		statusFilter,
	)

	statusFilter, err = indexedExpiryStatusFilter([]waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	})
	require.NoError(t, err)
	require.Equal(
		t,
		[]arkrpc.VTXOStatus{
			arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
		},
		statusFilter,
	)
}
