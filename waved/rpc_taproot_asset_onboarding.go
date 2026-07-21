package waved

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	assetOnboardingPending = waverpc.TaprootAssetOnboardingState_TAPROOT_ASSET_ONBOARDING_STATE_PENDING_CONFIRMATION //nolint:ll
	assetOnboardingReady   = waverpc.TaprootAssetOnboardingState_TAPROOT_ASSET_ONBOARDING_STATE_READY                //nolint:ll
)

// TaprootAssetOnboardingService is the programmatic client-side orchestration
// boundary. Only waved's optional tapd adapter implements it in production.
type TaprootAssetOnboardingService interface {
	Onboard(context.Context,
		*tapassets.OnboardingRequest) (
		*tapassets.OnboardingResult,
		error,
	)
}

// ConfigureTaprootAssetOnboarding installs the concrete tap-sdk workflow while
// keeping all Wavelength wallet, operator, and persistence dependencies inside
// the daemon. It is called by the optional tapd lifecycle registrar.
func (r *RPCServer) ConfigureTaprootAssetOnboarding(wallet *tapsdk.Wallet,
	store tapassets.Store) error {

	if r == nil || r.server == nil {
		return fmt.Errorf("daemon server unavailable")
	}
	onboarder, err := tapassets.NewOnboarder(tapassets.OnboarderConfig{
		Wallet: wallet,
		Store:  store,
		Signer: r.server.signTaprootAssetOnboardingAnchor,
		DeriveOwnerKey: func(ctx context.Context) (
			*keychain.KeyDescriptor, error) {

			if r.server.proofKeyBackend == nil {
				return nil, fmt.Errorf("wallet key backend " +
					"not initialized")
			}

			return r.server.proofKeyBackend.DeriveNextKey(
				ctx, types.VTXOOwnerKeyFamily,
			)
		},
		Registrar: r.server.registerTaprootAssetOnboarding,
	})
	if err != nil {
		return err
	}
	r.server.cfg.TaprootAssetOnboarder = onboarder

	return nil
}

// OnboardTaprootAsset implements the user-facing durable onboarding RPC.
func (r *RPCServer) OnboardTaprootAsset(ctx context.Context,
	req *waverpc.OnboardTaprootAssetRequest) (
	*waverpc.OnboardTaprootAssetResponse, error) {

	if r == nil || r.server == nil || r.server.cfg == nil {
		return nil, status.Error(
			codes.Unavailable, "daemon unavailable",
		)
	}
	if req == nil || req.GetIdempotencyKey() == "" ||
		req.GetAssetRef() == "" || req.GetAssetAmount() == 0 ||
		len(req.GetInputProofFile()) == 0 || req.GetMaxFeeSat() == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "idempotency key, asset "+
				"ref, amount, proof, and maximum fee are "+
				"required",
		)
	}
	if (req.GetFeeRateSatPerVbyte() == 0) ==
		(req.GetTargetConf() == 0) {
		return nil, status.Error(
			codes.InvalidArgument, "exactly one of fee rate "+
				"and confirmation target is required",
		)
	}
	if r.server.WalletLifecycleState() != WalletStateReady {
		return nil, status.Error(
			codes.FailedPrecondition, "wallet is not ready",
		)
	}
	onboarder := r.server.cfg.TaprootAssetOnboarder
	if onboarder == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"Taproot Asset onboarding is disabled",
		)
	}
	terms := r.server.loadOperatorTerms()
	if terms == nil || terms.PubKey == nil || terms.VTXOExitDelay == 0 {
		return nil, status.Error(
			codes.FailedPrecondition,
			"operator terms are not ready",
		)
	}
	minimumCarrier := terms.MinVTXOAmountFloor()
	if minimumCarrier <= 0 {
		return nil, status.Error(
			codes.FailedPrecondition,
			"operator returned an invalid minimum VTXO amount",
		)
	}
	carrierValue := req.GetCarrierValueSat()
	if carrierValue == 0 {
		carrierValue = uint64(minimumCarrier)
	}
	if carrierValue < uint64(minimumCarrier) {
		return nil, status.Errorf(codes.InvalidArgument, "carrier "+
			"value %d is below operator minimum %d", carrierValue,
			minimumCarrier)
	}
	if carrierValue > math.MaxInt64 {
		return nil, status.Error(
			codes.InvalidArgument,
			"carrier value exceeds the supported Bitcoin range",
		)
	}

	result, err := onboarder.Onboard(ctx, &tapassets.OnboardingRequest{
		RequestID:   req.GetIdempotencyKey(),
		AssetRef:    req.GetAssetRef(),
		AssetAmount: req.GetAssetAmount(),
		ProofFile: append(
			[]byte(nil), req.GetInputProofFile()...,
		),
		CarrierValueSat:    carrierValue,
		FeeRateSatPerVByte: req.GetFeeRateSatPerVbyte(),
		TargetConf:         req.GetTargetConf(),
		MaxFeeSat:          req.GetMaxFeeSat(),
		OperatorKey:        terms.PubKey,
		ExitDelay:          terms.VTXOExitDelay,
	})
	if errors.Is(err, tapassets.ErrReconciliationRequired) {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "onboard Taproot "+
			"Asset: %v", err)
	}
	if result == nil {
		return nil, status.Error(
			codes.Internal, "onboarding service returned no result",
		)
	}

	response := &waverpc.OnboardTaprootAssetResponse{
		Outpoint:     result.Outpoint.String(),
		ValueSat:     result.ValueSat,
		PkScript:     append([]byte(nil), result.PkScript...),
		ActualFeeSat: result.ActualFeeSat,
		TaprootAssetRoot: append(
			[]byte(nil), result.TaprootAssetRoot[:]...,
		),
	}
	switch result.Status {
	case tapassets.OnboardingStatusPendingConfirmation:
		response.State = assetOnboardingPending

	case tapassets.OnboardingStatusReady:
		desc, materializeErr := r.materializeTaprootAssetOnboarding(
			ctx, result,
		)
		if materializeErr != nil {
			return nil, status.Errorf(codes.Internal,
				"materialize onboarded Taproot Asset: %v",
				materializeErr)
		}
		response.State = assetOnboardingReady
		response.ConfirmationHeight = desc.CreatedHeight

	default:
		return nil, status.Error(
			codes.Internal,
			"onboarding service returned unknown state",
		)
	}

	return response, nil
}

type onboardingWalletKit interface {
	SignPsbt(context.Context, *psbt.Packet) (*psbt.Packet, error)

	FinalizePsbt(context.Context, *psbt.Packet, string) (*psbt.Packet,
		*wire.MsgTx, error)
}

func (s *Server) signTaprootAssetOnboardingAnchor(ctx context.Context,
	anchorPSBT []byte) ([]byte, error) {

	if s.cfg.Wallet.Type != WalletTypeLnd || !s.lnd.IsSome() {
		return nil, fmt.Errorf("Taproot Asset onboarding requires " +
			"the LND wallet backend shared with tapd")
	}

	return signTaprootAssetOnboardingAnchor(
		ctx, s.lnd.UnsafeFromSome().WalletKit, anchorPSBT,
	)
}

func signTaprootAssetOnboardingAnchor(ctx context.Context,
	walletKit onboardingWalletKit, anchorPSBT []byte) ([]byte, error) {

	if walletKit == nil {
		return nil, fmt.Errorf("LND WalletKit is required")
	}
	packet, err := psbtutil.Parse(anchorPSBT)
	if err != nil {
		return nil, err
	}
	signed, err := walletKit.SignPsbt(ctx, packet)
	if err != nil {
		return nil, fmt.Errorf("sign onboarding PSBT with LND: %w", err)
	}
	finalized, _, err := walletKit.FinalizePsbt(ctx, signed, "")
	if err != nil {
		return nil, fmt.Errorf("finalize onboarding PSBT with LND: %w",
			err)
	}

	return psbtutil.Serialize(finalized)
}

func (s *Server) registerTaprootAssetOnboarding(ctx context.Context,
	registration tapassets.OnboardingRegistration) (
	*tapassets.OnboardingRegistrationResult, error) {

	client := s.operatorArkClient()
	if client == nil {
		return nil, fmt.Errorf("operator connection not initialized")
	}
	response, err := client.RegisterTaprootAssetVTXO(
		ctx, &arkrpc.RegisterTaprootAssetVTXORequest{
			TransferPackage: append(
				[]byte(nil), registration.TransferPackage...,
			),
			FinalAnchorPsbt: append(
				[]byte(nil), registration.FinalAnchorPSBT...,
			),
			VtxoPolicyTemplate: append(
				[]byte(nil), registration.PolicyTemplate...,
			),
			TaprootAssetRoot: append(
				[]byte(nil),
				registration.TaprootAssetRoot[:]...,
			),
		},
	)
	if status.Code(err) == codes.FailedPrecondition {
		return nil, tapassets.ErrOnboardingPendingConfirmation
	}
	if err != nil {
		return nil, err
	}
	if response == nil || len(response.GetTxid()) != chainhash.HashSize {
		return nil, fmt.Errorf("operator returned invalid onboarding " +
			"outpoint")
	}
	var txid chainhash.Hash
	copy(txid[:], response.GetTxid())

	return &tapassets.OnboardingRegistrationResult{
		Outpoint: wire.OutPoint{
			Hash:  txid,
			Index: response.GetOutputIndex(),
		},
		ConfirmationHeight: response.GetConfirmationHeight(),
	}, nil
}

func (r *RPCServer) materializeTaprootAssetOnboarding(ctx context.Context,
	result *tapassets.OnboardingResult) (*vtxo.Descriptor, error) {

	if result == nil || result.Status != tapassets.OnboardingStatusReady ||
		result.ConfirmationHeight <= 0 {
		return nil, fmt.Errorf("ready onboarding result is required")
	}
	if r.server.vtxoStore == nil || !r.server.vtxoMgrRef.IsSome() {
		return nil, fmt.Errorf("VTXO runtime is not ready")
	}
	tapscript, err := arkscript.VTXOTapScript(
		result.OwnerKey.PubKey, result.OperatorKey, result.ExitDelay,
	)
	if err != nil {
		return nil, err
	}
	root := result.TaprootAssetRoot
	desc := &vtxo.Descriptor{
		Outpoint:         result.Outpoint,
		Amount:           btcutil.Amount(result.ValueSat),
		PolicyTemplate:   append([]byte(nil), result.PolicyTemplate...),
		PkScript:         append([]byte(nil), result.PkScript...),
		TaprootAssetRoot: &root,
		ClientKey:        result.OwnerKey,
		OperatorKey:      result.OperatorKey,
		TapScript:        tapscript,
		RoundID: "taproot-asset-onboarding-" +
			result.Outpoint.Hash.String(),
		CommitmentTxID: result.Outpoint.Hash,
		BatchExpiry:    math.MaxInt32,
		RelativeExpiry: result.ExitDelay,
		CreatedHeight:  result.ConfirmationHeight,
		Status:         vtxo.VTXOStatusLive,
	}
	if err := r.server.vtxoStore.SaveVTXO(ctx, desc); err != nil {
		existing, getErr := r.server.vtxoStore.GetVTXO(
			ctx, desc.Outpoint,
		)
		if getErr != nil || !sameOnboardedVTXO(existing, desc) {
			return nil, err
		}
		desc = existing
	}

	err = r.server.vtxoMgrRef.UnsafeFromSome().Tell(
		ctx, &vtxo.VTXOsMaterializedNotification{
			VTXOs: []*vtxo.Descriptor{desc},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("notify VTXO manager: %w", err)
	}

	return desc, nil
}

func sameOnboardedVTXO(left, right *vtxo.Descriptor) bool {
	if left == nil || right == nil || left.Outpoint != right.Outpoint ||
		left.Amount != right.Amount || left.TaprootAssetRoot == nil ||
		right.TaprootAssetRoot == nil ||
		*left.TaprootAssetRoot != *right.TaprootAssetRoot ||
		left.RelativeExpiry != right.RelativeExpiry ||
		left.CreatedHeight != right.CreatedHeight ||
		left.ClientKey.PubKey == nil || right.ClientKey.PubKey == nil ||
		left.OperatorKey == nil || right.OperatorKey == nil {
		return false
	}

	return bytes.Equal(left.PolicyTemplate, right.PolicyTemplate) &&
		bytes.Equal(left.PkScript, right.PkScript) &&
		left.ClientKey.KeyLocator == right.ClientKey.KeyLocator &&
		left.ClientKey.PubKey.IsEqual(right.ClientKey.PubKey) &&
		left.OperatorKey.IsEqual(right.OperatorKey)
}

var _ TaprootAssetOnboardingService = (*tapassets.Onboarder)(nil)
