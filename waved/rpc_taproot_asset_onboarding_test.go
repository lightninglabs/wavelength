package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestOnboardTaprootAssetPendingThenReady proves the RPC does not persist a
// pending output, then atomically adopts and activates the exact ready result.
// A later retry treats the duplicate descriptor as idempotent.
func TestOnboardTaprootAssetPendingThenReady(t *testing.T) {
	t.Parallel()

	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	root := chainhash.HashH([]byte("onboarding-asset-root"))
	policy, err := arkscript.NewVTXOPolicy(
		owner.PubKey(), operator.PubKey(), 144,
	)
	require.NoError(t, err)
	policyBytes, err := policy.Template.Encode()
	require.NoError(t, err)
	descTemplate := &vtxo.Descriptor{
		PolicyTemplate:   policyBytes,
		TaprootAssetRoot: &root,
	}
	pkScript, err := descTemplate.EffectivePkScript()
	require.NoError(t, err)
	txid := chainhash.HashH([]byte("onboarding-anchor"))
	outpoint := wire.OutPoint{Hash: txid, Index: 0}
	ownerKey := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: types.VTXOOwnerKeyFamily,
			Index:  9,
		},
		PubKey: owner.PubKey(),
	}
	ready := &tapassets.OnboardingResult{
		Status:             tapassets.OnboardingStatusReady,
		Outpoint:           outpoint,
		ValueSat:           1_000,
		ActualFeeSat:       125,
		PolicyTemplate:     policyBytes,
		PkScript:           pkScript,
		TaprootAssetRoot:   root,
		OwnerKey:           ownerKey,
		OperatorKey:        operator.PubKey(),
		ExitDelay:          144,
		ConfirmationHeight: 321,
	}
	pending := *ready
	pending.Status = tapassets.OnboardingStatusPendingConfirmation
	pending.ConfirmationHeight = 0
	onboarder := &testTaprootAssetOnboarder{
		results: []*tapassets.OnboardingResult{
			&pending,
			ready,
			ready,
		},
	}
	vtxoStore, _, _ := newSendOORTestStores(t)

	materialized := make(chan *vtxo.Descriptor, 2)
	managerBehavior := actor.NewFunctionBehavior(
		func(_ context.Context,
			msg vtxo.ManagerMsg) fn.Result[vtxo.ManagerResp] {

			notification, ok :=
				msg.(*vtxo.VTXOsMaterializedNotification)
			if !ok || len(notification.VTXOs) != 1 {
				return fn.Err[vtxo.ManagerResp](
					errors.New(
						"unexpected manager message",
					),
				)
			}
			materialized <- notification.VTXOs[0]

			return fn.Ok[vtxo.ManagerResp](
				&vtxo.VTXOsMaterializedResp{},
			)
		},
	)
	manager := actor.NewActor(actor.ActorConfig[
		vtxo.ManagerMsg, vtxo.ManagerResp,
	]{
		ID:          "taproot-asset-onboarding-test-manager",
		Behavior:    managerBehavior,
		MailboxSize: 2,
	})
	manager.Start()
	t.Cleanup(manager.Stop)

	cfg := DefaultConfig()
	cfg.TaprootAssetOnboarder = onboarder
	server := &Server{
		cfg:         cfg,
		vtxoStore:   vtxoStore,
		vtxoMgrRef:  fn.Some(manager.Ref()),
		walletReady: make(chan struct{}),
	}
	server.walletState.Store(int32(WalletStateReady))
	server.operatorTerms.Store(&types.OperatorTerms{
		PubKey:        operator.PubKey(),
		VTXOExitDelay: 144,
		MinVTXOAmount: 1_000,
	})
	rpcServer := &RPCServer{server: server}
	request := &waverpc.OnboardTaprootAssetRequest{
		IdempotencyKey: "onboarding-id",
		AssetRef: "asset:00000000000000000000000000000000" +
			"00000000000000000000000000000001",
		AssetAmount:        21,
		InputProofFile:     []byte("proof"),
		MaxFeeSat:          250,
		FeeRateSatPerVbyte: 2,
	}

	response, err := rpcServer.OnboardTaprootAsset(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, assetOnboardingPending, response.State)
	_, err = vtxoStore.GetVTXO(t.Context(), outpoint)
	require.ErrorIs(t, err, vtxo.ErrVTXONotFound)

	response, err = rpcServer.OnboardTaprootAsset(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, assetOnboardingReady, response.State)
	require.Equal(t, outpoint.String(), response.Outpoint)
	require.Equal(t, int32(321), response.ConfirmationHeight)
	require.Equal(t, int64(1_000), response.ValueSat)
	require.Equal(t, uint64(125), response.ActualFeeSat)
	stored, err := vtxoStore.GetVTXO(t.Context(), outpoint)
	require.NoError(t, err)
	require.True(t, sameOnboardedVTXO(stored, <-materialized))
	require.Empty(t, stored.Ancestry)
	require.Equal(t, txid, stored.CommitmentTxID)

	response, err = rpcServer.OnboardTaprootAsset(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, assetOnboardingReady, response.State)
	require.True(t, sameOnboardedVTXO(stored, <-materialized))
	require.Len(t, onboarder.requests, 3)
	for _, captured := range onboarder.requests {
		require.Equal(t, operator.PubKey(), captured.OperatorKey)
		require.Equal(t, uint32(144), captured.ExitDelay)
		require.Equal(t, uint64(1_000), captured.CarrierValueSat)
		require.Equal(t, uint64(2), captured.FeeRateSatPerVByte)
		require.Zero(t, captured.TargetConf)
		require.Equal(t, uint64(250), captured.MaxFeeSat)
	}
}

// TestOnboardTaprootAssetValidatesCarrierAndFeePolicy rejects economic
// parameters before an onboarding service can reserve or commit anything.
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
		PubKey:        operator.PubKey(),
		VTXOExitDelay: 144,
		MinVTXOAmount: 1_000,
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

// TestRegisterTaprootAssetOnboarding maps only the operator's confirmation
// precondition to the durable pending state and preserves raw outpoint bytes.
func TestRegisterTaprootAssetOnboarding(t *testing.T) {
	t.Parallel()

	client := &stubArkServiceClient{
		registerErr: status.Error(
			codes.FailedPrecondition, "needs confirmations",
		),
	}
	server := &Server{arkClient: client}
	registration := tapassets.OnboardingRegistration{
		TransferPackage: []byte("package"),
		FinalAnchorPSBT: []byte("psbt"),
		PolicyTemplate:  []byte("policy"),
		TaprootAssetRoot: tapsdk.Hash(
			chainhash.HashH(
				[]byte("registration-root"),
			),
		),
	}
	_, err := server.registerTaprootAssetOnboarding(
		t.Context(), registration,
	)
	require.ErrorIs(t, err, tapassets.ErrOnboardingPendingConfirmation)

	txid := chainhash.HashH([]byte("registered-anchor"))
	client.registerErr = nil
	client.registerResp = &arkrpc.RegisterTaprootAssetVTXOResponse{
		Txid:               txid[:],
		OutputIndex:        2,
		ConfirmationHeight: 999,
	}
	result, err := server.registerTaprootAssetOnboarding(
		t.Context(), registration,
	)
	require.NoError(t, err)
	require.Equal(t, wire.OutPoint{Hash: txid, Index: 2}, result.Outpoint)
	require.Equal(t, int32(999), result.ConfirmationHeight)
}

type testTaprootAssetOnboarder struct {
	requests []*tapassets.OnboardingRequest
	results  []*tapassets.OnboardingResult
}

func (o *testTaprootAssetOnboarder) Onboard(_ context.Context,
	request *tapassets.OnboardingRequest) (*tapassets.OnboardingResult,
	error) {

	clone := *request
	clone.ProofFile = append([]byte(nil), request.ProofFile...)
	o.requests = append(o.requests, &clone)
	if len(o.results) == 0 {
		return nil, errors.New("no onboarding result")
	}
	result := o.results[0]
	o.results = o.results[1:]

	return result, nil
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
