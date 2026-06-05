package waved

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/virtualchannel"
	clientvtxo "github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	daemonrpc "github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type virtualChannelSelection struct {
	rpc     *daemonrpc.VirtualChannelSelectedVTXO
	funding virtualchannel.FundingInput
	desc    *clientvtxo.Descriptor
}

type roundVirtualChannelActivator struct {
	server *Server
}

// ActivateRoundVirtualChannel negotiates a no-publish lnd channel backed by
// the exact VTXO leaf produced by the current round before final input
// signatures are released to the operator.
func (a *roundVirtualChannelActivator) ActivateRoundVirtualChannel(
	ctx context.Context,
	req round.RoundVirtualChannelActivationRequest) error {

	if a == nil || a.server == nil {
		return fmt.Errorf("server is not initialized")
	}
	if req.VTXORequest.VirtualChannel == nil {
		return fmt.Errorf("virtual channel intent is missing")
	}

	vcReq := &daemonrpc.RequestVirtualChannelIntentRequest{
		CapacitySat:    int64(req.VTXORequest.VirtualChannel.Capacity),
		Private:        req.VTXORequest.VirtualChannel.Private,
		ZeroConf:       req.VTXORequest.VirtualChannel.ZeroConf,
		IdempotencyKey: req.VTXORequest.VirtualChannel.IdempotencyKey,
	}

	selected := []virtualChannelSelection{{
		rpc: &daemonrpc.VirtualChannelSelectedVTXO{
			Outpoint:  req.Outpoint.String(),
			AmountSat: int64(req.Amount),
		},
		funding: virtualchannel.FundingInput{
			BackingVTXO: virtualchannel.BackingVTXO{
				OutPoint: req.Outpoint,
				Amount:   req.Amount,
				PkScript: append(
					[]byte(nil), req.PkScript...,
				),
				PolicyTemplate: append(
					[]byte(nil), req.PolicyTemplate...,
				),
			},
			PkScript: append([]byte(nil), req.PkScript...),
		},
		desc: &clientvtxo.Descriptor{
			Outpoint: req.Outpoint,
			Amount:   req.Amount,
			PolicyTemplate: append(
				[]byte(nil), req.PolicyTemplate...,
			),
			PkScript:    append([]byte(nil), req.PkScript...),
			ClientKey:   req.ClientKey,
			OperatorKey: req.VTXORequest.OperatorKey,
		},
	}}

	rpcServer := &RPCServer{server: a.server}
	_, err := rpcServer.requestVirtualChannelIntentWithSelection(
		ctx, vcReq, selected,
	)

	return err
}

// OpenVirtualChannel negotiates an lnd channel backed by existing VTXO(s).
func (r *RPCServer) OpenVirtualChannel(ctx context.Context,
	req *daemonrpc.OpenVirtualChannelRequest) (
	*daemonrpc.OpenVirtualChannelResponse, error) {

	if err := r.validateVirtualChannelOpenState(); err != nil {
		return nil, err
	}
	if err := validateOpenVirtualChannelRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	selected, locked, err := r.selectVirtualChannelVTXOs(ctx, req)
	if err != nil {
		return nil, err
	}
	releaseLocks := locked != nil
	if releaseLocks {
		defer func() {
			if releaseLocks {
				r.unlockSelectedVTXOsBestEffort(ctx, locked)
			}
		}()
	}

	total := virtualChannelSelectedTotal(selected)
	if total < btcutil.Amount(req.CapacitySat) {
		return nil, status.Errorf(codes.InvalidArgument, "selected "+
			"VTXOs total %d sats below channel capacity %d sats",
			total, req.CapacitySat)
	}

	resp := &daemonrpc.OpenVirtualChannelResponse{
		Status:        "preview",
		SelectedVtxos: virtualChannelSelectedRPC(selected),
	}
	if req.DryRun {
		return resp, nil
	}
	operatorClient := r.server.operatorArkClient()
	if operatorClient == nil {
		return nil, status.Errorf(codes.Unavailable, "operator "+
			"connection not initialized")
	}
	if total == btcutil.Amount(req.CapacitySat) {
		return nil, status.Errorf(codes.InvalidArgument, "selected "+
			"VTXOs must exceed channel capacity to pay the "+
			"virtual funding fee")
	}
	if err := r.populateVirtualChannelSelectionDescriptors(
		ctx, selected,
	); err != nil {
		return nil, err
	}

	peer, err := route.NewVertexFromBytes(req.PeerNodePubkey)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"peer_node_pubkey: %v", err)
	}

	lndSvc := r.server.lnd.UnsafeFromSome()
	pendingID, err := randomVirtualChannelPendingID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate pending "+
			"channel id: %v", err)
	}

	pendingReq := operatorVirtualChannelPendingOpenRequest(
		pendingID, req, lndSvc.NodePubkey[:], selected,
	)
	if _, err := operatorClient.RegisterVirtualChannel(
		ctx, pendingReq,
	); err != nil {
		return nil, proxyUpstreamError(
			err, "RegisterVirtualChannel pending failed",
		)
	}

	activation, err := virtualchannel.ActivateNoPublishFunding(
		ctx, lndSvc.Client, virtualchannel.ActivationRequest{
			Peer:             peer,
			Capacity:         btcutil.Amount(req.CapacitySat),
			PushAmount:       btcutil.Amount(req.PushSat),
			Private:          req.Private,
			PendingChannelID: pendingID,
			BackingInputs: virtualChannelFundingInputs(
				selected,
			),
		},
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "lnd virtual "+
			"funding activation failed: %v", err)
	}

	reg := virtualChannelRegistration(req, activation, selected)
	var backingTx bytes.Buffer
	if err := activation.BackingTx.Serialize(&backingTx); err != nil {
		return nil, status.Errorf(codes.Internal, "serialize backing "+
			"transaction: %v", err)
	}

	if err := r.server.vcStore.InsertVirtualChannel(ctx, reg); err != nil {
		return nil, status.Errorf(codes.Internal, "persist virtual "+
			"channel registration: %v", err)
	}

	operatorReq := operatorVirtualChannelRegistrationRequest(
		reg, lndSvc.NodePubkey[:], selected, backingTx.Bytes(),
	)
	if _, err := operatorClient.RegisterVirtualChannel(
		ctx, operatorReq,
	); err != nil {

		_, markErr := r.server.vcStore.MarkVirtualChannelFailed(
			ctx, reg.ID,
		)
		if markErr != nil {
			r.server.log.WarnS(ctx, "Failed to mark virtual "+
				"channel registration failed", markErr)
		}

		return nil, proxyUpstreamError(
			err, "RegisterVirtualChannel failed",
		)
	}

	signer := lndbackend.NewClientWallet(lndSvc.Signer, lndSvc.WalletKit)
	sigs, err := virtualchannel.SignBackingInputs(
		signer, activation.BackingTx,
		virtualChannelSigningInputs(selected),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign virtual "+
			"channel backing tx: %v", err)
	}

	cosignResp, err := operatorClient.CosignVirtualChannelBacking(
		ctx, operatorVirtualChannelCosignRequest(reg.ID, sigs),
	)
	if err != nil {
		return nil, proxyUpstreamError(
			err, "CosignVirtualChannelBacking failed",
		)
	}

	signedBackingTx := wire.NewMsgTx(2)
	if err := signedBackingTx.Deserialize(
		bytes.NewReader(cosignResp.SignedBackingTx),
	); err != nil {
		return nil, status.Errorf(codes.Internal, "decode signed "+
			"backing transaction: %v", err)
	}

	changed, err := r.server.vcStore.MarkVirtualChannelActive(
		ctx, reg.ID, signedBackingTx,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "activate virtual "+
			"channel: %v", err)
	}
	if !changed {
		return nil, status.Errorf(codes.FailedPrecondition, "virtual "+
			"channel is not negotiating or backing txid does not "+
			"match")
	}
	releaseLocks = false

	resp.Status = string(virtualchannel.StatusActive)
	resp.VirtualChannelId = reg.ID[:]
	resp.PendingChannelId = activation.PendingChannelID[:]
	resp.ChannelPoint = activation.ChannelPoint.String()
	resp.BackingTx = cosignResp.SignedBackingTx
	resp.FundingPsbt = activation.FundingPsbt

	return resp, nil
}

func (r *RPCServer) validateVirtualChannelOpenState() error {
	if r.server == nil || r.server.cfg == nil {
		return status.Errorf(codes.Internal, "server not initialized")
	}
	if r.server.cfg.Wallet.Type != WalletTypeLnd {
		return status.Errorf(codes.FailedPrecondition, "virtual "+
			"channels require wallet.type=%s", WalletTypeLnd)
	}
	if !r.server.lnd.IsSome() {
		return status.Errorf(codes.FailedPrecondition, "lnd wallet "+
			"not connected")
	}
	if r.server.vtxoStore == nil {
		return status.Errorf(codes.Internal, "VTXO store not "+
			"initialized")
	}
	if r.server.vcStore == nil {
		return status.Errorf(codes.Internal, "virtual channel store "+
			"not initialized")
	}

	return nil
}

// RequestVirtualChannelIntent negotiates an operator-liquidity virtual channel
// backed by existing VTXO(s).
func (r *RPCServer) RequestVirtualChannelIntent(ctx context.Context,
	req *daemonrpc.RequestVirtualChannelIntentRequest) (
	*daemonrpc.RequestVirtualChannelIntentResponse, error) {

	if r.server == nil || r.server.cfg == nil {
		return nil, status.Errorf(codes.Internal, "server not "+
			"initialized")
	}
	if r.server.cfg.Wallet.Type != WalletTypeLnd {
		return nil, status.Errorf(codes.FailedPrecondition, "virtual "+
			"channels require lnd wallet mode")
	}
	if !r.server.lnd.IsSome() {
		return nil, status.Errorf(codes.FailedPrecondition, "lnd "+
			"services not initialized")
	}
	if r.server.vcStore == nil || r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal, "virtual channel "+
			"stores not initialized")
	}
	if err := validateRequestVirtualChannelIntentRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.RoundFunded {
		return r.requestRoundFundedVirtualChannelIntent(ctx, req)
	}

	openReq := openRequestFromVirtualChannelIntent(req)
	selected, locked, err := r.selectVirtualChannelVTXOs(ctx, openReq)
	if err != nil {
		return nil, err
	}
	if locked != nil {
		defer r.unlockSelectedVTXOsBestEffort(ctx, locked)
	}
	total := virtualChannelSelectedTotal(selected)
	if total <= btcutil.Amount(req.CapacitySat) {
		return nil, status.Errorf(codes.InvalidArgument, "selected "+
			"VTXOs total %d must exceed capacity %d to cover the "+
			"virtual channel backing fee", total, req.CapacitySat)
	}

	resp := &daemonrpc.RequestVirtualChannelIntentResponse{
		Status: "preview",
		SelectedVtxos: virtualChannelSelectedRPC(
			selected,
		),
	}
	if req.DryRun {
		return resp, nil
	}

	if err := r.populateVirtualChannelSelectionDescriptors(
		ctx, selected,
	); err != nil {
		return nil, err
	}

	operatorClient := r.server.operatorArkClient()
	if operatorClient == nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"operator connection not initialized")
	}
	operatorInfo, err := operatorClient.GetInfo(
		ctx, &arkrpc.GetInfoRequest{},
	)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "operator "+
			"GetInfo: %v", err)
	}
	if _, err := btcec.ParsePubKey(operatorInfo.LndNodePubkey); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "operator "+
			"lnd_node_pubkey is invalid: %v", err)
	}

	pendingID, err := randomVirtualChannelPendingID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate pending "+
			"channel id: %v", err)
	}

	lndSvc := r.server.lnd.UnsafeFromSome()
	pending := clientVirtualChannelPendingOpen(
		pendingID, operatorInfo.LndNodePubkey, req, selected,
	)
	if err := r.server.vcStore.InsertVirtualChannelPendingOpen(
		ctx, pending,
	); err != nil {
		return nil, status.Errorf(codes.Internal, "persist virtual "+
			"channel pending open: %v", err)
	}

	intentResp, err := operatorClient.RequestVirtualChannelIntent(
		ctx, operatorVirtualChannelIntentRequest(
			pendingID, req, lndSvc.NodePubkey[:], selected,
		),
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "operator "+
			"virtual channel intent: %v", err)
	}

	reg, err := clientVirtualChannelRegistrationFromIntent(
		intentResp, operatorInfo.LndNodePubkey, req, selected,
	)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if reg.PendingChannelID != pendingID {
		return nil, status.Error(
			codes.InvalidArgument,
			"operator returned unexpected pending_channel_id",
		)
	}
	if err := r.server.vcStore.InsertVirtualChannel(ctx, reg); err != nil {
		return nil, status.Errorf(codes.Internal, "persist virtual "+
			"channel registration: %v", err)
	}

	signer := lndbackend.NewClientWallet(lndSvc.Signer, lndSvc.WalletKit)
	sigs, err := virtualchannel.SignBackingInputs(
		signer, reg.BackingTx, virtualChannelSigningInputs(selected),
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "sign "+
			"virtual channel backing tx: %v", err)
	}
	cosignResp, err := operatorClient.CosignVirtualChannelBacking(
		ctx, operatorVirtualChannelCosignRequest(reg.ID, sigs),
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "cosign "+
			"virtual channel backing tx: %v", err)
	}

	signedBackingTx := wire.NewMsgTx(2)
	if err := signedBackingTx.Deserialize(
		bytes.NewReader(cosignResp.SignedBackingTx),
	); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"signed backing tx from operator: %v", err)
	}
	changed, err := r.server.vcStore.MarkVirtualChannelActive(
		ctx, reg.ID, signedBackingTx,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "activate virtual "+
			"channel: %v", err)
	}
	if !changed {
		return nil, status.Errorf(codes.FailedPrecondition, "virtual "+
			"channel is not negotiating or backing txid does not "+
			"match")
	}

	resp.Status = string(virtualchannel.StatusActive)
	resp.VirtualChannelId = reg.ID[:]
	resp.PendingChannelId = reg.PendingChannelID[:]
	resp.ChannelPoint = reg.ChannelPoint.String()
	resp.BackingTx = cosignResp.SignedBackingTx
	resp.FundingPsbt = reg.FundingPsbt

	return resp, nil
}

func (r *RPCServer) requestVirtualChannelIntentWithSelection(
	ctx context.Context, req *daemonrpc.RequestVirtualChannelIntentRequest,
	selected []virtualChannelSelection) (
	*daemonrpc.RequestVirtualChannelIntentResponse, error) {

	if r.server == nil || r.server.cfg == nil {
		return nil, status.Errorf(codes.Internal, "server not "+
			"initialized")
	}
	if r.server.cfg.Wallet.Type != WalletTypeLnd {
		return nil, status.Errorf(codes.FailedPrecondition, "virtual "+
			"channels require lnd wallet mode")
	}
	if !r.server.lnd.IsSome() {
		return nil, status.Errorf(codes.FailedPrecondition, "lnd "+
			"services not initialized")
	}
	if r.server.vcStore == nil {
		return nil, status.Errorf(codes.Internal, "virtual channel "+
			"store not initialized")
	}
	if err := validateRequestVirtualChannelIntentRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	total := virtualChannelSelectedTotal(selected)
	if total <= btcutil.Amount(req.CapacitySat) {
		return nil, status.Errorf(codes.InvalidArgument, "selected "+
			"VTXOs total %d must exceed capacity %d to cover the "+
			"virtual channel backing fee", total, req.CapacitySat)
	}
	if len(virtualChannelSigningInputs(selected)) != len(selected) {
		return nil, status.Error(
			codes.InvalidArgument,
			"selected VTXOs must include signing metadata",
		)
	}

	operatorClient := r.server.operatorArkClient()
	if operatorClient == nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"operator connection not initialized")
	}
	operatorInfo, err := operatorClient.GetInfo(
		ctx, &arkrpc.GetInfoRequest{},
	)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "operator "+
			"GetInfo: %v", err)
	}
	if _, err := btcec.ParsePubKey(operatorInfo.LndNodePubkey); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "operator "+
			"lnd_node_pubkey is invalid: %v", err)
	}

	pendingID, err := randomVirtualChannelPendingID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate pending "+
			"channel id: %v", err)
	}

	lndSvc := r.server.lnd.UnsafeFromSome()
	pending := clientVirtualChannelPendingOpen(
		pendingID, operatorInfo.LndNodePubkey, req, selected,
	)
	if err := r.server.vcStore.InsertVirtualChannelPendingOpen(
		ctx, pending,
	); err != nil {
		return nil, status.Errorf(codes.Internal, "persist virtual "+
			"channel pending open: %v", err)
	}

	intentResp, err := operatorClient.RequestVirtualChannelIntent(
		ctx, operatorVirtualChannelIntentRequest(
			pendingID, req, lndSvc.NodePubkey[:], selected,
		),
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "operator "+
			"virtual channel intent: %v", err)
	}

	reg, err := clientVirtualChannelRegistrationFromIntent(
		intentResp, operatorInfo.LndNodePubkey, req, selected,
	)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if reg.PendingChannelID != pendingID {
		return nil, status.Error(
			codes.InvalidArgument,
			"operator returned unexpected pending_channel_id",
		)
	}
	if err := r.server.vcStore.InsertVirtualChannel(ctx, reg); err != nil {
		return nil, status.Errorf(codes.Internal, "persist virtual "+
			"channel registration: %v", err)
	}

	signer := lndbackend.NewClientWallet(lndSvc.Signer, lndSvc.WalletKit)
	sigs, err := virtualchannel.SignBackingInputs(
		signer, reg.BackingTx, virtualChannelSigningInputs(selected),
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "sign "+
			"virtual channel backing tx: %v", err)
	}
	cosignResp, err := operatorClient.CosignVirtualChannelBacking(
		ctx, operatorVirtualChannelCosignRequest(reg.ID, sigs),
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "cosign "+
			"virtual channel backing tx: %v", err)
	}

	signedBackingTx := wire.NewMsgTx(2)
	if err := signedBackingTx.Deserialize(
		bytes.NewReader(cosignResp.SignedBackingTx),
	); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"signed backing tx from operator: %v", err)
	}
	changed, err := r.server.vcStore.MarkVirtualChannelActive(
		ctx, reg.ID, signedBackingTx,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "activate virtual "+
			"channel: %v", err)
	}
	if !changed {
		return nil, status.Errorf(codes.FailedPrecondition, "virtual "+
			"channel is not negotiating or backing txid does not "+
			"match")
	}

	return &daemonrpc.RequestVirtualChannelIntentResponse{
		Status:           string(virtualchannel.StatusActive),
		VirtualChannelId: reg.ID[:],
		PendingChannelId: reg.PendingChannelID[:],
		ChannelPoint:     reg.ChannelPoint.String(),
		BackingTx:        cosignResp.SignedBackingTx,
		FundingPsbt:      reg.FundingPsbt,
		SelectedVtxos:    virtualChannelSelectedRPC(selected),
	}, nil
}

func (r *RPCServer) requestRoundFundedVirtualChannelIntent(ctx context.Context,
	req *daemonrpc.RequestVirtualChannelIntentRequest) (
	*daemonrpc.RequestVirtualChannelIntentResponse, error) {

	if req.DryRun {
		return &daemonrpc.RequestVirtualChannelIntentResponse{
			Status: "preview",
		}, nil
	}
	if len(req.BackingVtxos) != 0 {
		return nil, status.Error(
			codes.InvalidArgument,
			"round_funded requests must not specify backing_vtxos",
		)
	}

	backingAmount := btcutil.Amount(req.BackingAmountSat)
	if backingAmount == 0 {
		backingAmount = btcutil.Amount(req.CapacitySat + 1_000)
	}
	if backingAmount <= btcutil.Amount(req.CapacitySat) {
		return nil, status.Error(
			codes.InvalidArgument,
			"backing amount must exceed capacity",
		)
	}

	roundRef := round.NewServiceKey().Ref(r.server.actorSystem)
	future := roundRef.Ask(ctx, &round.RegisterVirtualChannelIntentRequest{
		BackingAmount:  backingAmount,
		Capacity:       btcutil.Amount(req.CapacitySat),
		Private:        req.Private,
		ZeroConf:       req.ZeroConf,
		IdempotencyKey: req.IdempotencyKey,
	})
	result := future.Await(ctx)
	if _, err := result.Unpack(); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "register "+
			"round virtual channel intent: %v", err)
	}

	return &daemonrpc.RequestVirtualChannelIntentResponse{
		Status: "requested",
	}, nil
}

// validateOpenVirtualChannelRequest checks request fields before any wallet
// reservation or durable state transition occurs.
func validateOpenVirtualChannelRequest(
	req *daemonrpc.OpenVirtualChannelRequest) error {

	if req == nil {
		return fmt.Errorf("request is nil")
	}
	if _, err := btcec.ParsePubKey(req.PeerNodePubkey); err != nil {
		return fmt.Errorf("invalid peer_node_pubkey: %w", err)
	}
	if req.CapacitySat <= 0 {
		return fmt.Errorf("capacity_sat must be positive")
	}
	if req.PushSat < 0 {
		return fmt.Errorf("push_sat must be non-negative")
	}
	if req.PushSat > req.CapacitySat {
		return fmt.Errorf("push_sat must not exceed capacity_sat")
	}
	if len(req.BackingVtxos) == 0 {
		return nil
	}

	seen := make(map[wire.OutPoint]struct{}, len(req.BackingVtxos))
	for idx, input := range req.BackingVtxos {
		if input == nil {
			return fmt.Errorf("backing_vtxos[%d] is nil", idx)
		}
		op, err := parseOutpointString(input.Outpoint)
		if err != nil {
			return fmt.Errorf("backing_vtxos[%d]: %w", idx, err)
		}
		if input.AmountSat < 0 {
			return fmt.Errorf("backing_vtxos[%d].amount_sat must "+
				"be non-negative", idx)
		}
		if _, ok := seen[op]; ok {
			return fmt.Errorf("duplicate backing VTXO %s", op)
		}
		seen[op] = struct{}{}
	}

	return nil
}

// validateRequestVirtualChannelIntentRequest checks request fields before any
// wallet reservation or durable state transition occurs.
func validateRequestVirtualChannelIntentRequest(
	req *daemonrpc.RequestVirtualChannelIntentRequest) error {

	if req == nil {
		return fmt.Errorf("request is nil")
	}
	if req.CapacitySat <= 0 {
		return fmt.Errorf("capacity_sat must be positive")
	}
	if !req.ZeroConf {
		return fmt.Errorf("zero_conf must be true")
	}
	if len(req.BackingVtxos) == 0 {
		return nil
	}

	seen := make(map[wire.OutPoint]struct{}, len(req.BackingVtxos))
	for idx, input := range req.BackingVtxos {
		if input == nil {
			return fmt.Errorf("backing_vtxos[%d] is nil", idx)
		}
		op, err := parseOutpointString(input.Outpoint)
		if err != nil {
			return fmt.Errorf("backing_vtxos[%d]: %w", idx, err)
		}
		if input.AmountSat < 0 {
			return fmt.Errorf("backing_vtxos[%d].amount_sat must "+
				"be non-negative", idx)
		}
		if _, ok := seen[op]; ok {
			return fmt.Errorf("duplicate backing VTXO %s", op)
		}
		seen[op] = struct{}{}
	}

	return nil
}

// openRequestFromVirtualChannelIntent adapts the selection-only fields.
func openRequestFromVirtualChannelIntent(
	req *daemonrpc.RequestVirtualChannelIntentRequest,
) *daemonrpc.OpenVirtualChannelRequest {

	return &daemonrpc.OpenVirtualChannelRequest{
		CapacitySat:    req.CapacitySat,
		Private:        req.Private,
		ZeroConf:       req.ZeroConf,
		BackingVtxos:   req.BackingVtxos,
		IdempotencyKey: req.IdempotencyKey,
		DryRun:         req.DryRun,
	}
}

// selectVirtualChannelVTXOs resolves explicit backing VTXOs or selects and
// locks wallet-managed VTXOs for automatic selection.
func (r *RPCServer) selectVirtualChannelVTXOs(ctx context.Context,
	req *daemonrpc.OpenVirtualChannelRequest) ([]virtualChannelSelection,
	*wallet.SelectAndLockVTXOsResponse, error) {

	if len(req.BackingVtxos) > 0 {
		selected, err := r.resolveVirtualChannelVTXOs(ctx, req)

		return selected, nil, err
	}

	if !r.server.walletRef.IsSome() {
		return nil, nil, status.Errorf(codes.Internal, "wallet actor "+
			"not initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()
	selectFuture := wRef.Ask(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount: btcutil.Amount(req.CapacitySat),
	})
	selectResp, err := selectFuture.Await(ctx).Unpack()
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "VTXO "+
			"selection failed: %v", err)
	}

	locked, ok := selectResp.(*wallet.SelectAndLockVTXOsResponse)
	if !ok {
		return nil, nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", selectResp)
	}

	return selectedVirtualChannelVTXOs(locked.SelectedVTXOs), locked, nil
}

// resolveVirtualChannelVTXOs loads caller-specified VTXOs from the durable VTXO
// store and checks optional amount hints.
func (r *RPCServer) resolveVirtualChannelVTXOs(ctx context.Context,
	req *daemonrpc.OpenVirtualChannelRequest) ([]virtualChannelSelection,
	error) {

	selected := make(
		[]virtualChannelSelection, 0,
		len(req.BackingVtxos),
	)
	for idx, input := range req.BackingVtxos {
		op, err := parseOutpointString(input.Outpoint)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"backing_vtxos[%d]: %v", idx, err)
		}

		desc, err := r.server.vtxoStore.GetVTXO(ctx, op)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "load "+
				"backing VTXO %s: %v", op, err)
		}
		if input.AmountSat > 0 &&
			input.AmountSat != int64(desc.Amount) {

			msg := fmt.Sprintf("backing VTXO %s amount hint %d "+
				"does not match stored amount %d", op,
				input.AmountSat, desc.Amount)

			return nil, status.Error(codes.InvalidArgument, msg)
		}

		selected = append(selected, virtualChannelSelection{
			rpc: &daemonrpc.VirtualChannelSelectedVTXO{
				Outpoint:  op.String(),
				AmountSat: int64(desc.Amount),
			},
			funding: virtualchannel.FundingInput{
				BackingVTXO: virtualchannel.BackingVTXO{
					OutPoint:       op,
					Amount:         desc.Amount,
					PkScript:       desc.PkScript,
					PolicyTemplate: desc.PolicyTemplate,
				},
				PkScript: append([]byte(nil), desc.PkScript...),
			},
			desc: desc,
		})
	}

	return selected, nil
}

// populateVirtualChannelSelectionDescriptors loads full VTXO descriptors for
// auto-selected outpoints so the client can produce owner signatures.
func (r *RPCServer) populateVirtualChannelSelectionDescriptors(
	ctx context.Context, selected []virtualChannelSelection) error {

	for idx := range selected {
		if selected[idx].desc != nil {
			continue
		}

		outpoint := selected[idx].funding.OutPoint
		desc, err := r.server.vtxoStore.GetVTXO(ctx, outpoint)
		if err != nil {
			return status.Errorf(codes.NotFound, "load backing "+
				"VTXO %s: %v", outpoint, err)
		}
		if desc.Amount != selected[idx].funding.Amount {
			return status.Errorf(codes.InvalidArgument, "backing "+
				"VTXO %s selected amount %d does not match "+
				"stored amount %d", outpoint,
				selected[idx].funding.Amount, desc.Amount)
		}

		selected[idx].desc = desc
		selected[idx].funding.PkScript = append(
			[]byte(nil), desc.PkScript...,
		)
		selected[idx].funding.BackingVTXO.PkScript = append(
			[]byte(nil), desc.PkScript...,
		)
		selected[idx].funding.BackingVTXO.PolicyTemplate = append(
			[]byte(nil), desc.PolicyTemplate...,
		)
	}

	return nil
}

// selectedVirtualChannelVTXOs converts wallet actor selections to the RPC
// response shape.
func selectedVirtualChannelVTXOs(
	selected []wallet.SelectedVTXO) []virtualChannelSelection {

	resp := make(
		[]virtualChannelSelection, 0, len(selected),
	)
	for _, selectedVTXO := range selected {
		resp = append(resp, virtualChannelSelection{
			rpc: &daemonrpc.VirtualChannelSelectedVTXO{
				Outpoint:  selectedVTXO.Outpoint.String(),
				AmountSat: int64(selectedVTXO.Amount),
			},
			funding: virtualchannel.FundingInput{
				BackingVTXO: virtualchannel.BackingVTXO{
					OutPoint: selectedVTXO.Outpoint,
					Amount:   selectedVTXO.Amount,
					PkScript: selectedVTXO.PkScript,
				},
				PkScript: append(
					[]byte(nil), selectedVTXO.PkScript...,
				),
			},
		})
	}

	return resp
}

// virtualChannelSelectedTotal sums selected backing VTXO values.
func virtualChannelSelectedTotal(
	selected []virtualChannelSelection) btcutil.Amount {

	var total btcutil.Amount
	for _, selectedVTXO := range selected {
		total += selectedVTXO.funding.Amount
	}

	return total
}

func virtualChannelSelectedRPC(
	selected []virtualChannelSelection,
) []*daemonrpc.VirtualChannelSelectedVTXO {

	resp := make(
		[]*daemonrpc.VirtualChannelSelectedVTXO, 0, len(selected),
	)
	for _, selectedVTXO := range selected {
		resp = append(resp, selectedVTXO.rpc)
	}

	return resp
}

func virtualChannelFundingInputs(
	selected []virtualChannelSelection) []virtualchannel.FundingInput {

	inputs := make([]virtualchannel.FundingInput, 0, len(selected))
	for _, selectedVTXO := range selected {
		inputs = append(inputs, selectedVTXO.funding)
	}

	return inputs
}

func virtualChannelSigningInputs(
	selected []virtualChannelSelection) []virtualchannel.SigningInput {

	inputs := make([]virtualchannel.SigningInput, 0, len(selected))
	for _, selectedVTXO := range selected {
		if selectedVTXO.desc == nil {
			continue
		}

		inputs = append(inputs, virtualchannel.SigningInput{
			BackingVTXO: selectedVTXO.funding.BackingVTXO,
			PkScript: append(
				[]byte(nil), selectedVTXO.desc.PkScript...,
			),
			PolicyTemplate: append(
				[]byte(nil),
				selectedVTXO.desc.PolicyTemplate...,
			),
			ClientKey: selectedVTXO.desc.ClientKey,
		})
	}

	return inputs
}

func virtualChannelRegistration(req *daemonrpc.OpenVirtualChannelRequest,
	result *virtualchannel.ActivationResult,
	selected []virtualChannelSelection) virtualchannel.Registration {

	var remote virtualchannel.NodePubKey
	copy(remote[:], req.PeerNodePubkey)

	backing := make([]virtualchannel.BackingVTXO, 0, len(selected))
	for _, selectedVTXO := range selected {
		backing = append(backing, selectedVTXO.funding.BackingVTXO)
	}

	return virtualchannel.Registration{
		ID:               deriveVirtualChannelID(result),
		PendingChannelID: result.PendingChannelID,
		ChannelPoint:     result.ChannelPoint,
		RemoteNodePubKey: remote,
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusNegotiating,
		Capacity:         btcutil.Amount(req.CapacitySat),
		LocalBalance: btcutil.Amount(
			req.CapacitySat - req.PushSat,
		),
		RemoteBalance: btcutil.Amount(req.PushSat),
		BackingTx:     result.BackingTx,
		FundingPsbt:   result.FundingPsbt,
		BackingVTXOs:  backing,
	}
}

// clientVirtualChannelPendingOpen creates the client-side acceptor state for
// an operator-liquidity virtual channel.
func clientVirtualChannelPendingOpen(pendingID virtualchannel.PendingChannelID,
	operatorNodePubKey []byte,
	req *daemonrpc.RequestVirtualChannelIntentRequest,
	selected []virtualChannelSelection) virtualchannel.PendingOpen {

	var remote virtualchannel.NodePubKey
	copy(remote[:], operatorNodePubKey)

	backing := make([]virtualchannel.BackingVTXO, 0, len(selected))
	for _, selectedVTXO := range selected {
		backing = append(backing, selectedVTXO.funding.BackingVTXO)
	}

	return virtualchannel.PendingOpen{
		PendingChannelID: pendingID,
		RemoteNodePubKey: remote,
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusNegotiating,
		Capacity:         btcutil.Amount(req.CapacitySat),
		LocalBalance:     0,
		RemoteBalance:    btcutil.Amount(req.CapacitySat),
		BackingVTXOs:     backing,
	}
}

// clientVirtualChannelRegistrationFromIntent builds the client-side durable
// registration from the operator's no-publish funding response.
func clientVirtualChannelRegistrationFromIntent(
	resp *arkrpc.RequestVirtualChannelIntentResponse,
	operatorNodePubKey []byte,
	req *daemonrpc.RequestVirtualChannelIntentRequest,
	selected []virtualChannelSelection) (virtualchannel.Registration,
	error) {

	if resp == nil {
		return virtualchannel.Registration{}, fmt.Errorf("operator " +
			"response is nil")
	}
	if len(resp.VirtualChannelId) != 32 {
		return virtualchannel.Registration{},
			fmt.Errorf("virtual_channel_id must be 32 bytes")
	}
	if len(resp.PendingChannelId) != 32 {
		return virtualchannel.Registration{},
			fmt.Errorf("pending_channel_id must be 32 bytes")
	}
	if _, err := btcec.ParsePubKey(operatorNodePubKey); err != nil {
		return virtualchannel.Registration{},
			fmt.Errorf("invalid operator lnd_node_pubkey: %w", err)
	}
	if req.CapacitySat <= 0 {
		return virtualchannel.Registration{},
			fmt.Errorf("capacity_sat must be positive")
	}
	if len(resp.FundingPsbt) == 0 {
		return virtualchannel.Registration{},
			fmt.Errorf("funding_psbt is required")
	}

	channelPoint, err := parseOutpointString(resp.ChannelPoint)
	if err != nil {
		return virtualchannel.Registration{},
			fmt.Errorf("invalid channel_point: %w", err)
	}

	backingTx := wire.NewMsgTx(2)
	if err := backingTx.Deserialize(
		bytes.NewReader(resp.BackingTx),
	); err != nil {
		return virtualchannel.Registration{},
			fmt.Errorf("invalid backing_tx: %w", err)
	}
	if backingTx.TxHash() != channelPoint.Hash {
		return virtualchannel.Registration{},
			fmt.Errorf("backing_tx txid does not match " +
				"channel_point")
	}
	if channelPoint.Index >= uint32(len(backingTx.TxOut)) {
		return virtualchannel.Registration{},
			fmt.Errorf("channel_point output index is out of range")
	}

	var (
		id        virtualchannel.ID
		pendingID virtualchannel.PendingChannelID
		remote    virtualchannel.NodePubKey
	)
	copy(id[:], resp.VirtualChannelId)
	copy(pendingID[:], resp.PendingChannelId)
	copy(remote[:], operatorNodePubKey)

	backing := make([]virtualchannel.BackingVTXO, 0, len(selected))
	for _, selectedVTXO := range selected {
		backing = append(backing, selectedVTXO.funding.BackingVTXO)
	}

	return virtualchannel.Registration{
		ID:               id,
		PendingChannelID: pendingID,
		ChannelPoint:     channelPoint,
		RemoteNodePubKey: remote,
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusNegotiating,
		Capacity:         btcutil.Amount(req.CapacitySat),
		LocalBalance:     0,
		RemoteBalance:    btcutil.Amount(req.CapacitySat),
		BackingTx:        backingTx,
		FundingPsbt:      resp.FundingPsbt,
		BackingVTXOs:     backing,
	}, nil
}

func deriveVirtualChannelID(
	result *virtualchannel.ActivationResult) virtualchannel.ID {

	hash := sha256.New()
	hash.Write(result.PendingChannelID[:])
	hash.Write(result.ChannelPoint.Hash[:])
	var index [4]byte
	index[0] = byte(result.ChannelPoint.Index)
	index[1] = byte(result.ChannelPoint.Index >> 8)
	index[2] = byte(result.ChannelPoint.Index >> 16)
	index[3] = byte(result.ChannelPoint.Index >> 24)
	hash.Write(index[:])

	var id virtualchannel.ID
	copy(id[:], hash.Sum(nil))

	return id
}

func operatorVirtualChannelRegistrationRequest(reg virtualchannel.Registration,
	clientNodePubKey []byte, selected []virtualChannelSelection,
	backingTx []byte) *arkrpc.RegisterVirtualChannelRequest {

	backing := make(
		[]*arkrpc.VirtualChannelBackingVTXO, 0, len(selected),
	)
	for _, selectedVTXO := range selected {
		backing = append(backing, &arkrpc.VirtualChannelBackingVTXO{
			Outpoint:       selectedVTXO.funding.OutPoint.String(),
			AmountSat:      int64(selectedVTXO.funding.Amount),
			PkScript:       selectedVTXO.funding.PkScript,
			PolicyTemplate: selectedVTXO.desc.PolicyTemplate,
		})
	}

	return &arkrpc.RegisterVirtualChannelRequest{
		VirtualChannelId:        reg.ID[:],
		PendingChannelId:        reg.PendingChannelID[:],
		ChannelPoint:            reg.ChannelPoint.String(),
		ClientNodePubkey:        clientNodePubKey,
		CapacitySat:             int64(reg.Capacity),
		OperatorLocalBalanceSat: int64(reg.RemoteBalance),
		ClientLocalBalanceSat:   int64(reg.LocalBalance),
		BackingTx:               backingTx,
		FundingPsbt:             reg.FundingPsbt,
		BackingVtxos:            backing,
	}
}

func operatorVirtualChannelPendingOpenRequest(
	pendingID virtualchannel.PendingChannelID,
	req *daemonrpc.OpenVirtualChannelRequest, clientNodePubKey []byte,
	selected []virtualChannelSelection,
) *arkrpc.RegisterVirtualChannelRequest {

	backing := make(
		[]*arkrpc.VirtualChannelBackingVTXO, 0, len(selected),
	)
	for _, selectedVTXO := range selected {
		backing = append(backing, &arkrpc.VirtualChannelBackingVTXO{
			Outpoint:       selectedVTXO.funding.OutPoint.String(),
			AmountSat:      int64(selectedVTXO.funding.Amount),
			PkScript:       selectedVTXO.funding.PkScript,
			PolicyTemplate: selectedVTXO.desc.PolicyTemplate,
		})
	}

	return &arkrpc.RegisterVirtualChannelRequest{
		PendingChannelId:        pendingID[:],
		ClientNodePubkey:        clientNodePubKey,
		CapacitySat:             req.CapacitySat,
		OperatorLocalBalanceSat: req.PushSat,
		ClientLocalBalanceSat:   req.CapacitySat - req.PushSat,
		BackingVtxos:            backing,
	}
}

func operatorVirtualChannelIntentRequest(
	pendingID virtualchannel.PendingChannelID,
	req *daemonrpc.RequestVirtualChannelIntentRequest,
	clientNodePubKey []byte,
	selected []virtualChannelSelection,
) *arkrpc.RequestVirtualChannelIntentRequest {

	backing := make(
		[]*arkrpc.VirtualChannelBackingVTXO, 0, len(selected),
	)
	for _, selectedVTXO := range selected {
		backing = append(backing, &arkrpc.VirtualChannelBackingVTXO{
			Outpoint:       selectedVTXO.funding.OutPoint.String(),
			AmountSat:      int64(selectedVTXO.funding.Amount),
			PkScript:       selectedVTXO.funding.PkScript,
			PolicyTemplate: selectedVTXO.desc.PolicyTemplate,
		})
	}

	return &arkrpc.RequestVirtualChannelIntentRequest{
		PendingChannelId: pendingID[:],
		ClientNodePubkey: clientNodePubKey,
		CapacitySat:      req.CapacitySat,
		Private:          req.Private,
		ZeroConf:         req.ZeroConf,
		BackingVtxos:     backing,
		IdempotencyKey:   req.IdempotencyKey,
	}
}

func operatorVirtualChannelCosignRequest(
	id virtualchannel.ID,
	sigs []virtualchannel.InputSignature,
) *arkrpc.CosignVirtualChannelBackingRequest {

	rpcSigs := make(
		[]*arkrpc.VirtualChannelInputSignature, 0, len(sigs),
	)
	for _, sig := range sigs {
		rpcSigs = append(rpcSigs, &arkrpc.VirtualChannelInputSignature{
			Outpoint:  sig.OutPoint.String(),
			Signature: sig.Signature,
		})
	}

	return &arkrpc.CosignVirtualChannelBackingRequest{
		VirtualChannelId: id[:],
		ClientSignatures: rpcSigs,
	}
}

func randomVirtualChannelPendingID() (virtualchannel.PendingChannelID, error) {
	var pendingID virtualchannel.PendingChannelID
	if _, err := io.ReadFull(rand.Reader, pendingID[:]); err != nil {
		return pendingID, err
	}

	return pendingID, nil
}
