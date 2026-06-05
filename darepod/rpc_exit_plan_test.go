package darepod

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newReadyExitPlanRPCServer() *RPCServer {
	walletReady := make(chan struct{})
	close(walletReady)

	return &RPCServer{
		server: &Server{
			walletReady:  walletReady,
			chainParams:  &chaincfg.RegressionNetParams,
			chainBackend: nil,
		},
	}
}

func TestGetExitPlanRejectsInvalidOutpoint(t *testing.T) {
	t.Parallel()

	r := newReadyExitPlanRPCServer()
	_, err := r.GetExitPlan(t.Context(), &ExitPlanRequest{
		Outpoint: "not-an-outpoint",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSweepWalletRejectsMissingDestination(t *testing.T) {
	t.Parallel()

	r := newReadyExitPlanRPCServer()
	_, err := r.SweepWallet(t.Context(), &SweepWalletRequest{})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSweepWalletRejectsNegativeFeeRate(t *testing.T) {
	t.Parallel()

	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	r := newReadyExitPlanRPCServer()
	_, err = r.SweepWallet(t.Context(), &SweepWalletRequest{
		DestinationAddress: addr.String(),
		FeeRateSatPerVByte: -1,
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestExitPlanRejectsNoAncestry(t *testing.T) {
	t.Parallel()

	var outpoint wire.OutPoint
	outpoint.Index = 1

	_, err := exitPlanRequiredUTXOCount(
		outpoint, &vtxo.Descriptor{},
	)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "no unilateral-exit ancestry")

	required, err := exitPlanRequiredUTXOCount(
		outpoint, &vtxo.Descriptor{
			Ancestry: []vtxo.Ancestry{{}},
		},
	)
	require.NoError(t, err)
	require.Equal(t, uint32(1), required)
}

func TestExitPlanFundingAddressReusesCachedAddress(t *testing.T) {
	t.Parallel()

	s := &Server{
		exitPlanFundingAddresses: map[string]string{
			"txid:0": "bcrt1preallocated",
		},
	}

	address, err := s.exitPlanFundingAddress(
		t.Context(), "txid:0", true,
	)
	require.NoError(t, err)
	require.Equal(t, "bcrt1preallocated", address)

	address, err = s.exitPlanFundingAddress(t.Context(), "txid:1", false)
	require.NoError(t, err)
	require.Empty(t, address)
}

func TestCapWalletFeeRate(t *testing.T) {
	t.Parallel()

	r := &RPCServer{
		server: &Server{
			cfg: &Config{
				Unroll: &UnrollConfig{
					MaxFeeRateSatPerVByte: 25,
				},
			},
		},
	}

	require.Equal(t, int64(24), r.capWalletFeeRate(24))
	require.Equal(t, int64(25), r.capWalletFeeRate(250))
}

func TestWalletSweepPreviewNoInputsCannotBroadcast(t *testing.T) {
	t.Parallel()

	resp := walletSweepPreview(nil, []byte{txscript.OP_TRUE}, 2)
	require.False(t, resp.CanBroadcast)
	require.Zero(t, resp.TotalInputSat)
	require.Contains(t, resp.FailureReason, "no confirmed")
}

func TestWalletSweepPreviewDustNetMessage(t *testing.T) {
	t.Parallel()

	var hash [32]byte
	hash[0] = 2
	resp := walletSweepPreview([]*wallet.Utxo{{
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		PkScript:      []byte{0x00, 0x14},
		Amount:        txconfirm.DustLimit + 10,
		Confirmations: 1,
	}}, []byte{txscript.OP_TRUE}, 1)

	require.False(t, resp.CanBroadcast)
	require.Contains(t, resp.FailureReason, "dust")
}

func TestWalletSweepPreviewPositiveNetCanBroadcast(t *testing.T) {
	t.Parallel()

	var hash [32]byte
	hash[0] = 1
	resp := walletSweepPreview([]*wallet.Utxo{{
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		PkScript:      []byte{0x00, 0x14},
		Amount:        btcutil.Amount(50_000),
		Confirmations: 1,
	}}, []byte{txscript.OP_TRUE}, 2)

	require.True(t, resp.CanBroadcast, resp.FailureReason)
	require.Equal(t, int64(50_000), resp.TotalInputSat)
	require.Positive(t, resp.EstimatedFeeSat)
	require.Equal(
		t, resp.TotalInputSat-resp.EstimatedFeeSat, resp.NetAmountSat,
	)
}

func TestRecommendedUnrollUTXOAmountUsesFloor(t *testing.T) {
	t.Parallel()

	recommended, err := recommendedUnrollUTXOAmount(1)
	require.NoError(t, err)
	require.GreaterOrEqual(
		t, recommended, preflightUnrollMinUTXOSat,
	)
	require.GreaterOrEqual(
		t, recommended, txconfirm.DustLimit,
	)
}
