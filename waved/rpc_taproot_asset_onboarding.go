package waved

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const ()

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
	})
	if err != nil {
		return err
	}
	r.server.cfg.TaprootAssetOnboarder = onboarder
	r.server.taprootAssetWallet = wallet
	r.server.taprootAssetStore = store

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
		req.GetMaxFeeSat() == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "idempotency key, asset "+
				"ref, amount, and maximum fee are required",
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
	if terms == nil || terms.PubKey == nil ||
		terms.BoardingExitDelay == 0 {
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

	// The anchor cannot confirm below the current height, so it seeds
	// the boarding confirmation watch after a restart or a late replay.
	heightHint, err := r.currentBlockHeight(ctx)
	if err != nil {
		return nil, err
	}

	// Without an explicit proof the daemon selects and exports the proofs
	// of its own tapd UTXOs, which must together cover the amount. A
	// replay must reuse the originally resolved proofs instead: the
	// onboarding itself spends the source UTXOs, so re-resolving after
	// success finds nothing and would break idempotency.
	var proofFiles [][]byte
	if explicit := req.GetInputProofFile(); len(explicit) != 0 {
		proofFiles = [][]byte{append([]byte(nil), explicit...)}
	} else {
		stored, _, loadErr := r.server.loadTaprootAssetBoardRequest(
			ctx, req.GetIdempotencyKey(),
		)
		switch {
		case loadErr == nil:
			// A slice written before multi-UTXO funding carries
			// its single proof in the singular field.
			proofFiles = stored.ProofFiles
			if len(proofFiles) == 0 &&
				len(stored.ProofFile) != 0 {

				proofFiles = [][]byte{stored.ProofFile}
			}

		case errors.Is(loadErr, tapassets.ErrStoreNotFound):
			proofFiles, err = tapassets.ResolveOwnedAssetProofs(
				ctx, r.server.taprootAssetWallet,
				req.GetAssetRef(), req.GetAssetAmount(),
			)
			if err != nil {
				return nil, status.Errorf(
					codes.FailedPrecondition, "resolve "+
						"owned asset proofs: %v", err)
			}

		default:
			return nil, status.Errorf(codes.Internal, "load "+
				"boarding replay request: %v", loadErr)
		}
	}

	// The onboarded output is spent as a round boarding input, so it
	// carries the boarding exit delay; AssetBoardingDisclosure replays
	// the onboarding under the same delay when the boarding completes.
	onboardingRequest := &tapassets.OnboardingRequest{
		RequestID:          req.GetIdempotencyKey(),
		AssetRef:           req.GetAssetRef(),
		AssetAmount:        req.GetAssetAmount(),
		ProofFiles:         proofFiles,
		CarrierValueSat:    carrierValue,
		FeeRateSatPerVByte: req.GetFeeRateSatPerVbyte(),
		TargetConf:         req.GetTargetConf(),
		MaxFeeSat:          req.GetMaxFeeSat(),
		OperatorKey:        terms.PubKey,
		ExitDelay:          terms.BoardingExitDelay,
	}
	result, err := onboarder.Onboard(ctx, onboardingRequest)
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

	// Persist the replay slice so BoardTaprootAsset can rebuild the
	// disclosure from the idempotency key alone. The onboarding is
	// idempotent, so a retry after a persist failure lands here again.
	err = r.server.storeTaprootAssetBoardRequest(
		ctx, onboardingRequest, uint32(heightHint),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "persist boarding "+
			"replay request: %v", err)
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
