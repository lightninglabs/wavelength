package lnruntime

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

const fundingPeerService = "arkchannelrpc.ArkChannelFundingPeerService"

const recoveryPackageInstalledEvent = arkchannelrpc.ChannelEventType_CHANNEL_EVENT_TYPE_RECOVERY_PACKAGE_INSTALLED //nolint:ll

var fundingPeerMethods = map[string]struct{}{
	"GetPeerInfo":                {},
	"RegisterPromotion":          {},
	"BindPreparedOOR":            {},
	"SignBacking":                {},
	"InstallBacking":             {},
	"InstallRecoveryPackage":     {},
	"ExportReceiveClaimRecovery": {},
	"FundingFinalized":           {},
	"ChannelActive":              {},
	"ApplyChannelEvent":          {},
	"CreateInvoice":              {},
	"PayInvoice":                 {},
	"PrepareOutgoingPayment":     {},
	"CancelOutgoingPayment":      {},
	"RegisterIncomingPayment":    {},
}

// OutgoingPaymentPreparation fixes the private source amount and active
// channel before the client dispatches its held same-hash HTLC.
type OutgoingPaymentPreparation struct {
	PaymentHash   lntypes.Hash
	PrivateAmount btcutil.Amount
	Fee           btcutil.Amount
	ChannelID     arkchannel.ID
	ReservedSCID  uint64
}

// PaymentBridgeCoordinator is the narrow swapserver-owned public Lightning
// boundary exposed to one authenticated private channel endpoint.
type PaymentBridgeCoordinator interface {
	PrepareOutgoingPayment(context.Context, [33]byte, string,
		btcutil.Amount) (OutgoingPaymentPreparation, error)

	CancelOutgoingPayment(context.Context, [33]byte, lntypes.Hash,
		string) error

	RegisterIncomingPayment(context.Context, [33]byte, lntypes.Hash,
		btcutil.Amount, uint64) error

	ReceiveClaimRecovery(context.Context, [33]byte, lntypes.Hash, uint64) (
		arkchannel.ReceiveClaimRecoverySource,
		arkchannel.RecoveryPackage, error)
}

// FundingPeerInfo contains immutable hub policy needed to construct a client
// funded channel intent.
type FundingPeerInfo struct {
	HubNodeKey       [33]byte
	HubArkKey        [33]byte
	HubChannelKey    [33]byte
	ArkOperatorKey   [33]byte
	ChannelDelay     uint32
	FunderDelay      uint32
	MinimumExitDelay uint32
}

// Validate rejects incomplete channel policy before terms are registered.
func (i FundingPeerInfo) Validate() error {
	for name, key := range map[string][33]byte{
		"hub node":     i.HubNodeKey,
		"hub Ark":      i.HubArkKey,
		"hub channel":  i.HubChannelKey,
		"Ark operator": i.ArkOperatorKey,
	} {
		if _, err := parseCompressedKey(name, key); err != nil {
			return err
		}
	}
	if i.ChannelDelay == 0 || i.FunderDelay <= i.ChannelDelay ||
		i.MinimumExitDelay == 0 {
		return fmt.Errorf("invalid Ark channel delay policy")
	}

	return nil
}

// ProcessFundingPeer is the authenticated remote surface used by one
// channel-creation process.
type ProcessFundingPeer interface {
	RecoveryCounterparty

	GetPeerInfo(context.Context) (FundingPeerInfo, error)

	RegisterPromotion(context.Context,
		arkchannel.Terms) (arkchannel.Record, error)

	BindPreparedOOR(context.Context, arkchannel.ID,
		arkchannel.VTXOBinding) (arkchannel.Record, error)

	ExportReceiveClaimRecovery(context.Context, arkchannel.ID) (
		arkchannel.ReceiveClaimRecoverySource,
		arkchannel.RecoveryPackage, error)

	CreateInvoice(context.Context, arkchannel.ID,
		btcutil.Amount) (lntypes.Hash, error)

	PayInvoice(context.Context, arkchannel.ID, lntypes.Hash,
		btcutil.Amount) error

	PrepareOutgoingPayment(context.Context, string,
		btcutil.Amount) (OutgoingPaymentPreparation, error)

	CancelOutgoingPayment(context.Context, lntypes.Hash, string) error

	RegisterIncomingPayment(context.Context, lntypes.Hash, btcutil.Amount,
		uint64) error
}

// IsFundingPeerRoute reports whether one mailbox route belongs to the channel
// creation and private payment protocol.
func IsFundingPeerRoute(service, method string) bool {
	if service != fundingPeerService {
		return false
	}

	_, ok := fundingPeerMethods[method]

	return ok
}

// MailboxFundingPeer adapts generated mailbox calls to ProcessFundingPeer.
type MailboxFundingPeer struct {
	client *arkchannelrpc.ArkChannelFundingPeerServiceMailboxClient
}

// NewMailboxFundingPeer constructs a typed funding peer.
func NewMailboxFundingPeer(client mailboxrpc.RPCClient) (*MailboxFundingPeer,
	error) {

	if client == nil {
		return nil, fmt.Errorf("Ark channel funding mailbox client " +
			"is required")
	}

	return &MailboxFundingPeer{
		client: arkchannelrpc.
			NewArkChannelFundingPeerServiceMailboxClient(
				client,
			),
	}, nil
}

// GetPeerInfo loads the hub's immutable channel policy.
func (p *MailboxFundingPeer) GetPeerInfo(ctx context.Context) (FundingPeerInfo,
	error) {

	response, err := p.client.GetPeerInfo(
		ctx, &arkchannelrpc.GetPeerInfoRequest{},
	)
	if err != nil {
		return FundingPeerInfo{}, err
	}

	return fundingPeerInfoFromRPC(response)
}

// RegisterPromotion registers the responder before the client prepares value.
func (p *MailboxFundingPeer) RegisterPromotion(ctx context.Context,
	terms arkchannel.Terms) (arkchannel.Record, error) {

	response, err := p.client.RegisterPromotion(
		ctx, &arkchannelrpc.RegisterPromotionRequest{
			Terms: channelTermsToRPC(terms),
		}, fundingRPCOptions(terms.ID, "register"),
	)
	if err != nil {
		return arkchannel.Record{}, err
	}

	return recordSummaryFromRPC(terms, response.GetChannel())
}

// BindPreparedOOR installs the exact prepared output at the responder before
// the funder emits an lnd open-channel message.
func (p *MailboxFundingPeer) BindPreparedOOR(ctx context.Context,
	id arkchannel.ID, binding arkchannel.VTXOBinding) (arkchannel.Record,
	error) {

	response, err := p.client.BindPreparedOOR(
		ctx, &arkchannelrpc.BindPreparedOORRequest{
			ChannelId: id[:], Binding: channelBindingToRPC(binding),
		}, fundingRPCOptions(id, "bind"),
	)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if response.GetChannel() == nil {
		return arkchannel.Record{}, fmt.Errorf("funding peer " +
			"returned an empty channel")
	}

	return arkchannel.Record{
		Revision: response.GetChannel().GetRevision(),
	}, nil
}

// SignBacking asks the remote endpoint to validate and sign the exact funding
// PSBT against its own lnd reservation.
func (p *MailboxFundingPeer) SignBacking(ctx context.Context, id arkchannel.ID,
	terms arkchannel.Terms, binding arkchannel.VTXOBinding,
	packet *psbt.Packet) (input.Signature, error) {

	if packet == nil {
		return nil, fmt.Errorf("lnd funding PSBT is required")
	}
	var encoded bytes.Buffer
	if err := packet.Serialize(&encoded); err != nil {
		return nil, err
	}
	response, err := p.client.SignBacking(
		ctx, &arkchannelrpc.SignBackingRequest{
			ChannelId: id[:], Terms: channelTermsToRPC(terms),
			Binding:     channelBindingToRPC(binding),
			FundingPsbt: encoded.Bytes(),
		}, fundingRPCOptions(id, "sign-backing"),
	)
	if err != nil {
		return nil, err
	}

	return schnorr.ParseSignature(response.GetSignature())
}

// InstallBacking registers the fully signed transaction at the remote lnd
// endpoint and its durable Ark FSM.
func (p *MailboxFundingPeer) InstallBacking(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms,
	binding arkchannel.VTXOBinding, backing arkchannel.Backing) error {

	_, err := p.client.InstallBacking(
		ctx, &arkchannelrpc.InstallBackingRequest{
			ChannelId: id[:], Terms: channelTermsToRPC(terms),
			Binding: channelBindingToRPC(binding),
			Backing: channelBackingToRPC(backing),
		}, fundingRPCOptions(id, "install-backing"),
	)

	return err
}

// InstallRecoveryPackage installs the complete recovery-only source package
// and arms its ancestry watches at the remote endpoint.
func (p *MailboxFundingPeer) InstallRecoveryPackage(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms,
	binding arkchannel.VTXOBinding,
	recovery arkchannel.RecoveryPackage) error {

	message, err := ChannelRecoveryToRPC(recovery)
	if err != nil {
		return err
	}
	_, err = p.client.InstallRecoveryPackage(
		ctx, &arkchannelrpc.InstallRecoveryPackageRequest{
			ChannelId: id[:], Terms: channelTermsToRPC(terms),
			Binding:  channelBindingToRPC(binding),
			Recovery: message,
		}, fundingRPCOptions(id, "install-recovery"),
	)

	return err
}

// ExportReceiveClaimRecovery fetches the operator-derived vHTLC source and
// finalized OOR lineage for one authenticated receive-claim channel.
func (p *MailboxFundingPeer) ExportReceiveClaimRecovery(ctx context.Context,
	id arkchannel.ID) (arkchannel.ReceiveClaimRecoverySource,
	arkchannel.RecoveryPackage, error) {

	response, err := p.client.ExportReceiveClaimRecovery(
		ctx, &arkchannelrpc.ExportReceiveClaimRecoveryRequest{
			ChannelId: id[:],
		}, fundingRPCOptions(id, "export-receive-claim-recovery"),
	)
	if err != nil {
		return arkchannel.ReceiveClaimRecoverySource{},
			arkchannel.RecoveryPackage{}, err
	}
	source, err := ReceiveClaimRecoverySourceFromRPC(response.GetSource())
	if err != nil {
		return arkchannel.ReceiveClaimRecoverySource{},
			arkchannel.RecoveryPackage{}, err
	}
	recovery, err := ChannelRecoveryFromRPC(
		response.GetRecovery(), arkchannel.VTXOBinding{
			OORSessionID: [32]byte(source.OutPoint.Hash),
		},
	)
	if err != nil {
		return arkchannel.ReceiveClaimRecoverySource{},
			arkchannel.RecoveryPackage{}, err
	}

	return source, recovery, nil
}

// FundingFinalized queries the remote lnd durability barrier.
func (p *MailboxFundingPeer) FundingFinalized(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	response, err := p.client.FundingFinalized(
		ctx, fundingStatusRequest(terms, backing),
	)
	if err != nil {
		return false, err
	}

	return response.GetReady(), nil
}

// ChannelActive queries the remote lnd link state.
func (p *MailboxFundingPeer) ChannelActive(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	response, err := p.client.ChannelActive(
		ctx, fundingStatusRequest(terms, backing),
	)
	if err != nil {
		return false, err
	}

	return response.GetReady(), nil
}

// ApplyChannelEvent records one cross-endpoint funding barrier.
func (p *MailboxFundingPeer) ApplyChannelEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	request, step, err := channelEventToRPC(id, event)
	if err != nil {
		return arkchannel.Record{}, err
	}
	response, err := p.client.ApplyChannelEvent(
		ctx, request, fundingRPCOptions(id, step),
	)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if response.GetChannel() == nil {
		return arkchannel.Record{}, fmt.Errorf("funding peer " +
			"returned an empty channel")
	}

	return arkchannel.Record{
		Revision: response.GetChannel().GetRevision(),
	}, nil
}

// CreateInvoice creates an invoice in the remote native lnd registry.
func (p *MailboxFundingPeer) CreateInvoice(ctx context.Context,
	id arkchannel.ID, amount btcutil.Amount) (lntypes.Hash, error) {

	response, err := p.client.CreateInvoice(
		ctx, &arkchannelrpc.PeerInvoiceRequest{
			ChannelId: id[:], AmountSat: int64(amount),
		},
	)
	if err != nil {
		return lntypes.Hash{}, err
	}

	return rpcPaymentHash(response.GetPaymentHash())
}

// PayInvoice pays one remote invoice over the active private channel.
func (p *MailboxFundingPeer) PayInvoice(ctx context.Context, id arkchannel.ID,
	hash lntypes.Hash, amount btcutil.Amount) error {

	response, err := p.client.PayInvoice(
		ctx, &arkchannelrpc.PeerPayInvoiceRequest{
			ChannelId: id[:], PaymentHash: hash[:],
			AmountSat: int64(amount),
		}, fundingRPCOptions(id, "pay/"+hex.EncodeToString(hash[:])),
	)
	if err != nil {
		return err
	}
	if !response.GetSettled() {
		return fmt.Errorf("remote native lnd payment did not settle")
	}

	return nil
}

// PrepareOutgoingPayment asks the operator to hold and dispatch one public
// invoice using a private source HTLC.
func (p *MailboxFundingPeer) PrepareOutgoingPayment(ctx context.Context,
	paymentRequest string, maxFee btcutil.Amount) (
	OutgoingPaymentPreparation, error) {

	if maxFee < 0 {
		return OutgoingPaymentPreparation{}, fmt.Errorf("maximum fee " +
			"cannot be negative")
	}
	response, err := p.client.PrepareOutgoingPayment(
		ctx, &arkchannelrpc.PrepareOutgoingPaymentRequest{
			PaymentRequest: paymentRequest,
			MaxFeeSat:      uint64(maxFee),
		},
	)
	if err != nil {
		return OutgoingPaymentPreparation{}, err
	}
	hash, err := rpcPaymentHash(response.GetPaymentHash())
	if err != nil {
		return OutgoingPaymentPreparation{}, err
	}
	amount, err := positiveRPCAmount(
		"private payment amount", response.GetPrivateAmountSat(),
	)
	if err != nil {
		return OutgoingPaymentPreparation{}, err
	}
	if response.GetFeeSat() < 0 {
		return OutgoingPaymentPreparation{}, fmt.Errorf("payment fee " +
			"cannot be negative")
	}
	id, err := rpcChannelID(response.GetChannelId())
	if err != nil {
		return OutgoingPaymentPreparation{}, err
	}
	if response.GetReservedScid() == 0 {
		return OutgoingPaymentPreparation{}, fmt.Errorf("private " +
			"payment SCID is required")
	}

	return OutgoingPaymentPreparation{
		PaymentHash: hash, PrivateAmount: amount,
		Fee: btcutil.Amount(response.GetFeeSat()), ChannelID: id,
		ReservedSCID: response.GetReservedScid(),
	}, nil
}

// CancelOutgoingPayment releases an outgoing hold invoice that the client
// could not lock before the operator dispatched its public destination.
func (p *MailboxFundingPeer) CancelOutgoingPayment(ctx context.Context,
	hash lntypes.Hash, reason string) error {

	if reason == "" {
		return fmt.Errorf("outgoing payment cancellation reason is " +
			"required")
	}
	response, err := p.client.CancelOutgoingPayment(
		ctx, &arkchannelrpc.CancelOutgoingPaymentRequest{
			PaymentHash: hash[:], Reason: reason,
		}, fundingRPCOptions(
			arkchannel.ID(hash),
			"cancel-outgoing-payment",
		),
	)
	if err != nil {
		return err
	}
	if !response.GetCancelled() {
		return fmt.Errorf("outgoing payment was not cancelled")
	}

	return nil
}

// RegisterIncomingPayment binds a future public route to this authenticated
// client after its native invoice exists.
func (p *MailboxFundingPeer) RegisterIncomingPayment(ctx context.Context,
	hash lntypes.Hash, amount btcutil.Amount, reservedSCID uint64) error {

	if amount <= 0 || reservedSCID == 0 {
		return fmt.Errorf("incoming payment amount and SCID are " +
			"required")
	}
	response, err := p.client.RegisterIncomingPayment(
		ctx, &arkchannelrpc.RegisterIncomingPaymentRequest{
			PaymentHash: hash[:], AmountSat: int64(amount),
			ReservedScid: reservedSCID,
		},
	)
	if err != nil {
		return err
	}
	if !response.GetRegistered() {
		return fmt.Errorf("incoming payment was not registered")
	}

	return nil
}

// FundingPeerRPCServerConfig contains one authenticated remote endpoint and
// the local native lnd process it may coordinate with.
type FundingPeerRPCServerConfig struct {
	RemoteNode [33]byte
	Info       FundingPeerInfo
	Service    *arkchannel.Service
	Funding    *NativeFundingEndpoint
	Node       *NativeNode
	Recovery   ChannelRecoveryManager
	Bridge     PaymentBridgeCoordinator
}

// FundingPeerRPCServer serves one client-bound funding process.
type FundingPeerRPCServer struct {
	cfg FundingPeerRPCServerConfig
}

// NewFundingPeerRPCServer constructs one authenticated process service.
func NewFundingPeerRPCServer(cfg FundingPeerRPCServerConfig) (
	*FundingPeerRPCServer, error) {

	if _, err := parseCompressedKey(
		"remote node", cfg.RemoteNode,
	); err != nil {
		return nil, err
	}
	if err := cfg.Info.Validate(); err != nil {
		return nil, err
	}
	if cfg.Service == nil || cfg.Funding == nil || cfg.Node == nil ||
		cfg.Recovery == nil {
		return nil, fmt.Errorf("complete native channel process is " +
			"required")
	}

	return &FundingPeerRPCServer{cfg: cfg}, nil
}

// GetPeerInfo returns immutable hub channel policy.
func (s *FundingPeerRPCServer) GetPeerInfo(context.Context,
	*arkchannelrpc.GetPeerInfoRequest) (*arkchannelrpc.GetPeerInfoResponse,
	error) {

	i := s.cfg.Info

	return &arkchannelrpc.GetPeerInfoResponse{
		HubNodeKey: i.HubNodeKey[:], HubArkKey: i.HubArkKey[:],
		HubChannelKey:  i.HubChannelKey[:],
		ArkOperatorKey: i.ArkOperatorKey[:],
		ChannelDelay:   i.ChannelDelay, FunderDelay: i.FunderDelay,
		MinExitDelay: i.MinimumExitDelay,
	}, nil
}

// RegisterPromotion registers immutable terms for the authenticated client.
func (s *FundingPeerRPCServer) RegisterPromotion(ctx context.Context,
	request *arkchannelrpc.RegisterPromotionRequest) (
	*arkchannelrpc.RegisterPromotionResponse, error) {

	terms, err := channelTermsFromRPC(request.GetTerms())
	if err != nil {
		return nil, err
	}
	if err := s.validateTerms(terms); err != nil {
		return nil, err
	}
	record, err := s.cfg.Service.RegisterPromotion(ctx, terms)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.RegisterPromotionResponse{
		Channel: ArkChannelRecordToRPC(record),
	}, nil
}

// BindPreparedOOR binds the exact prepared source before funding starts.
func (s *FundingPeerRPCServer) BindPreparedOOR(ctx context.Context,
	request *arkchannelrpc.BindPreparedOORRequest) (
	*arkchannelrpc.BindPreparedOORResponse, error) {

	id, record, err := s.channel(ctx, request.GetChannelId())
	if err != nil {
		return nil, err
	}
	binding, err := channelBindingFromRPC(request.GetBinding())
	if err != nil {
		return nil, err
	}
	if err := binding.Validate(record.Snapshot.Terms); err != nil {
		return nil, err
	}
	record, err = s.cfg.Service.BindPreparedOOR(ctx, id, binding)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.BindPreparedOORResponse{
		Channel: ArkChannelRecordToRPC(record),
	}, nil
}

// SignBacking validates and signs the remote funding PSBT.
func (s *FundingPeerRPCServer) SignBacking(ctx context.Context,
	request *arkchannelrpc.SignBackingRequest) (
	*arkchannelrpc.SignBackingResponse, error) {

	id, err := rpcChannelID(request.GetChannelId())
	if err != nil {
		return nil, err
	}
	terms, err := channelTermsFromRPC(request.GetTerms())
	if err != nil {
		return nil, err
	}
	if id != terms.ID {
		return nil, fmt.Errorf("channel ID does not match terms")
	}
	if err := s.validateTerms(terms); err != nil {
		return nil, err
	}
	binding, err := channelBindingFromRPC(request.GetBinding())
	if err != nil {
		return nil, err
	}
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(
			request.GetFundingPsbt(),
		),
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("decode lnd funding PSBT: %w", err)
	}
	signature, err := s.cfg.Funding.SignBacking(
		ctx, id, terms, binding, packet,
	)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.SignBackingResponse{
		Signature: signature.Serialize(),
	}, nil
}

// InstallBacking installs the exact fully signed channel transaction.
func (s *FundingPeerRPCServer) InstallBacking(ctx context.Context,
	request *arkchannelrpc.InstallBackingRequest) (
	*arkchannelrpc.InstallBackingResponse, error) {

	id, err := rpcChannelID(request.GetChannelId())
	if err != nil {
		return nil, err
	}
	terms, err := channelTermsFromRPC(request.GetTerms())
	if err != nil {
		return nil, err
	}
	if id != terms.ID {
		return nil, fmt.Errorf("channel ID does not match terms")
	}
	if err := s.validateTerms(terms); err != nil {
		return nil, err
	}
	binding, err := channelBindingFromRPC(request.GetBinding())
	if err != nil {
		return nil, err
	}
	backing, err := channelBackingFromRPC(request.GetBacking())
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Funding.InstallBacking(
		ctx, id, terms, binding, backing,
	); err != nil {
		return nil, err
	}

	return &arkchannelrpc.InstallBackingResponse{}, nil
}

// InstallRecoveryPackage persists and watches the exact finalized source
// package before acknowledging the activation barrier.
func (s *FundingPeerRPCServer) InstallRecoveryPackage(ctx context.Context,
	request *arkchannelrpc.InstallRecoveryPackageRequest) (
	*arkchannelrpc.InstallRecoveryPackageResponse, error) {

	id, err := rpcChannelID(request.GetChannelId())
	if err != nil {
		return nil, err
	}
	terms, err := channelTermsFromRPC(request.GetTerms())
	if err != nil {
		return nil, err
	}
	if id != terms.ID {
		return nil, fmt.Errorf("channel ID does not match terms")
	}
	if err := s.validateTerms(terms); err != nil {
		return nil, err
	}
	binding, err := channelBindingFromRPC(request.GetBinding())
	if err != nil {
		return nil, err
	}
	recovery, err := ChannelRecoveryFromRPC(
		request.GetRecovery(), binding,
	)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Recovery.InstallRecoveryPackage(
		ctx, id, terms, binding, recovery,
	); err != nil {
		return nil, err
	}

	return &arkchannelrpc.InstallRecoveryPackageResponse{}, nil
}

// ExportReceiveClaimRecovery returns only the receive-claim source authorized
// by swapd's durable payment record for this authenticated client.
func (s *FundingPeerRPCServer) ExportReceiveClaimRecovery(ctx context.Context,
	request *arkchannelrpc.ExportReceiveClaimRecoveryRequest) (
	*arkchannelrpc.ExportReceiveClaimRecoveryResponse, error) {

	_, record, err := s.channel(ctx, request.GetChannelId())
	if err != nil {
		return nil, err
	}
	terms := record.Snapshot.Terms
	if terms.Kind != arkchannel.KindReceiveClaim ||
		terms.Funder != arkchannel.PartyClient {
		return nil, fmt.Errorf("channel is not a client-funded " +
			"receive claim")
	}
	if s.cfg.Bridge == nil {
		return nil, fmt.Errorf("channel payment bridge is not " +
			"configured")
	}
	source, recovery, err := s.cfg.Bridge.ReceiveClaimRecovery(
		ctx, s.cfg.RemoteNode, lntypes.Hash(terms.PaymentHash),
		terms.ReservedSCID,
	)
	if err != nil {
		return nil, err
	}
	if source.Amount <= terms.Capacity {
		return nil, fmt.Errorf("receive-claim source has no channel " +
			"backing reserve")
	}
	if err := recovery.ValidateReceiveClaim(source); err != nil {
		return nil, err
	}
	recoveryMessage, err := ChannelRecoveryToRPC(recovery)
	if err != nil {
		return nil, err
	}
	sourceMessage, err := ReceiveClaimRecoverySourceToRPC(source)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.ExportReceiveClaimRecoveryResponse{
		Source: sourceMessage, Recovery: recoveryMessage,
	}, nil
}

// FundingFinalized reports the remote native lnd durability barrier.
func (s *FundingPeerRPCServer) FundingFinalized(ctx context.Context,
	request *arkchannelrpc.FundingStatusRequest) (
	*arkchannelrpc.FundingStatusResponse, error) {

	terms, backing, err := s.fundingStatus(request)
	if err != nil {
		return nil, err
	}
	ready, err := s.cfg.Funding.FundingFinalized(ctx, terms, backing)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.FundingStatusResponse{Ready: ready}, nil
}

// ChannelActive reports whether the remote native lnd link is active.
func (s *FundingPeerRPCServer) ChannelActive(ctx context.Context,
	request *arkchannelrpc.FundingStatusRequest) (
	*arkchannelrpc.FundingStatusResponse, error) {

	terms, backing, err := s.fundingStatus(request)
	if err != nil {
		return nil, err
	}
	ready, err := s.cfg.Funding.ChannelActive(ctx, terms, backing)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.FundingStatusResponse{Ready: ready}, nil
}

// ApplyChannelEvent records one authenticated cross-endpoint barrier.
func (s *FundingPeerRPCServer) ApplyChannelEvent(ctx context.Context,
	request *arkchannelrpc.ApplyChannelEventRequest) (
	*arkchannelrpc.ApplyChannelEventResponse, error) {

	id, _, err := s.channel(ctx, request.GetChannelId())
	if err != nil {
		return nil, err
	}
	event, err := channelEventFromRPC(request)
	if err != nil {
		return nil, err
	}
	var record arkchannel.Record
	if _, materialize := event.(*arkchannel.Materialize); materialize {
		record, err = s.cfg.Service.Materialize(ctx, id)
	} else {
		record, err = s.cfg.Funding.ApplyChannelEvent(ctx, id, event)
	}
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.ApplyChannelEventResponse{
		Channel: ArkChannelRecordToRPC(record),
	}, nil
}

// CreateInvoice creates a native invoice for one active owned channel.
func (s *FundingPeerRPCServer) CreateInvoice(ctx context.Context,
	request *arkchannelrpc.PeerInvoiceRequest) (
	*arkchannelrpc.PeerInvoiceResponse, error) {

	_, _, err := s.channel(ctx, request.GetChannelId())
	if err != nil {
		return nil, err
	}
	amount, err := positiveRPCAmount(
		"invoice amount", request.GetAmountSat(),
	)
	if err != nil {
		return nil, err
	}
	_, hash, err := s.cfg.Node.AddInvoice(ctx, amount)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.PeerInvoiceResponse{
		PaymentHash: hash[:],
	}, nil
}

// PayInvoice sends one native fixed-route payment for an active channel.
func (s *FundingPeerRPCServer) PayInvoice(ctx context.Context,
	request *arkchannelrpc.PeerPayInvoiceRequest) (
	*arkchannelrpc.PeerPayInvoiceResponse, error) {

	_, record, err := s.channel(ctx, request.GetChannelId())
	if err != nil {
		return nil, err
	}
	hash, err := rpcPaymentHash(request.GetPaymentHash())
	if err != nil {
		return nil, err
	}
	amount, err := positiveRPCAmount(
		"payment amount", request.GetAmountSat(),
	)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Node.PayInvoice(ctx, record, hash, amount); err != nil {
		return nil, err
	}

	return &arkchannelrpc.PeerPayInvoiceResponse{Settled: true}, nil
}

// PrepareOutgoingPayment delegates public invoice admission to the operator's
// durable bridge manager while retaining authenticated client identity here.
func (s *FundingPeerRPCServer) PrepareOutgoingPayment(ctx context.Context,
	request *arkchannelrpc.PrepareOutgoingPaymentRequest) (
	*arkchannelrpc.PrepareOutgoingPaymentResponse, error) {

	if s.cfg.Bridge == nil {
		return nil, fmt.Errorf("payment bridge is not configured")
	}
	if request.GetPaymentRequest() == "" {
		return nil, fmt.Errorf("payment request is required")
	}
	if request.GetMaxFeeSat() > uint64(btcutil.MaxSatoshi) {
		return nil, fmt.Errorf("maximum fee exceeds maximum money")
	}
	preparation, err := s.cfg.Bridge.PrepareOutgoingPayment(
		ctx, s.cfg.RemoteNode, request.GetPaymentRequest(),
		btcutil.Amount(
			request.GetMaxFeeSat(),
		),
	)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.PrepareOutgoingPaymentResponse{
		PaymentHash:      preparation.PaymentHash[:],
		PrivateAmountSat: int64(preparation.PrivateAmount),
		FeeSat:           int64(preparation.Fee),
		ChannelId:        preparation.ChannelID[:],
		ReservedScid:     preparation.ReservedSCID,
	}, nil
}

// CancelOutgoingPayment records source failure before public dispatch and
// causes the operator worker to cancel its private hold invoice.
func (s *FundingPeerRPCServer) CancelOutgoingPayment(ctx context.Context,
	request *arkchannelrpc.CancelOutgoingPaymentRequest) (
	*arkchannelrpc.CancelOutgoingPaymentResponse, error) {

	if s.cfg.Bridge == nil {
		return nil, fmt.Errorf("payment bridge is not configured")
	}
	hash, err := rpcPaymentHash(request.GetPaymentHash())
	if err != nil {
		return nil, err
	}
	if request.GetReason() == "" || len(request.GetReason()) > 256 {
		return nil, fmt.Errorf("valid payment cancellation reason is " +
			"required")
	}
	if err := s.cfg.Bridge.CancelOutgoingPayment(
		ctx, s.cfg.RemoteNode, hash, request.GetReason(),
	); err != nil {
		return nil, err
	}

	return &arkchannelrpc.CancelOutgoingPaymentResponse{
		Cancelled: true,
	}, nil
}

// RegisterIncomingPayment records an authenticated future-SCID reservation
// before the client returns its BOLT 11 invoice.
func (s *FundingPeerRPCServer) RegisterIncomingPayment(ctx context.Context,
	request *arkchannelrpc.RegisterIncomingPaymentRequest) (
	*arkchannelrpc.RegisterIncomingPaymentResponse, error) {

	if s.cfg.Bridge == nil {
		return nil, fmt.Errorf("payment bridge is not configured")
	}
	hash, err := rpcPaymentHash(request.GetPaymentHash())
	if err != nil {
		return nil, err
	}
	amount, err := positiveRPCAmount(
		"incoming payment amount", request.GetAmountSat(),
	)
	if err != nil {
		return nil, err
	}
	if request.GetReservedScid() == 0 {
		return nil, fmt.Errorf("incoming payment SCID is required")
	}
	if err := s.cfg.Bridge.RegisterIncomingPayment(
		ctx, s.cfg.RemoteNode, hash, amount, request.GetReservedScid(),
	); err != nil {
		return nil, err
	}

	return &arkchannelrpc.RegisterIncomingPaymentResponse{
		Registered: true,
	}, nil
}

// fundingStatus parses and validates one status query.
func (s *FundingPeerRPCServer) fundingStatus(
	request *arkchannelrpc.FundingStatusRequest) (arkchannel.Terms,
	arkchannel.Backing, error) {

	terms, err := channelTermsFromRPC(request.GetTerms())
	if err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}
	if err := s.validateTerms(terms); err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}
	backing, err := channelBackingFromRPC(request.GetBacking())
	if err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}

	return terms, backing, nil
}

// channel loads one channel and enforces authenticated ownership.
func (s *FundingPeerRPCServer) channel(ctx context.Context, rawID []byte) (
	arkchannel.ID, arkchannel.Record, error) {

	id, err := rpcChannelID(rawID)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Record{}, err
	}
	record, err := s.cfg.Service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Record{}, err
	}
	if err := s.validateTerms(record.Snapshot.Terms); err != nil {
		return arkchannel.ID{}, arkchannel.Record{}, err
	}

	return id, record, nil
}

// validateTerms binds all operations to the authenticated remote node.
func (s *FundingPeerRPCServer) validateTerms(terms arkchannel.Terms) error {
	if err := terms.Validate(); err != nil {
		return err
	}
	if terms.ClientNodeKey != s.cfg.RemoteNode ||
		terms.HubNodeKey != s.cfg.Info.HubNodeKey {
		return fmt.Errorf("channel belongs to another peer")
	}

	return nil
}

// fundingRPCOptions derives stable idempotency for one mutating channel step.
func fundingRPCOptions(id arkchannel.ID, step string) mailboxrpc.RPCOptions {
	idempotencyKey := "ark-funding/" + hex.EncodeToString(id[:]) + "/" +
		step

	return mailboxrpc.RPCOptions{
		IdempotencyKey: idempotencyKey,
	}
}

// fundingStatusRequest serializes one authoritative lnd status query.
func fundingStatusRequest(terms arkchannel.Terms,
	backing arkchannel.Backing) *arkchannelrpc.FundingStatusRequest {

	return &arkchannelrpc.FundingStatusRequest{
		Terms:   channelTermsToRPC(terms),
		Backing: channelBackingToRPC(backing),
	}
}

// fundingPeerInfoFromRPC validates the hub's advertised policy.
func fundingPeerInfoFromRPC(response *arkchannelrpc.GetPeerInfoResponse) (
	FundingPeerInfo, error) {

	if response == nil {
		return FundingPeerInfo{}, fmt.Errorf("funding peer info is " +
			"required")
	}
	var info FundingPeerInfo
	for name, source := range map[string]struct {
		destination *[33]byte
		value       []byte
	}{
		"hub node": {
			&info.HubNodeKey,
			response.GetHubNodeKey(),
		},
		"hub Ark": {
			&info.HubArkKey,
			response.GetHubArkKey(),
		},
		"hub channel": {
			&info.HubChannelKey, response.GetHubChannelKey(),
		},
		"Ark operator": {
			&info.ArkOperatorKey, response.GetArkOperatorKey(),
		},
	} {
		if err := copyFixed(
			name, source.destination[:], source.value,
		); err != nil {
			return FundingPeerInfo{}, err
		}
	}
	info.ChannelDelay = response.GetChannelDelay()
	info.FunderDelay = response.GetFunderDelay()
	info.MinimumExitDelay = response.GetMinExitDelay()

	return info, info.Validate()
}

// channelTermsToRPC serializes immutable Ark channel terms.
func channelTermsToRPC(terms arkchannel.Terms) *arkchannelrpc.ChannelTerms {
	return &arkchannelrpc.ChannelTerms{
		ChannelId: terms.ID[:], Kind: uint32(terms.Kind),
		Funder:           uint32(terms.Funder),
		PendingChannelId: terms.PendingChannelID[:],
		ReservedScid:     terms.ReservedSCID, CapacitySat: int64(
			terms.Capacity,
		),
		ClientNodeKey: terms.ClientNodeKey[:],
		HubNodeKey:    terms.HubNodeKey[:],
		PaymentHash:   terms.PaymentHash[:],
		Vtxo: &arkchannelrpc.ChannelVTXOTerms{
			ClientArkKey:     terms.VTXO.ClientArkKey[:],
			HubArkKey:        terms.VTXO.HubArkKey[:],
			ArkOperatorKey:   terms.VTXO.ArkOperatorKey[:],
			ClientChannelKey: terms.VTXO.ClientChannelKey[:],
			HubChannelKey:    terms.VTXO.HubChannelKey[:],
			FunderKey:        terms.VTXO.FunderKey[:],
			ChannelDelay:     terms.VTXO.ChannelDelay,
			FunderDelay:      terms.VTXO.FunderDelay,
			MinExitDelay:     terms.VTXO.MinExitDelay,
		},
	}
}

// channelTermsFromRPC parses and validates immutable Ark channel terms.
func channelTermsFromRPC(message *arkchannelrpc.ChannelTerms) (arkchannel.Terms,
	error) {

	if message == nil || message.GetVtxo() == nil {
		return arkchannel.Terms{}, fmt.Errorf("complete channel " +
			"terms are required")
	}
	capacity, err := positiveRPCAmount(
		"channel capacity", message.GetCapacitySat(),
	)
	if err != nil {
		return arkchannel.Terms{}, err
	}
	var terms arkchannel.Terms
	fields := map[string]struct {
		destination []byte
		value       []byte
	}{
		"channel ID": {
			terms.ID[:],
			message.GetChannelId(),
		},
		"pending channel ID": {
			terms.PendingChannelID[:],
			message.GetPendingChannelId(),
		},
		"client node key": {
			terms.ClientNodeKey[:], message.GetClientNodeKey(),
		},
		"hub node key": {
			terms.HubNodeKey[:],
			message.GetHubNodeKey(),
		},
		"payment hash": {
			terms.PaymentHash[:],
			message.GetPaymentHash(),
		},
		"client Ark key": {
			terms.VTXO.ClientArkKey[:], message.
				GetVtxo().
				GetClientArkKey(),
		},
		"hub Ark key": {
			terms.VTXO.HubArkKey[:], message.
				GetVtxo().
				GetHubArkKey(),
		},
		"Ark operator key": {
			terms.VTXO.ArkOperatorKey[:],
			message.GetVtxo().GetArkOperatorKey(),
		},
		"client channel key": {
			terms.VTXO.ClientChannelKey[:],
			message.GetVtxo().GetClientChannelKey(),
		},
		"hub channel key": {
			terms.VTXO.HubChannelKey[:],
			message.GetVtxo().GetHubChannelKey(),
		},
		"funder key": {
			terms.VTXO.FunderKey[:], message.
				GetVtxo().
				GetFunderKey(),
		},
	}
	for name, field := range fields {
		if err := copyFixed(
			name, field.destination, field.value,
		); err != nil {
			return arkchannel.Terms{}, err
		}
	}
	terms.Kind = arkchannel.Kind(message.GetKind())
	terms.Funder = arkchannel.Party(message.GetFunder())
	terms.ReservedSCID = message.GetReservedScid()
	terms.Capacity = capacity
	terms.VTXO.ChannelDelay = message.GetVtxo().GetChannelDelay()
	terms.VTXO.FunderDelay = message.GetVtxo().GetFunderDelay()
	terms.VTXO.MinExitDelay = message.GetVtxo().GetMinExitDelay()

	return terms, terms.Validate()
}

// channelBindingToRPC serializes the exact prepared OOR output.
func channelBindingToRPC(
	binding arkchannel.VTXOBinding) *arkchannelrpc.ChannelVTXOBinding {

	return &arkchannelrpc.ChannelVTXOBinding{
		OorSessionId:   binding.OORSessionID[:],
		OutpointTxid:   binding.OutPoint.Hash[:],
		OutpointIndex:  binding.OutPoint.Index,
		AmountSat:      int64(binding.Amount),
		ArkTransaction: append([]byte(nil), binding.ArkTransaction...),
		PolicyTemplate: append([]byte(nil), binding.PolicyTemplate...),
		PkScript:       append([]byte(nil), binding.PkScript...),
	}
}

// channelBindingFromRPC parses one prepared OOR binding.
func channelBindingFromRPC(message *arkchannelrpc.ChannelVTXOBinding) (
	arkchannel.VTXOBinding, error) {

	if message == nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("channel VTXO " +
			"binding is required")
	}
	amount, err := positiveRPCAmount(
		"channel VTXO amount", message.GetAmountSat(),
	)
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	hash, err := rpcHash("channel VTXO txid", message.GetOutpointTxid())
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	var session [32]byte
	if err := copyFixed(
		"OOR session ID", session[:], message.GetOorSessionId(),
	); err != nil {
		return arkchannel.VTXOBinding{}, err
	}

	return arkchannel.VTXOBinding{
		OORSessionID: session,
		OutPoint: wire.OutPoint{
			Hash: hash, Index: message.GetOutpointIndex(),
		},
		Amount: amount,
		ArkTransaction: append(
			[]byte(nil), message.GetArkTransaction()...,
		),
		PolicyTemplate: append(
			[]byte(nil), message.GetPolicyTemplate()...,
		),
		PkScript: append([]byte(nil), message.GetPkScript()...),
	}, nil
}

// channelBackingToRPC serializes a signed VTXO-to-channel transaction.
func channelBackingToRPC(
	backing arkchannel.Backing) *arkchannelrpc.ChannelBacking {

	return &arkchannelrpc.ChannelBacking{
		Transaction:       append([]byte(nil), backing.Transaction...),
		ChannelPointTxid:  backing.ChannelPoint.Hash[:],
		ChannelPointIndex: backing.ChannelPoint.Index,
	}
}

// channelBackingFromRPC parses a signed VTXO-to-channel transaction.
func channelBackingFromRPC(message *arkchannelrpc.ChannelBacking) (
	arkchannel.Backing, error) {

	if message == nil {
		return arkchannel.Backing{}, fmt.Errorf("channel backing is " +
			"required")
	}
	hash, err := rpcHash(
		"channel point txid", message.GetChannelPointTxid(),
	)
	if err != nil {
		return arkchannel.Backing{}, err
	}

	return arkchannel.Backing{
		Transaction: append([]byte(nil), message.GetTransaction()...),
		ChannelPoint: wire.OutPoint{
			Hash: hash, Index: message.GetChannelPointIndex(),
		},
	}, nil
}

// channelEventToRPC serializes the funding barriers used by the negotiator.
func channelEventToRPC(id arkchannel.ID, event arkchannel.Event) (
	*arkchannelrpc.ApplyChannelEventRequest, string, error) {

	request := &arkchannelrpc.ApplyChannelEventRequest{ChannelId: id[:]}
	switch event := event.(type) {
	case *arkchannel.FundingFinalized:
		request.EventType = arkchannelrpc.
			ChannelEventType_CHANNEL_EVENT_TYPE_FUNDING_FINALIZED
		request.Party = uint32(event.Party)

		return request, "funding-finalized/" + event.Party.String(), nil

	case *arkchannel.OORFinalized:
		request.EventType = arkchannelrpc.
			ChannelEventType_CHANNEL_EVENT_TYPE_OOR_FINALIZED
		request.OorSessionId = event.SessionID[:]

		return request, "oor-finalized", nil

	case *arkchannel.RecoveryPackageInstalled:
		request.EventType = recoveryPackageInstalledEvent

		return request, "recovery-package-installed", nil

	case *arkchannel.ChannelActive:
		request.EventType = arkchannelrpc.
			ChannelEventType_CHANNEL_EVENT_TYPE_CHANNEL_ACTIVE
		request.ChannelPointTxid = event.ChannelPointHash[:]
		request.ChannelPointIndex = event.ChannelPointIndex

		return request, "channel-active", nil

	case *arkchannel.Materialize:
		request.EventType = arkchannelrpc.
			ChannelEventType_CHANNEL_EVENT_TYPE_MATERIALIZE

		return request, "materialize", nil

	case *arkchannel.BackingPublished:
		request.EventType = arkchannelrpc.
			ChannelEventType_CHANNEL_EVENT_TYPE_BACKING_PUBLISHED
		request.ChannelPointTxid = event.TxID[:]

		return request, "backing-published", nil

	default:
		return nil, "", fmt.Errorf("unsupported remote channel "+
			"event %T", event)
	}
}

// channelEventFromRPC parses one supported funding barrier.
func channelEventFromRPC(request *arkchannelrpc.ApplyChannelEventRequest) (
	arkchannel.Event, error) {

	switch request.GetEventType() {
	case arkchannelrpc.
		ChannelEventType_CHANNEL_EVENT_TYPE_FUNDING_FINALIZED:

		party := arkchannel.Party(request.GetParty())
		if party != arkchannel.PartyClient &&
			party != arkchannel.PartyHub {
			return nil, fmt.Errorf("invalid funding party %d",
				party)
		}

		return &arkchannel.FundingFinalized{Party: party}, nil

	case arkchannelrpc.ChannelEventType_CHANNEL_EVENT_TYPE_OOR_FINALIZED:
		var session [32]byte
		if err := copyFixed(
			"OOR session ID", session[:], request.GetOorSessionId(),
		); err != nil {
			return nil, err
		}

		return &arkchannel.OORFinalized{SessionID: session}, nil

	case arkchannelrpc.
		ChannelEventType_CHANNEL_EVENT_TYPE_RECOVERY_PACKAGE_INSTALLED:
		return &arkchannel.RecoveryPackageInstalled{}, nil

	case arkchannelrpc.ChannelEventType_CHANNEL_EVENT_TYPE_CHANNEL_ACTIVE:
		hash, err := rpcHash(
			"channel point txid", request.GetChannelPointTxid(),
		)
		if err != nil {
			return nil, err
		}

		return &arkchannel.ChannelActive{
			ChannelPointHash:  hash,
			ChannelPointIndex: request.GetChannelPointIndex(),
		}, nil

	case arkchannelrpc.ChannelEventType_CHANNEL_EVENT_TYPE_MATERIALIZE:
		return &arkchannel.Materialize{}, nil

	case arkchannelrpc.
		ChannelEventType_CHANNEL_EVENT_TYPE_BACKING_PUBLISHED:

		hash, err := rpcHash(
			"channel point txid", request.GetChannelPointTxid(),
		)
		if err != nil {
			return nil, err
		}

		return &arkchannel.BackingPublished{TxID: hash}, nil

	default:
		return nil, fmt.Errorf("unsupported channel event type %d",
			request.GetEventType())
	}
}

// ChannelRecoveryToRPC serializes the endpoint-neutral recovery package.
func ChannelRecoveryToRPC(recovery arkchannel.RecoveryPackage) (
	*arkchannelrpc.ChannelRecoveryPackage, error) {

	if recovery.Descriptor.ChainDepth > math.MaxInt32 {
		return nil, fmt.Errorf("channel recovery chain depth is out " +
			"of range")
	}
	desc := recovery.Descriptor
	message := &arkchannelrpc.ChannelRecoveryPackage{
		SourceDescriptor: &arkchannelrpc.ChannelRecoveryDescriptor{
			RoundId:             desc.RoundID,
			CommitmentTxid:      desc.CommitmentTxID[:],
			BatchExpiry:         desc.BatchExpiry,
			ChainDepth:          int32(desc.ChainDepth),
			CreatedHeight:       desc.CreatedHeight,
			ConstructionVersion: desc.ConstructionVersion,
		},
		OorPackages: make(
			[]*arkchannelrpc.ChannelRecoveryOORPackage, 0,
			len(recovery.Packages),
		),
	}
	for i := range desc.Ancestry {
		entry := desc.Ancestry[i]
		message.SourceDescriptor.Ancestry = append(
			message.SourceDescriptor.Ancestry,
			&arkchannelrpc.ChannelRecoveryAncestry{
				TreePath:         entry.TreePath,
				CommitmentTxid:   entry.CommitmentTxID[:],
				InputIndices:     entry.InputIndices,
				TreeDepth:        entry.TreeDepth,
				CommitmentHeight: entry.CommitmentHeight,
			},
		)
	}
	for i := range recovery.Packages {
		entry := recovery.Packages[i]
		message.OorPackages = append(
			message.OorPackages,
			&arkchannelrpc.ChannelRecoveryOORPackage{
				SessionId:       entry.SessionID[:],
				Direction:       entry.Direction,
				ArkPsbt:         entry.ArkPSBT,
				CheckpointPsbts: entry.Checkpoints,
			},
		)
	}

	return message, nil
}

// ChannelRecoveryFromRPC parses one recovery package and validates its target
// binding before storage sees any artifact.
func ChannelRecoveryFromRPC(message *arkchannelrpc.ChannelRecoveryPackage,
	source arkchannel.VTXOBinding) (arkchannel.RecoveryPackage, error) {

	if message == nil || message.GetSourceDescriptor() == nil {
		return arkchannel.RecoveryPackage{}, fmt.Errorf("channel " +
			"recovery package is required")
	}
	descMessage := message.GetSourceDescriptor()
	commitmentTxID, err := rpcHash(
		"recovery commitment txid", descMessage.GetCommitmentTxid(),
	)
	if err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	recovery := arkchannel.RecoveryPackage{
		Descriptor: arkchannel.RecoveryDescriptor{
			RoundID:        descMessage.GetRoundId(),
			CommitmentTxID: commitmentTxID,
			BatchExpiry:    descMessage.GetBatchExpiry(),
			ChainDepth:     int(descMessage.GetChainDepth()),
			CreatedHeight:  descMessage.GetCreatedHeight(),
			ConstructionVersion: descMessage.
				GetConstructionVersion(),
		},
	}
	for i, entry := range descMessage.GetAncestry() {
		if entry == nil {
			return arkchannel.RecoveryPackage{}, fmt.Errorf(
				"channel recovery ancestry %d is nil", i)
		}
		txID, err := rpcHash(
			"recovery ancestry commitment txid",
			entry.GetCommitmentTxid(),
		)
		if err != nil {
			return arkchannel.RecoveryPackage{}, err
		}
		recovery.Descriptor.Ancestry = append(
			recovery.Descriptor.Ancestry,
			arkchannel.RecoveryAncestry{
				TreePath: append(
					[]byte(nil), entry.GetTreePath()...,
				),
				CommitmentTxID: txID,
				InputIndices: append(
					[]uint32(nil),
					entry.GetInputIndices()...,
				),
				TreeDepth:        entry.GetTreeDepth(),
				CommitmentHeight: entry.GetCommitmentHeight(),
			},
		)
	}
	for i, entry := range message.GetOorPackages() {
		if entry == nil {
			return arkchannel.RecoveryPackage{}, fmt.Errorf(
				"channel recovery OOR package %d is nil", i)
		}
		sessionID, err := rpcHash(
			"recovery OOR session ID", entry.GetSessionId(),
		)
		if err != nil {
			return arkchannel.RecoveryPackage{}, err
		}
		checkpoints := make([][]byte, len(entry.GetCheckpointPsbts()))
		for j := range entry.GetCheckpointPsbts() {
			checkpoints[j] = append(
				[]byte(nil), entry.GetCheckpointPsbts()[j]...,
			)
		}
		recovery.Packages = append(
			recovery.Packages, arkchannel.RecoveryOORPackage{
				SessionID: sessionID,
				Direction: entry.GetDirection(),
				ArkPSBT: append(
					[]byte(nil), entry.GetArkPsbt()...,
				),
				Checkpoints: checkpoints,
			},
		)
	}
	if err := recovery.Validate(source); err != nil {
		return arkchannel.RecoveryPackage{}, err
	}

	return recovery, nil
}

// ReceiveClaimRecoverySourceToRPC serializes one swap-authorized vHTLC source.
func ReceiveClaimRecoverySourceToRPC(
	source arkchannel.ReceiveClaimRecoverySource) (
	*arkchannelrpc.ReceiveClaimRecoverySource, error) {

	if err := source.Validate(); err != nil {
		return nil, err
	}

	return &arkchannelrpc.ReceiveClaimRecoverySource{
		Txid:        source.OutPoint.Hash[:],
		OutputIndex: source.OutPoint.Index, AmountSat: int64(
			source.Amount,
		),
		PkScript: append([]byte(nil), source.PkScript...),
	}, nil
}

// ReceiveClaimRecoverySourceFromRPC parses one swap-authorized vHTLC source.
func ReceiveClaimRecoverySourceFromRPC(
	message *arkchannelrpc.ReceiveClaimRecoverySource) (
	arkchannel.ReceiveClaimRecoverySource, error) {

	if message == nil {
		return arkchannel.ReceiveClaimRecoverySource{}, fmt.Errorf(
			"receive-claim recovery source is required")
	}
	txID, err := rpcHash("receive-claim recovery txid", message.GetTxid())
	if err != nil {
		return arkchannel.ReceiveClaimRecoverySource{}, err
	}
	source := arkchannel.ReceiveClaimRecoverySource{
		OutPoint: wire.OutPoint{
			Hash: txID, Index: message.GetOutputIndex(),
		},
		Amount:   btcutil.Amount(message.GetAmountSat()),
		PkScript: append([]byte(nil), message.GetPkScript()...),
	}
	if err := source.Validate(); err != nil {
		return arkchannel.ReceiveClaimRecoverySource{}, err
	}

	return source, nil
}

// recordSummaryFromRPC validates the immutable identity in a peer response.
func recordSummaryFromRPC(terms arkchannel.Terms,
	message *arkchannelrpc.ArkChannel) (arkchannel.Record, error) {

	if message == nil {
		return arkchannel.Record{}, fmt.Errorf("funding peer " +
			"returned an empty channel")
	}
	id, err := rpcChannelID(message.GetChannelId())
	if err != nil {
		return arkchannel.Record{}, err
	}
	if id != terms.ID {
		return arkchannel.Record{}, fmt.Errorf("funding peer " +
			"returned another channel")
	}

	return arkchannel.Record{
		Snapshot: arkchannel.Snapshot{
			Terms: terms,
		},
		Revision: message.GetRevision(),
	}, nil
}

// rpcPaymentHash parses one fixed-width Lightning payment hash.
func rpcPaymentHash(raw []byte) (lntypes.Hash, error) {
	var hash lntypes.Hash
	if err := copyFixed("payment hash", hash[:], raw); err != nil {
		return lntypes.Hash{}, err
	}

	return hash, nil
}

// positiveRPCAmount parses a strictly positive signed wire amount.
func positiveRPCAmount(name string, value int64) (btcutil.Amount, error) {
	amount, err := rpcAmount(name, value)
	if err != nil {
		return 0, err
	}
	if amount == 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}

	return amount, nil
}

// copyFixed copies one exact-width byte field.
func copyFixed(name string, destination, source []byte) error {
	if len(source) != len(destination) {
		return fmt.Errorf("%s must be %d bytes", name, len(destination))
	}
	copy(destination, source)

	return nil
}

// parseCompressedKey validates one serialized secp256k1 public key.
func parseCompressedKey(name string, key [33]byte) (*btcec.PublicKey, error) {
	parsed, err := btcec.ParsePubKey(key[:])
	if err != nil {
		return nil, fmt.Errorf("invalid %s key: %w", name, err)
	}

	return parsed, nil
}

var _ ProcessFundingPeer = (*MailboxFundingPeer)(nil)

//nolint:ll // Keeping the generated interface name explicit aids API audits.
var _ arkchannelrpc.ArkChannelFundingPeerServiceMailboxServer = (*FundingPeerRPCServer)(nil)
