package swaps

import (
	"bytes"
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/darepo-client/rpc/restclient"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/grpc"
)

// GRPCSwapServerConn implements SwapServerConn using the shared generated
// swaprpc client stubs.
type GRPCSwapServerConn struct {
	client swaprpc.SwapServiceClient
}

const wireCreditFundingLightningReceive = swaprpc.
	CreditFundingSource_CREDIT_FUNDING_SOURCE_LIGHTNING_RECEIVE

// NewGRPCSwapServerConn creates a gRPC-backed SwapServerConn from one connected
// gRPC client connection.
func NewGRPCSwapServerConn(conn grpc.ClientConnInterface) *GRPCSwapServerConn {
	return &GRPCSwapServerConn{
		client: swaprpc.NewSwapServiceClient(conn),
	}
}

// NewRESTSwapServerConn creates a REST-backed SwapServerConn for one
// grpc-gateway base address.
func NewRESTSwapServerConn(addr string,
	opts ...restclient.Option) *GRPCSwapServerConn {

	return &GRPCSwapServerConn{
		client: restclient.NewSwapServiceClient(addr, opts...),
	}
}

// RequestChannelID asks the swap server for one route hint for a
// Lightning-to-Ark receive flow.
func (g *GRPCSwapServerConn) RequestChannelID(ctx context.Context,
	vhtlcPubkey *btcec.PublicKey, paymentHash lntypes.Hash,
	amountSat btcutil.Amount, expirySeconds uint32) (*OutSwapQuote, error) {

	if vhtlcPubkey == nil {
		return nil, fmt.Errorf("vHTLC pubkey must be provided")
	}
	if amountSat <= 0 {
		return nil, fmt.Errorf("receive amount must be positive")
	}

	resp, err := g.client.RequestChannelId(
		ctx, &swaprpc.RequestChannelIdRequest{
			ExpirySeconds: expirySeconds,
			ClientVhtlcPubkey: vhtlcPubkey.
				SerializeCompressed(),
			PaymentHash: append(
				[]byte(nil), paymentHash[:]...,
			),
			AmountSat: uint64(amountSat),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("RequestChannelId RPC: %w", err)
	}

	hintPath, err := routeHintPathFromProto(resp.GetRouteHintPath())
	if err != nil {
		return nil, err
	}
	if len(hintPath) == 0 {
		return nil, fmt.Errorf("route hint path must be provided")
	}

	settlementType, err := settlementTypeFromProto(resp.GetSettlementType())
	if err != nil {
		return nil, err
	}

	requestedAmountSat := resp.GetRequestedAmountSat()
	if requestedAmountSat == 0 {
		requestedAmountSat = uint64(amountSat)
	}
	vhtlcAmountSat := resp.GetVhtlcAmountSat()
	if vhtlcAmountSat == 0 {
		vhtlcAmountSat = requestedAmountSat
	}

	return &OutSwapQuote{
		RouteHintPath:      hintPath,
		ReceiveAmountSat:   btcutil.Amount(requestedAmountSat),
		PayerFeeMsat:       resp.GetPayerFeeMsat(),
		RequestedAmountSat: requestedAmountSat,
		AvailableCreditSat: resp.GetAvailableCreditSat(),
		AttachedCreditSat:  resp.GetAttachedCreditSat(),
		VHTLCAmountSat:     vhtlcAmountSat,
		DustLimitSat:       resp.GetDustLimitSat(),
		SettlementType:     settlementType,
	}, nil
}

// AcknowledgeOutSwapHTLC tells the swap server this receiver validated and
// durably accepted the out-swap HTLC event.
func (g *GRPCSwapServerConn) AcknowledgeOutSwapHTLC(ctx context.Context,
	paymentHash lntypes.Hash, vhtlcPubkey *btcec.PublicKey) error {

	if vhtlcPubkey == nil {
		return fmt.Errorf("vHTLC pubkey must be provided")
	}

	_, err := g.client.AcknowledgeOutSwapHtlc(
		ctx, &swaprpc.AcknowledgeOutSwapHtlcRequest{
			PaymentHash: append(
				[]byte(nil), paymentHash[:]...,
			),
			ClientVhtlcPubkey: vhtlcPubkey.SerializeCompressed(),
		},
	)
	if err != nil {
		return fmt.Errorf("AcknowledgeOutSwapHtlc RPC: %w", err)
	}

	return nil
}

// CreateInSwap initiates one Ark-to-Lightning swap on the swap server.
func (g *GRPCSwapServerConn) CreateInSwap(ctx context.Context, invoice string,
	maxFeeSat uint64, clientVhtlcPubkey *btcec.PublicKey) (*InSwapConfig,
	error) {

	return g.CreateInSwapWithCredits(
		ctx, invoice, maxFeeSat, clientVhtlcPubkey, nil, 0,
	)
}

// CreateInSwapWithCredits initiates one Ark-to-Lightning swap and authorizes
// the server to reserve credits from accountPubKey when maxCreditSat is
// non-zero or credit is mandatory.
func (g *GRPCSwapServerConn) CreateInSwapWithCredits(ctx context.Context,
	invoice string, maxFeeSat uint64, clientVhtlcPubkey *btcec.PublicKey,
	accountPubKey []byte, maxCreditSat uint64) (*InSwapConfig, error) {

	if clientVhtlcPubkey == nil {
		return nil, fmt.Errorf("client vHTLC pubkey must be provided")
	}

	resp, err := g.client.CreateInSwap(
		ctx, &swaprpc.CreateInSwapRequest{
			Invoice:   invoice,
			MaxFeeSat: maxFeeSat,
			ClientVhtlcPubkey: clientVhtlcPubkey.
				SerializeCompressed(),
			AccountPubkey: append([]byte(nil), accountPubKey...),
			MaxCreditSat:  maxCreditSat,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("CreateInSwap RPC: %w", err)
	}

	return inSwapConfigFromProto(resp)
}

// QuoteInSwap previews one Ark-to-Lightning swap on the swap server without
// creating durable state.
func (g *GRPCSwapServerConn) QuoteInSwap(ctx context.Context, invoice string,
	maxFeeSat uint64) (*InSwapQuote, error) {

	return g.QuoteInSwapWithCredits(ctx, invoice, maxFeeSat, nil, 0)
}

// QuoteInSwapWithCredits previews one Ark-to-Lightning swap with optional
// credit use.
func (g *GRPCSwapServerConn) QuoteInSwapWithCredits(ctx context.Context,
	invoice string, maxFeeSat uint64, accountPubKey []byte,
	maxCreditSat uint64) (*InSwapQuote, error) {

	resp, err := g.client.QuoteInSwap(
		ctx, &swaprpc.QuoteInSwapRequest{
			Invoice:       invoice,
			MaxFeeSat:     maxFeeSat,
			AccountPubkey: append([]byte(nil), accountPubKey...),
			MaxCreditSat:  maxCreditSat,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("QuoteInSwap RPC: %w", err)
	}

	return inSwapQuoteFromProto(resp)
}

// CreateCredit starts one server-owned credit funding operation.
func (g *GRPCSwapServerConn) CreateCredit(ctx context.Context,
	accountPubKey []byte, req CreateCreditRequest) (*CreditOperation,
	error) {

	source, err := creditFundingSourceToProto(req.Source)
	if err != nil {
		return nil, err
	}

	resp, err := g.client.CreateCredit(ctx, &swaprpc.CreateCreditRequest{
		AccountPubkey:  append([]byte(nil), accountPubKey...),
		IdempotencyKey: req.IdempotencyKey,
		Source:         source,
		AmountSat:      req.AmountSat,
		Memo:           req.Memo,
	})
	if err != nil {
		return nil, fmt.Errorf("CreateCredit RPC: %w", err)
	}

	op := creditOperationFromCreate(resp)

	return &op, nil
}

// RedeemCredit materializes available credits back into an Ark output.
func (g *GRPCSwapServerConn) RedeemCredit(ctx context.Context,
	accountPubKey []byte, req RedeemCreditRequest) (*CreditRedemption,
	error) {

	resp, err := g.client.RedeemCredit(ctx, &swaprpc.RedeemCreditRequest{
		AccountPubkey:  append([]byte(nil), accountPubKey...),
		IdempotencyKey: req.IdempotencyKey,
		AmountSat:      req.AmountSat,
		DestinationPubkey: append(
			[]byte(nil), req.DestinationPubKey...,
		),
	})
	if err != nil {
		return nil, fmt.Errorf("RedeemCredit RPC: %w", err)
	}

	op := CreditOperation{
		OperationID: resp.GetOperationId(),
		Type:        CreditOperationRedemption,
		State:       creditStateFromProto(resp.GetState()),
		AmountSat:   resp.GetRedeemedSat(),
		SessionID:   resp.GetSessionId(),
	}

	return &CreditRedemption{
		Operation:   op,
		DebitedSat:  resp.GetDebitedSat(),
		RedeemedSat: resp.GetRedeemedSat(),
		SessionID:   resp.GetSessionId(),
	}, nil
}

// ListCredits returns the account's server-authoritative credit snapshot.
func (g *GRPCSwapServerConn) ListCredits(ctx context.Context,
	accountPubKey []byte, limit uint32) (*CreditSnapshot, error) {

	resp, err := g.client.ListCredits(ctx, &swaprpc.ListCreditsRequest{
		AccountPubkey: append([]byte(nil), accountPubKey...),
		Limit:         limit,
	})
	if err != nil {
		return nil, fmt.Errorf("ListCredits RPC: %w", err)
	}

	snapshot := &CreditSnapshot{
		FinalizedSat: resp.GetFinalizedSat(),
		ReservedSat:  resp.GetReservedSat(),
		AvailableSat: resp.GetAvailableSat(),
	}
	for _, op := range resp.GetOperations() {
		snapshot.Operations = append(
			snapshot.Operations, creditOperationFromProto(op),
		)
	}
	for _, entry := range resp.GetLedgerEntries() {
		snapshot.LedgerEntries = append(
			snapshot.LedgerEntries,
			creditLedgerEntryFromProto(entry),
		)
	}

	return snapshot, nil
}

// AuthorizeInSwapRefund asks the swap server to sign one prepared cooperative
// in-swap refund.
func (g *GRPCSwapServerConn) AuthorizeInSwapRefund(ctx context.Context,
	paymentHash lntypes.Hash, vhtlcOutpoint string, vhtlcAmountSat int64,
	vhtlcPolicyTemplate, refundSpendPath, checkpointPSBT []byte) (
	*InSwapRefundAuthorization, error) {

	if vhtlcOutpoint == "" {
		return nil, fmt.Errorf("vHTLC outpoint must be provided")
	}
	if vhtlcAmountSat <= 0 {
		return nil, fmt.Errorf("vHTLC amount must be positive")
	}

	resp, err := g.client.AuthorizeInSwapRefund(
		ctx, &swaprpc.AuthorizeInSwapRefundRequest{
			PaymentHash:    append([]byte(nil), paymentHash[:]...),
			VhtlcOutpoint:  vhtlcOutpoint,
			VhtlcAmountSat: uint64(vhtlcAmountSat),
			VhtlcPolicyTemplate: append(
				[]byte(nil), vhtlcPolicyTemplate...,
			),
			RefundSpendPath: append(
				[]byte(nil), refundSpendPath...,
			),
			CheckpointPsbt: append([]byte(nil), checkpointPSBT...),
		},
	)
	if err != nil {
		return nil, err
	}

	sig := resp.GetSignature()
	if sig == nil {
		return nil, fmt.Errorf("AuthorizeInSwapRefund RPC returned " +
			"empty signature")
	}

	return &InSwapRefundAuthorization{
		Signature: TaprootScriptSignature{
			PubKey: append([]byte(nil), sig.GetPubkey()...),
			WitnessScript: append(
				[]byte(nil), sig.GetWitnessScript()...,
			),
			Signature: append([]byte(nil), sig.GetSignature()...),
			SigHash:   sig.GetSighash(),
		},
		FailureReason: resp.GetFailureReason(),
	}, nil
}

// SignInSwapForfeit asks the swap server to sign its participant share for one
// exact in-swap refresh forfeit transaction.
func (g *GRPCSwapServerConn) SignInSwapForfeit(ctx context.Context,
	payload *ForfeitSignaturePayload) (*ForfeitParticipantSignature,
	error) {

	protoPayload, err := forfeitSignaturePayloadToProto(payload)
	if err != nil {
		return nil, err
	}

	resp, err := g.client.SignInSwapForfeit(
		ctx, &swaprpc.SignInSwapForfeitRequest{
			Payload: protoPayload,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("SignInSwapForfeit RPC: %w", err)
	}

	sig, err := forfeitParticipantSignatureFromProto(resp.GetSignature())
	if err != nil {
		return nil, fmt.Errorf("SignInSwapForfeit RPC returned "+
			"invalid signature: %w", err)
	}

	return sig, nil
}

// SubmitOutSwapForfeitSignature submits the receiver participant signature for
// one mailbox-delivered out-swap refresh request.
func (g *GRPCSwapServerConn) SubmitOutSwapForfeitSignature(ctx context.Context,
	payload *ForfeitSignaturePayload,
	signature *ForfeitParticipantSignature) error {

	protoPayload, err := forfeitSignaturePayloadToProto(payload)
	if err != nil {
		return err
	}
	protoSignature, err := forfeitParticipantSignatureToProto(signature)
	if err != nil {
		return err
	}

	_, err = g.client.SubmitOutSwapForfeitSignature(
		ctx, &swaprpc.SubmitOutSwapForfeitSignatureRequest{
			Payload:   protoPayload,
			Signature: protoSignature,
		},
	)
	if err != nil {
		return fmt.Errorf("SubmitOutSwapForfeitSignature RPC: %w", err)
	}

	return nil
}

// Close is a no-op because the caller owns the underlying grpc.ClientConn.
func (g *GRPCSwapServerConn) Close() error {
	return nil
}

// routeHintFromProto converts one generated swaprpc route hint into the local
// SDK shape.
func routeHintFromProto(hint *swaprpc.RouteHint) (*RouteHint, error) {
	if hint == nil {
		return nil, fmt.Errorf("route hint must be provided")
	}

	routeHint := &RouteHint{
		NodeID:          append([]byte(nil), hint.GetNodeId()...),
		ChannelID:       hint.GetChannelId(),
		FeeBaseMsat:     hint.GetFeeBaseMsat(),
		FeePropPpm:      hint.GetFeeProportionalPpm(),
		CltvExpiryDelta: hint.GetCltvExpiryDelta(),
	}

	if err := validateRouteHint(routeHint); err != nil {
		return nil, err
	}

	return routeHint, nil
}

// routeHintPathFromProto converts one generated route-hint path into the SDK
// shape while preserving hop order.
func routeHintPathFromProto(hints []*swaprpc.RouteHint) ([]*RouteHint, error) {
	if len(hints) == 0 {
		return nil, nil
	}

	routeHintPath := make([]*RouteHint, 0, len(hints))
	for i, hint := range hints {
		routeHint, err := routeHintFromProto(hint)
		if err != nil {
			return nil, fmt.Errorf("route hint path hop %d: %w", i,
				err)
		}

		routeHintPath = append(routeHintPath, routeHint)
	}

	return routeHintPath, nil
}

// vhtlcConfigFromProto converts one generated swaprpc vHTLC config into the
// local SDK shape.
func vhtlcConfigFromProto(cfg *swaprpc.VHTLCConfig) (*VHTLCConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("vHTLC config must be provided")
	}

	if cfg.GetRefundLocktime() == 0 {
		return nil, fmt.Errorf("vHTLC refund locktime must be set")
	}

	if cfg.GetUnilateralClaimDelay() == 0 {
		return nil, fmt.Errorf("vHTLC unilateral claim delay must be " +
			"set")
	}

	if cfg.GetUnilateralRefundDelay() == 0 {
		return nil, fmt.Errorf("vHTLC unilateral refund delay must " +
			"be set")
	}

	if cfg.GetUnilateralRefundWithoutReceiverDelay() == 0 {
		return nil, fmt.Errorf("vHTLC unilateral " +
			"refund-without-receiver delay must be set")
	}

	if len(cfg.GetSwapserverPubkey()) == 0 {
		return nil, fmt.Errorf("vHTLC swap server pubkey must be set")
	}

	return &VHTLCConfig{
		RefundLocktime:        cfg.GetRefundLocktime(),
		UnilateralClaimDelay:  cfg.GetUnilateralClaimDelay(),
		UnilateralRefundDelay: cfg.GetUnilateralRefundDelay(),
		UnilateralRefundWithoutReceiverDelay: cfg.
			GetUnilateralRefundWithoutReceiverDelay(),
		SwapServerPubkey: append(
			[]byte(nil), cfg.GetSwapserverPubkey()...,
		),
	}, nil
}

// outSwapHtlcEventFromProto converts one generated funded-HTLC event into the
// local SDK shape.
func outSwapHtlcEventFromProto(event *swaprpc.OutSwapHtlcEvent) (
	*OutSwapHtlcEvent, error) {

	if event == nil {
		return nil, fmt.Errorf("out-swap HTLC event must be provided")
	}
	if len(event.GetPaymentHash()) != lntypes.HashSize {
		return nil, fmt.Errorf("out-swap event payment hash must be "+
			"%d bytes", lntypes.HashSize)
	}
	if event.GetAmountSat() > math.MaxInt64 {
		return nil, fmt.Errorf("out-swap event amount exceeds int64")
	}

	var paymentHash lntypes.Hash
	copy(paymentHash[:], event.GetPaymentHash())

	cfg, err := vhtlcConfigFromProto(event.GetVhtlcConfig())
	if err != nil {
		return nil, err
	}

	var parts []OutSwapHtlcPart
	for _, part := range event.GetParts() {
		if part == nil {
			return nil, fmt.Errorf("out-swap event part must be " +
				"provided")
		}
		if len(part.GetOnionBlob()) == 0 {
			return nil, fmt.Errorf("out-swap event part missing " +
				"onion blob")
		}

		parts = append(parts, OutSwapHtlcPart{
			AmountMsat: lnwire.MilliSatoshi(part.GetAmountMsat()),
			OnionBlob: append(
				[]byte(nil), part.GetOnionBlob()...,
			),
		})
	}

	return &OutSwapHtlcEvent{
		PaymentHash:        paymentHash,
		AmountSat:          int64(event.GetAmountSat()),
		RequestedAmountSat: event.GetRequestedAmountSat(),
		AttachedCreditSat:  event.GetAttachedCreditSat(),
		OnionBlob: append(
			[]byte(nil), event.GetOnionBlob()...,
		),
		VHTLCConfig: *cfg,
		Parts:       parts,
	}, nil
}

// inArkHtlcEventFromProto converts one generated p2p event into the local SDK
// shape.
func inArkHtlcEventFromProto(event *swaprpc.InArkHtlcEvent) (*InArkHtlcEvent,
	error) {

	if event == nil {
		return nil, fmt.Errorf("in-ark HTLC event must be provided")
	}
	if len(event.GetPaymentHash()) != lntypes.HashSize {
		return nil, fmt.Errorf("in-ark event payment hash must be "+
			"%d bytes", lntypes.HashSize)
	}
	if event.GetAmountSat() > math.MaxInt64 ||
		event.GetVhtlcAmountSat() > math.MaxInt64 {
		return nil, fmt.Errorf("in-ark event amount exceeds int64")
	}

	var paymentHash lntypes.Hash
	copy(paymentHash[:], event.GetPaymentHash())

	senderKey, err := btcec.ParsePubKey(event.GetSenderPubkey())
	if err != nil {
		return nil, fmt.Errorf("parse in-ark sender pubkey: %w", err)
	}

	cfg, err := vhtlcConfigFromProto(event.GetVhtlcConfig())
	if err != nil {
		return nil, err
	}

	return &InArkHtlcEvent{
		PaymentHash:    paymentHash,
		AmountSat:      int64(event.GetAmountSat()),
		SenderPubkey:   senderKey,
		VHTLCConfig:    *cfg,
		VHTLCOutpoint:  event.GetVhtlcOutpoint(),
		VHTLCAmountSat: int64(event.GetVhtlcAmountSat()),
	}, nil
}

// forfeitSignaturePayloadToProto converts the SDK signing transcript into the
// wire shape used by the swap server.
func forfeitSignaturePayloadToProto(payload *ForfeitSignaturePayload) (
	*swaprpc.ForfeitSignaturePayload, error) {

	if payload == nil {
		return nil, fmt.Errorf("forfeit signature payload must be " +
			"provided")
	}
	if len(payload.RequestID) == 0 {
		return nil, fmt.Errorf("forfeit signature request id must be " +
			"provided")
	}
	if payload.VHTLCOutpoint == "" {
		return nil, fmt.Errorf("vHTLC outpoint must be provided")
	}
	if payload.VHTLCAmountSat <= 0 {
		return nil, fmt.Errorf("vHTLC amount must be positive")
	}
	if payload.ConnectorOutpoint == "" {
		return nil, fmt.Errorf("connector outpoint must be provided")
	}
	if payload.ConnectorAmountSat <= 0 {
		return nil, fmt.Errorf("connector amount must be positive")
	}

	return &swaprpc.ForfeitSignaturePayload{
		RequestId: append([]byte(nil), payload.RequestID...),
		PaymentHash: append(
			[]byte(nil), payload.PaymentHash[:]...,
		),
		VhtlcOutpoint:       payload.VHTLCOutpoint,
		VhtlcAmountSat:      uint64(payload.VHTLCAmountSat),
		VhtlcPkScript:       bytes.Clone(payload.VHTLCPkScript),
		VhtlcPolicyTemplate: bytes.Clone(payload.VHTLCPolicyTemplate),
		ForfeitSpendPath: append(
			[]byte(nil), payload.ForfeitSpendPath...,
		),
		UnsignedForfeitTx: append(
			[]byte(nil), payload.UnsignedForfeitTx...,
		),
		ConnectorOutpoint:  payload.ConnectorOutpoint,
		ConnectorAmountSat: uint64(payload.ConnectorAmountSat),
		ConnectorPkScript: append(
			[]byte(nil), payload.ConnectorPkScript...,
		),
		ServerForfeitPkScript: append(
			[]byte(nil), payload.ServerForfeitPkScript...,
		),
	}, nil
}

// forfeitSignaturePayloadFromProto converts a wire signing transcript into the
// SDK shape used by receive sessions.
func forfeitSignaturePayloadFromProto(
	payload *swaprpc.ForfeitSignaturePayload) (*ForfeitSignaturePayload,
	error) {

	if payload == nil {
		return nil, fmt.Errorf("forfeit signature payload must be " +
			"provided")
	}
	if len(payload.GetRequestId()) == 0 {
		return nil, fmt.Errorf("forfeit signature request id must be " +
			"provided")
	}
	if len(payload.GetPaymentHash()) != lntypes.HashSize {
		return nil, fmt.Errorf("forfeit signature payment hash must "+
			"be %d bytes", lntypes.HashSize)
	}
	if payload.GetVhtlcOutpoint() == "" {
		return nil, fmt.Errorf("vHTLC outpoint must be provided")
	}
	if payload.GetVhtlcAmountSat() == 0 ||
		payload.GetVhtlcAmountSat() > math.MaxInt64 {
		return nil, fmt.Errorf("vHTLC amount must fit positive int64")
	}
	if payload.GetConnectorOutpoint() == "" {
		return nil, fmt.Errorf("connector outpoint must be provided")
	}
	if payload.GetConnectorAmountSat() == 0 ||
		payload.GetConnectorAmountSat() > math.MaxInt64 {
		return nil, fmt.Errorf("connector amount must fit positive " +
			"int64")
	}

	var paymentHash lntypes.Hash
	copy(paymentHash[:], payload.GetPaymentHash())

	return &ForfeitSignaturePayload{
		RequestID:      append([]byte(nil), payload.GetRequestId()...),
		PaymentHash:    paymentHash,
		VHTLCOutpoint:  payload.GetVhtlcOutpoint(),
		VHTLCAmountSat: int64(payload.GetVhtlcAmountSat()),
		VHTLCPkScript:  bytes.Clone(payload.GetVhtlcPkScript()),
		VHTLCPolicyTemplate: append(
			[]byte(nil), payload.GetVhtlcPolicyTemplate()...,
		),
		ForfeitSpendPath: append(
			[]byte(nil), payload.GetForfeitSpendPath()...,
		),
		UnsignedForfeitTx: append(
			[]byte(nil), payload.GetUnsignedForfeitTx()...,
		),
		ConnectorOutpoint:  payload.GetConnectorOutpoint(),
		ConnectorAmountSat: int64(payload.GetConnectorAmountSat()),
		ConnectorPkScript: append(
			[]byte(nil), payload.GetConnectorPkScript()...,
		),
		ServerForfeitPkScript: append(
			[]byte(nil), payload.GetServerForfeitPkScript()...,
		),
	}, nil
}

// forfeitParticipantSignatureToProto converts a participant signature into the
// generated wire shape.
func forfeitParticipantSignatureToProto(sig *ForfeitParticipantSignature) (
	*swaprpc.ForfeitParticipantSignature, error) {

	if sig == nil {
		return nil, fmt.Errorf("forfeit participant signature must " +
			"be provided")
	}
	if len(sig.PubKey) == 0 {
		return nil, fmt.Errorf("forfeit participant pubkey must be " +
			"provided")
	}
	if len(sig.Signature) == 0 {
		return nil, fmt.Errorf("forfeit participant signature bytes " +
			"must be provided")
	}

	return &swaprpc.ForfeitParticipantSignature{
		Pubkey:    append([]byte(nil), sig.PubKey...),
		Signature: append([]byte(nil), sig.Signature...),
	}, nil
}

// forfeitParticipantSignatureFromProto converts one generated participant
// signature into the SDK shape.
func forfeitParticipantSignatureFromProto(
	sig *swaprpc.ForfeitParticipantSignature) (*ForfeitParticipantSignature,
	error) {

	if sig == nil {
		return nil, fmt.Errorf("forfeit participant signature must " +
			"be provided")
	}
	if len(sig.GetPubkey()) == 0 {
		return nil, fmt.Errorf("forfeit participant pubkey must be " +
			"provided")
	}
	if len(sig.GetSignature()) == 0 {
		return nil, fmt.Errorf("forfeit participant signature bytes " +
			"must be provided")
	}

	return &ForfeitParticipantSignature{
		PubKey:    append([]byte(nil), sig.GetPubkey()...),
		Signature: append([]byte(nil), sig.GetSignature()...),
	}, nil
}

// inSwapConfigFromProto converts one generated in-swap response into the local
// SDK shape.
func inSwapConfigFromProto(resp *swaprpc.CreateInSwapResponse) (*InSwapConfig,
	error) {

	if resp == nil {
		return nil, fmt.Errorf("in-swap response must be provided")
	}

	if len(resp.GetPaymentHash()) != lntypes.HashSize {
		return nil, fmt.Errorf("in-swap response payment hash must be "+
			"%d bytes", lntypes.HashSize)
	}

	var paymentHash lntypes.Hash
	copy(paymentHash[:], resp.GetPaymentHash())

	if resp.GetExpiry() == nil {
		return nil, fmt.Errorf("in-swap response missing expiry")
	}

	expiryTime := resp.GetExpiry().AsTime()
	if expiryTime.IsZero() {
		return nil, fmt.Errorf("in-swap response expiry must be " +
			"non-zero")
	}

	settlementType, err := settlementTypeFromProto(
		resp.GetSettlementType(),
	)
	if err != nil {
		return nil, err
	}

	creditQuote := creditQuoteFromProto(resp.GetCreditQuote())
	if settlementType == SettlementTypeCredit {
		if resp.GetAmountSat() != 0 {
			return nil, fmt.Errorf("credit in-swap response " +
				"amount must be zero")
		}

		preimage, err := lntypes.MakePreimage(resp.GetPreimage())
		if err != nil {
			return nil, fmt.Errorf("parse credit pay preimage: %w",
				err)
		}
		if preimage.Hash() != paymentHash {
			return nil, fmt.Errorf("credit pay preimage does not " +
				"match payment hash")
		}

		return &InSwapConfig{
			PaymentHash:    paymentHash,
			AmountSat:      0,
			FeeSat:         resp.GetFeeSat(),
			Expiry:         expiryTime,
			SettlementType: settlementType,
			CreditQuote:    creditQuote,
			Preimage:       &preimage,
		}, nil
	}

	if resp.GetAmountSat() == 0 {
		return nil, fmt.Errorf("in-swap response amount must be " +
			"positive")
	}
	if resp.GetAmountSat() > math.MaxInt64 {
		return nil, fmt.Errorf("in-swap response amount exceeds " +
			"int64 range")
	}
	if len(resp.GetServerPubkey()) == 0 {
		return nil, fmt.Errorf("in-swap response missing server pubkey")
	}

	serverPubkey, err := btcec.ParsePubKey(resp.GetServerPubkey())
	if err != nil {
		return nil, fmt.Errorf("parse server pubkey: %w", err)
	}

	cfg, err := vhtlcConfigFromProto(resp.GetVhtlcConfig())
	if err != nil {
		return nil, err
	}

	return &InSwapConfig{
		PaymentHash:    paymentHash,
		AmountSat:      int64(resp.GetAmountSat()),
		FeeSat:         resp.GetFeeSat(),
		ServerPubkey:   serverPubkey,
		VHTLCConfig:    *cfg,
		Expiry:         expiryTime,
		SettlementType: settlementType,
		CreditQuote:    creditQuote,
	}, nil
}

// inSwapQuoteFromProto converts one generated in-swap quote into the local SDK
// shape.
func inSwapQuoteFromProto(resp *swaprpc.QuoteInSwapResponse) (*InSwapQuote,
	error) {

	if resp == nil {
		return nil, fmt.Errorf("in-swap quote response must be " +
			"provided")
	}

	if len(resp.GetPaymentHash()) != lntypes.HashSize {
		return nil, fmt.Errorf("in-swap quote payment hash must be "+
			"%d bytes", lntypes.HashSize)
	}

	if resp.GetInvoiceAmountSat() == 0 {
		return nil, fmt.Errorf("in-swap quote invoice amount must be " +
			"positive")
	}

	if resp.GetFeeSat() > maxInt64Uint {
		return nil, fmt.Errorf("in-swap quote fee exceeds int64 range")
	}

	if resp.GetInvoiceAmountSat() > maxInt64Uint-resp.GetFeeSat() {
		return nil, fmt.Errorf("in-swap quote amount exceeds int64 " +
			"range")
	}

	if resp.GetExpiry() == nil {
		return nil, fmt.Errorf("in-swap quote missing expiry")
	}

	expiryTime := resp.GetExpiry().AsTime()
	if expiryTime.IsZero() {
		return nil, fmt.Errorf("in-swap quote expiry must be non-zero")
	}

	settlementType, err := settlementTypeFromProto(
		resp.GetSettlementType(),
	)
	if err != nil {
		return nil, err
	}

	creditQuote := creditQuoteFromProto(resp.GetCreditQuote())
	switch settlementType {
	case SettlementTypeCredit:
		if resp.GetAmountSat() != 0 {
			return nil, fmt.Errorf("credit in-swap quote amount " +
				"must be zero")
		}

	case SettlementTypeMixed:
		if creditQuote == nil {
			return nil, fmt.Errorf("mixed in-swap quote missing " +
				"credit quote")
		}
		if resp.GetAmountSat() != creditQuote.ArkFundingSat {
			return nil, fmt.Errorf("mixed in-swap quote amount %d "+
				"does not equal ark funding %d",
				resp.GetAmountSat(), creditQuote.ArkFundingSat)
		}

	default:
		if resp.GetAmountSat() == 0 {
			return nil, fmt.Errorf("in-swap quote amount must be " +
				"positive")
		}

		expectedAmount := resp.GetInvoiceAmountSat() + resp.GetFeeSat()
		if resp.GetAmountSat() != expectedAmount {
			return nil, fmt.Errorf("in-swap quote amount %d does "+
				"not equal invoice amount %d plus fee %d",
				resp.GetAmountSat(), resp.GetInvoiceAmountSat(),
				resp.GetFeeSat())
		}
	}

	var paymentHash lntypes.Hash
	copy(paymentHash[:], resp.GetPaymentHash())

	return &InSwapQuote{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: resp.GetInvoiceAmountSat(),
		AmountSat:        resp.GetAmountSat(),
		FeeSat:           resp.GetFeeSat(),
		Expiry:           expiryTime,
		SettlementType:   settlementType,
		ExceedsMaxFee:    resp.GetExceedsMaxFee(),
		CreditQuote:      creditQuote,
	}, nil
}

func creditFundingSourceToProto(source CreditFundingSource) (
	swaprpc.CreditFundingSource, error) {

	switch source {
	case CreditFundingLightningReceive:
		return wireCreditFundingLightningReceive, nil

	case CreditFundingArkTopUp:
		wireSource := swaprpc.
			CreditFundingSource_CREDIT_FUNDING_SOURCE_ARK_TOPUP

		return wireSource, nil

	default:
		return 0, fmt.Errorf("unknown credit funding source %q", source)
	}
}

func creditOperationFromCreate(
	resp *swaprpc.CreateCreditResponse) CreditOperation {

	if resp == nil {
		return CreditOperation{}
	}

	op := CreditOperation{
		OperationID: resp.GetOperationId(),
		Type:        CreditOperationFunding,
		State:       creditStateFromProto(resp.GetState()),
		AmountSat:   resp.GetAmountSat(),
		PaymentHash: creditPaymentHashFromProto(
			resp.GetPaymentHash(),
		),
		Invoice: resp.GetInvoice(),
		DestinationKey: append(
			[]byte(nil), resp.GetDestinationPubkey()...,
		),
	}
	if expiresAt := resp.GetExpiresAt(); expiresAt != nil {
		expires := expiresAt.AsTime()
		op.ExpiresAt = &expires
	}

	return op
}

func creditOperationFromProto(resp *swaprpc.CreditOperation) CreditOperation {
	if resp == nil {
		return CreditOperation{}
	}

	op := CreditOperation{
		OperationID: resp.GetOperationId(),
		Type:        creditTypeFromProto(resp.GetType()),
		State:       creditStateFromProto(resp.GetState()),
		AmountSat:   resp.GetAmountSat(),
		PaymentHash: creditPaymentHashFromProto(
			resp.GetPaymentHash(),
		),
		Invoice: resp.GetInvoice(),
		DestinationKey: append(
			[]byte(nil), resp.GetDestinationPubkey()...,
		),
		SessionID: resp.GetSessionId(),
		LastError: resp.GetLastError(),
	}
	if createdAt := resp.GetCreatedAt(); createdAt != nil {
		op.CreatedAt = createdAt.AsTime()
	}
	if updatedAt := resp.GetUpdatedAt(); updatedAt != nil {
		op.UpdatedAt = updatedAt.AsTime()
	}
	if completedAt := resp.GetCompletedAt(); completedAt != nil {
		completed := completedAt.AsTime()
		op.CompletedAt = &completed
	}

	return op
}

func creditLedgerEntryFromProto(
	resp *swaprpc.CreditLedgerEntry) CreditLedgerEntry {

	if resp == nil {
		return CreditLedgerEntry{}
	}

	entry := CreditLedgerEntry{
		EntryID:     resp.GetEntryId(),
		OperationID: resp.GetOperationId(),
		Direction:   resp.GetDirection(),
		AmountSat:   resp.GetAmountSat(),
	}
	if createdAt := resp.GetCreatedAt(); createdAt != nil {
		entry.CreatedAt = createdAt.AsTime()
	}

	return entry
}

func creditPaymentHashFromProto(raw []byte) *lntypes.Hash {
	if len(raw) != lntypes.HashSize {
		return nil
	}

	var hash lntypes.Hash
	copy(hash[:], raw)

	return &hash
}

func creditTypeFromProto(typ swaprpc.CreditOperationType) CreditOperationType {
	switch typ {
	case swaprpc.CreditOperationType_CREDIT_OPERATION_TYPE_FUNDING:
		return CreditOperationFunding

	case swaprpc.CreditOperationType_CREDIT_OPERATION_TYPE_PAY:
		return CreditOperationPay

	case swaprpc.CreditOperationType_CREDIT_OPERATION_TYPE_REDEMPTION:
		return CreditOperationRedemption

	case swaprpc.CreditOperationType_CREDIT_OPERATION_TYPE_RECEIVE:
		return CreditOperationReceive

	default:
		return ""
	}
}

func creditStateFromProto(
	state swaprpc.CreditOperationState) CreditOperationState {

	switch state {
	case swaprpc.CreditOperationState_CREDIT_OPERATION_STATE_CREATED:
		return CreditStateCreated

	case swaprpc.
		CreditOperationState_CREDIT_OPERATION_STATE_AWAITING_PAYMENT:
		return CreditStateAwaitingPayment

	case swaprpc.CreditOperationState_CREDIT_OPERATION_STATE_CREDITED:
		return CreditStateCredited

	case swaprpc.CreditOperationState_CREDIT_OPERATION_STATE_RESERVED:
		return CreditStateReserved

	case swaprpc.
		CreditOperationState_CREDIT_OPERATION_STATE_PAYING_LIGHTNING:
		return CreditStatePayingLightning

	case swaprpc.CreditOperationState_CREDIT_OPERATION_STATE_DEBITED:
		return CreditStateDebited

	case swaprpc.CreditOperationState_CREDIT_OPERATION_STATE_SENDING_OOR:
		return CreditStateSendingOOR

	case swaprpc.CreditOperationState_CREDIT_OPERATION_STATE_REDEEMED:
		return CreditStateRedeemed

	case swaprpc.CreditOperationState_CREDIT_OPERATION_STATE_RELEASED:
		return CreditStateReleased

	case swaprpc.CreditOperationState_CREDIT_OPERATION_STATE_EXPIRED:
		return CreditStateExpired

	case swaprpc.CreditOperationState_CREDIT_OPERATION_STATE_FAILED:
		return CreditStateFailed

	default:
		return ""
	}
}

func creditQuoteFromProto(resp *swaprpc.CreditQuote) *CreditQuote {
	if resp == nil {
		return nil
	}

	return &CreditQuote{
		MustUseCredit:      resp.GetMustUseCredit(),
		CreditAppliedSat:   resp.GetCreditAppliedSat(),
		CreditShortfallSat: resp.GetCreditShortfallSat(),
		CreditTopupSat:     resp.GetCreditTopupSat(),
		ArkFundingSat:      resp.GetArkFundingSat(),
	}
}

// settlementTypeFromProto converts the wire settlement enum, treating
// unspecified as Lightning for compatibility with older test fixtures.
func settlementTypeFromProto(wireType swaprpc.SettlementType) (SettlementType,
	error) {

	switch wireType {
	case swaprpc.SettlementType_SETTLEMENT_TYPE_UNSPECIFIED,
		swaprpc.SettlementType_SETTLEMENT_TYPE_LIGHTNING:
		return SettlementTypeLightning, nil

	case swaprpc.SettlementType_SETTLEMENT_TYPE_IN_ARK:
		return SettlementTypeInArk, nil

	case swaprpc.SettlementType_SETTLEMENT_TYPE_CREDIT:
		return SettlementTypeCredit, nil

	case swaprpc.SettlementType_SETTLEMENT_TYPE_MIXED:
		return SettlementTypeMixed, nil

	default:
		return "", fmt.Errorf("unknown settlement type %v", wireType)
	}
}

// Ensure GRPCSwapServerConn satisfies the SwapServerConn interface.
var _ SwapServerConn = (*GRPCSwapServerConn)(nil)
