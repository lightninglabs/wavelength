package lnruntime

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
)

const cooperativeClosePeerService = "arkchannelrpc.ArkChannelPeerService"

var cooperativeClosePeerMethods = map[string]struct{}{
	"GetCooperativeCloseTerms": {},
	"ConfirmCooperativeClose":  {},
	"CompleteCooperativeClose": {},

	"PublishCooperativeClose": {},
	"AbortCooperativeClose":   {},
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

// GetCooperativeCloseTerms reads the hub's payout terms without quiescing it.
func (p *MailboxCooperativeClosePeer) GetCooperativeCloseTerms(
	ctx context.Context, id arkchannel.ID, clientScript []byte) (
	arkchannel.CooperativeCloseRequest, error) {

	// Redelivery of one quote is idempotent, but a later request must
	// consult the hub wallet again, including after a policy rotation.
	key, err := mailboxrpc.NewIdempotencyKey()
	if err != nil {
		return arkchannel.CooperativeCloseRequest{}, err
	}
	resp, err := p.client.GetCooperativeCloseTerms(
		ctx, &arkchannelrpc.GetCooperativeCloseTermsRequest{
			ChannelId: id[:],
			ClientDeliveryScript: append(
				[]byte(nil), clientScript...,
			),
		}, mailboxrpc.RPCOptions{IdempotencyKey: key},
	)
	if err != nil {
		return arkchannel.CooperativeCloseRequest{}, err
	}

	return cooperativeCloseRequestFromRPC(resp.GetRequest())
}

// CompleteCooperativeClose submits the exact OOR proposal to the hub.
func (p *MailboxCooperativeClosePeer) CompleteCooperativeClose(
	ctx context.Context, id arkchannel.ID,
	request arkchannel.CooperativeCloseRequest,
	proposal arkchannel.CooperativeCloseProposal) (
	arkchannel.CooperativeClose, error) {

	resp, err := p.client.CompleteCooperativeClose(
		ctx, &arkchannelrpc.CompleteCooperativeCloseRequest{
			ChannelId: id[:], Request: cooperativeCloseRequestToRPC(
				request,
			),
			Proposal: cooperativeCloseProposalToRPC(
				proposal,
			),
		}, closeRPCOptions(id, "complete"),
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}

	return cooperativeCloseFromRPC(resp.GetSettlement())
}

// ConfirmCooperativeClose acknowledges the locally stored signed package by ID.
func (p *MailboxCooperativeClosePeer) ConfirmCooperativeClose(
	ctx context.Context, id arkchannel.ID, receipt chainhash.Hash) error {

	_, err := p.client.ConfirmCooperativeClose(ctx,
		&arkchannelrpc.ConfirmCooperativeCloseRequest{
			ChannelId: id[:], AuthorizationReceipt: receipt[:],
		}, closeRPCOptions(id, "confirm"),
	)

	return err
}

// PublishCooperativeClose tells the hub which client-finalized OOR session was
// selected, then waits for hub-local archival.
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

// AbortCooperativeClose releases the remote pre-signature barrier.
func (p *MailboxCooperativeClosePeer) AbortCooperativeClose(ctx context.Context,
	id arkchannel.ID) error {

	// An old success is not permission to resume a later attempt. The
	// envelope keeps this key across redelivery, but a new abort must
	// consult the hub's current unsigned state.
	key, err := mailboxrpc.NewIdempotencyKey()
	if err != nil {
		return err
	}
	_, err = p.client.AbortCooperativeClose(
		ctx, &arkchannelrpc.AbortCooperativeCloseRequest{
			ChannelId: id[:],
		}, mailboxrpc.RPCOptions{IdempotencyKey: key},
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

// GetCooperativeCloseTerms returns the payout terms for the authenticated peer.
func (s *CooperativeClosePeerRPCServer) GetCooperativeCloseTerms(
	ctx context.Context,
	req *arkchannelrpc.GetCooperativeCloseTermsRequest) (
	*arkchannelrpc.GetCooperativeCloseTermsResponse, error) {

	id, err := s.channelID(ctx, req.GetChannelId())
	if err != nil {
		return nil, err
	}
	request, err := s.hub.GetCooperativeCloseTerms(
		ctx, id, req.GetClientDeliveryScript(),
	)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.GetCooperativeCloseTermsResponse{
		Request: cooperativeCloseRequestToRPC(request),
	}, nil
}

// CompleteCooperativeClose validates the proposal and returns the hub's 3-of-3
// OOR authorization.
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
	request, err := cooperativeCloseRequestFromRPC(req.GetRequest())
	if err != nil {
		return nil, err
	}
	settlement, err := s.hub.CompleteCooperativeClose(
		ctx, id, request, proposal,
	)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.CompleteCooperativeCloseResponse{
		Settlement: cooperativeCloseToRPC(settlement),
	}, nil
}

// ConfirmCooperativeClose matches the client's receipt to the stored payout.
func (s *CooperativeClosePeerRPCServer) ConfirmCooperativeClose(
	ctx context.Context,
	req *arkchannelrpc.ConfirmCooperativeCloseRequest) (
	*arkchannelrpc.ConfirmCooperativeCloseResponse, error) {

	id, err := s.channelID(ctx, req.GetChannelId())
	if err != nil {
		return nil, err
	}
	receipt, err := rpcHash(
		"authorization receipt", req.GetAuthorizationReceipt(),
	)
	if err != nil {
		return nil, err
	}
	if err := s.hub.ConfirmCooperativeClose(ctx, id, receipt); err != nil {
		return nil, err
	}

	return &arkchannelrpc.ConfirmCooperativeCloseResponse{}, nil
}

// PublishCooperativeClose waits for OOR finalization and hub-local archival.
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

	return s.hub.local.GetChannel(ctx, id)
}

// ArkChannelRecordToRPC converts the compact durable channel summary exposed
// by local and peer process RPCs.
func ArkChannelRecordToRPC(record arkchannel.Record) *arkchannelrpc.ArkChannel {
	snapshot := record.Snapshot
	channel := &arkchannelrpc.ArkChannel{
		ChannelId:    snapshot.Terms.ID[:],
		Kind:         snapshot.Terms.Kind.String(),
		Phase:        arkChannelPublicPhase(snapshot),
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
		channel.RefreshOorTxid =
			snapshot.CooperativeClose.TxID[:]
	}

	return channel
}

// arkChannelPublicPhase keeps internal durable phase names out of the local
// API where the 3-of-3 OOR operation is presented as an in-Ark refresh.
func arkChannelPublicPhase(snapshot arkchannel.Snapshot) string {
	switch snapshot.Phase {
	case arkchannel.PhaseCoopClosing:
		return "refreshing"

	case arkchannel.PhaseCoopCloseSigned:
		return "refresh_authorized"

	case arkchannel.PhaseCoopClosePublished:
		return "refresh_oor_finalized"

	case arkchannel.PhaseClosed:
		if snapshot.CooperativeClose != nil {
			return "refreshed"
		}

		return snapshot.Phase.String()

	default:
		return snapshot.Phase.String()
	}
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
	result := arkchannel.CooperativeCloseRequest{
		Initiator: arkchannel.PartyClient,
		ClientDeliveryScript: append(
			[]byte(nil), request.GetClientDeliveryScript()...,
		),
		HubDeliveryScript: append(
			[]byte(nil), request.GetHubDeliveryScript()...,
		),
	}
	if err := result.Validate(); err != nil {
		return arkchannel.CooperativeCloseRequest{}, err
	}

	return result, nil
}

// cooperativeCloseProposalToRPC serializes the unsigned OOR checkpoint.
func cooperativeCloseProposalToRPC(
	proposal arkchannel.CooperativeCloseProposal) *arkchannelrpc.CooperativeCloseProposal { //nolint:ll

	return &arkchannelrpc.CooperativeCloseProposal{
		Transaction:      append([]byte(nil), proposal.Transaction...),
		CommitmentHeight: proposal.CommitmentHeight,
		ClientBalanceSat: int64(proposal.ClientBalance),
		HubBalanceSat:    int64(proposal.HubBalance),
		ClientOutputSat:  int64(proposal.ClientOutput),
		HubOutputSat:     int64(proposal.HubOutput),
	}
}

// cooperativeCloseProposalFromRPC validates the unsigned OOR checkpoint.
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

	return arkchannel.CooperativeCloseProposal{
		Transaction: append(
			[]byte(nil), proposal.GetTransaction()...,
		),
		CommitmentHeight: proposal.GetCommitmentHeight(),
		ClientBalance:    clientBalance,
		HubBalance:       hubBalance,
		ClientOutput:     clientOutput,
		HubOutput:        hubOutput,
	}, nil
}

// cooperativeCloseToRPC serializes the hub-authorized OOR close.
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
