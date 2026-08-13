package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestOnboardTaprootAssetValidatesCarrierAndFeePolicy proves the RPC
// rejects a carrier below the operator minimum and an ambiguous fee
// choice before reaching the onboarding service.
func TestOnboardTaprootAssetValidatesCarrierAndFeePolicy(t *testing.T) {
	t.Parallel()

	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	onboarder := &testTaprootAssetOnboarder{}
	cfg := DefaultConfig()
	cfg.TaprootAssetOnboarder = onboarder
	server := &Server{cfg: cfg, walletReady: make(chan struct{})}
	server.walletState.Store(int32(WalletStateReady))
	server.operatorTerms.Store(&types.OperatorTerms{
		PubKey:            operator.PubKey(),
		BoardingExitDelay: 144,
		MinVTXOAmount:     1_000,
	})
	rpcServer := &RPCServer{server: server}
	request := &waverpc.OnboardTaprootAssetRequest{
		IdempotencyKey:     "onboarding-id",
		AssetRef:           "asset-ref",
		AssetAmount:        21,
		InputProofFile:     []byte("proof"),
		MaxFeeSat:          250,
		CarrierValueSat:    999,
		FeeRateSatPerVbyte: 2,
	}

	_, err = rpcServer.OnboardTaprootAsset(t.Context(), request)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "below operator minimum")
	require.Empty(t, onboarder.requests)

	request.CarrierValueSat = 1_000
	request.TargetConf = 6
	_, err = rpcServer.OnboardTaprootAsset(t.Context(), request)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "exactly one")
	require.Empty(t, onboarder.requests)
}

// TestOnboardTaprootAssetRequiresReadyFeature covers the public fail-closed
// checks before the orchestration service can make an external call.
func TestOnboardTaprootAssetRequiresReadyFeature(t *testing.T) {
	t.Parallel()

	request := &waverpc.OnboardTaprootAssetRequest{
		IdempotencyKey: "id",
		AssetRef:       "asset-ref",
		AssetAmount:    1,
		InputProofFile: []byte("proof"),
		MaxFeeSat:      1,
		TargetConf:     6,
	}
	cfg := DefaultConfig()
	server := &Server{cfg: cfg, walletReady: make(chan struct{})}
	rpcServer := &RPCServer{server: server}

	_, err := rpcServer.OnboardTaprootAsset(t.Context(), request)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	server.walletState.Store(int32(WalletStateReady))
	_, err = rpcServer.OnboardTaprootAsset(t.Context(), request)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = rpcServer.OnboardTaprootAsset(
		t.Context(), &waverpc.OnboardTaprootAssetRequest{},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestSignTaprootAssetOnboardingAnchor exercises the exact SignPsbt then
// FinalizePsbt boundary used with the LND wallet shared by tapd.
func TestSignTaprootAssetOnboardingAnchor(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, nil, nil))
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	raw, err := psbtutil.Serialize(packet)
	require.NoError(t, err)
	walletKit := &testOnboardingWalletKit{}

	finalized, err := signTaprootAssetOnboardingAnchor(
		t.Context(), walletKit, raw,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"sign", "finalize"}, walletKit.calls)
	require.Equal(t, raw, finalized)

	walletKit.signErr = errors.New("sign failed")
	_, err = signTaprootAssetOnboardingAnchor(
		t.Context(), walletKit, raw,
	)
	require.ErrorContains(t, err, "sign failed")
}

// testTaprootAssetOnboarder records the requests the RPC forwards so a
// test can assert that validation rejected one before it got that far.
type testTaprootAssetOnboarder struct {
	requests []*tapassets.OnboardingRequest
	result   *tapassets.OnboardingResult
	err      error
}

func (o *testTaprootAssetOnboarder) Onboard(_ context.Context,
	request *tapassets.OnboardingRequest) (*tapassets.OnboardingResult,
	error) {

	o.requests = append(o.requests, request)

	return o.result, o.err
}

type testOnboardingWalletKit struct {
	calls    []string
	signErr  error
	finalErr error
}

func (w *testOnboardingWalletKit) SignPsbt(_ context.Context,
	packet *psbt.Packet) (*psbt.Packet, error) {

	w.calls = append(w.calls, "sign")
	if w.signErr != nil {
		return nil, w.signErr
	}

	return packet, nil
}

func (w *testOnboardingWalletKit) FinalizePsbt(_ context.Context,
	packet *psbt.Packet, _ string) (*psbt.Packet, *wire.MsgTx, error) {

	w.calls = append(w.calls, "finalize")
	if w.finalErr != nil {
		return nil, nil, w.finalErr
	}

	return packet, packet.UnsignedTx, nil
}

var _ TaprootAssetOnboardingService = (*testTaprootAssetOnboarder)(nil)
var _ onboardingWalletKit = (*testOnboardingWalletKit)(nil)
