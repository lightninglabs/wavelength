package waved

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newAssetGuardVTXO returns a live asset-bearing VTXO descriptor. Round,
// leave, and sweep flows carry no asset transition, so every enumeration
// feeding them must exclude such descriptors.
func newAssetGuardVTXO(t *testing.T, hashByte byte) *vtxo.Descriptor {
	t.Helper()

	desc := newRefreshEstimateVTXO(t, hashByte, 100_000, 1_000)
	root := chainhash.Hash{0xAA}
	desc.TaprootAssetRoot = &root
	desc.TaprootAssetRef = "test-asset-ref"
	desc.TaprootAssetAmount = 1_000

	return desc
}

// TestRefreshAllSkipsAssetVTXOs ensures selection=all never routes an
// asset-bearing VTXO into a refresh round.
func TestRefreshAllSkipsAssetVTXOs(t *testing.T) {
	t.Parallel()

	const height = int32(900)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	btcDesc := newRefreshEstimateVTXO(t, 0x01, 100_000, 1_000)
	assetDesc := newAssetGuardVTXO(t, 0x02)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), btcDesc))
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), assetDesc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, []string{outpointStr(btcDesc)}, resp.QueuedOutpoints,
	)
}

// TestRefreshExplicitAcceptsAssetVTXO ensures a directly named
// asset-bearing VTXO enters the refresh preview: the wallet reissues
// its units through the round's asset transition, so the old blanket
// rejection no longer applies.
func TestRefreshExplicitAcceptsAssetVTXO(t *testing.T) {
	t.Parallel()

	const height = int32(900)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	assetDesc := newAssetGuardVTXO(t, 0x03)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), assetDesc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						outpointStr(assetDesc),
					},
				},
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

// TestLeaveAllSkipsAssetVTXOs ensures selection=all never routes an
// asset-bearing VTXO into a leave (offboard) round.
func TestLeaveAllSkipsAssetVTXOs(t *testing.T) {
	t.Parallel()

	const height = int32(900)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	btcDesc := newRefreshEstimateVTXO(t, 0x04, 100_000, 1_000)
	assetDesc := newAssetGuardVTXO(t, 0x05)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), btcDesc))
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), assetDesc))

	resp, err := r.LeaveVTXOs(
		t.Context(), &waverpc.LeaveVTXOsRequest{
			Selection: &waverpc.LeaveVTXOsRequest_All{
				All: true,
			},
			DefaultDestination: &waverpc.LeaveDestination{
				Target: &waverpc.LeaveDestination_PkScript{
					PkScript: validP2TRPkScript(0xBB),
				},
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, []string{outpointStr(btcDesc)}, resp.QueuedOutpoints,
	)
}

// TestLeaveExplicitRejectsAssetVTXO ensures a directly named
// asset-bearing VTXO cannot be left.
func TestLeaveExplicitRejectsAssetVTXO(t *testing.T) {
	t.Parallel()

	const height = int32(900)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	assetDesc := newAssetGuardVTXO(t, 0x06)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), assetDesc))

	_, err := r.LeaveVTXOs(
		t.Context(), &waverpc.LeaveVTXOsRequest{
			Selection: &waverpc.LeaveVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						outpointStr(assetDesc),
					},
				},
			},
			DefaultDestination: &waverpc.LeaveDestination{
				Target: &waverpc.LeaveDestination_PkScript{
					PkScript: validP2TRPkScript(0xBB),
				},
			},
			DryRun: true,
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "cannot be left")
}

// TestSendOnChainSweepAllSkipsAssetVTXOs ensures sweep_all over a wallet
// holding only asset-bearing VTXOs fails cleanly instead of consuming
// them.
func TestSendOnChainSweepAllSkipsAssetVTXOs(t *testing.T) {
	t.Parallel()

	const height = int32(900)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	svc := &fakeArkService{
		responseFn: scaledEstimateFn,
		getInfoResponse: &arkrpc.GetInfoResponse{
			Pubkey: operatorPriv.
				PubKey().
				SerializeCompressed(),
			VtxoExitDelay: 144,
			DustLimit:     1000,
		},
	}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	assetDesc := newAssetGuardVTXO(t, 0x07)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), assetDesc))

	_, err = r.SendOnChain(
		t.Context(), &waverpc.SendOnChainRequest{
			Destination: &waverpc.LeaveDestination{
				Target: &waverpc.LeaveDestination_PkScript{
					PkScript: validP2TRPkScript(0xBB),
				},
			},
			Amount: &waverpc.SendOnChainRequest_SweepAll{
				SweepAll: true,
			},
		},
	)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "no sweepable live VTXOs")
}
