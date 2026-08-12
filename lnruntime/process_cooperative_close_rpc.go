package lnruntime

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	cooperativeClosePeerService = "arkchannelrpc.ArkChannelPeerService"

	closeAckClientSigned    = arkchannelrpc.CooperativeCloseAcknowledgement_COOPERATIVE_CLOSE_ACKNOWLEDGEMENT_CLIENT_SIGNED    //nolint:ll
	closeAckClientFinalized = arkchannelrpc.CooperativeCloseAcknowledgement_COOPERATIVE_CLOSE_ACKNOWLEDGEMENT_CLIENT_FINALIZED //nolint:ll
)

var cooperativeClosePeerMethods = map[string]struct{}{
	"BeginCooperativeClose":       {},
	"CompleteCooperativeClose":    {},
	"AcknowledgeCooperativeClose": {},
	"PublishCooperativeClose":     {},
	"AbortCooperativeClose":       {},
}

// IsCooperativeClosePeerRoute reports whether one mailbox route belongs to the
// narrow direct-close peer protocol.
func IsCooperativeClosePeerRoute(service, method string) bool {
	if service != cooperativeClosePeerService {
		return false
	}

	_, ok := cooperativeClosePeerMethods[method]

	return ok
}

// MailboxCooperativeClosePeer adapts the generated mailbox client to the
// process-owned cooperative-close protocol.
type MailboxCooperativeClosePeer struct {
	client *arkchannelrpc.ArkChannelPeerServiceMailboxClient
}

// NewMailboxCooperativeClosePeer constructs a typed close peer.
func NewMailboxCooperativeClosePeer(client mailboxrpc.RPCClient) (
	*MailboxCooperativeClosePeer, error) {

	if client == nil {
		return nil, fmt.Errorf("cooperative close mailbox client is " +
			"required")
	}

	return &MailboxCooperativeClosePeer{
		client: arkchannelrpc.NewArkChannelPeerServiceMailboxClient(
			client,
		),
	}, nil
}

// BeginCooperativeClose asks the hub to persist and quiesce its endpoint.
func (p *MailboxCooperativeClosePeer) BeginCooperativeClose(ctx context.Context,
	id arkchannel.ID, clientScript []byte, feeRate chainfee.SatPerKWeight) (
	arkchannel.CooperativeCloseRequest, CleanChannelState,
	*arkchannel.CooperativeClose, error) {

	resp, err := p.client.BeginCooperativeClose(
		ctx, &arkchannelrpc.BeginCooperativeCloseRequest{
			ChannelId: id[:],
			ClientDeliveryScript: append(
				[]byte(nil), clientScript...,
			),
			FeeRateSatPerKw: uint64(feeRate),
		}, closeRPCOptions(id, "begin"),
	)
	if err != nil {
		return arkchannel.CooperativeCloseRequest{}, CleanChannelState{}, //nolint:ll
			nil, err
	}
	request, err := cooperativeCloseRequestFromRPC(resp.GetRequest())
	if err != nil {
		return arkchannel.CooperativeCloseRequest{}, CleanChannelState{}, //nolint:ll
			nil, err
	}
	state, err := cleanChannelStateFromRPC(resp.GetCleanState())
	if err != nil {
		return arkchannel.CooperativeCloseRequest{}, CleanChannelState{}, //nolint:ll
			nil, err
	}
	var settlement *arkchannel.CooperativeClose
	if resp.GetSettlement() != nil {
		closeValue, err := cooperativeCloseFromRPC(resp.GetSettlement())
		if err != nil {
			return arkchannel.CooperativeCloseRequest{},
				CleanChannelState{}, nil, err
		}
		settlement = &closeValue
	}

	return request, state, settlement, nil
}

// CompleteCooperativeClose submits the client-first signature to the hub.
func (p *MailboxCooperativeClosePeer) CompleteCooperativeClose(
	ctx context.Context, id arkchannel.ID,
	proposal arkchannel.CooperativeCloseProposal,
	clientSig input.Signature) (arkchannel.CooperativeClose, error) {

	if clientSig == nil {
		return arkchannel.CooperativeClose{}, fmt.Errorf("client " +
			"close signature is required")
	}
	resp, err := p.client.CompleteCooperativeClose(
		ctx, &arkchannelrpc.CompleteCooperativeCloseRequest{
			ChannelId: id[:],
			Proposal: cooperativeCloseProposalToRPC(
				proposal,
			),
			ClientSignature: clientSig.Serialize(),
		}, closeRPCOptions(id, "complete"),
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}

	return cooperativeCloseFromRPC(resp.GetSettlement())
}

// AcknowledgeCooperativeCloseSigned records the client's durable signature
// barrier at the hub.
func (p *MailboxCooperativeClosePeer) AcknowledgeCooperativeCloseSigned(
	ctx context.Context, id arkchannel.ID,
	settlement arkchannel.CooperativeClose) error {

	_, err := p.client.AcknowledgeCooperativeClose(
		ctx, &arkchannelrpc.AcknowledgeCooperativeCloseRequest{
			ChannelId:       id[:],
			Acknowledgement: closeAckClientSigned,
			Settlement:      cooperativeCloseToRPC(settlement),
		}, closeRPCOptions(id, "signed"),
	)

	return err
}

// PublishCooperativeClose tells the hub which client-published settlement was
// confirmed, then waits for hub-local archival.
func (p *MailboxCooperativeClosePeer) PublishCooperativeClose(
	ctx context.Context, id arkchannel.ID, txID chainhash.Hash) (
	chainhash.Hash, error) {

	resp, err := p.client.PublishCooperativeClose(
		ctx, &arkchannelrpc.PublishCooperativeCloseRequest{
			ChannelId: id[:], SettlementTxid: txID[:],
		}, closeRPCOptions(id, "publish"),
	)
	if err != nil {
		return chainhash.Hash{}, err
	}
	if !resp.GetHubFinalized() {
		return chainhash.Hash{}, fmt.Errorf("hub did not finalize " +
			"cooperative close")
	}

	return rpcHash("settlement txid", resp.GetSettlementTxid())
}

// AcknowledgeCooperativeCloseFinalized records local archival at the hub.
func (p *MailboxCooperativeClosePeer) AcknowledgeCooperativeCloseFinalized(
	ctx context.Context, id arkchannel.ID) error {

	_, err := p.client.AcknowledgeCooperativeClose(
		ctx, &arkchannelrpc.AcknowledgeCooperativeCloseRequest{
			ChannelId:       id[:],
			Acknowledgement: closeAckClientFinalized,
		}, closeRPCOptions(id, "finalized"),
	)

	return err
}

// AbortCooperativeClose releases the remote pre-signature barrier.
func (p *MailboxCooperativeClosePeer) AbortCooperativeClose(ctx context.Context,
	id arkchannel.ID) error {

	_, err := p.client.AbortCooperativeClose(
		ctx, &arkchannelrpc.AbortCooperativeCloseRequest{
			ChannelId: id[:],
		}, closeRPCOptions(id, "abort"),
	)

	return err
}

// CooperativeClosePeerRPCServer binds one authenticated client identity to a
// hub process. Channel IDs belonging to another client are rejected before a
// local lnd or signing side effect.
type CooperativeClosePeerRPCServer struct {
	clientNode [33]byte
	hub        *HubCooperativeCloseProcess
}

// NewCooperativeClosePeerRPCServer constructs a client-bound mailbox server.
func NewCooperativeClosePeerRPCServer(clientNode [33]byte,
	hub *HubCooperativeCloseProcess) (*CooperativeClosePeerRPCServer,
	error) {

	if clientNode == ([33]byte{}) {
		return nil, fmt.Errorf("cooperative close client node is " +
			"required")
	}
	if hub == nil {
		return nil, fmt.Errorf("hub cooperative close process is " +
			"required")
	}

	return &CooperativeClosePeerRPCServer{
		clientNode: clientNode,
		hub:        hub,
	}, nil
}

// BeginCooperativeClose persists the request before returning clean lnd state.
func (s *CooperativeClosePeerRPCServer) BeginCooperativeClose(
	ctx context.Context, req *arkchannelrpc.BeginCooperativeCloseRequest) (
	*arkchannelrpc.BeginCooperativeCloseResponse, error) {

	id, err := s.channelID(ctx, req.GetChannelId())
	if err != nil {
		return nil, err
	}
	feeRate, err := rpcFeeRate(req.GetFeeRateSatPerKw())
	if err != nil {
		return nil, err
	}
	request, state, settlement, err := s.hub.BeginCooperativeClose(
		ctx, id, req.GetClientDeliveryScript(), feeRate,
	)
	if err != nil {
		return nil, err
	}
	resp := &arkchannelrpc.BeginCooperativeCloseResponse{
		Request:    cooperativeCloseRequestToRPC(request),
		CleanState: cleanChannelStateToRPC(state),
	}
	if settlement != nil {
		resp.Settlement = cooperativeCloseToRPC(*settlement)
	}

	return resp, nil
}

// CompleteCooperativeClose verifies and completes the client-first transcript.
func (s *CooperativeClosePeerRPCServer) CompleteCooperativeClose(
	ctx context.Context,
	req *arkchannelrpc.CompleteCooperativeCloseRequest) (
	*arkchannelrpc.CompleteCooperativeCloseResponse, error) {

	id, err := s.channelID(ctx, req.GetChannelId())
	if err != nil {
		return nil, err
	}
	proposal, err := cooperativeCloseProposalFromRPC(req.GetProposal())
	if err != nil {
		return nil, err
	}
	clientSig, err := schnorr.ParseSignature(req.GetClientSignature())
	if err != nil {
		return nil, fmt.Errorf("parse client cooperative close "+
			"signature: %w", err)
	}
	settlement, err := s.hub.CompleteCooperativeClose(
		ctx, id, proposal, clientSig,
	)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.CompleteCooperativeCloseResponse{
		Settlement: cooperativeCloseToRPC(settlement),
	}, nil
}

// AcknowledgeCooperativeClose accepts only the two client-owned barrier facts.
func (s *CooperativeClosePeerRPCServer) AcknowledgeCooperativeClose(
	ctx context.Context,
	req *arkchannelrpc.AcknowledgeCooperativeCloseRequest) (
	*arkchannelrpc.AcknowledgeCooperativeCloseResponse, error) {

	id, err := s.channelID(ctx, req.GetChannelId())
	if err != nil {
		return nil, err
	}
	switch req.GetAcknowledgement() {
	case closeAckClientSigned:
		settlement, err := cooperativeCloseFromRPC(req.GetSettlement())
		if err != nil {
			return nil, err
		}
		if err := s.hub.AcknowledgeCooperativeCloseSigned(
			ctx, id, settlement,
		); err != nil {
			return nil, err
		}

	case closeAckClientFinalized:
		if req.GetSettlement() != nil {
			return nil, fmt.Errorf("finalized acknowledgement " +
				"must not carry a settlement")
		}
		if err := s.hub.AcknowledgeCooperativeCloseFinalized(
			ctx, id,
		); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unknown cooperative close " +
			"acknowledgement")
	}
	record, err := s.hubRecord(ctx, id)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.AcknowledgeCooperativeCloseResponse{
		Channel: ArkChannelRecordToRPC(record),
	}, nil
}

// PublishCooperativeClose waits for publication and hub-local archival.
func (s *CooperativeClosePeerRPCServer) PublishCooperativeClose(
	ctx context.Context,
	req *arkchannelrpc.PublishCooperativeCloseRequest) (
	*arkchannelrpc.PublishCooperativeCloseResponse, error) {

	id, err := s.channelID(ctx, req.GetChannelId())
	if err != nil {
		return nil, err
	}
	txID, err := rpcHash(
		"settlement txid", req.GetSettlementTxid(),
	)
	if err != nil {
		return nil, err
	}
	txID, err = s.hub.PublishCooperativeClose(ctx, id, txID)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.PublishCooperativeCloseResponse{
		SettlementTxid: txID[:],
		HubFinalized:   true,
	}, nil
}

// AbortCooperativeClose restores the hub only before a signed artifact exists.
func (s *CooperativeClosePeerRPCServer) AbortCooperativeClose(
	ctx context.Context, req *arkchannelrpc.AbortCooperativeCloseRequest) (
	*arkchannelrpc.AbortCooperativeCloseResponse, error) {

	id, err := s.channelID(ctx, req.GetChannelId())
	if err != nil {
		return nil, err
	}
	if err := s.hub.AbortCooperativeClose(ctx, id); err != nil {
		return nil, err
	}
	record, err := s.hubRecord(ctx, id)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.AbortCooperativeCloseResponse{
		Channel: ArkChannelRecordToRPC(record),
	}, nil
}

// channelID parses the request ID and verifies its authenticated client owner.
func (s *CooperativeClosePeerRPCServer) channelID(ctx context.Context,
	raw []byte) (arkchannel.ID, error) {

	id, err := rpcChannelID(raw)
	if err != nil {
		return arkchannel.ID{}, err
	}
	record, err := s.hubRecord(ctx, id)
	if err != nil {
		return arkchannel.ID{}, err
	}
	if record.Snapshot.Terms.ClientNodeKey != s.clientNode {
		return arkchannel.ID{}, fmt.Errorf("channel belongs to " +
			"another client")
	}

	return id, nil
}

// hubRecord loads one channel from the hub's durable FSM service.
func (s *CooperativeClosePeerRPCServer) hubRecord(ctx context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	service, err := s.hub.stateService()
	if err != nil {
		return arkchannel.Record{}, err
	}

	return service.GetChannel(ctx, id)
}

// ArkChannelRecordToRPC converts the compact durable channel summary exposed
// by local and peer process RPCs.
func ArkChannelRecordToRPC(record arkchannel.Record) *arkchannelrpc.ArkChannel {
	snapshot := record.Snapshot
	channel := &arkchannelrpc.ArkChannel{
		ChannelId:    snapshot.Terms.ID[:],
		Kind:         snapshot.Terms.Kind.String(),
		Phase:        snapshot.Phase.String(),
		Funder:       snapshot.Terms.Funder.String(),
		CapacitySat:  int64(snapshot.Terms.Capacity),
		Revision:     record.Revision,
		ReservedScid: snapshot.Terms.ReservedSCID,
	}
	if snapshot.Source != nil {
		channel.SourceOutpoint = snapshot.Source.OutPoint.String()
	}
	if snapshot.Backing != nil {
		channel.ChannelPoint = snapshot.Backing.ChannelPoint.String()
	}
	if snapshot.CooperativeClose != nil {
		channel.CooperativeCloseTxid =
			snapshot.CooperativeClose.TxID[:]
	}

	return channel
}

// closeRPCOptions derives a stable idempotency key for one channel step.
func closeRPCOptions(id arkchannel.ID, step string) mailboxrpc.RPCOptions {
	return mailboxrpc.RPCOptions{
		IdempotencyKey: "ark-close/" + hex.EncodeToString(id[:]) + "/" +
			step,
	}
}

// rpcChannelID parses a fixed-width channel identifier.
func rpcChannelID(raw []byte) (arkchannel.ID, error) {
	var id arkchannel.ID
	if len(raw) != len(id) {
		return id, fmt.Errorf("channel id must be %d bytes", len(id))
	}
	copy(id[:], raw)

	return id, nil
}

// rpcFeeRate validates a wire fee rate before converting its signed type.
func rpcFeeRate(value uint64) (chainfee.SatPerKWeight, error) {
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("cooperative close fee rate overflows")
	}
	feeRate := chainfee.SatPerKWeight(value)
	if feeRate < chainfee.FeePerKwFloor {
		return 0, fmt.Errorf("cooperative close fee rate is below " +
			"floor")
	}

	return feeRate, nil
}

// rpcAmount rejects negative wire amounts before converting them.
func rpcAmount(name string, value int64) (btcutil.Amount, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}

	return btcutil.Amount(value), nil
}

// rpcHash parses one fixed-width transaction hash.
func rpcHash(name string, raw []byte) (chainhash.Hash, error) {
	var hash chainhash.Hash
	if len(raw) != len(hash) {
		return hash, fmt.Errorf("%s must be %d bytes", name, len(hash))
	}
	copy(hash[:], raw)

	return hash, nil
}

// cooperativeCloseRequestToRPC serializes immutable negotiated close terms.
func cooperativeCloseRequestToRPC(
	request arkchannel.CooperativeCloseRequest) *arkchannelrpc.CooperativeCloseRequest { //nolint:ll

	return &arkchannelrpc.CooperativeCloseRequest{
		ClientDeliveryScript: append(
			[]byte(nil), request.ClientDeliveryScript...,
		),
		HubDeliveryScript: append(
			[]byte(nil), request.HubDeliveryScript...,
		),
		FeeRateSatPerKw: uint64(request.FeeRate),
	}
}

// cooperativeCloseRequestFromRPC validates negotiated close terms from RPC.
func cooperativeCloseRequestFromRPC(
	request *arkchannelrpc.CooperativeCloseRequest) (
	arkchannel.CooperativeCloseRequest, error) {

	if request == nil {
		return arkchannel.CooperativeCloseRequest{}, fmt.Errorf(
			"cooperative close request is required")
	}
	feeRate, err := rpcFeeRate(request.GetFeeRateSatPerKw())
	if err != nil {
		return arkchannel.CooperativeCloseRequest{}, err
	}
	result := arkchannel.CooperativeCloseRequest{
		Initiator: arkchannel.PartyClient,
		ClientDeliveryScript: append(
			[]byte(nil), request.GetClientDeliveryScript()...,
		),
		HubDeliveryScript: append(
			[]byte(nil), request.GetHubDeliveryScript()...,
		),
		FeeRate: feeRate,
	}
	if err := result.Validate(); err != nil {
		return arkchannel.CooperativeCloseRequest{}, err
	}

	return result, nil
}

// cleanChannelStateToRPC serializes one quiesced lnd channel snapshot.
func cleanChannelStateToRPC(
	state CleanChannelState) *arkchannelrpc.CleanChannelState {

	return &arkchannelrpc.CleanChannelState{
		ChannelPointTxid:  state.ChannelPoint.Hash[:],
		ChannelPointIndex: state.ChannelPoint.Index,
		LocalBalanceSat:   int64(state.LocalBalance),
		RemoteBalanceSat:  int64(state.RemoteBalance),
		CapacitySat:       int64(state.Capacity),
		CommitmentHeight:  state.CommitmentHeight,
		LocalInitiator:    state.LocalInitiator,
	}
}

// cleanChannelStateFromRPC validates one quiesced lnd channel snapshot.
func cleanChannelStateFromRPC(state *arkchannelrpc.CleanChannelState) (
	CleanChannelState, error) {

	if state == nil {
		return CleanChannelState{}, fmt.Errorf("clean channel state " +
			"is required")
	}
	hash, err := rpcHash("channel point txid", state.GetChannelPointTxid())
	if err != nil {
		return CleanChannelState{}, err
	}
	local, err := rpcAmount("local balance", state.GetLocalBalanceSat())
	if err != nil {
		return CleanChannelState{}, err
	}
	remote, err := rpcAmount("remote balance", state.GetRemoteBalanceSat())
	if err != nil {
		return CleanChannelState{}, err
	}
	capacity, err := rpcAmount("capacity", state.GetCapacitySat())
	if err != nil {
		return CleanChannelState{}, err
	}

	return CleanChannelState{
		ChannelPoint: wire.OutPoint{
			Hash: hash, Index: state.GetChannelPointIndex(),
		},
		LocalBalance:     local,
		RemoteBalance:    remote,
		Capacity:         capacity,
		CommitmentHeight: state.GetCommitmentHeight(),
		LocalInitiator:   state.GetLocalInitiator(),
	}, nil
}

// cooperativeCloseProposalToRPC serializes the client-signed proposal.
func cooperativeCloseProposalToRPC(
	proposal arkchannel.CooperativeCloseProposal) *arkchannelrpc.CooperativeCloseProposal { //nolint:ll

	return &arkchannelrpc.CooperativeCloseProposal{
		Transaction:      append([]byte(nil), proposal.Transaction...),
		CommitmentHeight: proposal.CommitmentHeight,
		ClientBalanceSat: int64(proposal.ClientBalance),
		HubBalanceSat:    int64(proposal.HubBalance),
		ClientOutputSat:  int64(proposal.ClientOutput),
		HubOutputSat:     int64(proposal.HubOutput),
		FeeSat:           int64(proposal.Fee),
	}
}

// cooperativeCloseProposalFromRPC validates the client-signed proposal.
func cooperativeCloseProposalFromRPC(
	proposal *arkchannelrpc.CooperativeCloseProposal) (
	arkchannel.CooperativeCloseProposal, error) {

	if proposal == nil {
		return arkchannel.CooperativeCloseProposal{}, fmt.Errorf(
			"cooperative close proposal is required")
	}
	clientBalance, err := rpcAmount(
		"client balance", proposal.GetClientBalanceSat(),
	)
	if err != nil {
		return arkchannel.CooperativeCloseProposal{}, err
	}
	hubBalance, err := rpcAmount("hub balance", proposal.GetHubBalanceSat())
	if err != nil {
		return arkchannel.CooperativeCloseProposal{}, err
	}
	clientOutput, err := rpcAmount(
		"client output", proposal.GetClientOutputSat(),
	)
	if err != nil {
		return arkchannel.CooperativeCloseProposal{}, err
	}
	hubOutput, err := rpcAmount("hub output", proposal.GetHubOutputSat())
	if err != nil {
		return arkchannel.CooperativeCloseProposal{}, err
	}
	fee, err := rpcAmount("fee", proposal.GetFeeSat())
	if err != nil {
		return arkchannel.CooperativeCloseProposal{}, err
	}

	return arkchannel.CooperativeCloseProposal{
		Transaction: append(
			[]byte(nil), proposal.GetTransaction()...,
		),
		CommitmentHeight: proposal.GetCommitmentHeight(),
		ClientBalance:    clientBalance,
		HubBalance:       hubBalance,
		ClientOutput:     clientOutput,
		HubOutput:        hubOutput,
		Fee:              fee,
	}, nil
}

// cooperativeCloseToRPC serializes the complete three-signature settlement.
func cooperativeCloseToRPC(
	settlement arkchannel.CooperativeClose) *arkchannelrpc.CooperativeClose { //nolint:ll

	return &arkchannelrpc.CooperativeClose{
		Proposal:    cooperativeCloseProposalToRPC(settlement.Proposal),
		Transaction: append([]byte(nil), settlement.Transaction...),
		Txid:        settlement.TxID[:],
	}
}

// cooperativeCloseFromRPC validates the complete settlement artifact.
func cooperativeCloseFromRPC(settlement *arkchannelrpc.CooperativeClose) (
	arkchannel.CooperativeClose, error) {

	if settlement == nil {
		return arkchannel.CooperativeClose{}, fmt.Errorf(
			"cooperative close settlement is required")
	}
	proposal, err := cooperativeCloseProposalFromRPC(
		settlement.GetProposal(),
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	txID, err := rpcHash("cooperative close txid", settlement.GetTxid())
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}

	return arkchannel.CooperativeClose{
		Proposal: proposal,
		Transaction: append(
			[]byte(nil), settlement.GetTransaction()...,
		),
		TxID: txID,
	}, nil
}

var _ ProcessCooperativeClosePeer = (*MailboxCooperativeClosePeer)(nil)

var _ arkchannelrpc.ArkChannelPeerServiceMailboxServer = (*CooperativeClosePeerRPCServer)(nil) //nolint:ll
