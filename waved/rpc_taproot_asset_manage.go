package waved

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// assetClaimVsizeEstimate is the conservative virtual size used to derive a
// default claim fee: one taproot script-path input with the composed exit
// witness plus one taproot output.
const assetClaimVsizeEstimate = 200

// BoardTaprootAsset implements the user-facing boarding-completion RPC.
func (r *RPCServer) BoardTaprootAsset(ctx context.Context,
	req *waverpc.BoardTaprootAssetRequest) (
	*waverpc.BoardTaprootAssetResponse, error) {

	if r == nil || r.server == nil || r.server.cfg == nil {
		return nil, status.Error(
			codes.Unavailable, "daemon unavailable",
		)
	}
	if req == nil || req.GetIdempotencyKey() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "idempotency key is required",
		)
	}
	if r.server.WalletLifecycleState() != WalletStateReady {
		return nil, status.Error(
			codes.FailedPrecondition, "wallet is not ready",
		)
	}
	if r.server.cfg.TaprootAssetOnboarder == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"Taproot Asset onboarding is disabled",
		)
	}

	result, err := r.server.BoardTaprootAsset(
		ctx, req.GetIdempotencyKey(),
	)
	if errors.Is(err, ErrAssetBoardingUnconfirmed) {
		return nil, status.Error(
			codes.FailedPrecondition, "onboarded output is not "+
				"confirmed yet; retry after the next block",
		)
	}
	if errors.Is(err, tapassets.ErrStoreNotFound) {
		return nil, status.Errorf(codes.NotFound, "no onboarding "+
			"found for idempotency key %q", req.GetIdempotencyKey())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "board Taproot "+
			"Asset: %v", err)
	}

	return &waverpc.BoardTaprootAssetResponse{
		Outpoint:       result.Outpoint.String(),
		ValueSat:       result.ValueSat,
		AssetRef:       result.AssetRef,
		AssetAmount:    result.AssetAmount,
		AlreadyBoarded: result.AlreadyBoarded,
	}, nil
}

// ClaimTaprootAssetVTXO implements the user-facing exit-claim RPC. The
// daemon gathers the leaf's lineage confirmations itself before delegating
// to the claim workflow.
func (r *RPCServer) ClaimTaprootAssetVTXO(ctx context.Context,
	req *waverpc.ClaimTaprootAssetVTXORequest) (
	*waverpc.ClaimTaprootAssetVTXOResponse, error) {

	if r == nil || r.server == nil || r.server.cfg == nil {
		return nil, status.Error(
			codes.Unavailable, "daemon unavailable",
		)
	}
	if req == nil || req.GetOutpoint() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "outpoint is required",
		)
	}
	outpoint, err := wire.NewOutPointFromString(req.GetOutpoint())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"outpoint: %v", err)
	}
	if req.GetFeeSat() < 0 {
		return nil, status.Error(
			codes.InvalidArgument, "fee must not be negative",
		)
	}
	if r.server.WalletLifecycleState() != WalletStateReady {
		return nil, status.Error(
			codes.FailedPrecondition, "wallet is not ready",
		)
	}

	feeSat := req.GetFeeSat()
	if feeSat == 0 {
		feeSat, err = r.server.estimateAssetClaimFee(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "estimate "+
				"claim fee: %v", err)
		}
	}

	confirmations, err := r.server.assetClaimConfirmations(ctx, *outpoint)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "gather claim "+
			"lineage confirmations: %v", err)
	}

	claim, err := r.server.ClaimAssetVTXO(
		ctx, *outpoint, feeSat, confirmations,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "claim Taproot "+
			"Asset VTXO: %v", err)
	}

	return &waverpc.ClaimTaprootAssetVTXOResponse{
		Txid:           claim.Txid.String(),
		AnchorOutpoint: claim.AnchorOutpoint.String(),
		OutputValueSat: claim.OutputValueSat,
		FeeSat:         feeSat,
	}, nil
}

// assetClaimConfirmations gathers the raw block and height of every anchor
// transaction in an exited leaf's lineage. The lineage txids come from the
// leaf's sealed package; the chain source resolves each to its confirmed
// block. The round's creation height serves as the rescan hint, since no
// lineage transaction can confirm before the commitment.
func (s *Server) assetClaimConfirmations(ctx context.Context,
	outpoint wire.OutPoint) (map[chainhash.Hash]tapsdk.AnchorConfirmation,
	error) {

	if s.vtxoStore == nil || s.actorSystem == nil {
		return nil, fmt.Errorf("daemon is not fully initialized")
	}
	desc, err := s.vtxoStore.GetVTXO(ctx, outpoint)
	if err != nil {
		return nil, fmt.Errorf("load VTXO %v: %w", outpoint, err)
	}
	if desc.TaprootAssetRoot == nil || desc.TaprootAssetRef == "" ||
		len(desc.TaprootAssetSealedPackage) == 0 {
		return nil, fmt.Errorf("VTXO %v carries no claimable Taproot "+
			"Asset state", outpoint)
	}
	if desc.Status != vtxo.VTXOStatusUnilateralExit {
		return nil, fmt.Errorf("VTXO %v is not in unilateral exit",
			outpoint)
	}

	source, err := tapassets.ResolveCreatedAssetProofSource(
		desc.TaprootAssetSealedPackage, outpoint, int64(desc.Amount),
		desc.TaprootAssetRef, desc.TaprootAssetAmount,
		*desc.TaprootAssetRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve claim lineage: %w", err)
	}

	var path tapsdk.AssetProofPath
	if err := path.UnmarshalBinary(source.CompactProofPath); err != nil {
		return nil, fmt.Errorf("decode claim proof path: %w", err)
	}

	confirmations := make(
		map[chainhash.Hash]tapsdk.AnchorConfirmation, len(path.Steps),
	)
	chainRef := chainsource.ChainSourceKey.Ref(s.actorSystem)
	for i := range path.Steps {
		summary, err := path.Steps[i].Summary()
		if err != nil {
			return nil, fmt.Errorf("summarize lineage step %d: %w",
				i, err)
		}

		txid := chainhash.Hash(summary.AnchorOutpoint.Txid)
		if _, ok := confirmations[txid]; ok {
			continue
		}

		event, err := s.lineageConfirmation(
			ctx, chainRef, txid, uint32(desc.CreatedHeight),
		)
		if err != nil {
			return nil, fmt.Errorf("confirm lineage tx %v: %w",
				txid, err)
		}

		var encoded []byte
		if event.Block != nil {
			var err error
			encoded, err = serializeBlock(event.Block)
			if err != nil {
				return nil, fmt.Errorf("serialize lineage "+
					"block: %w", err)
			}
		}
		if len(encoded) == 0 {
			return nil, fmt.Errorf("lineage confirmation for %v "+
				"carries no block", txid)
		}

		confirmations[txid] = tapsdk.AnchorConfirmation{
			BlockHeight: uint32(event.BlockHeight),
			Block:       encoded,
		}
	}

	return confirmations, nil
}

// lineageConfirmation resolves one lineage transaction's confirmation with
// the full block attached. The transactions are already confirmed by the
// time a claim runs, so the await is bounded by the historical-dispatch
// rescan rather than a future block.
func (s *Server) lineageConfirmation(ctx context.Context,
	chainRef chainSourceRef, txid chainhash.Hash, heightHint uint32) (
	*chainsource.ConfirmationEvent, error) {

	resp, err := chainRef.Ask(
		context.WithoutCancel(ctx), &chainsource.RegisterConfRequest{
			CallerID:     "taproot-asset-claim-" + txid.String(),
			Txid:         &txid,
			TargetConfs:  1,
			HeightHint:   heightHint,
			IncludeBlock: true,
		},
	).Await(ctx).Unpack()
	if err != nil {
		return nil, fmt.Errorf("register confirmation watch: %w", err)
	}
	confResp, ok := resp.(*chainsource.RegisterConfResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected chain source response %T",
			resp)
	}

	event, err := confResp.Future.Await(ctx).Unpack()
	if err != nil {
		return nil, fmt.Errorf("await confirmation: %w", err)
	}

	return &event, nil
}

// chainSourceRef is the actor handle the confirmation helpers ask.
type chainSourceRef = actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
]

// serializeBlock encodes a block in wire format for the tap-sdk proof
// assembler.
func serializeBlock(block *wire.MsgBlock) ([]byte, error) {
	var buf bytes.Buffer
	if err := block.Serialize(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// estimateAssetClaimFee derives a default claim fee from the shared LND
// wallet's estimator at a conservative confirmation target.
func (s *Server) estimateAssetClaimFee(ctx context.Context) (int64, error) {
	if s.cfg.Wallet.Type != WalletTypeLnd || !s.lnd.IsSome() {
		return 0, fmt.Errorf("claim fee estimation requires the LND " +
			"wallet backend; pass fee_sat explicitly")
	}

	rate, err := s.lnd.UnsafeFromSome().WalletKit.EstimateFeeRate(ctx, 6)
	if err != nil {
		return 0, fmt.Errorf("estimate fee rate: %w", err)
	}

	fee := int64(rate.FeePerKVByte()) * assetClaimVsizeEstimate / 1000
	if fee <= 0 {
		return 0, fmt.Errorf("fee estimator returned a non-positive " +
			"rate")
	}

	return fee, nil
}

// taprootAssetBalances aggregates asset-bearing VTXO holdings by asset
// reference. live holds the non-terminal set (live and mid-spend states);
// exiting holds VTXOs in unilateral exit. Output order is stable by
// reference so repeated calls diff cleanly.
func taprootAssetBalances(
	live, exiting []*vtxo.Descriptor) []*waverpc.TaprootAssetBalance {

	byRef := make(map[string]*waverpc.TaprootAssetBalance)
	balance := func(ref string) *waverpc.TaprootAssetBalance {
		entry, ok := byRef[ref]
		if !ok {
			entry = &waverpc.TaprootAssetBalance{AssetRef: ref}
			byRef[ref] = entry
		}

		return entry
	}

	for _, desc := range live {
		if desc.TaprootAssetRef == "" {
			continue
		}
		entry := balance(desc.TaprootAssetRef)
		if desc.Status == vtxo.VTXOStatusLive {
			entry.LiveAmount += desc.TaprootAssetAmount
			entry.LiveVtxoCount++
		} else {
			entry.PendingAmount += desc.TaprootAssetAmount
		}
	}
	for _, desc := range exiting {
		if desc.TaprootAssetRef == "" {
			continue
		}
		balance(desc.TaprootAssetRef).ExitingAmount +=
			desc.TaprootAssetAmount
	}

	refs := make([]string, 0, len(byRef))
	for ref := range byRef {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	balances := make([]*waverpc.TaprootAssetBalance, 0, len(refs))
	for _, ref := range refs {
		balances = append(balances, byRef[ref])
	}

	return balances
}

// filterDescriptorsByAssetRef keeps the descriptors whose asset reference
// is equivalent to ref. References parse through tap-sdk so an issuance-ID
// form matches its stored canonical form; an unparseable ref falls back to
// exact string comparison.
func filterDescriptorsByAssetRef(descriptors []*vtxo.Descriptor,
	ref string) []*vtxo.Descriptor {

	want, wantErr := tapsdk.ParseAssetRef(ref)
	matches := func(stored string) bool {
		if stored == ref {
			return true
		}
		if wantErr != nil {
			return false
		}
		parsed, err := tapsdk.ParseAssetRef(stored)
		if err != nil {
			return false
		}

		return parsed.Equivalent(want)
	}

	filtered := make([]*vtxo.Descriptor, 0, len(descriptors))
	for _, desc := range descriptors {
		if desc.TaprootAssetRef != "" &&
			matches(desc.TaprootAssetRef) {

			filtered = append(filtered, desc)
		}
	}

	return filtered
}
