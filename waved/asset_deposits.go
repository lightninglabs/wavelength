package waved

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// defaultAssetDepositTargetConf is the funding confirmation target used
	// when a deposit sets neither a fee rate nor a target.
	defaultAssetDepositTargetConf = 6

	// maxAssetDepositRequestID bounds a deposit's request ID.
	maxAssetDepositRequestID = 128

	// assetDepositPollInterval is how often published deposits are checked
	// for a confirmed proof.
	assetDepositPollInterval = 10 * time.Second

	// assetDepositPollTimeout bounds one pass over published deposits.
	assetDepositPollTimeout = time.Minute
)

// assetDepositTapd is the slice of the tapd client that selects and exports
// deposit proofs.
type assetDepositTapd interface {
	GetInfo(context.Context) (*tapsdk.Info, error)

	ListUtxos(context.Context,
		*tapsdk.ListUtxosRequest) (
		map[string]*tapsdk.ManagedUtxo,
		error,
	)

	ListTransfers(context.Context,
		*tapsdk.ListTransfersRequest) ([]*tapsdk.AssetTransfer, error)

	ExportProof(context.Context, tapsdk.AssetRef, tapsdk.PubKey,
		*tapsdk.Outpoint) (*tapsdk.ProofFile, error)
}

// assetOnboarder funds and publishes one deposit output.
type assetOnboarder interface {
	Onboard(context.Context,
		*tapassets.OnboardingRequest) (
		*tapassets.OnboardingResult,
		error,
	)
}

// boardingFeeQuoter returns the operator's boarding fee quote for an input of
// the given value.
type boardingFeeQuoter func(context.Context, btcutil.Amount) (btcutil.Amount,
	error)

// assetBoardingRegistrar registers a deposit with the next round.
type assetBoardingRegistrar func(context.Context,
	*round.RegisterAssetBoardingRequest) error

// assetDepositStore persists deposits.
type assetDepositStore interface {
	CreateAssetDeposit(context.Context,
		*db.AssetDepositRequest) (*db.AssetDeposit, error)

	GetAssetDeposit(context.Context, string) (*db.AssetDeposit, error)

	ListAssetDeposits(context.Context) ([]*db.AssetDeposit, error)

	ListAssetDepositsByStatus(context.Context,
		db.AssetDepositStatus) ([]*db.AssetDeposit, error)

	MarkAssetDepositPublished(context.Context, string,
		*db.AssetDepositOutput) error

	MarkAssetDepositConfirmed(context.Context, string, []byte, uint32) error

	SetAssetDepositError(context.Context, string, string) error

	AssetDepositRound(context.Context, string) (string, error)
}

// assetDepositService funds asset deposits from the tapd wallet and tracks
// them until their proofs confirm.
type assetDepositService struct {
	log       btclog.Logger
	store     assetDepositStore
	tapd      assetDepositTapd
	onboarder assetOnboarder

	// terms returns the operator terms the deposit policy commits to.
	terms func() *types.OperatorTerms

	// quoteBoardingFee sizes the round fee the deposit value must cover.
	quoteBoardingFee boardingFeeQuoter

	// chainParams derives deposit addresses for round boarding.
	chainParams *chaincfg.Params

	// registerBoarding hands a confirmed deposit to the round actor.
	registerBoarding assetBoardingRegistrar

	// mu serializes funding, because tapd wallet input selection must not
	// interleave between deposits.
	mu sync.Mutex

	// poll asks the watcher to check published deposits now.
	poll chan struct{}
}

// assetDepositConfig holds the dependencies of a deposit service.
type assetDepositConfig struct {
	log              btclog.Logger
	store            assetDepositStore
	tapd             assetDepositTapd
	onboarder        assetOnboarder
	terms            func() *types.OperatorTerms
	quoteBoardingFee boardingFeeQuoter
	chainParams      *chaincfg.Params
	registerBoarding assetBoardingRegistrar
}

// newAssetDepositService returns a deposit service over its dependencies.
func newAssetDepositService(cfg *assetDepositConfig) *assetDepositService {
	return &assetDepositService{
		log:              cfg.log,
		store:            cfg.store,
		tapd:             cfg.tapd,
		onboarder:        cfg.onboarder,
		terms:            cfg.terms,
		quoteBoardingFee: cfg.quoteBoardingFee,
		chainParams:      cfg.chainParams,
		registerBoarding: cfg.registerBoarding,
		poll:             make(chan struct{}, 1),
	}
}

// Deposit funds the requested deposit, or resumes the stored deposit with
// the same request ID. A deposit that published returns without funding
// again.
func (d *assetDepositService) Deposit(ctx context.Context,
	req *waverpc.DepositAssetRequest) (*db.AssetDeposit, error) {

	if err := validateDepositAssetRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	deposit, err := d.store.GetAssetDeposit(ctx, req.RequestId)
	switch {
	case errors.Is(err, db.ErrAssetDepositNotFound):
		deposit, err = d.create(ctx, req)
		if err != nil {
			return nil, err
		}

	case err != nil:
		return nil, status.Errorf(codes.Internal, "load asset "+
			"deposit: %v", err)

	case !depositMatchesRequest(deposit, req):
		return nil, status.Errorf(codes.AlreadyExists, "request_id %s "+
			"belongs to a different deposit", req.RequestId)
	}
	if deposit.Status != db.AssetDepositFunding {
		return deposit, nil
	}

	return d.fund(ctx, deposit)
}

// create sizes the deposit, selects its funding proofs and records the
// request.
func (d *assetDepositService) create(ctx context.Context,
	req *waverpc.DepositAssetRequest) (*db.AssetDeposit, error) {

	terms := d.terms()
	if terms == nil || terms.PubKey == nil || terms.BoardingExitDelay == 0 {
		return nil, status.Error(
			codes.Unavailable, "operator terms are not loaded",
		)
	}

	roundFee, err := d.quoteBoardingFee(ctx, 2*terms.MinVTXOAmountFloor())
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "quote "+
			"boarding fee: %v", err)
	}
	value := depositValue(terms, roundFee)

	proofs, err := tapassets.SelectOnboardingProofs(
		ctx, d.tapd, req.AssetRef, req.AssetAmount,
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "select "+
			"asset to deposit: %v", err)
	}

	deposit, err := d.store.CreateAssetDeposit(ctx, &db.AssetDepositRequest{
		RequestID:          req.RequestId,
		AssetRef:           req.AssetRef,
		AssetAmount:        req.AssetAmount,
		CarrierValueSat:    uint64(value),
		FeeRateSatPerVByte: req.SatPerVbyte,
		TargetConf:         depositTargetConf(req),
		MaxFeeSat:          req.MaxFeeSat,
		OperatorKey:        terms.PubKey,
		ExitDelay:          terms.BoardingExitDelay,
		FundingProofs:      proofs,
	})
	if errors.Is(err, db.ErrAssetDepositMismatch) {
		return nil, status.Errorf(codes.AlreadyExists, "request_id %s "+
			"belongs to a different deposit", req.RequestId)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "record asset "+
			"deposit: %v", err)
	}

	return deposit, nil
}

// fund runs the onboarding of a funding deposit and records its output. The
// onboarding journal makes a retry of an interrupted attempt replay it rather
// than fund twice.
func (d *assetDepositService) fund(ctx context.Context,
	deposit *db.AssetDeposit) (*db.AssetDeposit, error) {

	req := &deposit.Request
	result, err := d.onboarder.Onboard(ctx, &tapassets.OnboardingRequest{
		RequestID:          req.RequestID,
		AssetRef:           req.AssetRef,
		AssetAmount:        req.AssetAmount,
		ProofFiles:         req.FundingProofs,
		CarrierValueSat:    req.CarrierValueSat,
		FeeRateSatPerVByte: req.FeeRateSatPerVByte,
		TargetConf:         req.TargetConf,
		MaxFeeSat:          req.MaxFeeSat,
		OperatorKey:        req.OperatorKey,
		ExitDelay:          req.ExitDelay,
	})
	if err != nil {
		d.recordError(ctx, req.RequestID, err)

		code := codes.Internal
		if errors.Is(err, tapassets.ErrReconciliationRequired) {
			code = codes.Unavailable
		}

		return nil, status.Errorf(code, "fund asset deposit %s: %v",
			req.RequestID, err)
	}

	output := &db.AssetDepositOutput{
		Outpoint:         result.Outpoint,
		ValueSat:         result.ValueSat,
		FeeSat:           result.ActualFeeSat,
		PkScript:         result.PkScript,
		PolicyTemplate:   result.PolicyTemplate,
		OwnerKey:         result.OwnerKey,
		Digest:           result.Digest,
		ScriptKey:        bytes.Clone(result.ScriptKey[:]),
		TaprootAssetRoot: result.TaprootAssetRoot,
		OPTrueWitness:    result.OPTrueWitness,
	}
	err = d.store.MarkAssetDepositPublished(ctx, req.RequestID, output)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "record published "+
			"asset deposit %s: %v", req.RequestID, err)
	}
	d.requestPoll()

	published, err := d.store.GetAssetDeposit(ctx, req.RequestID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load asset "+
			"deposit: %v", err)
	}

	return published, nil
}

// Board joins the next round with a confirmed deposit. The asset VTXO gets the
// operator's minimum VTXO amount, and the BTC VTXO targets the rest of the
// deposit's value after the quoted round fee; the operator's quote sets its
// final value. Board takes no lock, because the round actor refuses a deposit
// that a tracked round holds in the turn that registers it.
func (d *assetDepositService) Board(ctx context.Context, requestID string) (
	*db.AssetDeposit, error) {

	if requestID == "" {
		return nil, status.Error(
			codes.InvalidArgument, "request_id is required",
		)
	}

	deposit, err := d.store.GetAssetDeposit(ctx, requestID)
	if errors.Is(err, db.ErrAssetDepositNotFound) {
		return nil, status.Errorf(codes.NotFound, "asset deposit %s "+
			"not found", requestID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load asset "+
			"deposit: %v", err)
	}
	if deposit.Status != db.AssetDepositConfirmed {
		return nil, status.Errorf(codes.FailedPrecondition, "asset "+
			"deposit %s is %s, not confirmed", requestID,
			deposit.Status)
	}
	roundID, err := d.store.AssetDepositRound(ctx, requestID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load asset deposit "+
			"round: %v", err)
	}
	if roundID != "" {
		return nil, status.Errorf(codes.AlreadyExists, "asset deposit "+
			"%s is boarded by round %s", requestID, roundID)
	}

	terms := d.terms()
	if terms == nil {
		return nil, status.Error(
			codes.Unavailable, "operator terms are not loaded",
		)
	}
	value := btcutil.Amount(deposit.Output.ValueSat)
	roundFee, err := d.quoteBoardingFee(ctx, value)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "quote "+
			"boarding fee: %v", err)
	}
	assetSat, changeSat := boardingAmounts(terms, value, roundFee)
	if changeSat < terms.MinVTXOAmountFloor() {
		return nil, status.Errorf(codes.FailedPrecondition, "asset "+
			"deposit of %v cannot fund a %v asset VTXO, the round "+
			"fee and a BTC VTXO", value, assetSat)
	}

	boarding, err := deposit.RoundBoarding()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	intent, err := round.NewAssetBoardingIntent(boarding, d.chainParams)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "asset deposit "+
			"%s: %v", requestID, err)
	}

	err = d.registerBoarding(ctx, &round.RegisterAssetBoardingRequest{
		Boarding:        intent,
		AssetVTXOAmount: assetSat,
		ChangeAmount:    changeSat,
	})
	if errors.Is(err, round.ErrBoardingInFlight) {
		return nil, status.Errorf(codes.AlreadyExists, "asset deposit "+
			"%s is already in a round", requestID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "register asset "+
			"deposit %s: %v", requestID, err)
	}
	d.log.InfoS(ctx, "Asset deposit registered for boarding",
		slog.String("request_id", requestID),
		slog.Int64("asset_vtxo_sat", int64(assetSat)),
		slog.Int64("change_sat", int64(changeSat)),
	)

	return deposit, nil
}

// Round returns the round that spends a deposit, or an empty ID.
func (d *assetDepositService) Round(ctx context.Context, requestID string) (
	string, error) {

	return d.store.AssetDepositRound(ctx, requestID)
}

// List returns every deposit.
func (d *assetDepositService) List(ctx context.Context) ([]*db.AssetDeposit,
	error) {

	return d.store.ListAssetDeposits(ctx)
}

// run checks published deposits for a confirmed proof until ctx ends.
func (d *assetDepositService) run(ctx context.Context) {
	ticker := time.NewTicker(assetDepositPollInterval)
	defer ticker.Stop()

	for {
		d.confirmPublished(ctx)

		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
		case <-d.poll:
		}
	}
}

// requestPoll wakes the watcher without blocking.
func (d *assetDepositService) requestPoll() {
	select {
	case d.poll <- struct{}{}:
	default:
	}
}

// confirmPublished stores the proof of every published deposit whose funding
// transfer has reached the operator's boarding depth. The operator rejects a
// shallower boarding input, and a reorg within that depth would change the
// block the proof commits to.
func (d *assetDepositService) confirmPublished(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, assetDepositPollTimeout)
	defer cancel()

	deposits, err := d.store.ListAssetDepositsByStatus(
		ctx, db.AssetDepositPublished,
	)
	if err != nil {
		d.log.DebugS(ctx, "Unable to list published asset deposits",
			slog.String("error", err.Error()),
		)

		return
	}
	terms := d.terms()
	if len(deposits) == 0 || terms == nil {
		return
	}
	depth := max(terms.MinConfirmations, wallet.MinBoardingConfs)
	info, err := d.tapd.GetInfo(ctx)
	if err != nil || info == nil {
		d.log.DebugS(ctx, "Unable to read the tapd chain tip",
			btclog.Fmt("error", "%v", err),
		)

		return
	}

	for _, deposit := range deposits {
		if deposit.Output == nil {
			continue
		}
		proof, height, err := tapassets.ExportOnboardedProof(
			ctx, d.tapd, deposit.Output.Outpoint,
		)
		if errors.Is(err, tapassets.ErrOnboardedProofPending) {
			continue
		}
		if err != nil {
			d.recordError(ctx, deposit.Request.RequestID, err)

			continue
		}
		if info.BlockHeight < height ||
			info.BlockHeight-height+1 < depth {

			continue
		}

		err = d.store.MarkAssetDepositConfirmed(
			ctx, deposit.Request.RequestID, proof, height,
		)
		if err != nil {
			d.recordError(ctx, deposit.Request.RequestID, err)

			continue
		}
		d.log.InfoS(ctx, "Asset deposit confirmed",
			slog.String("request_id", deposit.Request.RequestID),
			slog.String(
				"outpoint", deposit.Output.Outpoint.String(),
			),
			slog.Int("height", int(height)),
		)
	}
}

// recordError stores the latest failure of a deposit.
func (d *assetDepositService) recordError(ctx context.Context, requestID string,
	cause error) {

	d.log.InfoS(ctx, "Asset deposit attempt failed",
		slog.String("request_id", requestID),
		slog.String("error", cause.Error()),
	)
	err := d.store.SetAssetDepositError(ctx, requestID, cause.Error())
	if err != nil {
		d.log.DebugS(ctx, "Unable to record asset deposit error",
			slog.String("request_id", requestID),
			slog.String("error", err.Error()),
		)
	}
}

// validateDepositAssetRequest checks the caller's deposit parameters.
func validateDepositAssetRequest(req *waverpc.DepositAssetRequest) error {
	switch {
	case req == nil:
		return fmt.Errorf("request is required")

	case req.RequestId == "" ||
		len(req.RequestId) > maxAssetDepositRequestID:
		return fmt.Errorf("request_id must be 1 to %d bytes",
			maxAssetDepositRequestID)

	case req.AssetRef == "":
		return fmt.Errorf("asset_ref is required")

	case req.AssetAmount == 0:
		return fmt.Errorf("asset_amount is required")

	case req.MaxFeeSat == 0:
		return fmt.Errorf("max_fee_sat is required")
	}

	return validateAssetRefFilter(req.AssetRef)
}

// depositMatchesRequest reports whether a stored deposit was created from
// the same caller parameters.
func depositMatchesRequest(deposit *db.AssetDeposit,
	req *waverpc.DepositAssetRequest) bool {

	stored := &deposit.Request

	return stored.AssetRef == req.AssetRef &&
		stored.AssetAmount == req.AssetAmount &&
		stored.FeeRateSatPerVByte == req.SatPerVbyte &&
		stored.TargetConf == depositTargetConf(req) &&
		stored.MaxFeeSat == req.MaxFeeSat
}

// depositTargetConf returns the confirmation target a deposit funds with.
// An explicit fee rate leaves none, because the funding takes exactly one fee
// policy.
func depositTargetConf(req *waverpc.DepositAssetRequest) uint32 {
	switch {
	case req.SatPerVbyte != 0:
		return 0

	case req.TargetConf != 0:
		return req.TargetConf

	default:
		return defaultAssetDepositTargetConf
	}
}

// depositValue sizes a deposit output to board: the asset VTXO and the BTC
// VTXO that takes the rest each need the minimum VTXO amount, and the round
// fee comes out of the same value. Doubling the fee quote lets the deposit
// board after fees rise; the unused part stays in the BTC VTXO.
func depositValue(terms *types.OperatorTerms,
	roundFee btcutil.Amount) btcutil.Amount {

	fee := max(roundFee, terms.MinOperatorFee)
	value := 2*terms.MinVTXOAmountFloor() + 2*fee

	return max(value, terms.MinBoardingAmount)
}

// boardingAmounts splits a deposit's value into the asset VTXO's satoshis and
// the BTC VTXO's target after the round fee.
func boardingAmounts(terms *types.OperatorTerms,
	value, roundFee btcutil.Amount) (btcutil.Amount, btcutil.Amount) {

	assetSat := terms.MinVTXOAmountFloor()
	fee := max(roundFee, terms.MinOperatorFee)

	return assetSat, value - assetSat - fee
}

// assetDepositToRPC projects a stored deposit and the round that spends it.
func assetDepositToRPC(deposit *db.AssetDeposit,
	roundID string) *waverpc.AssetDeposit {

	resp := &waverpc.AssetDeposit{
		RoundId:         roundID,
		RequestId:       deposit.Request.RequestID,
		AssetRef:        deposit.Request.AssetRef,
		AssetAmount:     deposit.Request.AssetAmount,
		ConfirmedHeight: deposit.ConfirmedHeight,
		Error:           deposit.LastError,
	}
	switch deposit.Status {
	case db.AssetDepositFunding:
		resp.Status = waverpc.
			AssetDepositStatus_ASSET_DEPOSIT_STATUS_FUNDING

	case db.AssetDepositPublished:
		resp.Status = waverpc.
			AssetDepositStatus_ASSET_DEPOSIT_STATUS_PUBLISHED

	case db.AssetDepositConfirmed:
		resp.Status = waverpc.
			AssetDepositStatus_ASSET_DEPOSIT_STATUS_CONFIRMED
	}
	if deposit.Output != nil {
		resp.Outpoint = deposit.Output.Outpoint.String()
		resp.ValueSat = deposit.Output.ValueSat
		resp.FeeSat = deposit.Output.FeeSat
	}

	return resp
}

// lndAnchorSigner signs a deposit transaction with the lnd node behind tapd
// and finalizes the inputs it signed: tapd's asset anchors. lnd skips inputs
// derived from another wallet, which the daemon wallet signs afterwards.
func lndAnchorSigner(walletKit lndclient.WalletKitClient) tapsdk.AnchorSigner {
	return func(ctx context.Context, anchorPsbt []byte) ([]byte, error) {
		packet, err := psbt.NewFromRawBytes(
			bytes.NewReader(anchorPsbt), false,
		)
		if err != nil {
			return nil, fmt.Errorf("parse anchor PSBT: %w", err)
		}
		signed, err := walletKit.SignPsbt(ctx, packet)
		if err != nil {
			return nil, fmt.Errorf("lnd SignPsbt: %w", err)
		}
		for idx := range signed.Inputs {
			input := &signed.Inputs[idx]
			if len(input.FinalScriptWitness) != 0 ||
				(len(input.TaprootKeySpendSig) == 0 &&
					len(input.PartialSigs) == 0) {

				continue
			}
			ok, err := psbt.MaybeFinalize(signed, idx)
			if err != nil {
				return nil, fmt.Errorf("finalize anchor input "+
					"%d: %w", idx, err)
			}
			if !ok {
				return nil, fmt.Errorf("anchor input %d is "+
					"not finalizable", idx)
			}
		}

		var buf bytes.Buffer
		if err := signed.Serialize(&buf); err != nil {
			return nil, fmt.Errorf("serialize anchor PSBT: %w", err)
		}

		return buf.Bytes(), nil
	}
}

// depositWallet is the part of a daemon wallet adapter that pays for deposit
// transactions.
type depositWallet interface {
	walletcore.OutputLeaser

	FundTemplate(ctx context.Context, packet *psbt.Packet,
		feeRateSatPerVByte int64, lockID wallet.LockID,
		lockExpiry time.Duration) (int32, error)

	FinalizePsbt(ctx context.Context, packet []byte) (*wire.MsgTx, error)
}

// depositFunder pays deposit transactions from the daemon wallet. The wallet
// is resolved per call, because it unlocks after startup.
type depositFunder struct {
	wallet      func() (depositWallet, error)
	estimateFee func(context.Context, uint32) (btcutil.Amount, error)
}

var _ tapassets.OnboardingFunder = (*depositFunder)(nil)

// FeeRate returns the chain backend's fee estimate in sat/vB.
func (f *depositFunder) FeeRate(ctx context.Context, targetConf uint32) (uint64,
	error) {

	rate, err := f.estimateFee(ctx, targetConf)
	if err != nil {
		return 0, err
	}
	if rate <= 0 {
		return 0, fmt.Errorf("fee estimate of %d sat/vB is not "+
			"positive", rate)
	}

	return uint64(rate), nil
}

// FundTemplate funds an outputs-only template from the daemon wallet.
func (f *depositFunder) FundTemplate(ctx context.Context, packet *psbt.Packet,
	feeRateSatPerVByte int64, lockID walletcore.LockID,
	lockExpiry time.Duration) (int32, error) {

	w, err := f.wallet()
	if err != nil {
		return 0, err
	}

	return w.FundTemplate(
		ctx, packet, feeRateSatPerVByte, lockID, lockExpiry,
	)
}

// FinalizePsbt signs the daemon wallet's inputs.
func (f *depositFunder) FinalizePsbt(ctx context.Context, packet []byte) (
	*wire.MsgTx, error) {

	w, err := f.wallet()
	if err != nil {
		return nil, err
	}

	return w.FinalizePsbt(ctx, packet)
}

// LeaseOutput leases a daemon wallet output.
func (f *depositFunder) LeaseOutput(ctx context.Context, id walletcore.LockID,
	op wire.OutPoint, expiry time.Duration) (time.Time, error) {

	w, err := f.wallet()
	if err != nil {
		return time.Time{}, err
	}

	return w.LeaseOutput(ctx, id, op, expiry)
}

// ReleaseOutput releases a daemon wallet output.
func (f *depositFunder) ReleaseOutput(ctx context.Context, id walletcore.LockID,
	op wire.OutPoint) error {

	w, err := f.wallet()
	if err != nil {
		return err
	}

	return w.ReleaseOutput(ctx, id, op)
}

// depositWallet returns the daemon wallet adapter that pays for deposits.
func (s *Server) depositWallet() (depositWallet, error) {
	sweepWallet, err := s.newSweepWallet()
	if err != nil {
		return nil, err
	}
	w, ok := sweepWallet.(depositWallet)
	if !ok {
		return nil, fmt.Errorf("wallet %T cannot fund deposits",
			sweepWallet)
	}

	return w, nil
}

// estimateDepositFee returns the chain backend's fee estimate in sat/vB.
func (s *Server) estimateDepositFee(ctx context.Context, targetConf uint32) (
	btcutil.Amount, error) {

	if s.chainBackend == nil {
		return 0, fmt.Errorf("chain backend is not ready")
	}

	return s.chainBackend.EstimateFee(ctx, targetConf)
}

// depositOwnerKeyDeriver derives deposit owner keys from the daemon wallet.
// The wallet is read at call time, because it unlocks after startup.
func (s *Server) depositOwnerKeyDeriver() tapassets.OnboardingKeyDeriver {
	return func(ctx context.Context) (*keychain.KeyDescriptor, error) {
		backend := s.proofKeyBackend
		if backend == nil {
			return nil, fmt.Errorf("wallet is not ready")
		}

		return backend.DeriveNextKey(ctx, wallet.BoardingKeyFamily)
	}
}

// registerAssetBoarding hands a deposit to the round actor, which joins a
// round with it in the same turn. The Ask outlives a cancelled caller, so a
// deposit is never queued without its join.
func (s *Server) registerAssetBoarding(ctx context.Context,
	req *round.RegisterAssetBoardingRequest) error {

	if s.actorSystem == nil {
		return fmt.Errorf("actor system not initialized")
	}

	roundRef := round.NewServiceKey().Ref(s.actorSystem)

	return roundRef.Ask(context.WithoutCancel(ctx), req).Await(ctx).Err()
}

// quoteBoardingFee asks the operator for the round fee of one boarding input.
func (s *Server) quoteBoardingFee(ctx context.Context, amount btcutil.Amount) (
	btcutil.Amount, error) {

	return s.quoteOperatorFee(ctx, int64(amount), true, 0)
}
