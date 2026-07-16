package waved

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

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

const (
	virtualChannelBackingMarginSat = 1_000
	maxVirtualChannelCapacitySat   = int64(btcutil.MaxSatoshi) -
		virtualChannelBackingMarginSat
	receiveChannelDispatchTimeout = 30 * time.Second
	receiveChannelArmTimeout      = 2 * time.Minute
)

// promoteVTXORequest is the fully derived promotion contract. The public RPC
// only supplies an amount and idempotency key; peer identity, balance split,
// privacy, and zero-conf behavior are protocol invariants.
type promoteVTXORequest struct {
	PeerNodePubkey []byte
	CapacitySat    int64
	IdempotencyKey string
}

type roundVirtualChannelCoordinator struct {
	server *Server
}

// ArmRoundVirtualChannel negotiates a no-publish lnd channel backed by
// the exact VTXO leaf produced by the current round before final input
// signatures are released to the operator.
func (a *roundVirtualChannelCoordinator) ArmRoundVirtualChannel(
	ctx context.Context,
	req round.RoundVirtualChannelBindingRequest) error {

	if a == nil || a.server == nil {
		return fmt.Errorf("server is not initialized")
	}
	if req.VTXORequest.VirtualChannel == nil {
		return fmt.Errorf("virtual channel intent is missing")
	}
	ctx, cancel := a.server.virtualChannelOperationContext(
		ctx, receiveChannelArmTimeout,
	)
	defer cancel()

	vcReq := &daemonrpc.RegisterReceiveChannelIntentRequest{
		AmountSat:      int64(req.VTXORequest.VirtualChannel.Capacity),
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
		ctx, vcReq, selected, req.RoundID.String(),
	)

	return err
}

// ConfirmRoundVirtualChannels moves only the matching receive-channel intents
// from armed to active after the client round FSM observes confirmation.
func (a *roundVirtualChannelCoordinator) ConfirmRoundVirtualChannels(
	ctx context.Context, roundID round.RoundID) error {

	if a == nil || a.server == nil || a.server.vcStore == nil {
		return fmt.Errorf("virtual channel store is not initialized")
	}

	_, err := a.server.vcStore.ConfirmRoundVirtualChannels(
		ctx, roundID.String(),
	)

	return err
}

// FailRoundVirtualChannels makes an exact failed round terminal for its bound
// virtual channels so lnd's activation gate cannot wait forever.
func (a *roundVirtualChannelCoordinator) FailRoundVirtualChannels(
	ctx context.Context, roundID round.RoundID) error {

	if a == nil || a.server == nil || a.server.vcStore == nil {
		return fmt.Errorf("virtual channel store is not initialized")
	}

	_, err := a.server.vcStore.FailRoundVirtualChannels(
		ctx, roundID.String(),
	)

	return err
}

// OpenVirtualChannel negotiates an lnd channel backed by existing VTXO(s).
func (r *RPCServer) OpenVirtualChannel(ctx context.Context,
	rpcReq *daemonrpc.OpenVirtualChannelRequest) (
	*daemonrpc.OpenVirtualChannelResponse, error) {

	if err := r.validateVirtualChannelOpenState(); err != nil {
		return nil, err
	}
	if err := validateOpenVirtualChannelRequest(rpcReq); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	req, err := r.derivePromoteVTXORequest(ctx, rpcReq)
	if err != nil {
		return nil, err
	}

	selected, locked, err := r.selectVirtualChannelVTXOs(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(selected) != 1 {
		return nil, status.Error(
			codes.InvalidArgument,
			"promoting a channel requires exactly one VTXO",
		)
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
	expectedBacking := btcutil.Amount(req.CapacitySat) +
		virtualChannelBackingMarginSat
	if total != expectedBacking {
		return nil, status.Errorf(codes.Internal, "selected VTXO "+
			"value %d does not equal channel capacity plus "+
			"backing margin %d", total, expectedBacking)
	}

	resp := &daemonrpc.OpenVirtualChannelResponse{
		SelectedVtxos:  virtualChannelSelectedRPC(selected),
		CapacitySat:    req.CapacitySat,
		IdempotencyKey: req.IdempotencyKey,
	}
	operatorClient := r.server.operatorArkClient()
	if operatorClient == nil {
		return nil, status.Errorf(codes.Unavailable, "operator "+
			"connection not initialized")
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

	reg, completed, err := r.negotiatePromotedVirtualChannel(
		ctx, req, selected, peer, operatorClient,
	)
	if err != nil {
		return nil, err
	}
	if completed != nil {
		releaseLocks = false

		return completed, nil
	}
	_, signedBackingTx, err := r.armPromotedVirtualChannel(
		ctx, reg, selected, operatorClient,
	)
	if err != nil {
		return nil, err
	}
	releaseLocks = false

	resp.Status = string(virtualchannel.StatusActive)
	resp.VirtualChannelId = reg.ID[:]
	resp.PendingChannelId = reg.PendingChannelID[:]
	resp.ChannelPoint = reg.ChannelPoint.String()
	resp.BackingTx = signedBackingTx
	resp.FundingPsbt = reg.FundingPsbt

	return resp, nil
}

// negotiatePromotedVirtualChannel registers the pending channel on both peers
// and waits until lnd has made the expected funding outpoint durable.
func (r *RPCServer) negotiatePromotedVirtualChannel(ctx context.Context,
	req *promoteVTXORequest, selected []virtualChannelSelection,
	peer route.Vertex, operatorClient arkrpc.ArkServiceClient) (
	virtualchannel.Registration, *daemonrpc.OpenVirtualChannelResponse,
	error) {

	lndSvc := r.server.lnd.UnsafeFromSome()
	pendingID := promotedChannelPendingID(req, lndSvc.NodePubkey[:])
	var (
		reg                    virtualchannel.Registration
		verifyPersistedFunding bool
	)
	existing, found, err := r.server.vcStore.
		FindVirtualChannelByPendingChannelID(
			ctx, pendingID,
		)
	if err != nil {
		return reg, nil, status.Errorf(codes.Internal, "find promoted "+
			"channel: %v", err)
	}
	if found {
		matches := promotedChannelRegistrationMatches(
			existing, req, selected,
		)
		if !matches {
			return reg, nil, status.Error(
				codes.AlreadyExists,
				"idempotency key is bound to another channel",
			)
		}

		switch existing.Status {
		case virtualchannel.StatusActive:
			response, err := promotedChannelResponse(
				existing, req.IdempotencyKey,
			)

			return reg, response, err

		case virtualchannel.StatusBackingArmed:
			_, err := r.server.vcStore.ActivateVirtualChannel(
				ctx, existing.ID,
			)
			if err != nil {
				return reg, nil, status.Errorf(codes.Internal,
					"resume promoted channel "+
						"activation: %v", err)
			}
			existing, err = r.server.vcStore.GetVirtualChannel(
				ctx, existing.ID,
			)
			if err != nil {
				return reg, nil, status.Errorf(codes.Internal,
					"reload promoted channel: %v", err)
			}
			response, err := promotedChannelResponse(
				existing, req.IdempotencyKey,
			)

			return reg, response, err

		case virtualchannel.StatusLNDNegotiating:
			reg = existing.Registration
			verifyPersistedFunding = true

		case virtualchannel.StatusFundingVerified:
			reg = existing.Registration

		default:
			return reg, nil, status.Errorf(codes.FailedPrecondition,
				"promoted channel is %s", existing.Status)
		}
	} else {
		pendingReq := operatorVirtualChannelPendingOpenRequest(
			pendingID, req, lndSvc.NodePubkey[:], selected,
		)
		if _, err := operatorClient.RegisterVirtualChannel(
			ctx, pendingReq,
		); err != nil {
			return reg, nil, proxyUpstreamError(
				err, "RegisterVirtualChannel pending failed",
			)
		}
		localPending := clientPromotedVirtualChannelPendingOpen(
			pendingID, req, selected,
		)
		if err := r.server.vcStore.InsertVirtualChannelPendingOpen(
			ctx, localPending,
		); err != nil {
			return reg, nil, status.Errorf(codes.Internal,
				"persist local virtual channel intent: %v", err)
		}
		_, err = r.server.vcStore.MarkVirtualChannelLNDNegotiating(
			ctx, pendingID,
		)
		if err != nil {
			return reg, nil, status.Errorf(codes.Internal, "arm "+
				"local virtual channel funding: %v", err)
		}

		persistFundingIntent := func(persistCtx context.Context,
			result *virtualchannel.ActivationResult) error {

			reg = virtualChannelRegistration(req, result, selected)

			return r.server.vcStore.InsertVirtualChannel(
				persistCtx, reg,
			)
		}
		_, err = virtualchannel.ActivateNoPublishFunding(
			ctx, lndSvc.Client, virtualchannel.ActivationRequest{
				Peer: peer,
				Capacity: btcutil.Amount(
					req.CapacitySat,
				),
				PushAmount:       0,
				Private:          true,
				PendingChannelID: pendingID,
				BackingInputs: virtualChannelFundingInputs(
					selected,
				),
				UpdateTimeout:        30 * time.Second,
				PersistFundingIntent: persistFundingIntent,
			},
		)
	}
	if err == nil && verifyPersistedFunding {
		err = virtualchannel.ConfirmNoPublishFunding(
			ctx, lndSvc.Client, reg.PendingChannelID,
			reg.FundingPsbt, reg.ChannelPoint, 30*time.Second,
		)
	}
	if err != nil {
		return reg, nil, status.Errorf(codes.Internal, "lnd virtual "+
			"funding activation failed: %v", err)
	}
	channel, err := virtualchannel.WaitForFundingVerified(
		ctx, r.server.vcStore, reg.ID,
	)
	if err != nil {
		return reg, nil, status.Errorf(codes.FailedPrecondition,
			"wait for local lnd channel registration: %v", err)
	}

	return channel.Registration, nil, nil
}

// armPromotedVirtualChannel exchanges the backing signatures and makes the
// signed cooperative parent durable on both peers.
func (r *RPCServer) armPromotedVirtualChannel(ctx context.Context,
	reg virtualchannel.Registration, selected []virtualChannelSelection,
	operatorClient arkrpc.ArkServiceClient) (*virtualchannel.Channel,
	[]byte, error) {

	lndSvc := r.server.lnd.UnsafeFromSome()
	var backingTx bytes.Buffer
	if err := reg.BackingTx.Serialize(&backingTx); err != nil {
		return nil, nil, status.Errorf(codes.Internal, "serialize "+
			"backing transaction: %v", err)
	}

	operatorReq := operatorVirtualChannelRegistrationRequest(
		reg, lndSvc.NodePubkey[:], selected, backingTx.Bytes(),
	)
	if _, err := operatorClient.RegisterVirtualChannel(
		ctx, operatorReq,
	); err != nil {
		return nil, nil, proxyUpstreamError(
			err, "RegisterVirtualChannel failed",
		)
	}

	signer := lndbackend.NewClientWallet(lndSvc.Signer, lndSvc.WalletKit)
	sigs, err := virtualchannel.SignBackingInputs(
		signer, reg.BackingTx, virtualChannelSigningInputs(selected),
	)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "sign virtual "+
			"channel backing tx: %v", err)
	}

	cosignResp, err := operatorClient.CosignVirtualChannelBacking(
		ctx, operatorVirtualChannelCosignRequest(reg.ID, sigs),
	)
	if err != nil {
		return nil, nil, proxyUpstreamError(
			err, "CosignVirtualChannelBacking failed",
		)
	}

	signedBackingTx := wire.NewMsgTx(2)
	if err := signedBackingTx.Deserialize(
		bytes.NewReader(cosignResp.SignedBackingTx),
	); err != nil {
		return nil, nil, status.Errorf(codes.Internal, "decode signed "+
			"backing transaction: %v", err)
	}

	channel, err := r.persistClientVirtualChannelBacking(
		ctx, reg.ID, signedBackingTx,
	)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "persist "+
			"virtual channel: %v", err)
	}
	if channel.Status != virtualchannel.StatusActive {
		return nil, nil, status.Errorf(codes.FailedPrecondition,
			"promoted virtual channel stopped at %s",
			channel.Status)
	}

	return channel, cosignResp.SignedBackingTx, nil
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

// RegisterReceiveChannelIntent registers an operator-funded channel output in
// the next round. Existing VTXOs use OpenVirtualChannel instead.
func (r *RPCServer) RegisterReceiveChannelIntent(ctx context.Context,
	req *daemonrpc.RegisterReceiveChannelIntentRequest) (
	*daemonrpc.RegisterReceiveChannelIntentResponse, error) {

	if err := r.validateVirtualChannelOpenState(); err != nil {
		return nil, err
	}
	if err := validateRegisterReceiveChannelIntentRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	requestKey, err := receiveChannelRequestKey(req.IdempotencyKey)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	req.IdempotencyKey = requestKey
	lndSvc := r.server.lnd.UnsafeFromSome()
	pendingID := receiveChannelPendingID(requestKey, lndSvc.NodePubkey[:])

	existing, found, err := r.server.vcStore.
		FindVirtualChannelByPendingChannelID(
			ctx, pendingID,
		)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "find receive "+
			"channel: %v", err)
	}
	if found {
		if existing.Kind != virtualchannel.KindReceiveChannel ||
			existing.Role != virtualchannel.RoleClient ||
			existing.Capacity != btcutil.Amount(req.AmountSat) {
			return nil, status.Error(
				codes.AlreadyExists,
				"idempotency key is bound to another channel",
			)
		}

		return receiveChannelRegistrationResponse(existing, requestKey)
	}

	pending, found, err := r.server.vcStore.FindVirtualChannelPendingOpen(
		ctx, pendingID,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "find receive "+
			"request: %v", err)
	}
	if found {
		if !receiveChannelRequestMatches(
			pending, requestKey, req.AmountSat,
		) {
			return nil, status.Error(
				codes.AlreadyExists,
				"idempotency key is bound to another channel",
			)
		}
		if pending.Status != virtualchannel.StatusRequested &&
			pending.Status != virtualchannel.StatusRoundRequested {
			return receiveChannelPendingResponse(pending)
		}
	} else {
		operatorClient := r.server.operatorArkClient()
		if operatorClient == nil {
			return nil, status.Error(
				codes.FailedPrecondition,
				"operator connection not initialized",
			)
		}
		operatorInfo, err := operatorClient.GetInfo(
			ctx, &arkrpc.GetInfoRequest{},
		)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable,
				"operator GetInfo: %v", err)
		}
		if _, err := btcec.ParsePubKey(
			operatorInfo.LndNodePubkey,
		); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"operator lnd_node_pubkey is invalid: %v", err)
		}

		var remote virtualchannel.NodePubKey
		copy(remote[:], operatorInfo.LndNodePubkey)
		pending = &virtualchannel.PendingOpen{
			Kind:             virtualchannel.KindReceiveChannel,
			RequestKey:       requestKey,
			StateVersion:     1,
			PendingChannelID: pendingID,
			RemoteNodePubKey: remote,
			Role:             virtualchannel.RoleClient,
			Status:           virtualchannel.StatusRequested,
			Capacity:         btcutil.Amount(req.AmountSat),
			LocalBalance:     0,
			RemoteBalance:    btcutil.Amount(req.AmountSat),
		}
		if err := r.server.vcStore.InsertVirtualChannelPendingOpen(
			ctx, *pending,
		); err != nil {
			return nil, status.Errorf(codes.Internal, "persist "+
				"receive channel request: %v", err)
		}
	}

	operationCtx, cancel := r.server.virtualChannelOperationContext(
		ctx, receiveChannelDispatchTimeout,
	)
	defer cancel()
	_, err = r.server.vcStore.MarkVirtualChannelRoundRequested(
		operationCtx, pendingID,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mark receive "+
			"channel round requested: %v", err)
	}
	if _, err := r.requestRoundFundedVirtualChannelIntent(
		operationCtx, req,
	); err != nil {
		return nil, err
	}

	pending, found, err = r.server.vcStore.FindVirtualChannelPendingOpen(
		ctx, pendingID,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload receive "+
			"request: %v", err)
	}
	if !found {
		return nil, status.Error(
			codes.Internal, "receive channel request disappeared",
		)
	}

	return receiveChannelPendingResponse(pending)
}

// virtualChannelOperationContext lets durable channel work outlive an RPC or
// actor delivery while still stopping on daemon shutdown and at a bounded
// deadline.
func (s *Server) virtualChannelOperationContext(requestCtx context.Context,
	timeout time.Duration) (context.Context, context.CancelFunc) {

	baseCtx := s.runCtx
	if baseCtx == nil {
		baseCtx = context.WithoutCancel(requestCtx)
	}

	return context.WithTimeout(baseCtx, timeout)
}

func (r *RPCServer) requestVirtualChannelIntentWithSelection(
	ctx context.Context, req *daemonrpc.RegisterReceiveChannelIntentRequest,
	selected []virtualChannelSelection, roundID string) (
	*daemonrpc.RegisterReceiveChannelIntentResponse, error) {

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
	if err := validateRegisterReceiveChannelIntentRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if roundID == "" {
		return nil, status.Error(
			codes.InvalidArgument, "round id is required",
		)
	}
	if len(selected) != 1 {
		return nil, status.Error(
			codes.InvalidArgument,
			"receive channel requires exactly one round VTXO",
		)
	}

	total := virtualChannelSelectedTotal(selected)
	if total <= btcutil.Amount(req.AmountSat) {
		return nil, status.Errorf(codes.InvalidArgument, "selected "+
			"VTXOs total %d must exceed capacity %d to cover the "+
			"virtual channel backing fee", total, req.AmountSat)
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

	lndSvc := r.server.lnd.UnsafeFromSome()
	reg, completed, err := r.negotiateReceiveVirtualChannel(
		ctx, req, selected, roundID, operatorClient, operatorInfo,
	)
	if err != nil {
		return nil, err
	}
	if completed != nil {
		return completed, nil
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
	channel, err := r.persistClientVirtualChannelBacking(
		ctx, reg.ID, signedBackingTx,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "persist virtual "+
			"channel: %v", err)
	}
	if channel.Status != virtualchannel.StatusBackingArmed {
		return nil, status.Errorf(codes.FailedPrecondition, "receive "+
			"virtual channel stopped at %s", channel.Status)
	}

	return &daemonrpc.RegisterReceiveChannelIntentResponse{
		Status:           string(virtualchannel.StatusBackingArmed),
		VirtualChannelId: reg.ID[:],
		PendingChannelId: reg.PendingChannelID[:],
		ChannelPoint:     reg.ChannelPoint.String(),
		BackingTx:        cosignResp.SignedBackingTx,
		FundingPsbt:      reg.FundingPsbt,
		SelectedVtxos:    virtualChannelSelectedRPC(selected),
		IdempotencyKey:   req.IdempotencyKey,
	}, nil
}

// negotiateReceiveVirtualChannel binds the exact round VTXO and waits until
// both lnd funding flows have made the channel point durable.
func (r *RPCServer) negotiateReceiveVirtualChannel(ctx context.Context,
	req *daemonrpc.RegisterReceiveChannelIntentRequest,
	selected []virtualChannelSelection, roundID string,
	operatorClient arkrpc.ArkServiceClient,
	operatorInfo *arkrpc.GetInfoResponse) (virtualchannel.Registration,
	*daemonrpc.RegisterReceiveChannelIntentResponse, error) {

	lndSvc := r.server.lnd.UnsafeFromSome()
	pendingID := receiveChannelPendingID(
		req.IdempotencyKey, lndSvc.NodePubkey[:],
	)
	var (
		reg                  virtualchannel.Registration
		operatorFundingReady bool
	)
	existing, found, err := r.server.vcStore.
		FindVirtualChannelByPendingChannelID(
			ctx, pendingID,
		)
	if err != nil {
		return reg, nil, status.Errorf(codes.Internal, "find receive "+
			"channel: %v", err)
	}
	if found {
		if !receiveChannelRegistrationMatchesRound(
			existing, req.AmountSat, roundID, selected,
		) {
			return reg, nil, status.Error(
				codes.AlreadyExists, "receive channel is "+
					"bound to another round VTXO",
			)
		}

		switch existing.Status {
		case virtualchannel.StatusBackingArmed,
			virtualchannel.StatusRoundConfirmed,
			virtualchannel.StatusActive:

			response, err := receiveChannelRegistrationResponse(
				existing, req.IdempotencyKey,
			)

			return reg, response, err

		case virtualchannel.StatusLNDNegotiating,
			virtualchannel.StatusFundingVerified:

			reg = existing.Registration

		default:
			return reg, nil, status.Errorf(codes.FailedPrecondition,
				"receive channel is %s", existing.Status)
		}
	} else {
		pending := clientVirtualChannelPendingOpen(
			pendingID, operatorInfo.LndNodePubkey, req, selected,
			roundID,
		)
		if _, err := r.server.vcStore.BindVirtualChannelPendingOpen(
			ctx, pending,
		); err != nil {
			return reg, nil, status.Errorf(codes.FailedPrecondition,
				"bind receive channel to round VTXO: %v", err)
		}
		_, err = r.server.vcStore.MarkVirtualChannelLNDNegotiating(
			ctx, pendingID,
		)
		if err != nil {
			return reg, nil, status.Errorf(codes.Internal, "arm "+
				"receive channel acceptor: %v", err)
		}

		intentResp, err := operatorClient.RequestVirtualChannelIntent(
			ctx, operatorVirtualChannelIntentRequest(
				pendingID, req, lndSvc.NodePubkey[:], selected,
				roundID,
			),
		)
		if err != nil {
			return reg, nil, status.Errorf(codes.FailedPrecondition,
				"operator virtual channel intent: %v", err)
		}
		operatorFundingReady = true

		reg, err = clientVirtualChannelRegistrationFromIntent(
			intentResp, operatorInfo.LndNodePubkey, req, selected,
			roundID,
		)
		if err != nil {
			return reg, nil, status.Error(
				codes.InvalidArgument, err.Error(),
			)
		}
		if reg.PendingChannelID != pendingID {
			return reg, nil, status.Error(
				codes.InvalidArgument, "operator returned "+
					"unexpected pending_channel_id",
			)
		}
		if err := r.server.vcStore.InsertVirtualChannel(
			ctx, reg,
		); err != nil {
			return reg, nil, status.Errorf(codes.Internal,
				"persist virtual channel registration: %v", err)
		}
	}

	if reg.Status != virtualchannel.StatusLNDNegotiating {
		return reg, nil, nil
	}
	if !operatorFundingReady {
		intentResp, err := operatorClient.RequestVirtualChannelIntent(
			ctx, operatorVirtualChannelIntentRequest(
				pendingID, req, lndSvc.NodePubkey[:], selected,
				roundID,
			),
		)
		if err != nil {
			return reg, nil, status.Errorf(codes.FailedPrecondition,
				"resume operator virtual channel intent: %v",
				err)
		}
		resumed, err := clientVirtualChannelRegistrationFromIntent(
			intentResp, operatorInfo.LndNodePubkey, req, selected,
			roundID,
		)
		if err != nil {
			return reg, nil, status.Error(
				codes.InvalidArgument, err.Error(),
			)
		}
		if err := r.server.vcStore.InsertVirtualChannel(
			ctx, resumed,
		); err != nil {
			return reg, nil, status.Errorf(codes.Internal,
				"validate resumed virtual channel: %v", err)
		}
	}

	channel, err := virtualchannel.WaitForFundingVerified(
		ctx, r.server.vcStore, reg.ID,
	)
	if err != nil {
		return reg, nil, status.Errorf(codes.FailedPrecondition,
			"wait for local lnd channel registration: %v", err)
	}

	return channel.Registration, nil, nil
}

// persistClientVirtualChannelBacking commits the exact signed funding parent,
// then applies only the activation edge allowed by the intent kind. Receive
// channels remain armed until their backing round confirms.
func (r *RPCServer) persistClientVirtualChannelBacking(ctx context.Context,
	id virtualchannel.ID, backingTx *wire.MsgTx) (*virtualchannel.Channel,
	error) {

	_, err := r.server.vcStore.ArmVirtualChannelBacking(ctx, id, backingTx)
	if err != nil {
		return nil, err
	}

	channel, err := r.server.vcStore.GetVirtualChannel(ctx, id)
	if err != nil {
		return nil, err
	}
	if channel.Kind == virtualchannel.KindPromoteVTXO &&
		channel.Status == virtualchannel.StatusBackingArmed {

		_, err = r.server.vcStore.ActivateVirtualChannel(ctx, id)
		if err != nil {
			return nil, err
		}
		channel, err = r.server.vcStore.GetVirtualChannel(ctx, id)
		if err != nil {
			return nil, err
		}
	}

	return channel, nil
}

func (r *RPCServer) requestRoundFundedVirtualChannelIntent(ctx context.Context,
	req *daemonrpc.RegisterReceiveChannelIntentRequest) (
	*daemonrpc.RegisterReceiveChannelIntentResponse, error) {

	backingAmount := btcutil.Amount(
		req.AmountSat + virtualChannelBackingMarginSat,
	)

	roundRef := round.NewServiceKey().Ref(r.server.actorSystem)
	future := roundRef.Ask(ctx, &round.RegisterVirtualChannelIntentRequest{
		BackingAmount:  backingAmount,
		Capacity:       btcutil.Amount(req.AmountSat),
		IdempotencyKey: req.IdempotencyKey,
	})
	result := future.Await(ctx)
	if _, err := result.Unpack(); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "register "+
			"round virtual channel intent: %v", err)
	}

	return &daemonrpc.RegisterReceiveChannelIntentResponse{
		Status: "requested",
	}, nil
}

// resumeRequestedReceiveChannels replays the durable handoff that may have
// been interrupted after request persistence but before round registration.
func (s *Server) resumeRequestedReceiveChannels(ctx context.Context) error {
	if s == nil || s.vcStore == nil {
		return fmt.Errorf("virtual channel store is not initialized")
	}

	requested, err := s.vcStore.ListVirtualChannelPendingOpensByStatus(
		ctx, virtualchannel.StatusRequested,
	)
	if err != nil {
		return fmt.Errorf("list requested receive channels: %w", err)
	}
	roundRequested, err := s.vcStore.ListVirtualChannelPendingOpensByStatus(
		ctx, virtualchannel.StatusRoundRequested,
	)
	if err != nil {
		return fmt.Errorf("list round-requested receive channels: %w",
			err)
	}
	pending := make(
		[]*virtualchannel.PendingOpen, 0,
		len(requested)+len(roundRequested),
	)
	pending = append(pending, requested...)
	pending = append(pending, roundRequested...)
	if len(pending) == 0 {
		return nil
	}

	roundRef := round.NewServiceKey().Ref(s.actorSystem)
	for _, intent := range pending {
		if intent.Kind != virtualchannel.KindReceiveChannel ||
			intent.Role != virtualchannel.RoleClient ||
			intent.RequestKey == "" {
			return fmt.Errorf("invalid durable receive channel "+
				"request %x", intent.PendingChannelID)
		}

		_, err = s.vcStore.MarkVirtualChannelRoundRequested(
			ctx, intent.PendingChannelID,
		)
		if err != nil {
			return fmt.Errorf("queue resumed receive channel "+
				"%x: %w", intent.PendingChannelID, err)
		}

		future := roundRef.Ask(
			ctx, &round.RegisterVirtualChannelIntentRequest{
				BackingAmount: intent.Capacity +
					virtualChannelBackingMarginSat,
				Capacity:       intent.Capacity,
				IdempotencyKey: intent.RequestKey,
			},
		)
		result := future.Await(ctx)
		if _, err := result.Unpack(); err != nil {
			return fmt.Errorf("resume receive channel %x: %w",
				intent.PendingChannelID, err)
		}

	}

	s.log.InfoS(ctx, "Resumed receive channel requests",
		"count", len(pending),
	)

	return nil
}

// validateOpenVirtualChannelRequest checks request fields before any wallet
// reservation or durable state transition occurs.
func validateOpenVirtualChannelRequest(
	req *daemonrpc.OpenVirtualChannelRequest) error {

	if req == nil {
		return fmt.Errorf("request is nil")
	}
	if req.AmountSat <= 0 {
		return fmt.Errorf("amount_sat must be positive")
	}
	if req.AmountSat > maxVirtualChannelCapacitySat {
		return fmt.Errorf("amount_sat is too large")
	}
	if len(req.IdempotencyKey) > 256 {
		return fmt.Errorf("idempotency_key must not exceed 256 bytes")
	}

	return nil
}

func (r *RPCServer) derivePromoteVTXORequest(ctx context.Context,
	req *daemonrpc.OpenVirtualChannelRequest) (*promoteVTXORequest, error) {

	operatorClient := r.server.operatorArkClient()
	if operatorClient == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"operator connection not initialized",
		)
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

	requestKey, err := receiveChannelRequestKey(req.IdempotencyKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "derive promotion "+
			"idempotency key: %v", err)
	}

	return &promoteVTXORequest{
		PeerNodePubkey: bytes.Clone(operatorInfo.LndNodePubkey),
		CapacitySat:    req.AmountSat,
		IdempotencyKey: requestKey,
	}, nil
}

// validateRegisterReceiveChannelIntentRequest checks request fields before any
// wallet reservation or durable state transition occurs.
func validateRegisterReceiveChannelIntentRequest(
	req *daemonrpc.RegisterReceiveChannelIntentRequest) error {

	if req == nil {
		return fmt.Errorf("request is nil")
	}
	if req.AmountSat <= 0 {
		return fmt.Errorf("amount_sat must be positive")
	}
	if req.AmountSat > maxVirtualChannelCapacitySat {
		return fmt.Errorf("amount_sat is too large")
	}
	if len(req.IdempotencyKey) > 256 {
		return fmt.Errorf("idempotency_key must not exceed 256 bytes")
	}

	return nil
}

// selectVirtualChannelVTXOs reserves exactly one live VTXO whose value is the
// requested channel capacity plus the internal funding margin. Requiring an
// exact match prevents the one-output backing transaction from turning excess
// wallet value into an accidental miner fee.
func (r *RPCServer) selectVirtualChannelVTXOs(ctx context.Context,
	req *promoteVTXORequest) ([]virtualChannelSelection,
	*wallet.SelectAndLockVTXOsResponse, error) {

	recovered, ok, err := r.recoverPromotedChannelSelection(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		return recovered, nil, nil
	}

	if !r.server.walletRef.IsSome() {
		return nil, nil, status.Errorf(codes.Internal, "wallet actor "+
			"not initialized")
	}

	target := btcutil.Amount(req.CapacitySat) +
		virtualChannelBackingMarginSat
	live, err := r.server.vtxoStore.ListVTXOsByStatus(
		ctx, clientvtxo.VTXOStatusLive,
	)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "list live "+
			"VTXOs for promotion: %v", err)
	}
	var selectedOutpoint *wire.OutPoint
	for _, candidate := range live {
		if candidate != nil && candidate.Amount == target {
			outpoint := candidate.Outpoint
			selectedOutpoint = &outpoint
			break
		}
	}
	if selectedOutpoint == nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "no "+
			"live VTXO exactly funds a %d sat channel plus the %d "+
			"sat backing margin", req.CapacitySat,
			virtualChannelBackingMarginSat)
	}

	wRef := r.server.walletRef.UnsafeFromSome()
	selectFuture := wRef.Ask(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount:      target,
		RequiredOutpoints: []wire.OutPoint{*selectedOutpoint},
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

	selected := selectedVirtualChannelVTXOs(locked.SelectedVTXOs)
	if len(selected) != 1 {
		return nil, locked, status.Errorf(codes.Internal, "wallet "+
			"reserved %d VTXOs for one exact promotion outpoint",
			len(selected))
	}
	if selected[0].funding.OutPoint != *selectedOutpoint ||
		selected[0].funding.Amount != target {
		return nil, locked, status.Errorf(codes.Internal, "wallet "+
			"reserved unexpected promotion VTXO %s (%d sats)",
			selected[0].funding.OutPoint,
			selected[0].funding.Amount)
	}

	return selected, locked, nil
}

// recoverPromotedChannelSelection returns the backing already owned by a
// durable promotion attempt. This lookup must run before wallet admission
// because the bound VTXO is no longer part of the live selection set.
func (r *RPCServer) recoverPromotedChannelSelection(ctx context.Context,
	req *promoteVTXORequest) ([]virtualChannelSelection, bool, error) {

	requestKey := strings.TrimSpace(req.IdempotencyKey)
	if requestKey == "" {
		return nil, false, nil
	}

	lndSvc := r.server.lnd.UnsafeFromSome()
	pendingID := promotedChannelPendingID(
		req, lndSvc.NodePubkey[:],
	)
	channel, found, err := r.server.vcStore.
		FindVirtualChannelByPendingChannelID(
			ctx, pendingID,
		)
	if err != nil {
		return nil, false, status.Errorf(codes.Internal, "find "+
			"promoted channel: %v", err)
	}
	if found {
		if !promotedChannelRequestMatches(
			req, channel.RemoteNodePubKey, channel.Role,
			channel.Capacity, channel.LocalBalance,
			channel.RemoteBalance, channel.BackingVTXOs,
		) {
			return nil, false, status.Error(
				codes.AlreadyExists,
				"idempotency key is bound to another channel",
			)
		}

		return virtualChannelSelectionsFromBacking(
			channel.BackingVTXOs,
		), true, nil
	}

	pending, found, err := r.server.vcStore.FindVirtualChannelPendingOpen(
		ctx, pendingID,
	)
	if err != nil {
		return nil, false, status.Errorf(codes.Internal, "find "+
			"promoted channel intent: %v", err)
	}
	if !found {
		return nil, false, nil
	}
	if !promotedChannelRequestMatches(
		req, pending.RemoteNodePubKey, pending.Role, pending.Capacity,
		pending.LocalBalance, pending.RemoteBalance,
		pending.BackingVTXOs,
	) {
		return nil, false, status.Error(
			codes.AlreadyExists,
			"idempotency key is bound to another channel",
		)
	}
	if pending.Status == virtualchannel.StatusFailed {
		return nil, false, status.Error(
			codes.FailedPrecondition,
			"promoted channel negotiation has failed",
		)
	}

	return virtualChannelSelectionsFromBacking(
		pending.BackingVTXOs,
	), true, nil
}

func virtualChannelSelectionsFromBacking(
	backing []virtualchannel.BackingVTXO) []virtualChannelSelection {

	selected := make([]virtualChannelSelection, 0, len(backing))
	for _, input := range backing {
		input.PkScript = bytes.Clone(input.PkScript)
		input.PolicyTemplate = bytes.Clone(input.PolicyTemplate)
		selected = append(selected, virtualChannelSelection{
			rpc: &daemonrpc.VirtualChannelSelectedVTXO{
				Outpoint:  input.OutPoint.String(),
				AmountSat: int64(input.Amount),
			},
			funding: virtualchannel.FundingInput{
				BackingVTXO: input,
				PkScript:    bytes.Clone(input.PkScript),
			},
		})
	}

	return selected
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

func virtualChannelRegistration(req *promoteVTXORequest,
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
		Kind:             virtualchannel.KindPromoteVTXO,
		PendingChannelID: result.PendingChannelID,
		ChannelPoint:     result.ChannelPoint,
		RemoteNodePubKey: remote,
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusLNDNegotiating,
		Capacity:         btcutil.Amount(req.CapacitySat),
		LocalBalance:     btcutil.Amount(req.CapacitySat),
		RemoteBalance:    0,
		BackingTx:        result.BackingTx,
		FundingPsbt:      result.FundingPsbt,
		BackingVTXOs:     backing,
	}
}

func clientPromotedVirtualChannelPendingOpen(
	pendingID virtualchannel.PendingChannelID, req *promoteVTXORequest,
	selected []virtualChannelSelection) virtualchannel.PendingOpen {

	var remote virtualchannel.NodePubKey
	copy(remote[:], req.PeerNodePubkey)

	backing := make([]virtualchannel.BackingVTXO, 0, len(selected))
	for _, selectedVTXO := range selected {
		backing = append(backing, selectedVTXO.funding.BackingVTXO)
	}

	return virtualchannel.PendingOpen{
		Kind:             virtualchannel.KindPromoteVTXO,
		RequestKey:       req.IdempotencyKey,
		PendingChannelID: pendingID,
		RemoteNodePubKey: remote,
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusFundingBound,
		Capacity:         btcutil.Amount(req.CapacitySat),
		LocalBalance:     btcutil.Amount(req.CapacitySat),
		RemoteBalance:    0,
		BackingVTXOs:     backing,
	}
}

// clientVirtualChannelPendingOpen creates the client-side acceptor state for
// an operator-liquidity virtual channel.
func clientVirtualChannelPendingOpen(pendingID virtualchannel.PendingChannelID,
	operatorNodePubKey []byte,
	req *daemonrpc.RegisterReceiveChannelIntentRequest,
	selected []virtualChannelSelection,
	roundID string) virtualchannel.PendingOpen {

	var remote virtualchannel.NodePubKey
	copy(remote[:], operatorNodePubKey)

	backing := make([]virtualchannel.BackingVTXO, 0, len(selected))
	for _, selectedVTXO := range selected {
		backing = append(backing, selectedVTXO.funding.BackingVTXO)
	}

	return virtualchannel.PendingOpen{
		Kind:             virtualchannel.KindReceiveChannel,
		RequestKey:       req.IdempotencyKey,
		RoundID:          roundID,
		PendingChannelID: pendingID,
		RemoteNodePubKey: remote,
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusFundingBound,
		Capacity:         btcutil.Amount(req.AmountSat),
		LocalBalance:     0,
		RemoteBalance:    btcutil.Amount(req.AmountSat),
		BackingVTXOs:     backing,
	}
}

// clientVirtualChannelRegistrationFromIntent builds the client-side durable
// registration from the operator's no-publish funding response.
func clientVirtualChannelRegistrationFromIntent(
	resp *arkrpc.RequestVirtualChannelIntentResponse,
	operatorNodePubKey []byte,
	req *daemonrpc.RegisterReceiveChannelIntentRequest,
	selected []virtualChannelSelection,
	roundID string) (virtualchannel.Registration, error) {

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
	if req.AmountSat <= 0 {
		return virtualchannel.Registration{},
			fmt.Errorf("amount_sat must be positive")
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

	reg := virtualchannel.Registration{
		ID:               id,
		Kind:             virtualchannel.KindReceiveChannel,
		RoundID:          roundID,
		StateVersion:     1,
		PendingChannelID: pendingID,
		ChannelPoint:     channelPoint,
		RemoteNodePubKey: remote,
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusLNDNegotiating,
		Capacity:         btcutil.Amount(req.AmountSat),
		LocalBalance:     0,
		RemoteBalance:    btcutil.Amount(req.AmountSat),
		BackingTx:        backingTx,
		FundingPsbt:      resp.FundingPsbt,
		BackingVTXOs:     backing,
	}
	if err := virtualchannel.ValidateBackingTemplate(
		reg, backingTx,
	); err != nil {
		return virtualchannel.Registration{}, fmt.Errorf("invalid "+
			"backing transaction template: %w", err)
	}

	return reg, nil
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
		VirtualChannelId: reg.ID[:],
		PendingChannelId: reg.PendingChannelID[:],
		ChannelPoint:     reg.ChannelPoint.String(),
		ClientNodePubkey: clientNodePubKey,
		CapacitySat:      int64(reg.Capacity),
		BackingTx:        backingTx,
		FundingPsbt:      reg.FundingPsbt,
		BackingVtxos:     backing,
	}
}

func operatorVirtualChannelPendingOpenRequest(
	pendingID virtualchannel.PendingChannelID,
	req *promoteVTXORequest, clientNodePubKey []byte,
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
		PendingChannelId: pendingID[:],
		ClientNodePubkey: clientNodePubKey,
		CapacitySat:      req.CapacitySat,
		BackingVtxos:     backing,
	}
}

func operatorVirtualChannelIntentRequest(
	pendingID virtualchannel.PendingChannelID,
	req *daemonrpc.RegisterReceiveChannelIntentRequest,
	clientNodePubKey []byte,
	selected []virtualChannelSelection,
	roundID string,
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
		CapacitySat:      req.AmountSat,
		BackingVtxos:     backing,
		RoundId:          roundID,
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

// promotedChannelPendingID is stable for an idempotent promotion request.
func promotedChannelPendingID(req *promoteVTXORequest,
	clientNodePubKey []byte) virtualchannel.PendingChannelID {

	hash := sha256.New()
	hash.Write([]byte("wavelength/promoted-channel-pending-id/v1"))
	hash.Write([]byte{0})
	hash.Write(clientNodePubKey)
	hash.Write([]byte{0})

	hash.Write([]byte(strings.TrimSpace(req.IdempotencyKey)))

	var pendingID virtualchannel.PendingChannelID
	copy(pendingID[:], hash.Sum(nil))

	return pendingID
}

// receiveChannelPendingID is stable across RPC and round-FSM retries.
func receiveChannelPendingID(requestKey string,
	clientNodePubKey []byte) virtualchannel.PendingChannelID {

	hash := sha256.New()
	hash.Write([]byte("wavelength/receive-channel-pending-id/v1"))
	hash.Write([]byte{0})
	hash.Write(clientNodePubKey)
	hash.Write([]byte{0})
	hash.Write([]byte(requestKey))

	var pendingID virtualchannel.PendingChannelID
	copy(pendingID[:], hash.Sum(nil))

	return pendingID
}

func receiveChannelRequestKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key != "" {
		return key, nil
	}

	var random [32]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return "", fmt.Errorf("generate receive channel "+
			"request key: %w", err)
	}

	return hex.EncodeToString(random[:]), nil
}

func receiveChannelRequestMatches(pending *virtualchannel.PendingOpen,
	requestKey string, amountSat int64) bool {

	return pending != nil &&
		pending.Kind == virtualchannel.KindReceiveChannel &&
		pending.RequestKey == requestKey &&
		pending.Role == virtualchannel.RoleClient &&
		pending.Capacity == btcutil.Amount(amountSat) &&
		pending.LocalBalance == 0 &&
		pending.RemoteBalance == btcutil.Amount(amountSat)
}

func promotedChannelRegistrationMatches(channel *virtualchannel.Channel,
	req *promoteVTXORequest, selected []virtualChannelSelection) bool {

	if channel == nil || req == nil || len(selected) != 1 ||
		len(channel.BackingVTXOs) != 1 ||
		channel.Kind != virtualchannel.KindPromoteVTXO ||
		channel.Role != virtualchannel.RoleClient ||
		channel.Capacity != btcutil.Amount(req.CapacitySat) ||
		channel.LocalBalance != btcutil.Amount(req.CapacitySat) ||
		channel.RemoteBalance != 0 {
		return false
	}

	var remote virtualchannel.NodePubKey
	copy(remote[:], req.PeerNodePubkey)
	if channel.RemoteNodePubKey != remote {
		return false
	}

	expected := selected[0].funding.BackingVTXO
	actual := channel.BackingVTXOs[0]

	return actual.OutPoint == expected.OutPoint &&
		actual.Amount == expected.Amount &&
		bytes.Equal(actual.PkScript, expected.PkScript) &&
		bytes.Equal(actual.PolicyTemplate, expected.PolicyTemplate)
}

func promotedChannelRequestMatches(req *promoteVTXORequest,
	remote virtualchannel.NodePubKey, role virtualchannel.Role, capacity,
	localBalance, remoteBalance btcutil.Amount,
	backing []virtualchannel.BackingVTXO) bool {

	if req == nil || role != virtualchannel.RoleClient ||
		capacity != btcutil.Amount(req.CapacitySat) ||
		localBalance != btcutil.Amount(req.CapacitySat) ||
		remoteBalance != 0 || len(backing) != 1 {
		return false
	}

	var expectedRemote virtualchannel.NodePubKey
	copy(expectedRemote[:], req.PeerNodePubkey)
	if remote != expectedRemote {
		return false
	}

	return backing[0].Amount == btcutil.Amount(req.CapacitySat)+
		virtualChannelBackingMarginSat
}

func promotedChannelResponse(channel *virtualchannel.Channel,
	requestKey string) (*daemonrpc.OpenVirtualChannelResponse, error) {

	if channel == nil || channel.BackingTx == nil {
		return nil, status.Error(
			codes.Internal,
			"promoted channel registration is incomplete",
		)
	}

	var backing bytes.Buffer
	if err := channel.BackingTx.Serialize(&backing); err != nil {
		return nil, status.Errorf(codes.Internal, "serialize promoted "+
			"channel backing: %v", err)
	}

	return &daemonrpc.OpenVirtualChannelResponse{
		Status:           string(channel.Status),
		VirtualChannelId: channel.ID[:],
		PendingChannelId: channel.PendingChannelID[:],
		ChannelPoint:     channel.ChannelPoint.String(),
		CapacitySat:      int64(channel.Capacity),
		IdempotencyKey:   requestKey,
		SelectedVtxos: []*daemonrpc.VirtualChannelSelectedVTXO{{
			Outpoint:  channel.BackingVTXOs[0].OutPoint.String(),
			AmountSat: int64(channel.BackingVTXOs[0].Amount),
		}},
		BackingTx:   backing.Bytes(),
		FundingPsbt: bytes.Clone(channel.FundingPsbt),
	}, nil
}

func receiveChannelRegistrationMatchesRound(channel *virtualchannel.Channel,
	amountSat int64, roundID string,
	selected []virtualChannelSelection) bool {

	if channel == nil ||
		channel.Kind != virtualchannel.KindReceiveChannel ||
		channel.Role != virtualchannel.RoleClient ||
		channel.Capacity != btcutil.Amount(amountSat) ||
		channel.RoundID != roundID || len(selected) != 1 ||
		len(channel.BackingVTXOs) != 1 {
		return false
	}

	expected := selected[0].funding.BackingVTXO
	actual := channel.BackingVTXOs[0]

	return actual.OutPoint == expected.OutPoint &&
		actual.Amount == expected.Amount &&
		bytes.Equal(actual.PkScript, expected.PkScript) &&
		bytes.Equal(actual.PolicyTemplate, expected.PolicyTemplate)
}

func receiveChannelPendingResponse(pending *virtualchannel.PendingOpen) (
	*daemonrpc.RegisterReceiveChannelIntentResponse, error) {

	if pending == nil {
		return nil, status.Error(
			codes.Internal, "receive channel request is nil",
		)
	}

	return &daemonrpc.RegisterReceiveChannelIntentResponse{
		Status:           string(pending.Status),
		PendingChannelId: pending.PendingChannelID[:],
		IdempotencyKey:   pending.RequestKey,
	}, nil
}

func receiveChannelRegistrationResponse(channel *virtualchannel.Channel,
	requestKey string) (*daemonrpc.RegisterReceiveChannelIntentResponse,
	error) {

	if channel == nil || channel.BackingTx == nil {
		return nil, status.Error(
			codes.Internal,
			"receive channel registration is incomplete",
		)
	}

	var backing bytes.Buffer
	if err := channel.BackingTx.Serialize(&backing); err != nil {
		return nil, status.Errorf(codes.Internal, "serialize receive "+
			"channel backing: %v", err)
	}
	selected := make(
		[]*daemonrpc.VirtualChannelSelectedVTXO, 0,
		len(channel.BackingVTXOs),
	)
	for _, vtxo := range channel.BackingVTXOs {
		selected = append(
			selected, &daemonrpc.VirtualChannelSelectedVTXO{
				Outpoint:  vtxo.OutPoint.String(),
				AmountSat: int64(vtxo.Amount),
			},
		)
	}

	return &daemonrpc.RegisterReceiveChannelIntentResponse{
		Status:           string(channel.Status),
		VirtualChannelId: channel.ID[:],
		PendingChannelId: channel.PendingChannelID[:],
		ChannelPoint:     channel.ChannelPoint.String(),
		SelectedVtxos:    selected,
		BackingTx:        backing.Bytes(),
		FundingPsbt:      bytes.Clone(channel.FundingPsbt),
		IdempotencyKey:   requestKey,
	}, nil
}
