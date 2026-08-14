package waved

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/tapassets"
)

// taprootAssetBoardRequestKeyPrefix namespaces the persisted replay slice of
// an onboarding request in the Taproot Asset journal, away from the
// onboarder's own state under the bare request ID.
const taprootAssetBoardRequestKeyPrefix = "board-request/"

// assetBoardingConfWait bounds how long BoardTaprootAsset blocks on the
// onboarded output's confirmation before telling the caller to retry.
const assetBoardingConfWait = 30 * time.Second

// taprootAssetBoardRequest is the durable replay slice of an onboarding
// request. It carries exactly the caller-owned fields plus the chain height
// at onboarding time; the operator key and exit delay are re-derived from
// live operator terms on every replay.
type taprootAssetBoardRequest struct {
	AssetRef    string `json:"asset_ref"`
	AssetAmount uint64 `json:"asset_amount"`

	// ProofFile is the funding proof of a slice written before onboarding
	// could select more than one UTXO. Newer slices carry ProofFiles.
	ProofFile []byte `json:"proof_file,omitempty"`

	// ProofFiles are the funding proofs of every UTXO the onboarding
	// spent. A replay runs after those UTXOs are gone, so the complete
	// set has to survive here rather than be re-selected.
	ProofFiles [][]byte `json:"proof_files,omitempty"`

	CarrierValueSat    uint64 `json:"carrier_value_sat"`
	FeeRateSatPerVByte uint64 `json:"fee_rate_sat_per_vbyte"`
	TargetConf         uint32 `json:"target_conf"`
	MaxFeeSat          uint64 `json:"max_fee_sat"`

	// HeightHint is the best chain height when the onboarding ran. The
	// anchor cannot confirm below it, so it seeds the confirmation
	// watch's rescan.
	HeightHint uint32 `json:"height_hint"`
}

// storeTaprootAssetBoardRequest persists the replay slice of a successful
// onboarding so a later BoardTaprootAsset can rebuild the disclosure from
// the idempotency key alone.
func (s *Server) storeTaprootAssetBoardRequest(ctx context.Context,
	req *tapassets.OnboardingRequest, heightHint uint32) error {

	if s.taprootAssetStore == nil {
		return fmt.Errorf("taproot asset store is not configured")
	}

	encoded, err := json.Marshal(&taprootAssetBoardRequest{
		AssetRef:           req.AssetRef,
		AssetAmount:        req.AssetAmount,
		ProofFile:          req.ProofFile,
		ProofFiles:         req.ProofFiles,
		CarrierValueSat:    req.CarrierValueSat,
		FeeRateSatPerVByte: req.FeeRateSatPerVByte,
		TargetConf:         req.TargetConf,
		MaxFeeSat:          req.MaxFeeSat,
		HeightHint:         heightHint,
	})
	if err != nil {
		return fmt.Errorf("encode board replay request: %w", err)
	}

	return s.taprootAssetStore.Store(
		ctx, taprootAssetBoardRequestKeyPrefix+req.RequestID, encoded,
	)
}

// loadTaprootAssetBoardRequest rebuilds the onboarding request persisted by
// a successful OnboardTaprootAsset call, together with the chain height at
// onboarding time.
func (s *Server) loadTaprootAssetBoardRequest(ctx context.Context,
	requestID string) (*tapassets.OnboardingRequest, uint32, error) {

	if s.taprootAssetStore == nil {
		return nil, 0, fmt.Errorf("taproot asset store is not " +
			"configured")
	}

	encoded, err := s.taprootAssetStore.Load(
		ctx, taprootAssetBoardRequestKeyPrefix+requestID,
	)
	if err != nil {
		return nil, 0, err
	}

	var stored taprootAssetBoardRequest
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return nil, 0, fmt.Errorf("decode board replay request: %w",
			err)
	}

	return &tapassets.OnboardingRequest{
		RequestID:          requestID,
		AssetRef:           stored.AssetRef,
		AssetAmount:        stored.AssetAmount,
		ProofFile:          stored.ProofFile,
		ProofFiles:         stored.ProofFiles,
		CarrierValueSat:    stored.CarrierValueSat,
		FeeRateSatPerVByte: stored.FeeRateSatPerVByte,
		TargetConf:         stored.TargetConf,
		MaxFeeSat:          stored.MaxFeeSat,
	}, stored.HeightHint, nil
}

// BoardTaprootAssetResult reports a completed (or replayed) asset boarding.
type BoardTaprootAssetResult struct {
	// Outpoint is the boarded composed output.
	Outpoint wire.OutPoint

	// ValueSat is the output's carrier Bitcoin value.
	ValueSat int64

	// AssetRef and AssetAmount identify the boarded units.
	AssetRef    string
	AssetAmount uint64

	// AlreadyBoarded reports an idempotent replay: the boarding intent
	// already existed and no new round registration was sent.
	AlreadyBoarded bool

	// FeeFunding names the Bitcoin VTXO this boarding pulled into the
	// round to pay its fee, and that VTXO's value. Nil when the round
	// already carried a fee-bearing output of the client's own.
	FeeFunding *assetRoundFeeFunding
}

// ErrAssetBoardingUnconfirmed reports an onboarded output that has not
// confirmed yet; the caller retries once it has.
var ErrAssetBoardingUnconfirmed = errors.New("onboarded output is not " +
	"confirmed yet")

// BoardTaprootAsset completes an onboarded output's path into a round. It
// rebuilds the boarding disclosure from the onboarding named by requestID,
// gathers the confirmation and the boarded asset proof itself, persists the
// boarding intent, and registers the boarding together with a matching
// asset VTXO request.
func (s *Server) BoardTaprootAsset(ctx context.Context, requestID string) (
	*BoardTaprootAssetResult, error) {

	onboardingReq, heightHint, err := s.loadTaprootAssetBoardRequest(
		ctx, requestID,
	)
	if err != nil {
		return nil, fmt.Errorf("load onboarding %q: %w", requestID, err)
	}

	disclosure, err := s.AssetBoardingDisclosure(ctx, onboardingReq)
	if err != nil {
		return nil, fmt.Errorf("rebuild boarding disclosure: %w", err)
	}

	result := &BoardTaprootAssetResult{
		Outpoint:    disclosure.Outpoint,
		AssetRef:    disclosure.AssetRef,
		AssetAmount: disclosure.AssetAmount,
	}

	// An existing boarding intent for the outpoint means an earlier call
	// already completed the round registration; replaying it would hand
	// the round actor a duplicate intent.
	existing, err := s.newBoardingStore().FetchBoardingIntentOutpoints(
		ctx,
	)
	if err != nil {
		return nil, fmt.Errorf("list boarding intents: %w", err)
	}
	for _, op := range existing {
		if op == disclosure.Outpoint {
			result.AlreadyBoarded = true

			return result, nil
		}
	}

	// The disclosure carries everything the operator authenticates except
	// the on-chain confirmation and the boarded proof; gather both.
	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		disclosure.KeyDesc.PubKey, disclosure.OperatorKey,
		disclosure.ExitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("encode boarding policy: %w", err)
	}
	pkScript, _, _, err := tapassets.ComposedBoardingScript(
		policyTemplate, [32]byte(disclosure.AssetCommitmentLeafHash),
	)
	if err != nil {
		return nil, fmt.Errorf("compose boarding script: %w", err)
	}

	conf, err := s.assetBoardingConfirmation(
		ctx, disclosure.Outpoint, pkScript, heightHint,
	)
	if err != nil {
		return nil, err
	}
	if int(disclosure.Outpoint.Index) >= len(conf.Tx.TxOut) {
		return nil, fmt.Errorf("confirmed transaction does not carry " +
			"the boarded output")
	}
	result.ValueSat = conf.Tx.TxOut[disclosure.Outpoint.Index].Value

	proofFile, err := tapassets.ExportBoardedProof(
		ctx, s.taprootAssetWallet, disclosure.Outpoint,
	)
	if errors.Is(err, tapassets.ErrBoardedProofPending) {
		return nil, ErrAssetBoardingUnconfirmed
	}
	if err != nil {
		return nil, err
	}

	disclosure.ConfTx = conf.Tx
	disclosure.ConfHeight = conf.BlockHeight
	disclosure.AssetProof = proofFile

	// The round needs an output whose amount the operator may shrink
	// before it can charge this boarding a fee, and the asset request
	// below is fixed. Resolve that first: the boarding is not yet
	// registered, so a client with no Bitcoin to spend fails here with
	// something it can act on rather than inside a round that would be
	// rejected at seal. An earlier retry of this call cannot have churned
	// a VTXO either — everything above only reads.
	funding, err := s.fundAssetRoundFee(ctx, result.ValueSat)
	if err != nil {
		return nil, err
	}
	result.FeeFunding = funding

	if err := s.RegisterAssetBoarding(ctx, disclosure); err != nil {
		return nil, fmt.Errorf("register asset boarding: %w", err)
	}

	err = s.RegisterAssetVTXORequest(
		ctx, btcutil.Amount(result.ValueSat), disclosure.AssetRef,
		disclosure.AssetAmount,
	)
	if err != nil {
		return nil, fmt.Errorf("register asset VTXO request: %w", err)
	}

	return result, nil
}

// assetBoardingConfirmation resolves the onboarded output's confirmation
// through the chain source. The wait is bounded: an unconfirmed output
// returns ErrAssetBoardingUnconfirmed rather than blocking the RPC until
// the next block.
func (s *Server) assetBoardingConfirmation(ctx context.Context,
	outpoint wire.OutPoint, pkScript []byte, heightHint uint32) (
	*chainsource.ConfirmationEvent, error) {

	if s.actorSystem == nil {
		return nil, fmt.Errorf("actor system not initialized")
	}

	// The LND backend refuses a zero hint; a stored slice predating the
	// hint falls back to the lowest scan start.
	if heightHint == 0 {
		heightHint = 1
	}

	chainRef := chainsource.ChainSourceKey.Ref(s.actorSystem)
	txid := outpoint.Hash
	resp, err := chainRef.Ask(
		context.WithoutCancel(ctx), &chainsource.RegisterConfRequest{
			CallerID: "taproot-asset-board-" +
				outpoint.String(),
			Txid:        &txid,
			PkScript:    pkScript,
			TargetConfs: 1,
			HeightHint:  heightHint,
		},
	).Await(ctx).Unpack()
	if err != nil {
		return nil, fmt.Errorf("register boarding confirmation "+
			"watch: %w", err)
	}
	confResp, ok := resp.(*chainsource.RegisterConfResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected chain source response %T",
			resp)
	}

	waitCtx, cancel := context.WithTimeout(ctx, assetBoardingConfWait)
	defer cancel()

	event, err := confResp.Future.Await(waitCtx).Unpack()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrAssetBoardingUnconfirmed
		}

		return nil, fmt.Errorf("await boarding confirmation: %w", err)
	}
	if event.Tx == nil {
		return nil, fmt.Errorf("boarding confirmation carries no " +
			"transaction")
	}

	return &event, nil
}
