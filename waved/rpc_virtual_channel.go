package waved

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lndbackend"
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

// OpenVirtualChannel negotiates an lnd channel backed by existing VTXO(s).
func (r *RPCServer) OpenVirtualChannel(ctx context.Context,
	req *daemonrpc.OpenVirtualChannelRequest) (
	*daemonrpc.OpenVirtualChannelResponse, error) {

	if r.server == nil || r.server.cfg == nil {
		return nil, status.Errorf(codes.Internal, "server not "+
			"initialized")
	}
	if r.server.cfg.Wallet.Type != WalletTypeLnd {
		return nil, status.Errorf(codes.FailedPrecondition, "virtual "+
			"channels require wallet.type=%s", WalletTypeLnd)
	}
	if !r.server.lnd.IsSome() {
		return nil, status.Errorf(codes.FailedPrecondition, "lnd "+
			"wallet not connected")
	}
	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal, "VTXO store not "+
			"initialized")
	}
	if r.server.vcStore == nil {
		return nil, status.Errorf(codes.Internal, "virtual channel "+
			"store not initialized")
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
			return nil, status.Errorf(codes.InvalidArgument,
				"backing VTXO %s amount hint %d does not "+
					"match stored amount %d", op, input.AmountSat,
				desc.Amount)
		}

		selected = append(selected, virtualChannelSelection{
			rpc: &daemonrpc.VirtualChannelSelectedVTXO{
				Outpoint:  op.String(),
				AmountSat: int64(desc.Amount),
			},
			funding: virtualchannel.FundingInput{
				BackingVTXO: virtualchannel.BackingVTXO{
					OutPoint: op,
					Amount:   desc.Amount,
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
	selected []virtualChannelSelection) []*daemonrpc.VirtualChannelSelectedVTXO {

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
			Outpoint: selectedVTXO.funding.OutPoint.String(),
			AmountSat: int64(
				selectedVTXO.funding.Amount,
			),
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
	selected []virtualChannelSelection) *arkrpc.RegisterVirtualChannelRequest {

	backing := make(
		[]*arkrpc.VirtualChannelBackingVTXO, 0, len(selected),
	)
	for _, selectedVTXO := range selected {
		backing = append(backing, &arkrpc.VirtualChannelBackingVTXO{
			Outpoint: selectedVTXO.funding.OutPoint.String(),
			AmountSat: int64(
				selectedVTXO.funding.Amount,
			),
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

func operatorVirtualChannelCosignRequest(id virtualchannel.ID,
	sigs []virtualchannel.InputSignature) *arkrpc.CosignVirtualChannelBackingRequest {

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
