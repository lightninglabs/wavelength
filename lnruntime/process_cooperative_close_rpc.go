package lnruntime

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
)

const (
	closeAckClientSigned    = arkchannelrpc.CooperativeCloseAcknowledgement_COOPERATIVE_CLOSE_ACKNOWLEDGEMENT_CLIENT_SIGNED    //nolint:ll
	closeAckClientFinalized = arkchannelrpc.CooperativeCloseAcknowledgement_COOPERATIVE_CLOSE_ACKNOWLEDGEMENT_CLIENT_FINALIZED //nolint:ll
)

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
	id arkchannel.ID, clientScript []byte) (
	arkchannel.CooperativeCloseRequest, CleanChannelState,
	*arkchannel.CooperativeClose, error) {

	resp, err := p.client.BeginCooperativeClose(
		ctx, &arkchannelrpc.BeginCooperativeCloseRequest{
			ChannelId: id[:],
			ClientDeliveryScript: append(
				[]byte(nil), clientScript...,
			),
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

// CompleteCooperativeClose submits the exact OOR proposal to the hub.
func (p *MailboxCooperativeClosePeer) CompleteCooperativeClose(
	ctx context.Context, id arkchannel.ID,
	proposal arkchannel.CooperativeCloseProposal) (
	arkchannel.CooperativeClose, error) {

	resp, err := p.client.CompleteCooperativeClose(
		ctx, &arkchannelrpc.CompleteCooperativeCloseRequest{
			ChannelId: id[:],
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

// AcknowledgeCooperativeCloseSigned records the client's durable authorization
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
