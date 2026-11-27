package waved

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/types"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/metrics"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// incomingAssetVerifier returns the daemon's incoming asset OOR verifier when
// Taproot Assets support is enabled, and nil when assets are disabled so
// default Bitcoin-only daemons skip asset overlays without breaking sibling
// Bitcoin outputs.
func (s *Server) incomingAssetVerifier() oor.IncomingAssetVerifier {
	if s == nil || s.assetClient == nil || s.embeddedAssets == nil {
		return nil
	}

	return s.verifyIncomingAssetOOR
}

// verifyIncomingAssetOOR verifies one incoming Taproot Asset OOR output
// against the daemon's embedded asset engine and the output's authenticated
// Bitcoin lineage and OOR package chain before materialization stores its
// sealed Ark package.
func (s *Server) verifyIncomingAssetOOR(ctx context.Context,
	req oor.IncomingAssetVerification) error {

	if s == nil || s.assetClient == nil || s.embeddedAssets == nil {
		return fmt.Errorf("%w: Taproot Assets engine is not enabled",
			oor.ErrInvalidIncomingAssetPackage)
	}
	if req.Recipient.Asset == nil {
		return fmt.Errorf("%w: missing incoming recipient asset",
			oor.ErrInvalidIncomingAssetPackage)
	}
	if req.ArkPSBT == nil || req.ArkPSBT.UnsignedTx == nil {
		return fmt.Errorf("%w: missing incoming ark psbt",
			oor.ErrInvalidIncomingAssetPackage)
	}
	if req.TaprootAssetTransfer == nil ||
		len(req.TaprootAssetTransfer.ArkPackage) == 0 {
		return fmt.Errorf("%w: missing incoming sealed ark "+
			"asset package", oor.ErrInvalidIncomingAssetPackage)
	}

	if err := validateAssetRefFilter(req.Recipient.Asset.Ref); err != nil {
		return fmt.Errorf("%w: %w", oor.ErrInvalidIncomingAssetPackage,
			err)
	}

	if err := trustAssetGroup(
		s.embeddedAssets, req.Recipient.Asset.Ref,
	); err != nil {
		return fmt.Errorf("%w: trust asset group: %w",
			oor.ErrInvalidIncomingAssetPackage, err)
	}

	chainVerifier, err := tapassets.NewLocalProofChainVerifier(
		s.assetClient,
	)
	if err != nil {
		return fmt.Errorf("create proof chain verifier: %w", err)
	}
	vtxoVerifier, err := tapassets.NewVTXOVerifier(chainVerifier)
	if err != nil {
		return fmt.Errorf("create asset VTXO verifier: %w", err)
	}

	lineage := lineageAnchorsFromAncestry(req.Metadata.Ancestry)
	checkpointTxs, err := unsignedTxsFromPSBTs(req.FinalCheckpointPSBTs)
	if err != nil {
		return fmt.Errorf("%w: %w", oor.ErrInvalidIncomingAssetPackage,
			err)
	}

	ancestors := make(
		[]tapassets.OORPackageTxs, 0, len(req.AncestorPackages),
	)
	for idx, pkg := range req.AncestorPackages {
		if pkg.ArkPSBT == nil || pkg.ArkPSBT.UnsignedTx == nil {
			return fmt.Errorf("%w: ancestor package %d "+
				"missing ark tx",
				oor.ErrInvalidIncomingAssetPackage, idx)
		}
		ancCPs, err := unsignedTxsFromPSBTs(pkg.FinalCheckpointPSBTs)
		if err != nil {
			return fmt.Errorf("%w: ancestor package %d: %w",
				oor.ErrInvalidIncomingAssetPackage, idx, err)
		}
		ancestors = append(ancestors, tapassets.OORPackageTxs{
			ArkTx:         pkg.ArkPSBT.UnsignedTx,
			CheckpointTxs: ancCPs,
		})
	}

	arkTx := req.ArkPSBT.UnsignedTx
	err = vtxoVerifier.VerifyOORAssetVTXO(
		ctx, tapassets.VerifyOORAssetVTXORequest{
			AssetRef:    req.Recipient.Asset.Ref,
			AssetAmount: req.Recipient.Asset.Amount,
			AssetRoot:   req.Recipient.Asset.Root,
			Outpoint: wire.OutPoint{
				Hash:  arkTx.TxHash(),
				Index: req.Recipient.OutputIndex,
			},
			ArkTx:            arkTx,
			CheckpointTxs:    checkpointTxs,
			AncestorPackages: ancestors,
			Lineage:          lineage,
			SealedArkPackage: req.TaprootAssetTransfer.ArkPackage,
		},
	)
	if err != nil {

		// Return proof-verifier errors unwrapped so transient chain
		// backend outages use ReceiveNotified's bounded exponential
		// backoff before terminating instead of permanently killing a
		// valid incoming asset transfer on the first RPC blip.
		return err
	}

	return nil
}

// SendAssetOOR initiates an out-of-round Taproot Asset transfer directly
// between the client and operator. It merges up to eight live asset VTXOs of
// the requested asset, leases an operator carrier float to fund the new asset
// leaves, returns round-originated carrier sats as sender Bitcoin change, and
// submits the pre-committed transfer to the durable OOR actor.
//
//nolint:funlen // Keep the asset OOR orchestration linear and auditable.
func (r *RPCServer) SendAssetOOR(ctx context.Context,
	req *waverpc.SendAssetOORRequest) (*waverpc.SendAssetOORResponse,
	error) {

	startTime := time.Now()

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if r.server.assetClient == nil || r.server.embeddedAssets == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "Taproot "+
			"Assets are not enabled on this daemon")
	}

	parsedRef, err := tapsdk.ParseAssetRef(req.GetAssetRef())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"asset_ref: %v", err)
	}
	assetRef := parsedRef.String()

	if req.GetAmount() == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "amount "+
			"must be positive")
	}
	if req.GetAdmissionDeadlineUnixNanos() < 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"admission_deadline_unix_nanos must not be negative")
	}

	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, operatorTermsError(
			"unable to fetch operator terms", err,
		)
	}
	if terms.OORCarrierPubKey == nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"operator does not advertise an OOR carrier float key")
	}
	if !operatorAdmitsAsset(terms, assetRef) {
		return nil, status.Errorf(codes.FailedPrecondition, "operator "+
			"does not admit asset %s", assetRef)
	}

	outputFloor := terms.MinVTXOAmountFloor()
	if outputFloor <= 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"operator minimum VTXO output floor must be positive")
	}

	recipientPolicy, err := resolveAssetOORRecipientPolicy(req, terms)
	if err != nil {
		return nil, err
	}

	if req.GetIdempotencyKey() != "" && !req.GetDryRun() {
		replay, handled, err := r.replaySendAssetOORByIdempotencyKey(
			ctx, req.GetIdempotencyKey(), assetRef, req.GetAmount(),
			recipientPolicy,
		)
		if err != nil {
			return nil, err
		}
		if handled {
			return replay, nil
		}
	}

	if deadline := req.GetAdmissionDeadlineUnixNanos(); deadline > 0 &&
		!time.Now().Before(time.Unix(0, deadline)) {
		return nil, status.Errorf(codes.FailedPrecondition, "OOR "+
			"admission deadline reached")
	}

	if req.GetDryRun() {
		return r.previewSendAssetOOR(
			ctx, assetRef, req.GetAmount(), outputFloor,
		)
	}

	if r.server.actorSystem == nil {
		return nil, status.Errorf(codes.Internal, "actor system not "+
			"initialized")
	}
	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal, "VTXO store not "+
			"initialized")
	}

	wRef, err := r.walletActor()
	if err != nil {
		return nil, err
	}

	selectResp, err := wRef.Ask(ctx, &wallet.SelectAndLockAssetVTXOsRequest{
		AssetRef:     assetRef,
		TargetAmount: req.GetAmount(),
		MaxInputs:    tapassets.MaxOORAssetInputs,
	}).Await(ctx).Unpack()
	if err != nil {
		return nil, status.Errorf(vtxoAdmissionCode(err), "asset VTXO "+
			"selection failed: %v", err)
	}

	lockedAsset, ok := selectResp.(*wallet.SelectAndLockAssetVTXOsResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", selectResp)
	}

	// Build a compatible SelectAndLockVTXOsResponse so error and canceled-
	// await cleanup reuse the exact same unlock helpers as SendOOR. Use a
	// single deferred unlock on a detached bounded context so a cancelled
	// request context can never leak locked VTXOs.
	lockedCompat := toCompatLockedVTXOs(lockedAsset)
	handoffDone := false
	defer func() {
		if handoffDone {
			return
		}
		unlockCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 5*time.Second,
		)
		defer cancel()

		r.unlockSelectedVTXOsBestEffort(unlockCtx, lockedCompat)
	}()

	policy := arkscript.CheckpointPolicy{
		OperatorKey: terms.PubKey,
		CSVDelay:    terms.VTXOExitDelay,
	}

	if err := trustAssetGroup(
		r.server.embeddedAssets, assetRef,
	); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "trust asset "+
			"group: %v", err)
	}

	builtInputs, err := r.loadAssetOORInputs(
		ctx, assetRef, terms.PubKey, lockedAsset.SelectedVTXOs,
	)
	if err != nil {
		return nil, err
	}

	assetChange := lockedAsset.TotalAssetSelected - req.GetAmount()
	newAssetLeafCount := 1
	if assetChange > 0 {
		newAssetLeafCount = 2
	}

	// Validate local carrier preconditions before leasing an operator
	// carrier float so local rejections never consume a float lease.
	var roundCarrierTotal btcutil.Amount
	for _, carrier := range builtInputs.carriers {
		if carrier.Origin == oortx.CarrierOriginRound {
			roundCarrierTotal += carrier.Amount
		}
	}
	if roundCarrierTotal > 0 && roundCarrierTotal < outputFloor {
		return nil, status.Errorf(codes.FailedPrecondition, "sender "+
			"Bitcoin change %d sat is below output floor %d sat",
			roundCarrierTotal, outputFloor)
	}

	requiredLeaseSat, err := oortx.RequiredCarrierLeaseSat(
		newAssetLeafCount, outputFloor,
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "required "+
			"carrier lease: %v", err)
	}

	lease, err := r.leaseOORCarrier(
		ctx, requiredLeaseSat, req.GetIdempotencyKey(),
	)
	if err != nil {
		return nil, err
	}

	floatInput, err := lease.ToTransferInput(
		terms.PubKey, terms.OORCarrierPubKey,
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid "+
			"operator carrier lease: %v", err)
	}

	carrierPlan, err := oortx.PlanAssetCarriers(oortx.CarrierPlanRequest{
		Inputs:            builtInputs.carriers,
		NewAssetLeafCount: newAssetLeafCount,
		OutputFloor:       outputFloor,
		LeaseValue:        lease.Value,
	})
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "plan "+
			"asset carriers: %v", err)
	}

	floatCPArtifact, err := oortx.BuildCheckpointPSBT(
		policy, oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: floatInput.VTXO.Outpoint,
				Output: wire.NewTxOut(
					int64(floatInput.VTXO.Amount),
					floatInput.VTXO.PkScript,
				),
			},
			OwnerLeafScript: floatInput.OwnerLeafScript,
			OwnerLeafPolicy: floatInput.OwnerLeafPolicy,
		},
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build carrier "+
			"float checkpoint: %v", err)
	}
	floatCPOut, err := floatCPArtifact.ToCheckpointOutput()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build carrier "+
			"float checkpoint output: %v", err)
	}

	assetOutputs := []tapassets.OORAssetOutput{{
		PolicyTemplate: recipientPolicy,
		Amount:         req.GetAmount(),
		Value:          carrierPlan.NewAssetLeafCarrier,
	}}
	if assetChange > 0 {
		assetChangeRecip, err := r.buildOORChangeRecipient(
			ctx, terms.PubKey, terms.VTXOExitDelay,
			carrierPlan.NewAssetLeafCarrier,
		)
		if err != nil {
			return nil, err
		}
		assetOutputs = append(assetOutputs, tapassets.OORAssetOutput{
			PolicyTemplate: assetChangeRecip.VTXOPolicyTemplate,
			Amount:         assetChange,
			Value:          carrierPlan.NewAssetLeafCarrier,
		})
	}

	var plainOutputs []oortx.RecipientOutput
	if carrierPlan.SenderChange > 0 {
		btcChangeRecip, err := r.buildOORChangeRecipient(
			ctx, terms.PubKey, terms.VTXOExitDelay,
			carrierPlan.SenderChange,
		)
		if err != nil {
			return nil, err
		}
		plainOutputs = append(plainOutputs, btcChangeRecip)
	}
	if carrierPlan.OperatorChange > 0 {
		plainOutputs = append(plainOutputs, oortx.RecipientOutput{
			PkScript:           bytes.Clone(lease.PkScript),
			Value:              carrierPlan.OperatorChange,
			VTXOPolicyTemplate: bytes.Clone(lease.PolicyTemplate),
		})
	}

	digest, err := deriveAssetOORTransferDigest(
		assetRef, req.GetAmount(), recipientPolicy,
		builtInputs.transferInputs, floatInput.VTXO.Outpoint,
		req.GetIdempotencyKey(),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "derive asset "+
			"transfer digest: %v", err)
	}

	tapWallet := tapsdk.NewWallet(
		r.server.assetClient, tapsdk.Network(r.server.cfg.Network),
	)
	builtTransfer, err := tapassets.BuildOORTransfer(
		ctx, tapWallet, tapassets.OORTransferRequest{
			AssetRef:         parsedRef,
			Digest:           digest,
			CheckpointPolicy: policy,
			Inputs:           builtInputs.assetInputs,
			Checkpoints:      []oortx.CheckpointOutput{floatCPOut},
			AssetOutputs:     assetOutputs,
			Outputs:          plainOutputs,
		},
	)
	if err != nil {
		code := codes.Internal
		if errors.Is(err, tapassets.ErrOORProofLimit) ||
			errors.Is(err, tapassets.ErrOOROrderingUnresolved) {

			code = codes.FailedPrecondition
		}

		return nil, status.Errorf(code, "build asset OOR transfer: %v",
			err)
	}

	allInputs := append(
		append(
			[]oor.TransferInput(nil), builtInputs.transferInputs...,
		),
		floatInput,
	)
	cpPSBTs := make([]*psbt.Packet, 0, len(builtTransfer.Checkpoints)+1)
	cpRoots := make(
		[]*chainhash.Hash, 0, len(builtTransfer.CheckpointAssetRoots)+1,
	)
	cpPkgs := make([][]byte, 0, len(builtTransfer.CheckpointPackages)+1)
	for i := range builtTransfer.Checkpoints {
		cpPSBTs = append(cpPSBTs, builtTransfer.Checkpoints[i].PSBT)
		root := builtTransfer.CheckpointAssetRoots[i]
		cpRoots = append(cpRoots, &root)
		cpPkgs = append(cpPkgs, builtTransfer.CheckpointPackages[i])
	}
	cpPSBTs = append(cpPSBTs, floatCPArtifact.PSBT)
	cpRoots = append(cpRoots, nil)
	cpPkgs = append(cpPkgs, nil)

	prepared := &oor.PreparedSubmitPackage{
		ArkPSBT:              builtTransfer.Ark,
		CheckpointPSBTs:      cpPSBTs,
		CheckpointAssetRoots: cpRoots,
		TaprootAssetTransfer: &oortx.TaprootAssetTransfer{
			Version:            oortx.TaprootAssetTransferVersion,
			ArkPackage:         builtTransfer.ArkPackage,
			CheckpointPackages: cpPkgs,
		},
	}
	if err := prepared.AttachTapLeafScripts(allInputs); err != nil {
		return nil, status.Errorf(codes.Internal, "attach asset OOR "+
			"tapleaf scripts: %v", err)
	}
	if err := prepared.Validate(
		policy, allInputs, builtTransfer.Recipients,
	); err != nil {
		return nil, status.Errorf(codes.Internal, "validate prepared "+
			"asset OOR submit: %v", err)
	}

	matchedRecipient, err := findAssetOORRecipientOutput(
		builtTransfer.Recipients, recipientPolicy, assetRef,
		req.GetAmount(),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "locate asset OOR "+
			"recipient output: %v", err)
	}

	recipientOP, err := oortx.RecipientOutPoint(
		prepared.ArkPSBT.UnsignedTx.TxHash(), builtTransfer.Recipients,
		matchedRecipient,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "derive asset OOR "+
			"recipient outpoint: %v", err)
	}

	var dispatchRequestData []byte
	if req.GetIdempotencyKey() != "" {
		dispatchRequestData, err = oor.NewOutgoingAssetReplayData(
			oor.OutgoingAssetReplay{
				OutputIndex: recipientOP.Index,
				SelectedInputCount: uint32(
					len(builtInputs.assetInputs),
				),
				AssetRef:          assetRef,
				AssetAmount:       req.GetAmount(),
				AssetChangeAmount: assetChange,
				SenderBtcChangeSat: int64(
					carrierPlan.SenderChange,
				),
				RecipientPolicy: recipientPolicy,
			},
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "build "+
				"asset OOR dispatch identity: %v", err)
		}
	}

	oorKey := oor.NewServiceKey()
	oorRef := oorKey.Ref(r.server.actorSystem)

	oorReq := &oor.StartTransferRequest{
		Policy:              policy,
		Inputs:              allInputs,
		Recipients:          builtTransfer.Recipients,
		PreparedSubmit:      prepared,
		IdempotencyKey:      req.GetIdempotencyKey(),
		DispatchRequestData: dispatchRequestData,
		AdmissionDeadlineUnixNanos: req.
			GetAdmissionDeadlineUnixNanos(),
	}

	oorCtx := actor.WithoutTx(context.WithoutCancel(ctx))
	future := oorRef.Ask(oorCtx, oorReq)
	oorResp, err := future.Await(ctx).Unpack()
	if err != nil {
		if errors.Is(err, oor.ErrOutgoingAdmissionExpired) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"OOR admission deadline reached")
		}
		if errors.Is(err, oor.ErrIdempotencyKeyConflict) {
			return nil, status.Errorf(codes.AlreadyExists, "OOR "+
				"session already uses a different "+
				"idempotency key")
		}
		if isAwaitContextError(ctx, err) {
			handoffDone = true
			r.cleanupSubmittedOORStart(
				ctx, future, lockedCompat, nil,
			)

			code := codes.Canceled
			if errors.Is(err, context.DeadlineExceeded) {
				code = codes.DeadlineExceeded
			}

			return nil, status.Errorf(code, "asset OOR transfer "+
				"response wait: %v", err)
		}

		r.server.emitMetric(ctx, &metrics.OORTransferSentMsg{
			Status:   "failed",
			Duration: time.Since(startTime),
		})

		return nil, status.Errorf(codes.Internal, "asset OOR transfer "+
			"failed: %v", err)
	}

	resp, ok := oorResp.(*oor.StartTransferResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", oorResp)
	}

	if resp.Existing {
		// Leave handoffDone false so the deferred cleanup releases the
		// discarded selection, do not emit a second submitted metric,
		// and return the stored attempt's identity and summary.
		if req.GetIdempotencyKey() != "" {
			replay, handled, replayErr := r.
				replaySendAssetOORByIdempotencyKey(
					ctx, req.GetIdempotencyKey(), assetRef,
					req.GetAmount(), recipientPolicy,
				)
			if replayErr != nil {
				return nil, replayErr
			}
			if handled {
				return replay, nil
			}
		}

		return &waverpc.SendAssetOORResponse{
			Status:    "submitted",
			SessionId: resp.SessionID.String(),
		}, nil
	}

	handoffDone = true

	r.server.log.InfoS(ctx, "Asset OOR transfer submitted",
		slog.String("session_id", resp.SessionID.String()),
		slog.Bool("existing_session", false),
		slog.String("asset_ref", assetRef),
		slog.Uint64("asset_amount", req.GetAmount()),
		slog.Uint64("asset_change_amount", assetChange),
		slog.Int("selected_input_count", len(builtInputs.assetInputs)),
		slog.Int64(
			"sender_btc_change_sat",
			int64(carrierPlan.SenderChange),
		),
		slog.Int64(
			"operator_float_change_sat",
			int64(carrierPlan.OperatorChange),
		),
		slog.Duration("duration", time.Since(startTime)),
	)

	r.server.emitMetric(ctx, &metrics.OORTransferSentMsg{
		SessionID: resp.SessionID.String(),
		Status:    "submitted",
		Duration:  time.Since(startTime),
	})

	return &waverpc.SendAssetOORResponse{
		Status:             "submitted",
		SessionId:          resp.SessionID.String(),
		RecipientOutpoint:  recipientOP.String(),
		SelectedInputCount: uint32(len(builtInputs.assetInputs)),
		AssetChangeAmount:  assetChange,
		SenderBtcChangeSat: int64(carrierPlan.SenderChange),
	}, nil
}

// assetOORInputs bundles the per-input representations built from selected
// asset VTXOs for tapassets.BuildOORTransfer, carrier planning, and the OOR
// session actor.
type assetOORInputs struct {
	assetInputs    []tapassets.OORAssetInput
	transferInputs []oor.TransferInput
	carriers       []oortx.CarrierInput
}

// loadAssetOORInputs loads the full descriptors, asset pieces, proof
// verifiers, and collaborative leaves for the selected asset VTXOs.
// Checkpoint owner leaves are built under currentOperatorKey so asset VTXOs
// created before an operator key rotation remain spendable OOR.
func (r *RPCServer) loadAssetOORInputs(ctx context.Context, assetRef string,
	currentOperatorKey *btcec.PublicKey,
	selected []wallet.SelectedAssetVTXO) (*assetOORInputs, error) {

	out := &assetOORInputs{
		assetInputs: make(
			[]tapassets.OORAssetInput, 0, len(selected),
		),
		transferInputs: make([]oor.TransferInput, 0, len(selected)),
		carriers:       make([]oortx.CarrierInput, 0, len(selected)),
	}

	for _, sv := range selected {
		desc, err := r.server.vtxoStore.GetVTXO(ctx, sv.Outpoint)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load "+
				"selected asset VTXO %s: %v", sv.Outpoint, err)
		}
		if desc == nil || desc.TaprootAssetRoot == nil ||
			!strings.EqualFold(desc.TaprootAssetRef, assetRef) ||
			desc.TaprootAssetAmount == 0 ||
			len(desc.TaprootAssetSealedPackage) == 0 {
			return nil, status.Errorf(codes.FailedPrecondition,
				"selected VTXO %s has incomplete asset state",
				sv.Outpoint)
		}
		if desc.ClientKey.PubKey == nil || desc.OperatorKey == nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"selected asset VTXO %s has no signing keys",
				sv.Outpoint)
		}

		pieces, err := tapassets.DecodeAssetVTXOClaimInputs(
			desc.TaprootAssetSealedPackage, desc.Outpoint,
		)
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"decode asset VTXO %s package: %v", sv.Outpoint,
				err)
		}

		var pieceTotal uint64
		for _, piece := range pieces {
			if piece.Amount > math.MaxUint64-pieceTotal {
				return nil, status.Errorf(codes.Internal,
					"asset VTXO %s piece amount overflow",
					sv.Outpoint)
			}
			pieceTotal += piece.Amount
		}
		if pieceTotal != desc.TaprootAssetAmount {
			return nil, status.Errorf(codes.FailedPrecondition,
				"asset VTXO %s package has %d units, want %d",
				sv.Outpoint, pieceTotal,
				desc.TaprootAssetAmount)
		}

		vtxoPolicy := bytes.Clone(desc.PolicyTemplate)
		if len(vtxoPolicy) == 0 {
			vtxoPolicy, err = arkscript.EncodeStandardVTXOTemplate(
				desc.ClientKey.PubKey, desc.OperatorKey,
				desc.RelativeExpiry,
			)
			if err != nil {
				return nil, status.Errorf(codes.Internal,
					"encode standard policy for asset "+
						"VTXO %s: %v", sv.Outpoint, err)
			}
		}

		checkpointOperatorKey := currentOperatorKey
		if checkpointOperatorKey == nil {
			checkpointOperatorKey = desc.OperatorKey
		}

		collabLeaf, err := arkscript.MultiSigCollabTapLeaf(
			desc.ClientKey.PubKey, checkpointOperatorKey,
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "derive "+
				"collab leaf for asset VTXO %s: %v",
				sv.Outpoint, err)
		}
		ownerLeafPolicy, err := arkscript.LeafTemplate{
			Node: &arkscript.Multisig{
				Keys: []*btcec.PublicKey{
					desc.ClientKey.PubKey,
					checkpointOperatorKey,
				},
			},
		}.Encode()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode "+
				"collab leaf policy for asset VTXO %s: %v",
				sv.Outpoint, err)
		}

		verifier, err := r.buildAssetInputProofVerifier(ctx, desc)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "build "+
				"proof verifier for asset VTXO %s: %v",
				sv.Outpoint, err)
		}

		carrierOrigin := oortx.CarrierOriginRound
		if desc.ChainDepth > 0 {
			carrierOrigin = oortx.CarrierOriginOOR
		}

		out.assetInputs = append(
			out.assetInputs, tapassets.OORAssetInput{
				Checkpoint: oortx.CheckpointInput{
					SpentVTXO: oortx.SpentVTXORef{
						Outpoint: desc.Outpoint,
						Output: wire.NewTxOut(
							int64(desc.Amount),
							desc.PkScript,
						),
					},
					OwnerLeafScript: collabLeaf.Script,
					OwnerLeafPolicy: ownerLeafPolicy,
				},
				Pieces:   pieces,
				Verifier: verifier,
			},
		)
		out.transferInputs = append(
			out.transferInputs, oor.TransferInput{
				VTXO:                      desc,
				OwnerLeafScript:           collabLeaf.Script,
				OwnerLeafPolicy:           ownerLeafPolicy,
				VTXOPolicyTemplate:        vtxoPolicy,
				ReserveEpoch:              sv.ReserveEpoch,
				TaprootAssetCarrierOrigin: carrierOrigin,
			},
		)
		out.carriers = append(out.carriers, oortx.CarrierInput{
			Amount: desc.Amount,
			Origin: carrierOrigin,
		})
	}

	return out, nil
}

// buildAssetInputProofVerifier constructs a ConfirmedProofVerifier bound to
// the daemon's embedded engine and the VTXO's persisted commitment-tree
// ancestry and local OOR package chain.
func (r *RPCServer) buildAssetInputProofVerifier(ctx context.Context,
	desc *vtxo.Descriptor) (customanchor.ConfirmedProofVerifier, error) {

	chainVerifier, err := tapassets.NewLocalProofChainVerifier(
		r.server.assetClient,
	)
	if err != nil {
		return nil, err
	}

	lineage := lineageAnchorsFromAncestry(desc.Ancestry)

	var packages []tapassets.OORPackageTxs
	if desc.ChainDepth > 0 {
		pkgStore := r.newLocalOORArtifactStore()
		if pkgStore == nil {
			return nil, fmt.Errorf("OOR artifact store not " +
				"initialized for OOR asset VTXO")
		}
		resolved, err := pkgStore.ResolveUnrollPackages(
			ctx, desc.Outpoint,
		)
		if err != nil {
			return nil, fmt.Errorf("resolve OOR packages for "+
				"%s: %w", desc.Outpoint, err)
		}
		for _, bundle := range resolved.Packages {
			if bundle == nil || bundle.ArkPSBT == nil ||
				bundle.ArkPSBT.UnsignedTx == nil {

				continue
			}
			cpTxs, err := unsignedTxsFromPSBTs(
				bundle.FinalCheckpointPSBTs,
			)
			if err != nil {
				return nil, fmt.Errorf("decode checkpoint txs "+
					"for session %s: %w", bundle.SessionID,
					err)
			}
			packages = append(packages, tapassets.OORPackageTxs{
				ArkTx:         bundle.ArkPSBT.UnsignedTx,
				CheckpointTxs: cpTxs,
			})
		}
	}

	return tapassets.NewOORProofVerifier(chainVerifier, lineage, packages)
}

// leaseOORCarrier requests one operator carrier float VTXO lease covering
// requiredSat over the authenticated operator mailbox client.
func (r *RPCServer) leaseOORCarrier(ctx context.Context,
	requiredSat btcutil.Amount, idempotencyKey string) (
	*oor.OORCarrierLease, error) {

	if r.oorCarrierLeaseOverride != nil {
		return r.oorCarrierLeaseOverride(
			ctx, requiredSat, idempotencyKey,
		)
	}

	if r.server.ark == nil {
		return nil, status.Errorf(codes.Unavailable, "authenticated "+
			"operator client not initialized")
	}

	var opts []mailboxrpc.RPCOptions
	if idempotencyKey != "" {
		opts = append(opts, mailboxrpc.RPCOptions{
			IdempotencyKey: "asset-oor-lease:" + idempotencyKey,
		})
	}

	resp, err := r.server.ark.LeaseOORCarrier(
		ctx, &arkrpc.LeaseOORCarrierRequest{
			RequiredSat: int64(requiredSat),
		}, opts...,
	)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "lease OOR "+
			"carrier float: %v", err)
	}

	outpoint, err := wire.NewOutPointFromString(resp.GetOutpoint())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "parse leased "+
			"carrier outpoint %q: %v", resp.GetOutpoint(), err)
	}

	lease := &oor.OORCarrierLease{
		Outpoint:       *outpoint,
		Value:          btcutil.Amount(resp.GetValueSat()),
		PolicyTemplate: bytes.Clone(resp.GetVtxoPolicyTemplate()),
		PkScript:       bytes.Clone(resp.GetPkScript()),
		ExpiresAtUnix:  resp.GetExpiresAtUnix(),
	}
	if err := lease.Validate(); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid "+
			"operator carrier lease: %v", err)
	}

	return lease, nil
}

// previewSendAssetOOR validates live asset VTXO availability without locking
// VTXOs, leasing a carrier float, or submitting a transfer.
func (r *RPCServer) previewSendAssetOOR(ctx context.Context, assetRef string,
	amount uint64, outputFloor btcutil.Amount) (
	*waverpc.SendAssetOORResponse, error) {

	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal, "VTXO store not "+
			"initialized")
	}

	liveDescs, err := r.server.vtxoStore.ListVTXOsByStatus(
		ctx, vtxo.VTXOStatusLive,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list live VTXOs: %v",
			err)
	}

	selected, totalSelected, err := vtxo.SelectAssetVTXOCandidates(
		liveDescs, assetRef, amount, tapassets.MaxOORAssetInputs,
	)
	if err != nil {
		return nil, status.Errorf(vtxoAdmissionCode(err), "asset VTXO "+
			"preview failed: %v", err)
	}

	var senderBTCChange btcutil.Amount
	for _, desc := range selected {
		if desc.ChainDepth == 0 {
			senderBTCChange += desc.Amount
		}
	}
	if senderBTCChange > 0 && senderBTCChange < outputFloor {
		return nil, status.Errorf(codes.FailedPrecondition, "sender "+
			"Bitcoin change %d sat is below output floor %d sat",
			senderBTCChange, outputFloor)
	}

	return &waverpc.SendAssetOORResponse{
		Status:             "preview",
		SelectedInputCount: uint32(len(selected)),
		AssetChangeAmount:  totalSelected - amount,
		SenderBtcChangeSat: int64(senderBTCChange),
	}, nil
}

// replaySendAssetOORByIdempotencyKey returns an existing durable keyed session
// before selecting asset VTXOs or leasing an operator carrier float, verifying
// that the caller's asset, amount, and destination policy match the original
// attempt.
func (r *RPCServer) replaySendAssetOORByIdempotencyKey(ctx context.Context, key,
	assetRef string, amount uint64, recipientPolicy []byte) (
	*waverpc.SendAssetOORResponse, bool, error) {

	attempt, found, err := r.findOutgoingOORSessionByIdempotencyKey(
		ctx, key,
	)
	if err != nil {
		return nil, true, err
	}
	if !found {
		return nil, false, nil
	}

	sessionID := oor.SessionID(attempt.SessionID)
	var (
		hasReplay bool
		replay    oor.OutgoingAssetReplay
	)
	if len(attempt.RequestData) > 0 {
		if !oor.IsOutgoingAssetReplayData(attempt.RequestData) {
			return nil, true, status.Errorf(codes.AlreadyExists,
				"idempotency key already identifies a "+
					"different OOR request")
		}

		replay, err = oor.DecodeOutgoingAssetReplayData(
			attempt.RequestData,
		)
		if err != nil {
			return nil, true, status.Errorf(codes.DataLoss,
				"decode asset OOR dispatch request: %v", err)
		}
		if replay.AssetRef != assetRef ||
			replay.AssetAmount != amount ||
			!bytes.Equal(replay.RecipientPolicy, recipientPolicy) {
			return nil, true, status.Errorf(codes.AlreadyExists,
				"idempotency key already identifies a "+
					"different asset OOR request")
		}
		hasReplay = true
	}

	replayStatus, prePONR, err := r.outgoingOORReplayStatus(ctx, sessionID)
	if err != nil {
		return nil, true, err
	}
	if replayStatus == "failed" {
		return &waverpc.SendAssetOORResponse{
			Status:           replayStatus,
			SessionId:        sessionID.String(),
			FailedBeforePonr: prePONR,
		}, true, nil
	}

	resp := &waverpc.SendAssetOORResponse{
		Status:    "submitted",
		SessionId: sessionID.String(),
	}
	if hasReplay {
		op := wire.OutPoint{
			Hash:  chainhash.Hash(sessionID),
			Index: replay.OutputIndex,
		}
		resp.RecipientOutpoint = op.String()
		resp.SelectedInputCount = replay.SelectedInputCount
		resp.AssetChangeAmount = replay.AssetChangeAmount
		resp.SenderBtcChangeSat = replay.SenderBtcChangeSat
	}

	return resp, true, nil
}

// resolveAssetOORRecipientPolicy converts the SendAssetOOR destination oneof
// into a validated uncomposed Ark VTXO policy template.
func resolveAssetOORRecipientPolicy(req *waverpc.SendAssetOORRequest,
	terms *types.OperatorTerms) ([]byte, error) {

	switch d := req.Destination.(type) {
	case *waverpc.SendAssetOORRequest_Pubkey:
		if len(d.Pubkey) == 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"recipient pubkey must be provided")
		}
		recipientKey, err := schnorr.ParsePubKey(d.Pubkey)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid recipient pubkey: %v", err)
		}

		policyBytes, err := arkscript.EncodeStandardVTXOTemplate(
			recipientKey, terms.PubKey, terms.VTXOExitDelay,
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode "+
				"recipient policy template: %v", err)
		}

		return policyBytes, nil

	case *waverpc.SendAssetOORRequest_PolicyTemplate:
		if len(d.PolicyTemplate) == 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"recipient policy_template must be provided")
		}
		template, err := arkscript.DecodePolicyTemplate(
			d.PolicyTemplate,
		)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"decode recipient policy_template: %v", err)
		}
		if _, err := template.Compile(); err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"compile recipient policy_template: %v", err)
		}

		nodes := make([]arkscript.Node, len(template.Leaves))
		for i, leaf := range template.Leaves {
			nodes[i] = leaf.Node
		}
		if err := arkscript.ValidatePolicy(
			nodes, arkscript.PolicyValidationOpts{
				OperatorKey: terms.PubKey,
			},
		); err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"policy violates Ark invariants: %v", err)
		}

		return bytes.Clone(d.PolicyTemplate), nil

	default:
		return nil, status.Errorf(codes.InvalidArgument, "recipient "+
			"destination (pubkey or policy_template) is required")
	}
}

// deriveAssetOORTransferDigest derives a non-zero 32-byte digest scoping the
// OP_TRUE script keys of this transfer's asset outputs.
func deriveAssetOORTransferDigest(assetRef string, amount uint64,
	recipientPolicy []byte, inputs []oor.TransferInput,
	floatOutpoint wire.OutPoint,
	idempotencyKey string) (tapsdk.Hash, error) {

	h := sha256.New()
	_, _ = h.Write([]byte("wavelength/asset-oor/v1"))
	_, _ = h.Write([]byte(assetRef))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], amount)
	_, _ = h.Write(buf[:])
	_, _ = h.Write(recipientPolicy)
	for _, in := range inputs {
		_, _ = h.Write(in.VTXO.Outpoint.Hash[:])
		binary.BigEndian.PutUint32(buf[:4], in.VTXO.Outpoint.Index)
		_, _ = h.Write(buf[:4])
	}
	_, _ = h.Write(floatOutpoint.Hash[:])
	binary.BigEndian.PutUint32(buf[:4], floatOutpoint.Index)
	_, _ = h.Write(buf[:4])

	if idempotencyKey != "" {
		_, _ = h.Write([]byte(idempotencyKey))
	} else {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return tapsdk.Hash{}, err
		}
		_, _ = h.Write(nonce[:])
	}

	var digest tapsdk.Hash
	copy(digest[:], h.Sum(nil))
	if digest == (tapsdk.Hash{}) {
		digest[0] = 1
	}

	return digest, nil
}

// findAssetOORRecipientOutput returns the canonical recipient output entry
// created for the requested recipient policy, asset reference, and amount.
func findAssetOORRecipientOutput(recipients []oortx.RecipientOutput,
	recipientPolicy []byte, assetRef string,
	amount uint64) (oortx.RecipientOutput, error) {

	for _, r := range recipients {
		if r.Asset == nil {
			continue
		}
		if r.Asset.Ref == assetRef && r.Asset.Amount == amount &&
			bytes.Equal(r.VTXOPolicyTemplate, recipientPolicy) {
			return r, nil
		}
	}

	return oortx.RecipientOutput{}, fmt.Errorf("recipient asset output " +
		"not found in built transfer")
}

// toCompatLockedVTXOs adapts a SelectAndLockAssetVTXOsResponse into the
// SelectAndLockVTXOsResponse shape expected by unlockSelectedVTXOsBestEffort
// and cleanupSubmittedOORStart.
func toCompatLockedVTXOs(
	locked *wallet.SelectAndLockAssetVTXOsResponse,
) *wallet.SelectAndLockVTXOsResponse {

	if locked == nil {
		return nil
	}

	selected := make([]wallet.SelectedVTXO, 0, len(locked.SelectedVTXOs))
	var total btcutil.Amount
	for _, sv := range locked.SelectedVTXOs {
		selected = append(selected, wallet.SelectedVTXO{
			Outpoint:     sv.Outpoint,
			ReserveEpoch: sv.ReserveEpoch,
			Amount:       sv.Amount,
			PkScript:     sv.PkScript,
		})
		total += sv.Amount
	}

	return &wallet.SelectAndLockVTXOsResponse{
		SelectedVTXOs: selected,
		TotalSelected: total,
	}
}

// lineageAnchorsFromAncestry converts VTXO ancestry fragments into
// tapassets.OORLineageAnchor entries for OOR proof verification.
func lineageAnchorsFromAncestry(
	ancestry []vtxo.Ancestry) []tapassets.OORLineageAnchor {

	anchors := make([]tapassets.OORLineageAnchor, 0, len(ancestry))
	for _, anc := range ancestry {
		if anc.CommitmentTxID == (chainhash.Hash{}) ||
			anc.TreePath == nil {

			continue
		}
		anchors = append(anchors, tapassets.OORLineageAnchor{
			CommitmentTxID: anc.CommitmentTxID,
			TreePath:       anc.TreePath,
		})
	}

	return anchors
}

// unsignedTxsFromPSBTs extracts unsigned transactions from a slice of PSBTs.
func unsignedTxsFromPSBTs(packets []*psbt.Packet) ([]*wire.MsgTx, error) {
	txs := make([]*wire.MsgTx, 0, len(packets))
	for i, pkt := range packets {
		if pkt == nil || pkt.UnsignedTx == nil {
			return nil, fmt.Errorf("psbt %d has nil unsigned tx", i)
		}
		txs = append(txs, pkt.UnsignedTx)
	}

	return txs, nil
}
